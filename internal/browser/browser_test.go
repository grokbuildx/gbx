package browser

import "testing"

func TestValidateTarget(t *testing.T) {
	valid := []string{
		"https://example.com/auth?code=123",
		"http://127.0.0.1:8080/callback",
		"HTTPS://accounts.google.com/o/oauth2/v2/auth",
	}
	for _, raw := range valid {
		if _, err := validateTarget(raw); err != nil {
			t.Fatalf("validateTarget(%q) unexpectedly failed: %v", raw, err)
		}
	}

	invalid := []string{
		"",
		"   ",
		"file:///etc/passwd",
		`\\evil\share\payload.exe`,
		"javascript:alert(1)",
		"-a Calculator",
		"https://example.com/\r\ncalc",
		"https://example.com/\x00foo",
		"https:///no-host",
	}
	for _, raw := range invalid {
		if got, err := validateTarget(raw); err == nil {
			t.Fatalf("validateTarget(%q) = %q, want error", raw, got)
		}
	}
}
