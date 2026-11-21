package runtimelog

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Session logs accumulate one file per runtime session. PruneSessionLogs keeps
// the newest maxFiles that are younger than maxAge, deleting the rest. It only
// touches gbx's own log files in its log directory and never fails a launch.
const (
	DefaultLogMaxAge   = 30 * 24 * time.Hour
	DefaultLogMaxFiles = 200
)

func PruneSessionLogs(maxAge time.Duration, maxFiles int) {
	dir, err := LogDir()
	if err != nil {
		return
	}
	PruneLogDir(dir, strings.TrimSuffix(DefaultLogName, filepath.Ext(DefaultLogName)), maxAge, maxFiles, time.Now())
}

// PruneLogDir removes gbx log files older than maxAge or beyond maxFiles in dir.
func PruneLogDir(dir, prefix string, maxAge time.Duration, maxFiles int, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type logEntry struct {
		path    string
		modTime time.Time
	}
	var files []logEntry
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, prefix) || filepath.Ext(name) != ".log" {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		files = append(files, logEntry{filepath.Join(dir, name), info.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modTime.After(files[j].modTime) })
	for index, file := range files {
		if index >= maxFiles || now.Sub(file.modTime) > maxAge {
			_ = os.Remove(file.path)
		}
	}
}
