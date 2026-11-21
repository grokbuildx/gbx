//go:build !windows

package fsutil

import (
	"errors"
	"os"
	"syscall"
)

func tryFileLock(f *os.File) (bool, error) {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return false, nil
		}
		return err == nil, err
	}
}

func unlockFile(f *os.File) {
	for errors.Is(syscall.Flock(int(f.Fd()), syscall.LOCK_UN), syscall.EINTR) {
	}
}
