package cmd

import (
	"testing"

	"github.com/grokbuildx/gbx/internal/selfupdate"
)

func TestReleaseAssetName(t *testing.T) {
	name, err := selfupdate.ReleaseAssetName()
	if err != nil {
		t.Skipf("unsupported platform for self-update: %v", err)
	}
	valid := map[string]bool{
		"gbx-darwin-amd64":    true,
		"gbx-darwin-arm64":    true,
		"gbx-linux-amd64":     true,
		"gbx-linux-arm64":     true,
		"gbx-win32-x64.exe":   true,
		"gbx-win32-arm64.exe": true,
	}
	if !valid[name] {
		t.Errorf("ReleaseAssetName() = %q, want one of the release asset names", name)
	}
}

func TestReleaseDownloadURL(t *testing.T) {
	if got := selfupdate.ReleaseDownloadURL("v1.5.6", "gbx-darwin-arm64"); got != "https://github.com/grokbuildx/gbx/releases/download/v1.5.6/gbx-darwin-arm64" {
		t.Errorf("versioned URL = %q", got)
	}
	if got := selfupdate.ReleaseDownloadURL("unknown", "gbx-linux-amd64"); got != "https://github.com/grokbuildx/gbx/releases/latest/download/gbx-linux-amd64" {
		t.Errorf("latest URL = %q", got)
	}
}

func TestHumanSize(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1048576, "1.0 MiB"},
		{5*1024*1024 + 512*1024, "5.5 MiB"},
	}
	for _, c := range cases {
		if got := selfupdate.HumanSize(c.n); got != c.want {
			t.Errorf("HumanSize(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}
