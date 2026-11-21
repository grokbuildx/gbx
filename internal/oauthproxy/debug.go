package oauthproxy

import (
	"context"
	"time"

	"github.com/grokbuildx/gbx/internal/runtimelog"
)

const defaultLogName = runtimelog.DefaultLogName

// LogLevel is gbx's persisted representation of the standard slog levels.
type LogLevel = runtimelog.LogLevel

const (
	LogLevelOff   = runtimelog.LogLevelOff
	LogLevelDebug = runtimelog.LogLevelDebug
	LogLevelInfo  = runtimelog.LogLevelInfo
	LogLevelWarn  = runtimelog.LogLevelWarn
	LogLevelError = runtimelog.LogLevelError
)

func ParseLogLevel(raw string) (LogLevel, bool)     { return runtimelog.ParseLogLevel(raw) }
func ConfigureLogLevel(level LogLevel)              { runtimelog.ConfigureLogLevel(level) }
func SetLogLevel(level LogLevel, path string) error { return runtimelog.SetLogLevel(level, path) }
func CloseLog()                                     { runtimelog.CloseLog() }
func LogDir() (string, error)                       { return runtimelog.LogDir() }
func ResolveLogTemplatePath() string                { return runtimelog.ResolveLogTemplatePath() }
func LogConfigured() bool                           { return runtimelog.LogConfigured() }
func LogEnabled() bool                              { return runtimelog.LogEnabled() }
func CurrentLogLevel() LogLevel                     { return runtimelog.CurrentLogLevel() }
func LogFilePath() string                           { return runtimelog.LogFilePath() }
func SessionLogPath(session string) string          { return runtimelog.SessionLogPath(session) }
func EnsureSessionLog(prefix string) (string, bool, error) {
	return runtimelog.EnsureSessionLog(prefix)
}
func SafeLogEndpoint(raw string) string { return runtimelog.SafeLogEndpoint(raw) }
func LogDebugEnabled() bool             { return runtimelog.LogDebugEnabled() }
func withRequestLogID(ctx context.Context) (context.Context, string) {
	return runtimelog.WithRequestLogID(ctx)
}
func requestLogID(ctx context.Context) string   { return runtimelog.RequestLogID(ctx) }
func logDuration(start time.Time) time.Duration { return runtimelog.LogDuration(start) }
func LogInfof(format string, args ...any)       { runtimelog.LogInfof(format, args...) }
func LogDebugf(format string, args ...any)      { runtimelog.LogDebugf(format, args...) }
func LogWarnf(format string, args ...any)       { runtimelog.LogWarnf(format, args...) }
func LogErrorf(format string, args ...any)      { runtimelog.LogErrorf(format, args...) }
func LogDebugEvent(event string, attrs ...any)  { runtimelog.LogDebugEvent(event, attrs...) }
func LogInfoEvent(event string, attrs ...any)   { runtimelog.LogInfoEvent(event, attrs...) }
func LogWarnEvent(event string, attrs ...any)   { runtimelog.LogWarnEvent(event, attrs...) }
func LogErrorEvent(event string, attrs ...any)  { runtimelog.LogErrorEvent(event, attrs...) }
func LogUpstreamEvent(status int, event string, attrs ...any) {
	runtimelog.LogUpstreamEvent(status, event, attrs...)
}
func DebugHTTPBody(label string, body []byte) { runtimelog.DebugHTTPBody(label, body) }
