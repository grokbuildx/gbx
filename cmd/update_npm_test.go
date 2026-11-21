package cmd

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/grokbuildx/gbx/internal/selfupdate"
)

func TestNpmUpdateArgsPinVersionAndIncludeOptional(t *testing.T) {
	want := []string{"install", "-g", "@grokbuildx/gbx@1.6.7", "--include=optional", "--prefer-online"}
	if got := selfupdate.NpmUpdateArgs("v1.6.7"); !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
}

func TestNpmGlobalPaths(t *testing.T) {
	root := filepath.Join(t.TempDir(), "prefix", "lib", "node_modules")
	paths, err := selfupdate.NpmGlobalPaths(root+"\n", "linux")
	if err != nil {
		t.Fatal(err)
	}
	if paths.Binary != filepath.Join(root, "@grokbuildx", "gbx", "bin", "gbx.exe") ||
		paths.Command != filepath.Join(filepath.Dir(filepath.Dir(root)), "bin", "gbx") {
		t.Fatalf("unexpected paths: %+v", paths)
	}
	root = filepath.Join(t.TempDir(), "prefix", "node_modules")
	paths, err = selfupdate.NpmGlobalPaths(root, "windows")
	if err != nil || paths.Command != filepath.Join(filepath.Dir(root), "gbx.cmd") {
		t.Fatalf("Windows paths = %+v, %v", paths, err)
	}
	for _, root := range []string{"", "node_modules", t.TempDir()} {
		if _, err := selfupdate.NpmGlobalPaths(root, "linux"); err == nil {
			t.Fatalf("invalid npm root %q accepted", root)
		}
	}
}

func TestCompleteNpmUpdateHandlesSilentAndPostinstallFailures(t *testing.T) {
	installFailure := errors.New("npm postinstall exited 1")
	missing := errors.New("native binary missing")
	for _, tc := range []struct {
		name       string
		installErr error
		verifyErr  error
		recoverErr error
		afterErr   error
		wantRepair bool
		wantOK     bool
	}{
		{name: "normal install", wantOK: true},
		{name: "silent optional skip", verifyErr: missing, wantRepair: true, wantOK: true},
		{name: "nonzero postinstall", installErr: installFailure, verifyErr: missing, wantRepair: true, wantOK: true},
		{name: "working binary but npm failed", installErr: installFailure},
		{name: "download failed", installErr: installFailure, verifyErr: missing, recoverErr: errors.New("download failed"), wantRepair: true},
		{name: "recovery still broken", verifyErr: missing, afterErr: missing, wantRepair: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installs, verifies, repairs := 0, 0, 0
			recovered, err := selfupdate.CompleteNpmUpdate(
				func() error { installs++; return tc.installErr },
				func() error {
					verifies++
					if verifies > 1 {
						return tc.afterErr
					}
					return tc.verifyErr
				},
				func() error { repairs++; return tc.recoverErr },
			)
			if (err == nil) != tc.wantOK || recovered != (tc.wantRepair && tc.wantOK) {
				t.Fatalf("recovered=%v, error=%v", recovered, err)
			}
			if installs != 1 || (repairs == 1) != tc.wantRepair {
				t.Fatalf("installs=%d verifies=%d repairs=%d", installs, verifies, repairs)
			}
			if tc.installErr != nil && tc.verifyErr == nil && !errors.Is(err, tc.installErr) {
				t.Fatal("npm failure was lost")
			}
		})
	}
}

// Build a native fixture rather than a Unix shell script: these checks also run
// on Windows, and must never execute the developer's actual gbx from PATH.
func TestVerifyNpmInstallChecksOnlyNpmDestination(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "main.go")
	binary := filepath.Join(dir, "gbx.exe")
	if err := os.WriteFile(source, []byte("package main\nimport \"fmt\"\nfunc main() { fmt.Println(\"gbx version v1.6.7\\nGrok Build 1.0.45\") }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("go", "build", "-o", binary, source).CombinedOutput(); err != nil {
		t.Fatalf("build version fixture: %v\n%s", err, out)
	}
	if err := selfupdate.VerifyNpmInstall(binary, "1.6.7"); err != nil {
		t.Fatal(err)
	}
	if err := selfupdate.VerifyNpmInstall(binary, "v1.6.8"); err == nil || !strings.Contains(err.Error(), "expected") {
		t.Fatalf("old version accepted: %v", err)
	}
	// Another working gbx on PATH cannot mask a missing npm target.
	t.Setenv("PATH", dir)
	if err := selfupdate.VerifyNpmInstall(filepath.Join(dir, "missing.exe"), "v1.6.7"); err == nil {
		t.Fatal("missing npm binary was accepted")
	}
}

func TestRunUpdateNpmRepairsPackageWhenBinaryIsAlreadyCurrent(t *testing.T) {
	withReleaseAndNpm(t, 200, `{"tag_name":"v1.6.7"}`, 200, `{"version":"1.6.7"}`)
	previous := Version
	Version = "v1.6.7"
	t.Cleanup(func() { Version = previous })
	dir := t.TempDir()
	root := filepath.Join(dir, "prefix", "lib", "node_modules")
	if runtime.GOOS == "windows" {
		root = filepath.Join(dir, "prefix", "node_modules")
	}
	paths, err := selfupdate.NpmGlobalPaths(root, runtime.GOOS)
	if err != nil {
		t.Fatal(err)
	}
	build := func(name, source string) string {
		t.Helper()
		file := filepath.Join(dir, name+".go")
		if err := os.WriteFile(file, []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
		binary := filepath.Join(dir, name)
		if runtime.GOOS == "windows" {
			binary += ".exe"
		}
		if out, err := exec.Command("go", "build", "-o", binary, file).CombinedOutput(); err != nil {
			t.Fatalf("build npm fixture: %v\n%s", err, out)
		}
		return binary
	}
	native := build("native", "package main\nimport \"fmt\"\nfunc main() { fmt.Println(\"gbx version v1.6.7\\nGrok Build 1.0.45\") }\n")
	build("npm", `package main
import ("fmt"; "os"; "path/filepath"; "runtime"; "strings")
func main() {
    if len(os.Args) == 3 && os.Args[1] == "root" && os.Args[2] == "-g" {
        fmt.Println(os.Getenv("GBX_TEST_NPM_ROOT")); return
    }
    if err := os.WriteFile(os.Getenv("GBX_TEST_NPM_ARGS"), []byte(strings.Join(os.Args[1:], "\n")), 0600); err != nil { panic(err) }
    data, err := os.ReadFile(os.Getenv("GBX_TEST_NPM_NATIVE")); if err != nil { panic(err) }
    target := os.Getenv("GBX_TEST_NPM_TARGET")
    if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil { panic(err) }
    if err := os.WriteFile(target, data, 0755); err != nil { panic(err) }
    command := os.Getenv("GBX_TEST_NPM_COMMAND")
    if err := os.MkdirAll(filepath.Dir(command), 0755); err != nil { panic(err) }
    if runtime.GOOS == "windows" {
        if err := os.WriteFile(command, []byte("npm shim fixture"), 0755); err != nil { panic(err) }
    } else if err := os.Symlink(target, command); err != nil { panic(err) }
}
`)
	t.Setenv("PATH", dir)
	t.Setenv("GBX_TEST_NPM_ROOT", root)
	t.Setenv("GBX_TEST_NPM_NATIVE", native)
	t.Setenv("GBX_TEST_NPM_TARGET", paths.Binary)
	t.Setenv("GBX_TEST_NPM_COMMAND", paths.Command)
	argsPath := filepath.Join(dir, "npm-args")
	t.Setenv("GBX_TEST_NPM_ARGS", argsPath)
	if err := runUpdate("npm"); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil || string(args) != strings.Join(selfupdate.NpmUpdateArgs("v1.6.7"), "\n") {
		t.Fatalf("npm repair was skipped or called incorrectly: %q, %v", args, err)
	}
}

func TestRestoreNpmCommandAfterInstallRemovedIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows recovery does not create symlinks")
	}
	root := filepath.Join(t.TempDir(), "lib", "node_modules")
	paths, err := selfupdate.NpmGlobalPaths(root, runtime.GOOS)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.Binary), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Binary, []byte("recovered executable"), 0o755); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := selfupdate.RestoreNpmCommand(paths); err != nil {
			t.Fatal(err)
		}
	}
	if data, err := os.ReadFile(paths.Command); err != nil || string(data) != "recovered executable" {
		t.Fatalf("recovered command = %q, %v", data, err)
	}
	if err := os.Remove(paths.Command); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Command, []byte("another install"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := selfupdate.RestoreNpmCommand(paths); err == nil {
		t.Fatal("another installation was overwritten")
	}
}

func TestPrereleaseTagsMatchTheirNpmVersion(t *testing.T) {
	// Tag v1.0.0-0 is npm 1.0.0-0; both name one release.
	if selfupdate.CanonicalReleaseTag("v1.0.0-0") != selfupdate.CanonicalReleaseTag("1.0.0-0") {
		t.Fatal("tag and npm version of one release compare unequal")
	}
	for in, want := range map[string]string{"v1.0.0-0": "1.0.0-0", "1.0.0-0": "1.0.0-0", "v2.1.3": "2.1.3"} {
		if got := selfupdate.NpmPackageVersion(in); got != want {
			t.Fatalf("NpmPackageVersion(%q) = %q, want %q", in, got, want)
		}
	}
	if args := selfupdate.NpmUpdateArgs("v1.0.0-1"); args[2] != "@grokbuildx/gbx@1.0.0-1" {
		t.Fatalf("npm args = %v", args)
	}
}
