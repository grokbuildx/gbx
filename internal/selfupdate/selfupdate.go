// Package selfupdate resolves gbx release versions, downloads and verifies
// release binaries against SHA256SUMS, and performs atomic in-place or npm
// package updates.
package selfupdate

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"
)

// RepoReleasesBase is the base URL for gbx's GitHub release asset downloads.
// Asset names are produced by .github/workflows/release.yml and consumed by bin/wrapper.js.
const RepoReleasesBase = "https://github.com/grokbuildx/gbx/releases/download"

// ReleaseArchiveSuffix is appended to a binary's asset name on GitHub releases:
// binaries ship gzip-compressed (about a third of their size). The npm package
// still carries them uncompressed.
const ReleaseArchiveSuffix = ".gz"

// MaxReleaseBinaryBytes caps the decompressed download so a corrupt or hostile
// archive cannot fill the disk. Release binaries are about 30 MiB.
var MaxReleaseBinaryBytes int64 = 256 << 20

const (
	defaultLatestReleaseAPI    = "https://api.github.com/repos/grokbuildx/gbx/releases/latest"
	defaultLatestReleaseWebURL = "https://github.com/grokbuildx/gbx/releases/latest"
	defaultNpmRegistryBase     = "https://registry.npmjs.org"
)

// LatestReleaseAPI is the GitHub endpoint naming the newest published release.
// It is the version source of truth: the GitHub release and its assets exist
// before the npm job that publishes the matching package has run.
var LatestReleaseAPI = defaultLatestReleaseAPI

// LatestReleaseWebURL is the non-API GitHub release redirect fallback used when
// LatestReleaseAPI is rate-limited (HTTP 403/429) or unreachable.
var LatestReleaseWebURL = defaultLatestReleaseWebURL

// NpmRegistryBase is the default npm registry used for version checks.
var NpmRegistryBase = defaultNpmRegistryBase

// HTTPClient is used for version metadata lookups (GitHub release and npm registry).
var HTTPClient = &http.Client{Timeout: 10 * time.Second}

// RunCommand executes an external update command (npm/go) attached to stdio.
func RunCommand(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, name, args...)
	command.Stdout, command.Stderr, command.Stdin = os.Stdout, os.Stderr, os.Stdin
	return command.Run()
}

// DetectInstallMethod infers how this gbx was installed from where its binary
// lives: inside an npm package, in a Go bin directory, or anywhere else
// (a downloaded release binary).
func DetectInstallMethod() string {
	path, err := os.Executable()
	if err != nil {
		return "self"
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return InstallMethodForPath(path, GoBinDirs())
}

// InstallMethodForPath classifies an executable path as "npm", "go", or "self".
func InstallMethodForPath(path string, goBins []string) string {
	slashed := filepath.ToSlash(path)
	if strings.Contains(slashed, "/node_modules/@grokbuildx/gbx/") {
		return "npm"
	}
	for _, platform := range []string{"darwin-x64", "darwin-arm64", "linux-x64", "linux-arm64", "win32-x64", "win32-arm64"} {
		if strings.Contains(slashed, "/node_modules/@grokbuildx/gbx-"+platform+"/") {
			return "npm"
		}
	}
	dir := filepath.Dir(path)
	for _, bin := range goBins {
		if bin != "" && filepath.Clean(bin) == dir {
			return "go"
		}
	}
	return "self"
}

// GoBinDirs returns the directories where `go install` places binaries.
func GoBinDirs() []string {
	var dirs []string
	if gobin := os.Getenv("GOBIN"); gobin != "" {
		dirs = append(dirs, gobin)
	}
	gopath := os.Getenv("GOPATH")
	if gopath == "" {
		if home, err := os.UserHomeDir(); err == nil {
			gopath = filepath.Join(home, "go")
		}
	}
	for _, entry := range filepath.SplitList(gopath) {
		dirs = append(dirs, filepath.Join(entry, "bin"))
	}
	return dirs
}

// LatestVersions resolves the version gbx update installs and whether npm
// carries it. The GitHub release is authoritative and appears first; npm is
// both the fallback when the GitHub API is unreachable (it is blocked on some
// networks) and the gate on the npm method.
func LatestVersions() (latest string, npmReady bool) {
	latest, npmReady, _ = LatestVersionsDetailed()
	return latest, npmReady
}

// LatestVersionsDetailed queries GitHub release and npm registry concurrently,
// returning a diagnostic error when neither source answered.
func LatestVersionsDetailed() (latest string, npmReady bool, err error) {
	var (
		release, npm       string
		releaseErr, npmErr error
		done               = make(chan struct{})
	)
	go func() {
		npm, npmErr = FetchLatestNpmVersion()
		close(done)
	}()
	release, releaseErr = FetchLatestReleaseVersion()
	<-done

	switch {
	case releaseErr == nil:
		latest = release
	case npmErr == nil:
		latest = npm
	default:
		return "unknown", false, fmt.Errorf("GitHub: %v; npm: %v", releaseErr, npmErr)
	}
	return latest, npmErr == nil && npm != "unknown" && CanonicalReleaseTag(npm) == CanonicalReleaseTag(latest), nil
}

// FetchLatestReleaseVersion names the newest published GitHub release.
// When the default GitHub JSON API is rate-limited or unreachable, it falls
// back to the unthrottled /releases/latest HTTP redirect header.
func FetchLatestReleaseVersion() (string, error) {
	tag, unstable, err := fetchLatestReleaseAPI(LatestReleaseAPI)
	if err == nil {
		return tag, nil
	}
	// If the API returned an explicit draft/prerelease status, honor that
	// rejection instead of bypassing it through the web redirect.
	if unstable {
		return "", err
	}
	if LatestReleaseAPI == defaultLatestReleaseAPI && LatestReleaseWebURL != "" {
		if webTag, webErr := fetchLatestReleaseRedirect(LatestReleaseWebURL); webErr == nil {
			return webTag, nil
		}
	}
	return "", err
}

func fetchLatestReleaseAPI(endpoint string) (tag string, unstable bool, err error) {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "gbx-selfupdate")
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("HTTP status %d", resp.StatusCode)
	}

	var result struct {
		TagName    string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return "", false, err
	}
	if result.TagName == "" {
		return "", false, fmt.Errorf("empty tag in response")
	}
	if result.Draft || result.Prerelease {
		return "", true, fmt.Errorf("latest release %s is not a stable release", result.TagName)
	}
	return CanonicalReleaseTag(result.TagName), false, nil
}

func fetchLatestReleaseRedirect(webURL string) (string, error) {
	client := &http.Client{
		Timeout:   HTTPClient.Timeout,
		Transport: HTTPClient.Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequest(http.MethodHead, webURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "gbx-selfupdate")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 300 || resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP status %d", resp.StatusCode)
	}
	return parseReleaseTagFromRedirect(resp.Header.Get("Location"))
}

func parseReleaseTagFromRedirect(location string) (string, error) {
	const marker = "/releases/tag/"
	idx := strings.Index(location, marker)
	if idx < 0 {
		return "", fmt.Errorf("unexpected release redirect %q", location)
	}
	tag := location[idx+len(marker):]
	if cut := strings.IndexAny(tag, "?#/"); cut >= 0 {
		tag = tag[:cut]
	}
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return "", fmt.Errorf("empty release tag in redirect %q", location)
	}
	return CanonicalReleaseTag(tag), nil
}

// FetchLatestNpmVersion names the newest version of the npm launcher package.
// When using the default transport, a custom registry configured via
// npm_config_registry or ~/.npmrc (e.g. a regional mirror) is tried first
// before falling back to registry.npmjs.org.
func FetchLatestNpmVersion() (string, error) {
	bases := npmRegistryCandidates()
	var lastErr error
	for _, base := range bases {
		version, err := fetchLatestNpmVersionFrom(base)
		if err == nil {
			return version, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		return "", fmt.Errorf("no npm registry configured")
	}
	return "", lastErr
}

func npmRegistryCandidates() []string {
	defaultBase := strings.TrimRight(strings.TrimSpace(NpmRegistryBase), "/")
	if defaultBase == "" {
		defaultBase = defaultNpmRegistryBase
	}
	// Preserve deterministic isolation when unit tests inject a custom Transport.
	if HTTPClient.Transport != nil {
		return []string{defaultBase}
	}
	var out []string
	if configured := ConfiguredNpmRegistry(); configured != "" && !strings.EqualFold(configured, defaultBase) {
		out = append(out, configured)
	}
	out = append(out, defaultBase)
	return out
}

// ConfiguredNpmRegistry returns the user's configured npm registry for
// @grokbuildx (from npm_config_registry env or ~/.npmrc), normalized without a
// trailing slash, or "" when unset.
func ConfiguredNpmRegistry() string {
	var npmrc string
	if home, err := os.UserHomeDir(); err == nil {
		if raw, err := os.ReadFile(filepath.Join(home, ".npmrc")); err == nil {
			npmrc = string(raw)
		}
	}
	return parseConfiguredNpmRegistry(os.Getenv("NPM_CONFIG_REGISTRY"), os.Getenv("npm_config_registry"), npmrc)
}

func parseConfiguredNpmRegistry(envUpper, envLower, npmrc string) string {
	for _, envVal := range []string{envUpper, envLower} {
		if u := sanitizeRegistryURL(envVal); u != "" {
			return u
		}
	}
	var fallback string
	for _, line := range strings.Split(npmrc, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		val = sanitizeRegistryURL(strings.Trim(strings.TrimSpace(val), `"'`))
		if val == "" {
			continue
		}
		if key == "@grokbuildx:registry" {
			return val
		}
		if key == "registry" && fallback == "" {
			fallback = val
		}
	}
	return fallback
}

func sanitizeRegistryURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "https://") && !strings.HasPrefix(raw, "http://") {
		return ""
	}
	return strings.TrimRight(raw, "/")
}

func fetchLatestNpmVersionFrom(registryBase string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(registryBase, "/")+"/@grokbuildx/gbx/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "gbx-selfupdate")
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP status %d", resp.StatusCode)
	}

	var result struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return "", err
	}
	if result.Version == "" {
		return "", fmt.Errorf("empty version in response")
	}
	return CanonicalReleaseTag(result.Version), nil
}

// ReleaseAssetName returns the GitHub release asset name for the current
// GOOS/GOARCH, matching the names produced by .github/workflows/release.yml and
// consumed by bin/wrapper.js.
func ReleaseAssetName() (string, error) {
	platform, arch := runtime.GOOS, runtime.GOARCH
	switch platform {
	case "darwin", "linux":
		if arch != "amd64" && arch != "arm64" {
			return "", fmt.Errorf("unsupported architecture for self-update: %s/%s", runtime.GOOS, runtime.GOARCH)
		}
	case "windows":
		platform = "win32"
		if arch == "amd64" {
			arch = "x64"
		}
	default:
		return "", fmt.Errorf("unsupported platform for self-update: %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	return fmt.Sprintf("gbx-%s-%s%s", platform, arch, ext), nil
}

// ReleaseChecksum fetches the release's SHA256SUMS and returns the digest
// listed for asset. A release without one for the asset is refused rather than
// installed unchecked.
func ReleaseChecksum(ctx context.Context, version, asset string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ReleaseDownloadURL(version, "SHA256SUMS"), nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download SHA256SUMS: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download SHA256SUMS: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", err
	}
	return ChecksumFor(string(data), asset)
}

// ChecksumFor finds asset in sha256sum output ("<hex>  <name>" per line).
func ChecksumFor(sums, asset string) (string, error) {
	for _, line := range strings.Split(sums, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == asset && len(fields[0]) == 64 {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("SHA256SUMS lists no checksum for %s", asset)
}

// ReleaseDownloadURL resolves a release asset download URL. When the version is
// known it pins the exact release; otherwise it follows GitHub's /latest redirect.
func ReleaseDownloadURL(version, asset string) string {
	if version != "" && version != "unknown" {
		return fmt.Sprintf("%s/%s/%s", RepoReleasesBase, CanonicalReleaseTag(version), asset)
	}
	return fmt.Sprintf("https://github.com/grokbuildx/gbx/releases/latest/download/%s", asset)
}

// CanonicalReleaseTag names a version in release-tag form. A release tag is
// its npm version with a leading v, so both compare equal.
func CanonicalReleaseTag(version string) string {
	return "v" + strings.TrimPrefix(strings.TrimSpace(version), "v")
}

// NpmPackageVersion is the npm version published for a release tag.
func NpmPackageVersion(version string) string {
	return strings.TrimPrefix(CanonicalReleaseTag(version), "v")
}

// DownloadReleaseBinary streams the gzip-compressed release asset at url into
// dest, decompressing on the way. A progress bar (of compressed bytes) is drawn
// on stderr when stderr is a terminal and the server reports a content length.
func DownloadReleaseBinary(ctx context.Context, url, dest, wantSHA256 string) (err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(dest)
		}
	}()

	progress := &DownloadProgress{
		Reader: resp.Body,
		Total:  resp.ContentLength,
		IsTerm: term.IsTerminal(os.Stderr.Fd()),
	}
	digest := sha256.New()
	archive, err := gzip.NewReader(io.TeeReader(progress, digest))
	if err != nil {
		return fmt.Errorf("download is not a gzip release archive: %w", err)
	}
	// Read one byte past the limit so an oversized archive is detected rather
	// than silently truncated.
	written, err := io.Copy(f, io.LimitReader(archive, MaxReleaseBinaryBytes+1))
	if err != nil {
		return fmt.Errorf("decompress release archive: %w", err)
	}
	if written > MaxReleaseBinaryBytes {
		return fmt.Errorf("release archive expands beyond %s", HumanSize(MaxReleaseBinaryBytes))
	}
	if err = archive.Close(); err != nil {
		return fmt.Errorf("decompress release archive: %w", err)
	}
	// gzip stops at the end of its stream; drain any trailing bytes so the
	// digest covers the whole downloaded file.
	if _, err = io.Copy(digest, progress); err != nil {
		return fmt.Errorf("read release archive: %w", err)
	}
	if wantSHA256 != "" {
		if got := hex.EncodeToString(digest.Sum(nil)); !strings.EqualFold(got, wantSHA256) {
			return fmt.Errorf("release archive checksum mismatch: got %s, want %s", got, wantSHA256)
		}
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = ValidateReleaseBinary(dest); err != nil {
		return err
	}

	if progress.IsTerm {
		renderDownloadProgress(progress.Done, progress.Total)
		fmt.Fprintln(os.Stderr)
	} else {
		fmt.Fprintf(os.Stderr, "Downloaded %s (%s unpacked)\n", HumanSize(progress.Done), HumanSize(written))
	}
	return nil
}

// DownloadProgress counts the compressed bytes read from the response and
// redraws the progress bar at most every 60ms.
type DownloadProgress struct {
	Reader   io.Reader
	Total    int64
	Done     int64
	IsTerm   bool
	lastDraw time.Time
}

func (p *DownloadProgress) Read(buf []byte) (int, error) {
	n, err := p.Reader.Read(buf)
	p.Done += int64(n)
	if p.IsTerm && n > 0 && time.Since(p.lastDraw) >= 60*time.Millisecond {
		renderDownloadProgress(p.Done, p.Total)
		p.lastDraw = time.Now()
	}
	return n, err
}

// ValidateReleaseBinary inspects path without executing downloaded code.
// Accept both package builds and release builds made from main.go (which list
// this module as a dependency).
func ValidateReleaseBinary(path string) error {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return fmt.Errorf("download is not a Go executable: %w", err)
	}
	const module = "github.com/grokbuildx/gbx"
	ours := info.Path == module || info.Main.Path == module
	if info.Path == "command-line-arguments" {
		for _, dep := range info.Deps {
			if dep.Path == module {
				ours = true
			}
		}
	}
	if !ours {
		return fmt.Errorf("download is not a gbx executable")
	}
	settings := make(map[string]string)
	for _, entry := range info.Settings {
		settings[entry.Key] = entry.Value
	}
	if settings["GOOS"] != runtime.GOOS || settings["GOARCH"] != runtime.GOARCH {
		return fmt.Errorf("download targets %s/%s, expected %s/%s", settings["GOOS"], settings["GOARCH"], runtime.GOOS, runtime.GOARCH)
	}
	return nil
}

// renderDownloadProgress draws a single-line progress bar. A missing content
// length falls back to an unbounded byte counter.
func renderDownloadProgress(done, total int64) {
	const width = 30
	if total <= 0 {
		fmt.Fprintf(os.Stderr, "\rDownloading... %s", HumanSize(done))
		return
	}
	frac := float64(done) / float64(total)
	if frac > 1 {
		frac = 1
	}
	filled := int(frac * width)
	bar := strings.Repeat("=", filled) + strings.Repeat("-", width-filled)
	fmt.Fprintf(os.Stderr, "\r[%s] %5.1f%% (%s / %s)", bar, frac*100, HumanSize(done), HumanSize(total))
}

// HumanSize renders a byte count with a binary-prefix suffix.
func HumanSize(n int64) string {
	const unit = 1024.0
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	v := float64(n)
	suffixes := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	i := -1
	for v >= unit && i < len(suffixes)-1 {
		v /= unit
		i++
	}
	return fmt.Sprintf("%.1f %s", v, suffixes[i])
}

// SelfUpdate downloads the current platform's release binary and atomically
// replaces the running gbx binary at os.Executable().
func SelfUpdate(ctx context.Context, version string) (string, error) {
	target, err := os.Executable()
	if err != nil {
		return "", err
	}
	target, err = filepath.EvalSymlinks(target)
	if err != nil {
		return "", err
	}
	return SelfUpdateTo(ctx, version, target)
}

// SelfUpdateTo also supports npm recovery: its target is resolved before npm
// starts, rather than looking up the running executable after npm removed it.
func SelfUpdateTo(ctx context.Context, version, target string) (string, error) {
	if runtime.GOOS == "windows" {
		return "", fmt.Errorf("self-update is not supported on Windows; use the npm or go method instead")
	}
	asset, err := ReleaseAssetName()
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(target)
	staging, err := os.CreateTemp(dir, "."+filepath.Base(target)+".download-*")
	if err != nil {
		return "", err
	}
	tmp := staging.Name()
	if err := staging.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	defer os.Remove(tmp)

	archive := asset + ReleaseArchiveSuffix
	wantSHA256, err := ReleaseChecksum(ctx, version, archive)
	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := DownloadReleaseBinary(ctx, ReleaseDownloadURL(version, archive), tmp, wantSHA256); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		os.Remove(tmp)
		return "", err
	}

	return InstallReleaseBinary(target, tmp)
}

// InstallReleaseBinary validates downloaded, backs up target if present, and
// atomically renames downloaded into target.
func InstallReleaseBinary(target, downloaded string) (backupPath string, err error) {
	if err := ValidateReleaseBinary(downloaded); err != nil {
		return "", err
	}
	current, err := os.Open(target)
	if os.IsNotExist(err) {
		// A failed npm install may already have removed the old binary.
		if err := os.Rename(downloaded, target); err != nil {
			return "", fmt.Errorf("install missing binary: %w", err)
		}
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer current.Close()
	info, err := current.Stat()
	if err != nil {
		return "", err
	}
	backup, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".backup-*")
	if err != nil {
		return "", err
	}
	backupPath = backup.Name()
	defer func() {
		_ = backup.Close()
		if err != nil {
			_ = os.Remove(backupPath)
		}
	}()
	if err = backup.Chmod(info.Mode().Perm()); err != nil {
		return backupPath, err
	}
	if _, err = io.Copy(backup, current); err != nil {
		return backupPath, err
	}
	if err = backup.Sync(); err != nil {
		return backupPath, err
	}
	if err = backup.Close(); err != nil {
		return backupPath, err
	}
	if err = os.Rename(downloaded, target); err != nil {
		return backupPath, fmt.Errorf("install new binary: %w", err)
	}
	return backupPath, nil
}
