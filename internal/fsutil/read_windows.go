//go:build windows

package fsutil

import (
	"errors"

	"golang.org/x/sys/windows"
)

// transientReadError reports the errors a read sees while another process
// is replacing the file.
func transientReadError(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION) ||
		errors.Is(err, windows.ERROR_ACCESS_DENIED)
}
