//go:build !windows

package fsutil

// transientReadError is always false: a rename never blocks readers here.
func transientReadError(error) bool { return false }
