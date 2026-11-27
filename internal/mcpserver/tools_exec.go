package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/grokbuildx/gbx/internal/process"
)

const (
	defaultExecTimeoutSec = 60
	maxExecTimeoutSec     = 300
	maxExecOutputBytes    = 256 << 10 // 256 KiB per stream
)

func (s *Server) registerExecTools() {
	s.addTool(ToolDef{
		Name:        "run_command",
		Description: "Run a non-interactive build, test, or linter shell command rooted inside the workspace (or worktree) with process-group timeout protection.",
		InputSchema: objectSchema(withTargetProps(map[string]JSONSchema{
			"command": {
				Type:        "string",
				Description: "Shell command to execute (e.g. 'go test ./...' or 'npm test').",
			},
			"cwd": {
				Type:        "string",
				Description: "Optional workspace-relative subdirectory to run the command in.",
			},
			"timeout_seconds": {
				Type:        "integer",
				Description: "Optional timeout in seconds (default 60, max 300).",
			},
		}), "command"),
		Annotations: mutatingAnnotations("Run Command", true, false),
	}, s.toolRunCommand)
}

func shellCommand(command string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.Command("cmd.exe", "/d", "/s", "/c", command)
	}
	return exec.Command("/bin/sh", "-c", command)
}

func (s *Server) toolRunCommand(ctx context.Context, raw json.RawMessage) (string, error) {
	if s.cfg.DisableExec {
		return "", errors.New("run_command is disabled by --no-exec")
	}
	var args struct {
		Workspace      string `json:"workspace"`
		Worktree       string `json:"worktree"`
		Command        string `json:"command"`
		Cwd            string `json:"cwd"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", err
	}
	commandStr := strings.TrimSpace(args.Command)
	if commandStr == "" {
		return "", errors.New("command is required")
	}
	ws, err := s.resolveWorkspace(ctx, args.Workspace, args.Worktree)
	if err != nil {
		return "", err
	}
	dir := ws.Path
	if strings.TrimSpace(args.Cwd) != "" {
		resolved, err := ws.resolve(args.Cwd)
		if err != nil {
			return "", err
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return "", err
		}
		if !info.IsDir() {
			return "", fmt.Errorf("cwd %q is not a directory", ws.relative(resolved))
		}
		dir = resolved
	}

	timeoutSec := args.TimeoutSeconds
	if timeoutSec <= 0 {
		timeoutSec = defaultExecTimeoutSec
	} else if timeoutSec > maxExecTimeoutSec {
		timeoutSec = maxExecTimeoutSec
	}

	execCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	cmd := shellCommand(commandStr)
	cmd.Dir = dir
	cmd.Env = commandEnv("CI=1", "GIT_TERMINAL_PROMPT=0")
	process.ConfigureGroup(cmd)

	stdout := limitedBuffer{limit: maxExecOutputBytes}
	stderr := limitedBuffer{limit: maxExecOutputBytes}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	started := time.Now()
	if err := cmd.Start(); err != nil {
		return "", err
	}
	stopKill := context.AfterFunc(execCtx, func() {
		process.KillTree(cmd)
	})
	waitErr := cmd.Wait()
	stopKill()
	durationMs := time.Since(started).Milliseconds()

	timedOut := errors.Is(execCtx.Err(), context.DeadlineExceeded)
	exitCode := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else if timedOut {
			exitCode = -1
		} else {
			return "", waitErr
		}
	}
	if timedOut && exitCode == 0 {
		exitCode = -1
	}

	return marshalToolJSON(map[string]any{
		"exit_code":        exitCode,
		"timed_out":        timedOut,
		"duration_ms":      durationMs,
		"stdout":           stdout.String(),
		"stderr":           stderr.String(),
		"stdout_truncated": stdout.truncated,
		"stderr_truncated": stderr.truncated,
	})
}
