package fsutil

import (
	"os"
	"time"
)

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
