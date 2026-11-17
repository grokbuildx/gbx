package grokbundle

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const testRevision = "2bdd1d6a6369de0e8c68132ea4539e9abd9e14a8"

func gz(t *testing.T, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// pin stands in for a release build pinning revision on linux/amd64 and serves
// archive from a local release host; it returns HOME and the request counter.
func pin(t *testing.T, home, revision string, archive []byte) *atomic.Int32 {
	t.Helper()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/grok-build-"+revision[:12]+"/grok-linux-amd64.gz" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(archive)
	}))
	t.Cleanup(server.Close)
	t.Setenv(DownloadEnv, server.URL)
	sum := sha256.Sum256(archive)
	saved := []string{Version, Revision, SHA256, goos, goarch}
	oldHome, oldProgress := homeDir, progressTo
	Version, Revision, SHA256, goos, goarch = "1.0.45", revision, hex.EncodeToString(sum[:]), "linux", "amd64"
	homeDir = func() (string, error) { return home, nil }
	progressTo = io.Discard
	resolved.path = ""
	t.Cleanup(func() {
		Version, Revision, SHA256, goos, goarch = saved[0], saved[1], saved[2], saved[3], saved[4]
		homeDir, progressTo = oldHome, oldProgress
		resolved.path = ""
	})
	return &requests
}

func TestPathDownloadsOncePerRevision(t *testing.T) {
	home := t.TempDir()
	requests := pin(t, home, testRevision, gz(t, "grok binary"))
	if _, ok := Installed(); ok {
		t.Fatal("Installed before any download")
	}
	path, err := Path(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".gbx", "grok", "rev-2bdd1d6a6369", "grok"); path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	if got, _ := os.ReadFile(path); string(got) != "grok binary" {
		t.Fatalf("binary = %q", got)
	}
	if info, _ := os.Stat(path); info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("binary is not executable: %v", info.Mode())
	}

	// A later gbx release pinning the same commit downloads nothing.
	requests2 := pin(t, home, testRevision, gz(t, "grok binary"))
	if again, err := Path(context.Background()); err != nil || again != path {
		t.Fatalf("same revision: %q, %v", again, err)
	}
	if requests.Load() != 1 || requests2.Load() != 0 {
		t.Fatalf("downloads = %d then %d; want 1 then 0", requests.Load(), requests2.Load())
	}

	// A new commit downloads once and removes the old binary.
	newRevision := strings.Repeat("9", 40)
	requests3 := pin(t, home, newRevision, gz(t, "new grok"))
	newPath, err := Path(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(newPath); string(got) != "new grok" || requests3.Load() != 1 {
		t.Fatalf("new revision = %q after %d downloads", got, requests3.Load())
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("old revision kept: %v", err)
	}
}

func TestPathRejectsAnArchiveWithTheWrongChecksum(t *testing.T) {
	home := t.TempDir()
	pin(t, home, testRevision, gz(t, "grok binary"))
	SHA256 = strings.Repeat("0", 64)
	if _, err := Path(context.Background()); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v, want checksum mismatch", err)
	}
	if _, ok := Installed(); ok {
		t.Fatal("a binary with the wrong checksum was installed")
	}
	entries, _ := os.ReadDir(filepath.Join(home, ".gbx", "grok", "rev-2bdd1d6a6369"))
	if len(entries) != 0 {
		t.Fatalf("download left files behind: %v", entries)
	}
}

func TestDescribeAndUnpinnedBuilds(t *testing.T) {
	pin(t, t.TempDir(), testRevision, gz(t, "grok"))
	if got := Describe(); got != "1.0.45 (open source, built from xai-org/grok-build@2bdd1d6a6369)" {
		t.Fatalf("Describe() = %q", got)
	}
	goos = "windows"
	if Available() {
		t.Fatal("Windows has no pinned Grok Build")
	}
	goos, Revision = "linux", ""
	if Available() || Describe() != "not pinned by this build" {
		t.Fatalf("unpinned: Available=%v Describe=%q", Available(), Describe())
	}
	if _, err := Path(context.Background()); err == nil {
		t.Fatal("Path() succeeded without a pin")
	}
}
