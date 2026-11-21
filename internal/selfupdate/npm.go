package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// NpmInstallPaths identifies the native binary and command shim/symlink of a
// global npm installation of @grokbuildx/gbx.
type NpmInstallPaths struct {
	Binary  string
	Command string
}

// NpmGlobalPaths uses npm's destination, not the executable that happens to be
// running. npm may remove or move that executable during an upgrade.
func NpmGlobalPaths(root, goos string) (NpmInstallPaths, error) {
	root = filepath.Clean(strings.TrimSpace(root))
	if !filepath.IsAbs(root) || filepath.Base(root) != "node_modules" {
		return NpmInstallPaths{}, fmt.Errorf("unexpected npm global root %q", root)
	}
	prefix := filepath.Dir(root)
	command := filepath.Join(prefix, "gbx.cmd")
	if goos != "windows" {
		if filepath.Base(prefix) != "lib" {
			return NpmInstallPaths{}, fmt.Errorf("unexpected npm global root %q", root)
		}
		command = filepath.Join(filepath.Dir(prefix), "bin", "gbx")
	}
	return NpmInstallPaths{
		Binary:  filepath.Join(root, "@grokbuildx", "gbx", "bin", "gbx.exe"),
		Command: command,
	}, nil
}

// NpmUpdateArgs constructs the npm install arguments pinning version.
func NpmUpdateArgs(version string) []string {
	return []string{"install", "-g", "@grokbuildx/gbx@" + NpmPackageVersion(version), "--include=optional", "--prefer-online"}
}

// UpdateViaNpm runs global npm installation, verifies the resulting binary and
// symlink, and falls back to downloading the verified GitHub release binary if
// npm's postinstall failed or skipped the optional native package.
func UpdateViaNpm(version string) (recovered bool, args []string, backupPath string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "npm", "root", "-g").Output()
	if err != nil {
		return false, nil, "", fmt.Errorf("find npm's global install destination: %w", err)
	}
	paths, err := NpmGlobalPaths(string(out), runtime.GOOS)
	if err != nil {
		return false, nil, "", err
	}
	args = NpmUpdateArgs(version)
	fmt.Printf("Updating via npm... Running 'npm %s'\n", strings.Join(args, " "))
	recovered, err = CompleteNpmUpdate(
		func() error { return RunCommand("npm", args...) },
		func() error { return VerifyNpmDestination(paths, version) },
		func() error {
			if runtime.GOOS == "windows" {
				return fmt.Errorf("Windows cannot repair the running executable in place")
			}
			fmt.Println("The npm-installed command is unusable; downloading the release binary directly...")
			dlCtx, dlCancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer dlCancel()
			if err := os.MkdirAll(filepath.Dir(paths.Binary), 0o755); err != nil {
				return err
			}
			backup, err := SelfUpdateTo(dlCtx, version, paths.Binary)
			if err != nil {
				return err
			}
			backupPath = backup
			return RestoreNpmCommand(paths)
		},
	)
	if err != nil {
		return false, args, backupPath, err
	}
	return recovered, args, backupPath, nil
}

// CompleteNpmUpdate verifies even after npm exits nonzero: postinstall can fail
// after npm removed the old platform package. A recovery must itself be verified
// before reporting a working command, and must not be called an npm success.
func CompleteNpmUpdate(install, verify, recoverFn func() error) (bool, error) {
	installErr := install()
	verifyErr := verify()
	if verifyErr == nil {
		return false, installErr
	}
	if err := recoverFn(); err != nil {
		return false, errors.Join(installErr, verifyErr, fmt.Errorf("binary recovery failed: %w", err))
	}
	if err := verify(); err != nil {
		return false, fmt.Errorf("verify recovered binary: %w", err)
	}
	return true, nil
}

// VerifyNpmDestination checks that the npm-installed binary reports want and
// that the global command symlink points to it.
func VerifyNpmDestination(paths NpmInstallPaths, want string) error {
	if err := VerifyNpmInstall(paths.Binary, want); err != nil {
		return err
	}
	info, err := os.Stat(paths.Command)
	if err != nil {
		return fmt.Errorf("npm command %s is missing: %w", paths.Command, err)
	}
	if info.IsDir() {
		return fmt.Errorf("npm command %s is a directory", paths.Command)
	}
	if runtime.GOOS != "windows" {
		target, err := filepath.EvalSymlinks(paths.Command)
		if err != nil {
			return err
		}
		binary, err := filepath.EvalSymlinks(paths.Binary)
		if err != nil {
			return err
		}
		if target != binary {
			return fmt.Errorf("%s points to another installation", paths.Command)
		}
	}
	return nil
}

// VerifyNpmInstall runs binary version and verifies it matches want.
func VerifyNpmInstall(binary, want string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binary, "version").Output()
	if err != nil {
		return fmt.Errorf("npm-installed %s does not run: %w", binary, err)
	}
	// The first line is gbx; later lines describe the bundled Grok Build.
	got, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	got = strings.TrimSpace(got)
	if !strings.HasPrefix(got, "gbx version ") || CanonicalReleaseTag(strings.TrimPrefix(got, "gbx version ")) != CanonicalReleaseTag(want) {
		return fmt.Errorf("npm-installed %s reports %q, expected gbx version %s", binary, got, CanonicalReleaseTag(want))
	}
	return nil
}

// RestoreNpmCommand restores a missing npm command symlink after a failed
// install without overwriting another installation that owns the command.
func RestoreNpmCommand(paths NpmInstallPaths) error {
	if target, err := filepath.EvalSymlinks(paths.Command); err == nil {
		binary, err := filepath.EvalSymlinks(paths.Binary)
		if err != nil {
			return err
		}
		if target == binary {
			return nil
		}
		return fmt.Errorf("%s points to another installation", paths.Command)
	}
	if _, err := os.Lstat(paths.Command); err == nil || !os.IsNotExist(err) {
		return fmt.Errorf("cannot restore existing npm command %s", paths.Command)
	}
	if err := os.MkdirAll(filepath.Dir(paths.Command), 0o755); err != nil {
		return err
	}
	target, err := filepath.Rel(filepath.Dir(paths.Command), paths.Binary)
	if err != nil {
		return err
	}
	return os.Symlink(target, paths.Command)
}
