package oauthproxy

import (
	"time"

	"github.com/grokbuildx/gbx/internal/runtimelog"
)

const (
	DefaultLogMaxAge   = runtimelog.DefaultLogMaxAge
	DefaultLogMaxFiles = runtimelog.DefaultLogMaxFiles
)

func PruneSessionLogs(maxAge time.Duration, maxFiles int) {
	runtimelog.PruneSessionLogs(maxAge, maxFiles)
}

func pruneLogDir(dir, prefix string, maxAge time.Duration, maxFiles int, now time.Time) {
	runtimelog.PruneLogDir(dir, prefix, maxAge, maxFiles, now)
}
