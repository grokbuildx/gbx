package webui

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// maxSocketPath is the longest Unix socket path every platform accepts
// (sun_path is 104 bytes on macOS, including the terminating NUL).
const maxSocketPath = 103

// DefaultSocketPath is where `gbx serve --socket` listens when it is given no
// path: next to the conversations, in a directory only this user can open.
func DefaultSocketPath() (string, error) {
	root, err := DefaultRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(root), "gbx.sock"), nil
}

type renamedUnixListener struct {
	*net.UnixListener
	finalPath     string
	unlinkOnClose bool
}

func (l *renamedUnixListener) Addr() net.Addr {
	return &net.UnixAddr{Name: l.finalPath, Net: "unix"}
}

func (l *renamedUnixListener) SetUnlinkOnClose(unlink bool) {
	l.unlinkOnClose = unlink
}

func (l *renamedUnixListener) Close() error {
	err := l.UnixListener.Close()
	if l.unlinkOnClose {
		_ = os.Remove(l.finalPath)
	}
	return err
}

// ListenUnix listens on a Unix socket at path that only this user can
// connect to. A socket left behind by a server that died is replaced; one
// another server still answers on is refused, as is any other file.
func ListenUnix(path string) (net.Listener, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if len(path) > maxSocketPath {
		return nil, fmt.Errorf("socket path %s is longer than %d bytes; pick a shorter one", path, maxSocketPath)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create socket directory: %w", err)
	}
	info, err := os.Lstat(path)
	switch {
	case err == nil && info.Mode()&fs.ModeSocket == 0:
		return nil, fmt.Errorf("%s exists and is not a socket", path)
	case err == nil:
		if conn, err := net.DialTimeout("unix", path, time.Second); err == nil {
			_ = conn.Close()
			return nil, fmt.Errorf("another server is already listening on %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket: %w", err)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}
	// If the parent directory is accessible to group or others (e.g., /tmp),
	// stage the socket inside a mode-0700 directory, tighten it to 0600, then
	// rename into place so there is no permission race window.
	dirInfo, statErr := os.Stat(dir)
	dirShared := statErr != nil || dirInfo.Mode().Perm()&0o077 != 0
	if dirShared {
		tmpDir, mkErr := os.MkdirTemp(dir, ".s")
		if mkErr != nil {
			return nil, fmt.Errorf("stage socket directory: %w", mkErr)
		}
		tmpSock := filepath.Join(tmpDir, "s")
		if len(tmpSock) > maxSocketPath {
			_ = os.RemoveAll(tmpDir)
			return nil, fmt.Errorf("socket path %s is too long to stage safely inside a shared directory; use a shorter path or a 0700 directory", path)
		}
		rawListener, listenErr := net.Listen("unix", tmpSock)
		if listenErr != nil {
			_ = os.RemoveAll(tmpDir)
			return nil, fmt.Errorf("listen on %s: %w", path, listenErr)
		}
		ul, ok := rawListener.(*net.UnixListener)
		if !ok {
			_ = rawListener.Close()
			_ = os.RemoveAll(tmpDir)
			return nil, fmt.Errorf("unexpected unix listener type")
		}
		ul.SetUnlinkOnClose(false)
		if chmodErr := os.Chmod(tmpSock, 0o600); chmodErr != nil {
			_ = ul.Close()
			_ = os.RemoveAll(tmpDir)
			return nil, fmt.Errorf("restrict socket: %w", chmodErr)
		}
		if renameErr := os.Rename(tmpSock, path); renameErr != nil {
			_ = ul.Close()
			_ = os.RemoveAll(tmpDir)
			return nil, fmt.Errorf("publish socket %s: %w", path, renameErr)
		}
		_ = os.Remove(tmpDir)
		return &renamedUnixListener{UnixListener: ul, finalPath: path, unlinkOnClose: true}, nil
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	// Connecting needs write permission on the socket. Closing the listener
	// removes the file.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("restrict socket: %w", err)
	}
	return listener, nil
}

// overUnixSocket reports whether r arrived on a Unix socket, which no
// browser page can reach and only this user can connect to.
func overUnixSocket(r *http.Request) bool {
	addr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	return ok && addr.Network() == "unix"
}
