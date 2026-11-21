// Package runtimelog owns gbx's per-session structured slog diagnostics,
// retention pruning, and token usage accumulation.
package runtimelog

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/grokbuildx/gbx/internal/paths"
)

// DefaultLogName is the base name for gbx's per-session runtime logs. The full
// base path can be overridden with GBX_LOG_FILE (GBX_DEBUG_LOG remains a
// compatibility fallback for existing scripts).
const DefaultLogName = "gbx-debug.log"

// LogLevel is gbx's persisted representation of the standard slog levels.
// "off" disables file logging entirely.
type LogLevel string

const (
	LogLevelOff   LogLevel = "off"
	LogLevelDebug LogLevel = "debug"
	LogLevelInfo  LogLevel = "info"
	LogLevelWarn  LogLevel = "warn"
	LogLevelError LogLevel = "error"
)

var (
	logStateMu  sync.RWMutex
	logLevel    = LogLevelOff
	logPath     string
	logFile     *os.File
	logManaged  bool   // true when opened automatically via AcquireSessionLog/EnsureSessionLog
	logRefCount int    // active runtime leases on an auto-opened session log
	logGen      uint64 // monotonic sink generation preventing stale lease releases
	logger      = slog.New(slog.NewTextHandler(io.Discard, nil))
	requestSeq  atomic.Uint64
)

type requestLogIDKey struct{}

// ParseLogLevel accepts gbx's standard logging levels.
func ParseLogLevel(raw string) (LogLevel, bool) {
	switch LogLevel(strings.ToLower(strings.TrimSpace(raw))) {
	case LogLevelOff:
		return LogLevelOff, true
	case LogLevelDebug:
		return LogLevelDebug, true
	case LogLevelInfo:
		return LogLevelInfo, true
	case LogLevelWarn, "warning":
		return LogLevelWarn, true
	case LogLevelError:
		return LogLevelError, true
	default:
		return LogLevelOff, false
	}
}

func (level LogLevel) slogLevel() slog.Level {
	switch level {
	case LogLevelDebug:
		return slog.LevelDebug
	case LogLevelWarn:
		return slog.LevelWarn
	case LogLevelError:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// ConfigureLogLevel records the logging threshold without creating a shared
// file. A session or temporary provider runtime opens its own sink when it starts.
func ConfigureLogLevel(level LogLevel) {
	logStateMu.Lock()
	defer logStateMu.Unlock()
	closeLogSinkLocked()
	logLevel = level
}

func closeLogSinkLocked() {
	if logFile != nil {
		_ = logFile.Close()
		logFile = nil
	}
	logPath = ""
	logManaged = false
	logRefCount = 0
	logGen++
	logger = slog.New(slog.NewTextHandler(io.Discard, nil))
}

func openLogSinkLocked(level LogLevel, path string, managed bool) error {
	if strings.TrimSpace(path) == "" {
		path = ResolveLogTemplatePath()
	}
	if logFile != nil && logPath == path && logLevel == level {
		if !managed && logManaged {
			logManaged = false
			logRefCount = 0
			logGen++
		}
		return nil
	}
	closeLogSinkLocked()
	logLevel = level
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create log directory %s: %w", dir, err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open log file %s: %w", path, err)
	}
	logPath = path
	logFile = f
	logManaged = managed
	logGen++
	logger = slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: level.slogLevel()}))
	logger.Log(context.Background(), slog.LevelInfo, "log_session_start",
		"configured_level", level, "path", path, "pid", os.Getpid())
	return nil
}

// SetLogLevel opens gbx's current per-session log sink. A level of "off"
// disables logging; all other levels use Go's standard slog text handler.
// File-system failures are returned instead of silently disabling diagnostics.
func SetLogLevel(level LogLevel, path string) error {
	logStateMu.Lock()
	defer logStateMu.Unlock()

	if level == LogLevelOff {
		closeLogSinkLocked()
		logLevel = LogLevelOff
		return nil
	}
	return openLogSinkLocked(level, path, false)
}

// CloseLog closes the active session file while preserving the configured
// threshold for the next session.
func CloseLog() {
	logStateMu.Lock()
	defer logStateMu.Unlock()
	closeLogSinkLocked()
}

// LogDir is ~/.gbx/logs, where gbx keeps its diagnostics.
func LogDir() (string, error) {
	return paths.LogDir()
}

// ResolveLogTemplatePath returns the filename template used to derive each
// suffixed session log. The template itself is never opened by gbx.
func ResolveLogTemplatePath() string {
	if v := strings.TrimSpace(os.Getenv("GBX_LOG_FILE")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("GBX_DEBUG_LOG")); v != "" {
		return v
	}
	dir, err := LogDir()
	if err != nil {
		return filepath.Join(os.TempDir(), DefaultLogName)
	}
	return filepath.Join(dir, DefaultLogName)
}

// LogConfigured reports whether a session should open a log file.
func LogConfigured() bool {
	logStateMu.RLock()
	defer logStateMu.RUnlock()
	return logLevel != LogLevelOff
}

// LogEnabled reports whether gbx's current session file is active.
func LogEnabled() bool {
	logStateMu.RLock()
	defer logStateMu.RUnlock()
	return logLevel != LogLevelOff && logFile != nil
}

// CurrentLogLevel reports the active logging threshold.
func CurrentLogLevel() LogLevel {
	logStateMu.RLock()
	defer logStateMu.RUnlock()
	return logLevel
}

// LogFilePath reports the active session log path, or an empty string when off.
func LogFilePath() string {
	logStateMu.RLock()
	defer logStateMu.RUnlock()
	return logPath
}

// SessionLogPath derives one log file per temporary session from the
// configured base path.
func SessionLogPath(session string) string {
	base := ResolveLogTemplatePath()
	session = sanitizeLogSessionName(session)
	if session == "" {
		return base
	}
	extension := filepath.Ext(base)
	return strings.TrimSuffix(base, extension) + "-" + session + extension
}

// AcquireSessionLog atomically ensures a session log is open when logging is
// configured and returns a release callback that closes auto-opened logs only
// after the final concurrent runtime lease ends.
func AcquireSessionLog(prefix string) (path string, release func(), err error) {
	logStateMu.Lock()
	defer logStateMu.Unlock()

	if logLevel == LogLevelOff {
		return "", func() {}, nil
	}
	if logFile != nil {
		if logManaged {
			logRefCount++
			gen := logGen
			var once sync.Once
			return logPath, func() { once.Do(func() { releaseSessionLogLease(gen) }) }, nil
		}
		return logPath, func() {}, nil
	}
	var random [6]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", nil, fmt.Errorf("generate log session name: %w", err)
	}
	session := fmt.Sprintf("%s_%x", sanitizeLogSessionName(prefix), random)
	path = SessionLogPath(session)
	if err := openLogSinkLocked(logLevel, path, true); err != nil {
		return "", nil, err
	}
	logRefCount = 1
	gen := logGen
	var once sync.Once
	return path, func() { once.Do(func() { releaseSessionLogLease(gen) }) }, nil
}

func releaseSessionLogLease(gen uint64) {
	logStateMu.Lock()
	defer logStateMu.Unlock()
	if gen != logGen || !logManaged || logRefCount <= 0 {
		return
	}
	logRefCount--
	if logRefCount == 0 {
		closeLogSinkLocked()
	}
}

// EnsureSessionLog opens a uniquely named file for a temporary runtime when a
// caller has not already opened the surrounding session's file. owned reports
// whether this call opened the file.
func EnsureSessionLog(prefix string) (path string, owned bool, err error) {
	logStateMu.Lock()
	defer logStateMu.Unlock()

	if logLevel == LogLevelOff || logFile != nil {
		return logPath, false, nil
	}
	var random [6]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", false, fmt.Errorf("generate log session name: %w", err)
	}
	session := fmt.Sprintf("%s_%x", sanitizeLogSessionName(prefix), random)
	path = SessionLogPath(session)
	if err := openLogSinkLocked(logLevel, path, true); err != nil {
		return "", false, err
	}
	logRefCount = 1
	return path, true, nil
}

func sanitizeLogSessionName(session string) string {
	var builder strings.Builder
	for _, char := range strings.TrimSpace(session) {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9':
			builder.WriteRune(char)
		case char == '_', char == '-', char == '.':
			builder.WriteRune(char)
		}
	}
	return strings.Trim(builder.String(), ".-_")
}

// SafeLogEndpoint keeps a URL useful for routing diagnostics without retaining
// userinfo, query parameters, or fragments, which commonly carry API keys on
// third-party gateways.
func SafeLogEndpoint(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "<invalid>"
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return parsed.String()
}

// LogDebugEnabled reports whether DEBUG entries are collected. HTTP payloads
// are deliberately DEBUG only because they can contain full prompts, tools, and
// user-provided secrets.
func LogDebugEnabled() bool {
	return CurrentLogLevel() == LogLevelDebug
}

// WithRequestLogID gives every request handled by a gbx-owned data plane a
// compact correlation ID.
func WithRequestLogID(ctx context.Context) (context.Context, string) {
	if ctx == nil {
		ctx = context.Background()
	}
	if id := RequestLogID(ctx); id != "" {
		return ctx, id
	}
	id := "r" + strconv.FormatUint(requestSeq.Add(1), 36)
	return context.WithValue(ctx, requestLogIDKey{}, id), id
}

// RequestLogID extracts the correlation ID attached by WithRequestLogID.
func RequestLogID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(requestLogIDKey{}).(string)
	return id
}

// LogDuration rounds elapsed time since start to milliseconds.
func LogDuration(start time.Time) time.Duration {
	return time.Since(start).Round(time.Millisecond)
}

func logf(level slog.Level, format string, args ...any) {
	logStateMu.RLock()
	l := logger
	ctx := context.Background()
	if !l.Enabled(ctx, level) {
		logStateMu.RUnlock()
		return
	}
	// Hold RLock across l.Log so CloseLog cannot close the underlying *os.File
	// concurrently with an in-flight write.
	l.Log(ctx, level, fmt.Sprintf(format, args...))
	logStateMu.RUnlock()
}

func logEvent(level slog.Level, event string, attrs ...any) {
	logStateMu.RLock()
	l := logger
	ctx := context.Background()
	if !l.Enabled(ctx, level) {
		logStateMu.RUnlock()
		return
	}
	l.Log(ctx, level, event, attrs...)
	logStateMu.RUnlock()
}

func LogInfof(format string, args ...any)  { logf(slog.LevelInfo, format, args...) }
func LogDebugf(format string, args ...any) { logf(slog.LevelDebug, format, args...) }
func LogWarnf(format string, args ...any)  { logf(slog.LevelWarn, format, args...) }
func LogErrorf(format string, args ...any) { logf(slog.LevelError, format, args...) }

func LogDebugEvent(event string, attrs ...any) { logEvent(slog.LevelDebug, event, attrs...) }
func LogInfoEvent(event string, attrs ...any)  { logEvent(slog.LevelInfo, event, attrs...) }
func LogWarnEvent(event string, attrs ...any)  { logEvent(slog.LevelWarn, event, attrs...) }
func LogErrorEvent(event string, attrs ...any) { logEvent(slog.LevelError, event, attrs...) }

func LogUpstreamEvent(status int, event string, attrs ...any) {
	switch {
	case status >= 500:
		LogErrorEvent(event, attrs...)
	case status >= 400:
		LogWarnEvent(event, attrs...)
	default:
		LogDebugEvent(event, attrs...)
	}
}

// DebugHTTPBody writes an explicitly debug-level HTTP payload. Callers must
// never pass headers because they can contain credentials.
func DebugHTTPBody(label string, body []byte) {
	if !LogDebugEnabled() {
		return
	}
	LogDebugf("http payload begin label=%q bytes=%d", label, len(body))
	LogDebugf("http payload data label=%q data=%s", label, body)
	LogDebugf("http payload end label=%q", label)
}
