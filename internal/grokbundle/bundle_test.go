package grokbundle

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withArchive(t *testing.T, content string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	oldArchive, oldHome, oldVersion, oldRevision := archive, homeDir, Version, Revision
	archive, homeDir = buf.Bytes(), func() (string, error) { return home, nil }
	Version, Revision = "1.0.45", "2bdd1d6a6369de0e8c68132ea4539e9abd9e14a8"
	extract.path = ""
	t.Cleanup(func() {
		archive, homeDir, Version, Revision = oldArchive, oldHome, oldVersion, oldRevision
		extract.path = ""
	})
	return home
}

func TestPathUnpacksOnceIntoAContentAddressedDirectory(t *testing.T) {
	home := withArchive(t, "grok binary")
	stale := filepath.Join(home, ".gbx", "grok", "0000000000000000")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(path, filepath.Join(home, ".gbx", "grok")+string(filepath.Separator)) {
		t.Fatalf("path %q outside ~/.gbx/grok", path)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "grok binary" {
		t.Fatalf("unpacked = %q, %v", got, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("unpacked binary is not executable: %v", info.Mode())
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("binary from another gbx version was kept: %v", err)
	}
	if again, err := Path(); err != nil || again != path {
		t.Fatalf("second Path() = %q, %v", again, err)
	}
}

func TestDescribeNamesTheOpenSourceRevision(t *testing.T) {
	withArchive(t, "grok binary")
	if got := Describe(); got != "1.0.45 (open source, built from xai-org/grok-build@2bdd1d6a6369)" {
		t.Fatalf("Describe() = %q", got)
	}
	archive = nil
	if Available() || Describe() != "not bundled in this build" {
		t.Fatalf("empty bundle: Available=%v Describe=%q", Available(), Describe())
	}
	if _, err := Path(); err == nil {
		t.Fatal("Path() succeeded without a bundle")
	}
}
