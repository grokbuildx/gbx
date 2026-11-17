// Package grokbundle holds the open-source Grok Build binary compiled into
// gbx release builds. Releases build it from the xai-org/grok-build commit
// pinned in third_party/grok-build/REVISION and embed it with the
// "grokbundle" build tag; development builds embed nothing.
package grokbundle

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// Set with -ldflags -X by scripts/build-release.sh.
var (
	// Version is the Grok Build version declared by the embedded source tree.
	Version = ""
	// Revision is the full xai-org/grok-build commit the binary was built from.
	Revision = ""
)

// SourceRepository is where the embedded Grok Build source lives.
const SourceRepository = "https://github.com/xai-org/grok-build"

// archive is the gzip-compressed binary; nil without the grokbundle tag.
var archive = embeddedArchive

// homeDir is replaceable in tests.
var homeDir = os.UserHomeDir

var extract struct {
	sync.Mutex
	path string
}

// Available reports whether this gbx carries a Grok Build binary.
func Available() bool { return len(archive) > 0 }

// ShortRevision is the first 12 characters of Revision.
func ShortRevision() string {
	if len(Revision) > 12 {
		return Revision[:12]
	}
	return Revision
}

// Describe is the one-line summary printed by gbx version and doctor.
func Describe() string {
	if !Available() {
		return "not bundled in this build"
	}
	version := Version
	if version == "" {
		version = "unknown"
	}
	return fmt.Sprintf("%s (open source, built from xai-org/grok-build@%s)", version, ShortRevision())
}

// key names the unpack directory. Release builds use the upstream source
// commit, so a gbx upgrade that keeps the same Grok Build reuses the binary
// already on disk instead of unpacking it again. Builds without a recorded
// revision fall back to the archive digest.
func key() string {
	if Revision != "" {
		return "rev-" + ShortRevision()
	}
	sum := sha256.Sum256(archive)
	return "sha-" + hex.EncodeToString(sum[:8])
}

func executableName() string {
	if runtime.GOOS == "windows" {
		return "grok.exe"
	}
	return "grok"
}

// Dir is where gbx unpacks the bundled binary: ~/.gbx/grok/rev-<commit>/.
// A gbx upgrade with a new Grok Build unpacks next to, never over, a binary
// an older gbx may still be running.
func Dir() (string, error) {
	home, err := homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gbx", "grok", key()), nil
}

// Path unpacks the bundled Grok Build on first use and returns its path.
func Path() (string, error) {
	if !Available() {
		return "", errors.New("this gbx build does not bundle Grok Build")
	}
	extract.Lock()
	defer extract.Unlock()
	if extract.path != "" {
		return extract.path, nil
	}
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	target := filepath.Join(dir, executableName())
	if info, err := os.Stat(target); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
		extract.path = target
		return target, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	staging, err := os.CreateTemp(dir, "."+executableName()+".unpack-*")
	if err != nil {
		return "", err
	}
	tmp := staging.Name()
	defer os.Remove(tmp)
	reader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		staging.Close()
		return "", fmt.Errorf("bundled Grok Build is corrupt: %w", err)
	}
	if _, err := io.Copy(staging, reader); err != nil {
		staging.Close()
		return "", fmt.Errorf("unpack bundled Grok Build: %w", err)
	}
	if err := staging.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, target); err != nil {
		return "", err
	}
	pruneOld(filepath.Dir(dir), filepath.Base(dir))
	extract.path = target
	return target, nil
}

// pruneOld removes binaries unpacked by other gbx versions. A directory in
// use by a running process stays usable on Unix and fails removal on Windows;
// either way it is retried on a later unpack.
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
