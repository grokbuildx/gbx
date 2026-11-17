package cmd

import (
	"path/filepath"
	"testing"
)

func TestInstallMethodRecognizesEveryNativeNpmPackage(t *testing.T) {
	for _, platform := range []string{"darwin-x64", "darwin-arm64", "linux-x64", "linux-arm64", "win32-x64", "win32-arm64"} {
		path := filepath.Join("root", "node_modules", "@grokbuildx", "gbx-"+platform, "bin", "gbx")
		if got := installMethodForPath(path, nil); got != "npm" {
			t.Fatalf("platform %s: method=%s", platform, got)
		}
	}
	if got := installMethodForPath(filepath.Join("root", "node_modules", "@grokbuildx", "gbx-unrelated", "bin", "gbx"), nil); got != "self" {
		t.Fatalf("unrelated package matched: %s", got)
	}
}
