package cloudsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/grokbuildx/gbx/internal/fsutil"
)

func (m *Manager) backupCurrent(tag string) (string, error) {
	files, hash, err := collectLocalFiles()
	if err != nil {
		if errors.Is(err, ErrNoLocalData) {
			return "", nil
		}
		return "", err
	}
	payload := snapshotPayload{
		Version: formatVersion, Hash: hash, Tag: "backup-" + tag,
		DeviceID: m.deviceID, CreatedAt: nowUTC(), Files: files,
	}
	plain, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	encrypted, err := sealCompressed(m.key, plain)
	if err != nil {
		return "", err
	}
	name := time.Now().UTC().Format("20060102T150405.000000000Z") + ".gbx"
	path := filepath.Join(m.localDir, backupsDirectory, name)
	if err := writeAtomic(path, encrypted, 0o600); err != nil {
		return "", fmt.Errorf("create encrypted local backup: %w", err)
	}
	return path, nil
}

func (m *Manager) applySnapshot(files []snapshotFile) (err error) {
	if err := validateSnapshotFiles(files); err != nil {
		return err
	}
	if err := os.MkdirAll(m.localDir, 0o700); err != nil {
		return err
	}
	// Coordinate with config.Update so a concurrent writer cannot interleave
	// with a snapshot restore.
	unlockConfig, err := fsutil.AcquireFileLock(context.Background(), filepath.Join(m.localDir, ".config.lock"))
	if err != nil {
		return fmt.Errorf("lock gbx config for restore: %w", err)
	}
	defer unlockConfig()

	stage, err := os.MkdirTemp(m.localDir, ".pull-stage-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	stageAuth := filepath.Join(stage, "auth")
	if err := os.MkdirAll(stageAuth, 0o700); err != nil {
		return err
	}
	var newConfigData []byte
	hasConfig := false
	for _, file := range files {
		if file.Path == "config.yaml" {
			hasConfig = true
			newConfigData = file.Data
			continue
		}
		target := filepath.Join(stage, filepath.FromSlash(file.Path))
		if err := writeAtomic(target, file.Data, 0o600); err != nil {
			return fmt.Errorf("stage %s: %w", file.Path, err)
		}
	}

	suffix, err := randomDeviceID()
	if err != nil {
		return err
	}
	configPath := filepath.Join(m.localDir, "config.yaml")
	authPath := filepath.Join(m.localDir, "auth")
	authBackup := filepath.Join(m.localDir, ".auth.pull-backup-"+suffix)
	var prevConfigData []byte
	hadConfig := false
	if data, readErr := fsutil.ReadFile(configPath); readErr == nil {
		hadConfig = true
		prevConfigData = data
	} else if !os.IsNotExist(readErr) {
		return fmt.Errorf("read existing config before restore: %w", readErr)
	}
	hadAuth := pathExists(authPath)

	rollback := func() {
		_ = os.RemoveAll(authPath)
		if hadAuth {
			_ = os.Rename(authBackup, authPath)
		}
		if hadConfig {
			_ = fsutil.WriteFileAtomic(configPath, prevConfigData, 0o600)
		} else {
			_ = os.Remove(configPath)
		}
	}
	if hadAuth {
		if err := os.Rename(authPath, authBackup); err != nil {
			return fmt.Errorf("prepare auth replacement: %w", err)
		}
	}
	if err := os.Rename(stageAuth, authPath); err != nil {
		rollback()
		return fmt.Errorf("replace auth directory: %w", err)
	}
	// Replace config.yaml atomically in place so concurrent readers never observe
	// a missing file and fall back to fresh-install defaults.
	if hasConfig {
		if err := fsutil.WriteFileAtomic(configPath, newConfigData, 0o600); err != nil {
			rollback()
			return fmt.Errorf("replace config: %w", err)
		}
	} else if hadConfig {
		if err := os.Remove(configPath); err != nil && !os.IsNotExist(err) {
			rollback()
			return fmt.Errorf("remove config: %w", err)
		}
	}
	if hadAuth {
		_ = os.RemoveAll(authBackup)
	}
	if err := os.Chmod(authPath, 0o700); err != nil {
		return err
	}
	return nil
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
