// Package grokbundle manages the open-source Grok Build that a gbx release
// pins. Releases build Grok Build from the xai-org/grok-build commit in
// third_party/grok-build/REVISION, publish it once per commit as the GitHub
// release grok-build-<commit>, and stamp the commit and each platform's
// archive digest into gbx with -ldflags. gbx downloads the archive on first
// use, so upgrades that keep the same commit download nothing.
// Development builds pin nothing and use a system Grok Build.
package grokbundle

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Set with -ldflags -X by scripts/build-release.sh.
var (
	// Version is the Grok Build version declared by the pinned source tree.
	Version = ""
	// Revision is the full xai-org/grok-build commit gbx pins.
	Revision = ""
	// SHA256 is the digest of this platform's grok-<platform>.gz archive.
	SHA256 = ""
)

// SourceRepository is where the pinned Grok Build source lives.
const SourceRepository = "https://github.com/xai-org/grok-build"

// DownloadEnv overrides the release download base, for mirrors and tests.
const DownloadEnv = "GBX_GROK_DOWNLOAD_URL"

const defaultDownloadBase = "https://github.com/grokbuildx/gbx/releases/download"

// maxBinaryBytes caps the unpacked size of a download.
var maxBinaryBytes int64 = 512 << 20

// Replaceable in tests.
var (
	homeDir              = os.UserHomeDir
	httpClient           = &http.Client{Timeout: 15 * time.Minute}
	progressTo io.Writer = os.Stderr
	goos                 = runtime.GOOS
	goarch               = runtime.GOARCH
)

var resolved struct {
	sync.Mutex
	path string
}

// Available reports whether this gbx pins a Grok Build for its platform.
func Available() bool { return Revision != "" && SHA256 != "" && platform() != "" }

// ShortRevision is the first 12 characters of Revision.
func ShortRevision() string {
	if len(Revision) > 12 {
		return Revision[:12]
	}
	return Revision
}

// ReleaseTag names the GitHub release that hosts this commit's binaries.
func ReleaseTag() string { return "grok-build-" + ShortRevision() }

// platform is the release asset suffix, matching the gbx assets.
func platform() string {
	switch goos + "/" + goarch {
	case "darwin/amd64", "darwin/arm64", "linux/amd64", "linux/arm64":
		return goos + "-" + goarch
	}
	return ""
}

// AssetName is this platform's archive in the Grok Build release.
func AssetName() string { return "grok-" + platform() + ".gz" }

// Describe is the one-line summary printed by gbx version and doctor.
func Describe() string {
	if !Available() {
		return "not pinned by this build"
	}
	version := Version
	if version == "" {
		version = "unknown"
	}
	return fmt.Sprintf("%s (open source, built from xai-org/grok-build@%s)", version, ShortRevision())
}

// Dir is where the pinned binary lives: ~/.gbx/grok/rev-<commit>/.
func Dir() (string, error) {
	home, err := homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gbx", "grok", "rev-"+ShortRevision()), nil
}

func binaryPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "grok"), nil
}

// Installed returns the pinned binary when it is already on disk, without
// downloading anything.
func Installed() (string, bool) {
	if !Available() {
		return "", false
	}
	path, err := binaryPath()
	if err != nil {
		return "", false
	}
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
		return path, true
	}
	return "", false
}

// DownloadURL is where this platform's archive is published.
func DownloadURL() string {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv(DownloadEnv)), "/")
	if base == "" {
		base = defaultDownloadBase
	}
	return base + "/" + ReleaseTag() + "/" + AssetName()
}

// Path returns the pinned Grok Build, downloading and verifying it the first
// time this commit is needed. A gbx upgrade that keeps the commit reuses it.
func Path(ctx context.Context) (string, error) {
	if !Available() {
		return "", errors.New("this gbx build does not pin a Grok Build")
	}
	resolved.Lock()
	defer resolved.Unlock()
	if resolved.path != "" {
		return resolved.path, nil
	}
	if path, ok := Installed(); ok {
		resolved.path = path
		return path, nil
	}
	path, err := binaryPath()
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	fmt.Fprintf(progressTo, "gbx: downloading open-source Grok Build %s (xai-org/grok-build@%s), once per Grok Build commit...\n", Version, ShortRevision())
	if err := download(ctx, DownloadURL(), path); err != nil {
		return "", fmt.Errorf("download Grok Build %s: %w", ShortRevision(), err)
	}
	pruneOld(filepath.Dir(dir), filepath.Base(dir))
	resolved.path = path
	return path, nil
}

func download(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	staging, err := os.CreateTemp(filepath.Dir(dest), ".grok.download-*")
	if err != nil {
		return err
	}
	tmp := staging.Name()
	defer os.Remove(tmp)
	defer staging.Close()

	digest := sha256.New()
	archive, err := gzip.NewReader(io.TeeReader(resp.Body, digest))
	if err != nil {
		return fmt.Errorf("not a gzip archive: %w", err)
	}
	written, err := io.Copy(staging, io.LimitReader(archive, maxBinaryBytes+1))
	if err != nil {
		return err
	}
	if written > maxBinaryBytes {
		return fmt.Errorf("archive expands beyond %d bytes", maxBinaryBytes)
	}
	// Hash the whole download, including anything after the gzip stream.
	if _, err := io.Copy(io.Discard, io.TeeReader(resp.Body, digest)); err != nil {
		return err
	}
	if got := hex.EncodeToString(digest.Sum(nil)); !strings.EqualFold(got, SHA256) {
		return fmt.Errorf("checksum mismatch: got %s, want %s", got, SHA256)
	}
	if err := staging.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

// pruneOld removes binaries for commits this gbx no longer pins. A binary an
// older gbx is still running stays usable on Unix.
func pruneOld(root, keep string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() && entry.Name() != keep && !strings.HasPrefix(entry.Name(), ".") {
			_ = os.RemoveAll(filepath.Join(root, entry.Name()))
		}
	}
}
