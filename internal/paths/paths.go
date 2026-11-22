// Package paths centralizes ~/.gbx directory and file path resolution across
// gbx subsystems so layout conventions live in one place.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

// GBXDir returns ~/.gbx.
func GBXDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".gbx"), nil
}

// ConfigPath returns ~/.gbx/config.yaml.
func ConfigPath() (string, error) {
	dir, err := GBXDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.yaml"), nil
}

// AuthDir returns ~/.gbx/auth.
func AuthDir() (string, error) {
	dir, err := GBXDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "auth"), nil
}

// LogDir returns ~/.gbx/logs.
func LogDir() (string, error) {
	dir, err := GBXDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "logs"), nil
}

// CacheDir returns ~/.gbx/cache.
func CacheDir() (string, error) {
	dir, err := GBXDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "cache"), nil
}

// ModelsDevCachePath returns ~/.gbx/cache/modelsdev.json.
func ModelsDevCachePath() (string, error) {
	dir, err := CacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "modelsdev.json"), nil
}

// ProxyDir returns ~/.gbx/proxy.
func ProxyDir() (string, error) {
	dir, err := GBXDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "proxy"), nil
}

// RunDir returns ~/.gbx/run.
func RunDir() (string, error) {
	dir, err := GBXDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "run"), nil
}

// ServeDir returns ~/.gbx/serve.
func ServeDir() (string, error) {
	dir, err := GBXDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "serve"), nil
}

// ServeSessionsDir returns ~/.gbx/serve/sessions.
func ServeSessionsDir() (string, error) {
	dir, err := ServeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "sessions"), nil
}
