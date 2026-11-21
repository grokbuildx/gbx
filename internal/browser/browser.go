// Package browser opens URLs in the user's default browser.
package browser

import (
	"fmt"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
)

func validateTarget(target string) (string, error) {
	trimmed := strings.TrimSpace(target)
	if trimmed == "" {
		return "", fmt.Errorf("browser target URL is empty")
	}
	if strings.ContainsAny(trimmed, "\r\n\x00") {
		return "", fmt.Errorf("browser target URL contains control characters")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("invalid browser target URL: %w", err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if (scheme != "http" && scheme != "https") || parsed.Host == "" {
		return "", fmt.Errorf("unsupported browser target URL scheme %q", parsed.Scheme)
	}
	return parsed.String(), nil
}

// Open starts the platform's URL handler for target and returns without
// waiting for it. Only http/https URLs are permitted, and the helper process
// is reaped asynchronously so long-lived hosts do not accumulate zombies.
func Open(target string) error {
	safeTarget, err := validateTarget(target)
	if err != nil {
		return err
	}
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", safeTarget)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", safeTarget)
	default:
		command = exec.Command("xdg-open", safeTarget)
	}
	if err := command.Start(); err != nil {
		return err
	}
	go func() {
		_ = command.Wait()
	}()
	return nil
}
