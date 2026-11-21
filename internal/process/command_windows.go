package process

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

var npmCmdShimPattern = regexp.MustCompile(`"%dp0%\\([^"]+\.(?:[cm]?js))"\s+%\*`)

func Command(path string, args ...string) *exec.Cmd {
	if ext := filepath.Ext(path); strings.EqualFold(ext, ".exe") || strings.EqualFold(ext, ".com") {
		return exec.Command(path, args...)
	}
	// Resolve standard npm .cmd shims directly to node.exe + JS entrypoint so
	// arguments bypass cmd.exe completely (avoiding BatBadBut / CVE-2024-24576).
	if nodePath, scriptPath, ok := resolveNpmCmdShim(path); ok {
		nodeArgs := make([]string, 0, len(args)+1)
		nodeArgs = append(nodeArgs, scriptPath)
		nodeArgs = append(nodeArgs, args...)
		return exec.Command(nodePath, nodeArgs...)
	}
	// Fallback for non-npm .cmd/.bat scripts: cmd.exe cannot safely pass raw
	// CR/LF/NUL or embedded quotes through batch %* expansion.
	for _, candidate := range append([]string{path}, args...) {
		if err := validateCmdArg(candidate); err != nil {
			cmd := exec.Command("cmd.exe")
			cmd.Err = err
			return cmd
		}
	}
	// /s removes the outer quote pair; /d and /v:off disable AutoRun and
	// delayed !variable! expansion in the caller's shell configuration.
	words := []string{escapeCmdMeta(path)}
	for _, arg := range args {
		words = append(words, escapeCmdMeta(quoteWindowsArg(arg)))
	}
	line := `"` + strings.Join(words, " ") + `"`
	cmd := exec.Command("cmd.exe", "/d", "/s", "/v:off", "/c", line)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine: syscall.EscapeArg(cmd.Path) + ` /d /s /v:off /c ` + line,
	}
	return cmd
}

func resolveNpmCmdShim(path string) (nodePath, scriptPath string, ok bool) {
	ext := filepath.Ext(path)
	if !strings.EqualFold(ext, ".cmd") && !strings.EqualFold(ext, ".bat") {
		return "", "", false
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > 64<<10 {
		return "", "", false
	}
	match := npmCmdShimPattern.FindSubmatch(raw)
	if len(match) < 2 {
		return "", "", false
	}
	dir := filepath.Dir(path)
	rel := filepath.Clean(string(match[1]))
	if filepath.IsAbs(rel) {
		return "", "", false
	}
	target := filepath.Join(dir, rel)
	if info, err := os.Stat(target); err != nil || !info.Mode().IsRegular() {
		return "", "", false
	}
	if localNode := filepath.Join(dir, "node.exe"); fileIsRegular(localNode) {
		return localNode, target, true
	}
	if nodeBin, err := exec.LookPath("node.exe"); err == nil {
		return nodeBin, target, true
	}
	return "", "", false
}

func fileIsRegular(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func validateCmdArg(value string) error {
	if strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("unsafe control character in Windows batch command argument")
	}
	if strings.ContainsRune(value, '"') {
		return fmt.Errorf("unsafe quote character in Windows batch command argument")
	}
	return nil
}

// quoteWindowsArg quotes for the native program reached through the batch
// file. Backslashes before a quote (including the closing quote) are doubled.
func quoteWindowsArg(arg string) string {
	var out strings.Builder
	out.WriteByte('"')
	slashes := 0
	for _, char := range arg {
		if char == '\\' {
			slashes++
			continue
		}
		if char == '"' {
			out.WriteString(strings.Repeat(`\`, slashes*2+1))
		} else {
			out.WriteString(strings.Repeat(`\`, slashes))
		}
		out.WriteRune(char)
		slashes = 0
	}
	out.WriteString(strings.Repeat(`\`, slashes*2))
	out.WriteByte('"')
	return out.String()
}

func escapeCmdMeta(value string) string {
	var out strings.Builder
	for _, char := range value {
		if strings.ContainsRune("()[]%!^\"`<>&|;, *?\t", char) {
			out.WriteByte('^')
		}
		out.WriteRune(char)
	}
	return out.String()
}
