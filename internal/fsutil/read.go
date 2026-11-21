package fsutil

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// ErrFileTooLarge is wrapped when ReadRegularFileLimited rejects a file larger
// than its configured byte budget.
var ErrFileTooLarge = errors.New("exceeds safety limit")

// readRetryWindow bounds how long ReadFile waits out a concurrent replace.
const readRetryWindow = time.Second

// ReadFile is os.ReadFile for files another process replaces with
// WriteFileAtomic. On Windows a read that overlaps the rename fails with a
// transient sharing or access error; ReadFile retries those briefly. Other
// errors, and every error on other platforms, are returned at once.
func ReadFile(path string) ([]byte, error) {
	deadline := time.Now().Add(readRetryWindow)
	delay := 5 * time.Millisecond
	for {
		data, err := os.ReadFile(path)
		if err == nil || !transientReadError(err) || time.Now().After(deadline) {
			return data, err
		}
		time.Sleep(delay)
		delay = min(delay*2, 100*time.Millisecond)
	}
}

// ReadRegularFileLimited reads at most limit bytes from a regular non-symlink
// file, retrying transient Windows sharing/access errors. It avoids loading
// oversized or special files into memory before rejecting them.
func ReadRegularFileLimited(path string, limit int64) ([]byte, error) {
	deadline := time.Now().Add(readRetryWindow)
	delay := 5 * time.Millisecond
	for {
		data, err := readRegularFileLimitedOnce(path, limit)
		if err == nil || !transientReadError(err) || time.Now().After(deadline) {
			return data, err
		}
		time.Sleep(delay)
		delay = min(delay*2, 100*time.Millisecond)
	}
}

func readRegularFileLimitedOnce(path string, limit int64) ([]byte, error) {
	linfo, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !linfo.Mode().IsRegular() || linfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("refuse to read non-regular file %s", path)
	}
	if limit >= 0 && linfo.Size() > limit {
		return nil, fmt.Errorf("file %s %w", path, ErrFileTooLarge)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("refuse to read non-regular file %s", path)
	}
	if limit >= 0 && info.Size() > limit {
		return nil, fmt.Errorf("file %s %w", path, ErrFileTooLarge)
	}
	if limit < 0 {
		return io.ReadAll(f)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file %s %w", path, ErrFileTooLarge)
	}
	return data, nil
}
