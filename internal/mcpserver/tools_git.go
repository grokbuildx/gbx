package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	maxGitStdoutBytes = 16 << 20
	maxGitStderrBytes = 256 << 10
	maxDiffBytes      = 1 << 20
)

type limitedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		b.truncated = true
		return len(p), nil
	}
	rem := b.limit - b.buf.Len()
	if rem <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > rem {
		_, _ = b.buf.Write(p[:rem])
		b.truncated = true
		return len(p), nil
	}
	_, _ = b.buf.Write(p)
	return len(p), nil
}

func (b *limitedBuffer) Bytes() []byte  { return b.buf.Bytes() }
func (b *limitedBuffer) String() string { return b.buf.String() }

type gitResult struct {
	stdout    []byte
	stderr    string
	code      int
	truncated bool
}

func runGit(ctx context.Context, ws Workspace, stdin []byte, args ...string) (gitResult, error) {
	gitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmdArgs := append([]string{
		"-c", "core.quotepath=off",
		"-c", "color.ui=false",
		"-c", "core.fsmonitor=false",
		"-c", "core.hooksPath=/dev/null",
	}, args...)
	cmd := exec.CommandContext(gitCtx, "git", cmdArgs...)
	cmd.Dir = ws.Path
	envExtra := []string{"GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}
	if len(args) > 0 && (args[0] == "status" || args[0] == "diff" || args[0] == "log" || args[0] == "rev-parse" || args[0] == "ls-files" || args[0] == "symbolic-ref") {
		envExtra = append(envExtra, "GIT_OPTIONAL_LOCKS=0")
	}
	cmd.Env = commandEnv(envExtra...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	stdout := limitedBuffer{limit: maxGitStdoutBytes}
	stderr := limitedBuffer{limit: maxGitStderrBytes}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := gitResult{
		stdout:    stdout.Bytes(),
		stderr:    strings.TrimSpace(stderr.String()),
		truncated: stdout.truncated,
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.code = exitErr.ExitCode()
		return res, nil
	}
	return res, err
}

func gitText(ctx context.Context, ws Workspace, args ...string) (string, error) {
	res, err := runGit(ctx, ws, nil, args...)
	if err != nil {
		return "", err
	}
	if res.code != 0 {
		msg := res.stderr
		if msg == "" {
			msg = strings.TrimSpace(string(res.stdout))
		}
		return "", fmt.Errorf("git %s: %s", args[0], msg)
	}
	return strings.TrimSpace(string(res.stdout)), nil
}

func (s *Server) registerGitTools() {
	s.addTool(ToolDef{
		Name:        "git_status",
		Description: "Inspect Git branch, upstream ahead/behind counts, and staged/unstaged/untracked file changes in the workspace.",
		InputSchema: objectSchema(workspaceTargetProps()),
		Annotations: readOnlyAnnotations("Git Status"),
	}, s.toolGitStatus)

	s.addTool(ToolDef{
		Name:        "git_diff",
		Description: "View unified Git diff of unstaged changes, staged changes, or changes against a base ref.",
		InputSchema: objectSchema(withTargetProps(map[string]JSONSchema{
			"staged": {
				Type:        "boolean",
				Description: "Show staged (--cached) changes instead of unstaged working tree changes.",
			},
			"base": {
				Type:        "string",
				Description: "Optional commit or branch ref to diff against (e.g. 'HEAD~1' or 'main').",
			},
			"path": {
				Type:        "string",
				Description: "Optional workspace-relative file or directory path filter.",
			},
		})),
		Annotations: readOnlyAnnotations("Git Diff"),
	}, s.toolGitDiff)

	s.addTool(ToolDef{
		Name:        "git_log",
		Description: "View recent Git commit history in the workspace.",
		InputSchema: objectSchema(withTargetProps(map[string]JSONSchema{
			"limit": {
				Type:        "integer",
				Description: "Maximum commits to return (default 20, max 100).",
			},
			"path": {
				Type:        "string",
				Description: "Optional workspace-relative file or directory path filter.",
			},
		})),
		Annotations: readOnlyAnnotations("Git Log"),
	}, s.toolGitLog)

	s.addTool(ToolDef{
		Name:        "git_commit",
		Description: "Stage specified workspace files (if paths provided) and create a Git commit with the given message.",
		InputSchema: objectSchema(withTargetProps(map[string]JSONSchema{
			"message": {
				Type:        "string",
				Description: "Commit message.",
			},
			"paths": {
				Type:        "array",
				Description: "Optional list of workspace-relative paths to stage before committing. Omit to commit already-staged changes.",
				Items:       &JSONSchema{Type: "string"},
			},
		}), "message"),
		Annotations: mutatingAnnotations("Git Commit", false, false),
	}, s.toolGitCommit)
}

type gitFileEntry struct {
	Path       string `json:"path"`
	OrigPath   string `json:"orig_path,omitempty"`
	Index      string `json:"index"`
	Worktree   string `json:"worktree"`
	Untracked  bool   `json:"untracked,omitempty"`
	Conflicted bool   `json:"conflicted,omitempty"`
	Added      int    `json:"added"`
	Deleted    int    `json:"deleted"`
}

type gitStatusView struct {
	Repo     bool           `json:"repo"`
	Branch   string         `json:"branch,omitempty"`
	Detached bool           `json:"detached,omitempty"`
	Upstream string         `json:"upstream,omitempty"`
	Ahead    int            `json:"ahead"`
	Behind   int            `json:"behind"`
	Added    int            `json:"added"`
	Deleted  int            `json:"deleted"`
	Files    []gitFileEntry `json:"files"`
}

func collectGitStatus(ctx context.Context, ws Workspace) (gitStatusView, error) {
	status := gitStatusView{Files: []gitFileEntry{}}
	res, err := runGit(ctx, ws, nil, "status", "--porcelain=v2", "--branch", "-z", "--untracked-files=all")
	if err != nil {
		return status, fmt.Errorf("git is not available: %w", err)
	}
	if res.code != 0 {
		return status, nil
	}
	status.Repo = true
	records := strings.Split(string(res.stdout), "\x00")
	for i := 0; i < len(records); i++ {
		rec := records[i]
		switch {
		case strings.HasPrefix(rec, "# branch.head "):
			status.Branch = strings.TrimPrefix(rec, "# branch.head ")
			if status.Branch == "(detached)" {
				status.Detached = true
			}
		case strings.HasPrefix(rec, "# branch.upstream "):
			status.Upstream = strings.TrimPrefix(rec, "# branch.upstream ")
		case strings.HasPrefix(rec, "# branch.ab "):
			fields := strings.Fields(strings.TrimPrefix(rec, "# branch.ab "))
			if len(fields) == 2 {
				status.Ahead, _ = strconv.Atoi(strings.TrimPrefix(fields[0], "+"))
				status.Behind, _ = strconv.Atoi(strings.TrimPrefix(fields[1], "-"))
			}
		case strings.HasPrefix(rec, "1 "), strings.HasPrefix(rec, "u "):
			nFields := 9
			if rec[0] == 'u' {
				nFields = 11
			}
			parts := strings.SplitN(rec, " ", nFields)
			if len(parts) == nFields {
				status.Files = append(status.Files, gitFileEntry{
					Path:       parts[nFields-1],
					Index:      parts[1][:1],
					Worktree:   parts[1][1:],
					Conflicted: rec[0] == 'u',
				})
			}
		case strings.HasPrefix(rec, "2 "):
			parts := strings.SplitN(rec, " ", 10)
			if len(parts) == 10 {
				entry := gitFileEntry{Path: parts[9], Index: parts[1][:1], Worktree: parts[1][1:]}
				if i+1 < len(records) {
					entry.OrigPath = records[i+1]
					i++
				}
				status.Files = append(status.Files, entry)
			}
		case strings.HasPrefix(rec, "? "):
			status.Files = append(status.Files, gitFileEntry{
				Path:      rec[2:],
				Index:     "?",
				Worktree:  "?",
				Untracked: true,
			})
		}
	}

	counts := map[string][2]int{}
	numstat, err := runGit(ctx, ws, nil, "diff", "--numstat", "-z", "HEAD")
	if err == nil && numstat.code != 0 {
		numstat, err = runGit(ctx, ws, nil, "diff", "--numstat", "-z", "--cached")
	}
	if err == nil && numstat.code == 0 {
		nsRecs := strings.Split(string(numstat.stdout), "\x00")
		for i := 0; i < len(nsRecs); i++ {
			fields := strings.SplitN(nsRecs[i], "\t", 3)
			if len(fields) != 3 {
				continue
			}
			p := fields[2]
			if p == "" && i+2 < len(nsRecs) {
				p = nsRecs[i+2]
				i += 2
			}
			a, _ := strconv.Atoi(fields[0])
			d, _ := strconv.Atoi(fields[1])
			counts[p] = [2]int{a, d}
		}
	}
	for i, f := range status.Files {
		if f.Untracked {
			if abs, err := ws.resolve(f.Path); err == nil {
				if info, err := os.Stat(abs); err == nil && info.Size() <= maxReadBytes {
					if data, err := os.ReadFile(abs); err == nil && !isBinaryBytes(data) {
						n := bytes.Count(data, []byte("\n"))
						if len(data) > 0 && data[len(data)-1] != '\n' {
							n++
						}
						counts[f.Path] = [2]int{n, 0}
					}
				}
			}
		}
		if c, ok := counts[f.Path]; ok {
			status.Files[i].Added, status.Files[i].Deleted = c[0], c[1]
			status.Added += c[0]
			status.Deleted += c[1]
		}
	}
	return status, nil
}

func (s *Server) toolGitStatus(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		Workspace string `json:"workspace"`
		Worktree  string `json:"worktree"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", err
	}
	ws, err := s.resolveWorkspace(ctx, args.Workspace, args.Worktree)
	if err != nil {
		return "", err
	}
	st, err := collectGitStatus(ctx, ws)
	if err != nil {
		return "", err
	}
	return marshalToolJSON(st)
}

func (s *Server) toolGitDiff(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		Workspace string `json:"workspace"`
		Worktree  string `json:"worktree"`
		Staged    bool   `json:"staged"`
		Base      string `json:"base"`
		Path      string `json:"path"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", err
	}
	ws, err := s.resolveWorkspace(ctx, args.Workspace, args.Worktree)
	if err != nil {
		return "", err
	}
	gitArgs := []string{"diff", "--no-ext-diff", "--no-color"}
	if args.Staged {
		gitArgs = append(gitArgs, "--cached")
	}
	base := strings.TrimSpace(args.Base)
	if base != "" {
		if strings.HasPrefix(base, "-") {
			return "", errors.New("invalid base ref")
		}
		gitArgs = append(gitArgs, base)
	}
	if strings.TrimSpace(args.Path) != "" {
		checked, err := ws.checkedPath(args.Path)
		if err != nil {
			return "", err
		}
		gitArgs = append(gitArgs, "--", checked)
	}
	res, err := runGit(ctx, ws, nil, gitArgs...)
	if err != nil {
		return "", err
	}
	if res.code > 1 {
		return "", errors.New(firstNonEmpty(res.stderr, "git diff failed"))
	}
	diff := res.stdout
	truncated := res.truncated
	if len(diff) > maxDiffBytes {
		diff = diff[:maxDiffBytes]
		truncated = true
	}
	return marshalToolJSON(map[string]any{
		"diff":      string(diff),
		"truncated": truncated,
	})
}

func (s *Server) toolGitLog(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		Workspace string `json:"workspace"`
		Worktree  string `json:"worktree"`
		Limit     int    `json:"limit"`
		Path      string `json:"path"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", err
	}
	ws, err := s.resolveWorkspace(ctx, args.Workspace, args.Worktree)
	if err != nil {
		return "", err
	}
	limit := args.Limit
	if limit <= 0 {
		limit = 20
	} else if limit > 100 {
		limit = 100
	}
	gitArgs := []string{"log", "-n", strconv.Itoa(limit), "--format=%H%x00%h%x00%an%x00%aI%x00%s%x1e"}
	if strings.TrimSpace(args.Path) != "" {
		checked, err := ws.checkedPath(args.Path)
		if err != nil {
			return "", err
		}
		gitArgs = append(gitArgs, "--", checked)
	}
	res, err := runGit(ctx, ws, nil, gitArgs...)
	if err != nil {
		return "", err
	}
	type commitEntry struct {
		Hash    string `json:"hash"`
		Short   string `json:"short"`
		Author  string `json:"author"`
		Date    string `json:"date"`
		Subject string `json:"subject"`
	}
	commits := []commitEntry{}
	if res.code == 0 {
		for _, rec := range strings.Split(string(res.stdout), "\x1e") {
			fields := strings.Split(strings.TrimSpace(rec), "\x00")
			if len(fields) == 5 {
				commits = append(commits, commitEntry{
					Hash:    fields[0],
					Short:   fields[1],
					Author:  fields[2],
					Date:    fields[3],
					Subject: fields[4],
				})
			}
		}
	}
	return marshalToolJSON(map[string]any{"commits": commits})
}

func (s *Server) toolGitCommit(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		Workspace string   `json:"workspace"`
		Worktree  string   `json:"worktree"`
		Message   string   `json:"message"`
		Paths     []string `json:"paths"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", err
	}
	msg := strings.TrimSpace(args.Message)
	if msg == "" {
		return "", errors.New("commit message is required")
	}
	ws, err := s.resolveWorkspace(ctx, args.Workspace, args.Worktree)
	if err != nil {
		return "", err
	}
	if len(args.Paths) > 0 {
		addArgs := []string{"add", "--"}
		for _, p := range args.Paths {
			checked, err := ws.checkedPath(p)
			if err != nil {
				return "", err
			}
			addArgs = append(addArgs, checked)
		}
		addRes, err := runGit(ctx, ws, nil, addArgs...)
		if err != nil {
			return "", err
		}
		if addRes.code != 0 {
			return "", errors.New(firstNonEmpty(addRes.stderr, string(addRes.stdout), "git add failed"))
		}
	}
	commitRes, err := runGit(ctx, ws, []byte(msg), "commit", "--file=-")
	if err != nil {
		return "", err
	}
	if commitRes.code != 0 {
		return "", errors.New(firstNonEmpty(commitRes.stderr, string(commitRes.stdout), "git commit failed"))
	}
	head, _ := gitText(ctx, ws, "rev-parse", "HEAD")
	st, _ := collectGitStatus(ctx, ws)
	return marshalToolJSON(map[string]any{
		"commit": head,
		"output": strings.TrimSpace(string(commitRes.stdout)),
		"status": st,
	})
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}
