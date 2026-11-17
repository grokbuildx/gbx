package groklaunch

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// withBundle stands in for the Grok Build compiled into release builds.
func withBundle(t *testing.T, path string) {
	t.Helper()
	old := bundledPath
	bundledPath = func() (string, bool) { return path, path != "" }
	t.Cleanup(func() { bundledPath = old })
}

func TestLookPathPrefersBundledGrokOverPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX executable bits")
	}
	bundled := filepath.Join(t.TempDir(), "grok")
	pathDir := t.TempDir()
	writeExecutable(t, bundled)
	writeExecutable(t, filepath.Join(pathDir, "grok"))
	t.Setenv("PATH", pathDir)
	t.Setenv(BinaryEnv, "")
	withBundle(t, bundled)

	if got, err := LookPath(); err != nil || got != bundled {
		t.Fatalf("LookPath = %q, %v; want bundled %q", got, err, bundled)
	}

	override := filepath.Join(t.TempDir(), "custom-grok")
	writeExecutable(t, override)
	t.Setenv(BinaryEnv, override)
	if got, err := LookPath(); err != nil || got != override {
		t.Fatalf("LookPath with %s = %q, %v; want %q", BinaryEnv, got, err, override)
	}
}

func TestLookPathFallsBackToPathWithoutBundle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX executable bits")
	}
	pathDir := t.TempDir()
	writeExecutable(t, filepath.Join(pathDir, "grok"))
	t.Setenv("PATH", pathDir)
	t.Setenv(BinaryEnv, "")
	withBundle(t, "")

	if got, err := LookPath(); err != nil || got != filepath.Join(pathDir, "grok") {
		t.Fatalf("LookPath = %q, %v; want PATH grok", got, err)
	}
}

func TestWithBundledEnvDisablesUpdaterOnlyForBundledGrok(t *testing.T) {
	bundled := filepath.Join(t.TempDir(), "grok")
	withBundle(t, bundled)

	env := []string{"A=1", "GROK_DISABLE_AUTOUPDATER=0"}
	got := WithBundledEnv(bundled, env)
	if len(got) != 2 || got[0] != "A=1" || got[1] != "GROK_DISABLE_AUTOUPDATER=1" {
		t.Fatalf("bundled env = %v", got)
	}
	if got := WithBundledEnv("/usr/local/bin/grok", env); len(got) != 2 || got[1] != "GROK_DISABLE_AUTOUPDATER=0" {
		t.Fatalf("system grok env changed: %v", got)
	}
}
