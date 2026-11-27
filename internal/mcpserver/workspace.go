package mcpserver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/grokbuildx/gbx/internal/fsutil"
	"github.com/grokbuildx/gbx/internal/paths"
)

var (
	errOutsideWorkspace = errors.New("path resolves outside the workspace or inside .git")
	worktreeNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
)

// PersistedConfig is stored at ~/.gbx/mcp.json so zero-argument `gbx mcp-server`
// works across MCP clients that do not implement roots/list.
type PersistedConfig struct {
	Workspaces     []string `json:"workspaces,omitempty"`
	WorkspaceRoots []string `json:"workspace_roots,omitempty"`
	BatchOnly      bool     `json:"batch_only,omitempty"`
}

// LoadPersistedConfig reads ~/.gbx/mcp.json (or custom path if non-empty).
// Missing files return an empty config with nil error without touching disk.
func LoadPersistedConfig(configPath string) (PersistedConfig, error) {
	if strings.TrimSpace(configPath) == "" {
		var err error
		configPath, err = paths.MCPConfigPath()
		if err != nil {
			return PersistedConfig{}, nil
		}
	}
	data, err := fsutil.ReadRegularFileLimited(configPath, 1<<20)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return PersistedConfig{}, nil
		}
		return PersistedConfig{}, err
	}
	var cfg PersistedConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return PersistedConfig{}, fmt.Errorf("parse %s: %w", configPath, err)
	}
	cfg.Workspaces = dedupeCleanPaths(cfg.Workspaces)
	cfg.WorkspaceRoots = dedupeCleanPaths(cfg.WorkspaceRoots)
	return cfg, nil
}

// SavePersistedConfig writes ~/.gbx/mcp.json atomically with mode 0600, merging
// with any existing entries when merge is true.
func SavePersistedConfig(configPath string, incoming PersistedConfig, merge bool) (string, PersistedConfig, error) {
	if strings.TrimSpace(configPath) == "" {
		var err error
		configPath, err = paths.MCPConfigPath()
		if err != nil {
			return "", PersistedConfig{}, err
		}
	}
	var merged PersistedConfig
	if merge {
		existing, _ := LoadPersistedConfig(configPath)
		merged.Workspaces = append(merged.Workspaces, existing.Workspaces...)
		merged.WorkspaceRoots = append(merged.WorkspaceRoots, existing.WorkspaceRoots...)
		merged.BatchOnly = existing.BatchOnly
	}
	if incoming.BatchOnly {
		merged.BatchOnly = true
	}
	merged.Workspaces = dedupeCleanPaths(append(merged.Workspaces, incoming.Workspaces...))
	merged.WorkspaceRoots = dedupeCleanPaths(append(merged.WorkspaceRoots, incoming.WorkspaceRoots...))

	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return configPath, merged, err
	}
	data, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return configPath, merged, err
	}
	data = append(data, '\n')
	if err := fsutil.WriteFileAtomic(configPath, data, 0o600); err != nil {
		return configPath, merged, err
	}
	return configPath, merged, nil
}

func dedupeCleanPaths(in []string) []string {
	var out []string
	seen := make(map[string]bool, len(in))
	for _, raw := range in {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		abs, err := filepath.Abs(trimmed)
		if err != nil {
			continue
		}
		clean := filepath.Clean(abs)
		if !seen[clean] {
			seen[clean] = true
			out = append(out, clean)
		}
	}
	return out
}

func splitEnvPaths(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	// Support both OS PATH list separator and comma/newline separators.
	var parts []string
	for _, line := range strings.Split(raw, "\n") {
		for _, chunk := range filepath.SplitList(line) {
			if t := strings.TrimSpace(chunk); t != "" {
				parts = append(parts, t)
			}
		}
	}
	return parts
}

// Workspace represents a validated project root or session worktree.
type Workspace struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Path   string `json:"path"`
	Source string `json:"source"` // "cli", "config", "env", "root", "dynamic", or "client_root"
	real   string
}

type workspaceManager struct {
	fixed       []Workspace
	roots       []string
	locked      bool
	worktreeDir string
	configPath  string

	mu           sync.RWMutex
	clientRoots  []Workspace
	dynamicFixed []Workspace
	dynamicRoots []string
}

func newWorkspace(dirPath, source string) (Workspace, error) {
	abs, err := filepath.Abs(dirPath)
	if err != nil {
		return Workspace{}, fmt.Errorf("resolve workspace %q: %w", dirPath, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return Workspace{}, fmt.Errorf("workspace %q: %w", abs, err)
	}
	if !info.IsDir() {
		return Workspace{}, fmt.Errorf("workspace %q is not a directory", abs)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return Workspace{}, fmt.Errorf("resolve workspace symlinks %q: %w", abs, err)
	}
	sum := sha256.Sum256([]byte(real))
	return Workspace{
		ID:     hex.EncodeToString(sum[:6]),
		Name:   filepath.Base(abs),
		Path:   abs,
		Source: source,
		real:   real,
	}, nil
}

func validateSafeDynamicDir(abs string, asRoot bool) error {
	clean := filepath.Clean(abs)
	if clean == filepath.Dir(clean) {
		return fmt.Errorf("refusing to expose filesystem root %q as a workspace", clean)
	}
	if !asRoot {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			if filepath.Clean(home) == clean {
				return fmt.Errorf("refusing to expose home directory %q directly as a single workspace; pass a code subdirectory (such as ~/Space/GitHub with as_root=true)", clean)
			}
		}
	}
	return nil
}

func newWorkspaceManager(cfg Config) (*workspaceManager, error) {
	wm := &workspaceManager{
		locked:      cfg.LockWorkspaces,
		worktreeDir: strings.TrimSpace(cfg.WorktreeDir),
		configPath:  strings.TrimSpace(cfg.ConfigPath),
	}
	seen := make(map[string]bool)
	seenRoots := make(map[string]bool)

	addFixed := func(p, source string, strict bool) error {
		if strings.TrimSpace(p) == "" {
			return nil
		}
		ws, err := newWorkspace(p, source)
		if err != nil {
			if strict {
				return err
			}
			return nil
		}
		if !seen[ws.real] {
			seen[ws.real] = true
			wm.fixed = append(wm.fixed, ws)
		}
		return nil
	}
	addRoot := func(r string, strict bool) error {
		if strings.TrimSpace(r) == "" {
			return nil
		}
		abs, err := filepath.Abs(r)
		if err != nil {
			if strict {
				return fmt.Errorf("resolve workspace root %q: %w", r, err)
			}
			return nil
		}
		info, err := os.Stat(abs)
		if err != nil || !info.IsDir() {
			if strict {
				return fmt.Errorf("workspace root %q is not a directory", abs)
			}
			return nil
		}
		clean := filepath.Clean(abs)
		if !seenRoots[clean] {
			seenRoots[clean] = true
			wm.roots = append(wm.roots, clean)
		}
		return nil
	}

	for _, p := range cfg.Workspaces {
		if err := addFixed(p, "cli", true); err != nil {
			return nil, err
		}
	}
	for _, r := range cfg.WorkspaceRoots {
		if err := addRoot(r, true); err != nil {
			return nil, err
		}
	}

	// When not locked, merge environment variables and ~/.gbx/mcp.json defaults.
	if !cfg.LockWorkspaces {
		for _, p := range splitEnvPaths(os.Getenv("GBX_MCP_WORKSPACES")) {
			_ = addFixed(p, "env", false)
		}
		for _, r := range splitEnvPaths(os.Getenv("GBX_MCP_WORKSPACE_ROOTS")) {
			_ = addRoot(r, false)
		}
		if persisted, err := LoadPersistedConfig(wm.configPath); err == nil {
			for _, p := range persisted.Workspaces {
				_ = addFixed(p, "config", false)
			}
			for _, r := range persisted.WorkspaceRoots {
				_ = addRoot(r, false)
			}
		}
	}

	if cfg.LockWorkspaces && len(wm.fixed) == 0 && len(wm.roots) == 0 {
		return nil, errors.New("--lock-workspaces requires at least one --workspace or --workspace-root")
	}
	return wm, nil
}

func (wm *workspaceManager) Locked() bool {
	return wm.locked
}

func (wm *workspaceManager) WorktreeStoreRoot() (string, error) {
	if wm.worktreeDir != "" {
		return filepath.Abs(wm.worktreeDir)
	}
	return paths.ServeDir()
}

// AddDynamic registers a workspace or workspace-root at runtime and optionally
// persists it to ~/.gbx/mcp.json (best-effort if filesystem is read-only/sandboxed).
func (wm *workspaceManager) AddDynamic(rawPath string, asRoot, persist bool) ([]Workspace, bool, error) {
	if wm.locked {
		return nil, false, errors.New("workspaces are locked by --lock-workspaces")
	}
	trimmed := strings.TrimSpace(rawPath)
	if trimmed == "" {
		return nil, false, errors.New("path is required")
	}
	abs, err := filepath.Abs(trimmed)
	if err != nil {
		return nil, false, err
	}
	if err := validateSafeDynamicDir(abs, asRoot); err != nil {
		return nil, false, err
	}
	ws, err := newWorkspace(abs, "dynamic")
	if err != nil {
		return nil, false, err
	}

	wm.mu.Lock()
	if asRoot {
		exists := false
		for _, r := range wm.dynamicRoots {
			if r == ws.Path {
				exists = true
				break
			}
		}
		if !exists {
			wm.dynamicRoots = append(wm.dynamicRoots, ws.Path)
		}
	} else {
		exists := false
		for _, existing := range wm.dynamicFixed {
			if existing.real == ws.real {
				exists = true
				break
			}
		}
		if !exists {
			wm.dynamicFixed = append(wm.dynamicFixed, ws)
		}
	}
	wm.mu.Unlock()

	persisted := false
	if persist {
		var incoming PersistedConfig
		if asRoot {
			incoming.WorkspaceRoots = []string{ws.Path}
		} else {
			incoming.Workspaces = []string{ws.Path}
		}
		if _, _, saveErr := SavePersistedConfig(wm.configPath, incoming, true); saveErr == nil {
			persisted = true
		}
	}
	return wm.List(), persisted, nil
}

// RemoveDynamic removes a dynamically or config-added workspace/root.
func (wm *workspaceManager) RemoveDynamic(targetPath string) (bool, error) {
	if wm.locked {
		return false, errors.New("workspaces are locked by --lock-workspaces")
	}
	abs, err := filepath.Abs(strings.TrimSpace(targetPath))
	if err != nil {
		return false, err
	}
	clean := filepath.Clean(abs)
	realTarget := clean
	if r, rErr := filepath.EvalSymlinks(clean); rErr == nil {
		realTarget = r
	}

	removed := false
	wm.mu.Lock()
	filteredFixed := wm.dynamicFixed[:0]
	for _, w := range wm.dynamicFixed {
		if w.Path == clean || w.real == realTarget {
			removed = true
			continue
		}
		filteredFixed = append(filteredFixed, w)
	}
	wm.dynamicFixed = filteredFixed

	filteredRoots := wm.dynamicRoots[:0]
	for _, r := range wm.dynamicRoots {
		if r == clean {
			removed = true
			continue
		}
		filteredRoots = append(filteredRoots, r)
	}
	wm.dynamicRoots = filteredRoots
	wm.mu.Unlock()

	// Also remove from persisted ~/.gbx/mcp.json if present.
	if cfg, loadErr := LoadPersistedConfig(wm.configPath); loadErr == nil && (len(cfg.Workspaces) > 0 || len(cfg.WorkspaceRoots) > 0) {
		var next PersistedConfig
		changed := false
		for _, p := range cfg.Workspaces {
			if filepath.Clean(p) == clean {
				changed = true
				continue
			}
			next.Workspaces = append(next.Workspaces, p)
		}
		for _, r := range cfg.WorkspaceRoots {
			if filepath.Clean(r) == clean {
				changed = true
				continue
			}
			next.WorkspaceRoots = append(next.WorkspaceRoots, r)
		}
		if changed {
			if _, _, saveErr := SavePersistedConfig(wm.configPath, next, false); saveErr == nil {
				removed = true
			}
		}
	}
	return removed, nil
}

// SetClientRoots updates dynamic workspaces reported by MCP roots/list.
func (wm *workspaceManager) SetClientRoots(uris []string) {
	if wm.locked {
		return
	}
	var parsed []Workspace
	seen := make(map[string]bool)
	for _, rawURI := range uris {
		localPath, ok := fileURIToPath(rawURI)
		if !ok {
			continue
		}
		ws, err := newWorkspace(localPath, "client_root")
		if err != nil || seen[ws.real] {
			continue
		}
		seen[ws.real] = true
		parsed = append(parsed, ws)
	}
	wm.mu.Lock()
	wm.clientRoots = parsed
	wm.mu.Unlock()
}

func fileURIToPath(raw string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", false
	}
	if !strings.Contains(trimmed, "://") {
		if filepath.IsAbs(trimmed) {
			return trimmed, true
		}
		return "", false
	}
	u, err := url.Parse(trimmed)
	if err != nil || !strings.EqualFold(u.Scheme, "file") {
		return "", false
	}
	if u.Host != "" && !strings.EqualFold(u.Host, "localhost") {
		return "", false
	}
	p := u.Path
	if runtime.GOOS == "windows" && len(p) >= 3 && p[0] == '/' && p[2] == ':' {
		p = p[1:]
	}
	if p == "" {
		return "", false
	}
	return filepath.FromSlash(p), true
}

func hasDotGit(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

// expandRootWorkspaces scans immediate subdirectories of root, and for grouping
// folders that are not themselves Git repositories (such as ~/Space/GitHub/Swifty),
// also scans one nested level for Git repositories (such as Swifty/SwiftyKafka).
func expandRootWorkspaces(root string, seen map[string]bool) []Workspace {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var children []Workspace
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		childPath := filepath.Join(root, entry.Name())
		ws, err := newWorkspace(childPath, "root")
		if err != nil {
			continue
		}
		if hasDotGit(ws.real) {
			if !seen[ws.real] {
				seen[ws.real] = true
				children = append(children, ws)
			}
			continue
		}

		// Non-Git grouping directory: scan immediate children for nested Git repos.
		subEntries, subErr := os.ReadDir(ws.real)
		if subErr == nil {
			for _, sub := range subEntries {
				if strings.HasPrefix(sub.Name(), ".") {
					continue
				}
				subPath := filepath.Join(childPath, sub.Name())
				if !hasDotGit(subPath) {
					continue
				}
				subWS, sErr := newWorkspace(subPath, "root")
				if sErr == nil && !seen[subWS.real] {
					seen[subWS.real] = true
					children = append(children, subWS)
				}
			}
		}
		if !seen[ws.real] {
			seen[ws.real] = true
			children = append(children, ws)
		}
	}
	sort.Slice(children, func(i, j int) bool {
		return strings.ToLower(children[i].Path) < strings.ToLower(children[j].Path)
	})
	return children
}

// List returns all currently available workspaces.
func (wm *workspaceManager) List() []Workspace {
	wm.mu.RLock()
	clientRoots := append([]Workspace(nil), wm.clientRoots...)
	dynFixed := append([]Workspace(nil), wm.dynamicFixed...)
	dynRoots := append([]string(nil), wm.dynamicRoots...)
	wm.mu.RUnlock()

	var out []Workspace
	seen := make(map[string]bool)
	add := func(ws Workspace) {
		if !seen[ws.real] {
			seen[ws.real] = true
			out = append(out, ws)
		}
	}
	for _, ws := range wm.fixed {
		add(ws)
	}
	for _, ws := range dynFixed {
		add(ws)
	}
	for _, ws := range clientRoots {
		// If a client root is a parent folder of multiple repos rather than a single repo,
		// include both the root itself and any discovered nested repos!
		add(ws)
		if !hasDotGit(ws.real) {
			for _, child := range expandRootWorkspaces(ws.Path, seen) {
				out = append(out, child)
			}
		}
	}
	allRoots := append(append([]string(nil), wm.roots...), dynRoots...)
	for _, root := range allRoots {
		for _, child := range expandRootWorkspaces(root, seen) {
			out = append(out, child)
		}
	}
	return out
}

// ResolveBase resolves the base project Workspace (before narrowing to a worktree).
func (wm *workspaceManager) ResolveBase(selector string) (Workspace, error) {
	list := wm.List()
	if len(list) == 0 {
		if wm.locked {
			return Workspace{}, errors.New("no workspace available")
		}
		return Workspace{}, errors.New("no workspace configured yet: call workspace_add with the project path (or code parent directory with as_root=true), or pass --workspace / --workspace-root to gbx mcp-server")
	}
	selector = strings.TrimSpace(selector)
	if selector == "" {
		if len(list) == 1 {
			return list[0], nil
		}
		names := make([]string, 0, len(list))
		for _, ws := range list {
			names = append(names, fmt.Sprintf("%s (%s)", ws.Name, ws.ID))
		}
		return Workspace{}, fmt.Errorf("multiple workspaces available (%s); specify the 'workspace' argument by name, id, or path", strings.Join(names, ", "))
	}

	// 1. Exact ID or path match.
	cleanedAbs, absErr := filepath.Abs(selector)
	cleanedReal := ""
	if absErr == nil {
		if r, err := filepath.EvalSymlinks(cleanedAbs); err == nil {
			cleanedReal = r
		}
	}
	for _, ws := range list {
		if ws.ID == selector || ws.Path == selector || (absErr == nil && ws.Path == cleanedAbs) || (cleanedReal != "" && ws.real == cleanedReal) {
			return ws, nil
		}
	}

	// 2. Case-insensitive Name or trailing relative path match (e.g. "Swifty/SwiftyKafka").
	slashedSel := strings.Trim(filepath.ToSlash(selector), "/")
	var matches []Workspace
	for _, ws := range list {
		if strings.EqualFold(ws.Name, selector) {
			matches = append(matches, ws)
			continue
		}
		if strings.Contains(slashedSel, "/") {
			wsSlash := filepath.ToSlash(ws.Path)
			if strings.HasSuffix(strings.ToLower(wsSlash), "/"+strings.ToLower(slashedSel)) {
				matches = append(matches, ws)
			}
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return Workspace{}, fmt.Errorf("workspace selector %q matches multiple folders; pass its workspace id or full path", selector)
	}
	return Workspace{}, fmt.Errorf("workspace %q is not registered; check workspace_list or call workspace_add first", selector)
}

// Resolve resolves both the base Workspace and an optional managed worktree name.
func (wm *workspaceManager) Resolve(selector, worktree string) (Workspace, error) {
	ws, err := wm.ResolveBase(selector)
	if err != nil {
		return Workspace{}, err
	}
	worktree = strings.TrimSpace(worktree)
	if worktree == "" {
		return ws, nil
	}
	return wm.worktreeWorkspace(ws, worktree)
}

func (wm *workspaceManager) worktreeDirFor(ws Workspace, name string) (string, error) {
	if !worktreeNamePattern.MatchString(name) {
		return "", fmt.Errorf("invalid worktree name %q: use 1-40 lowercase letters, digits, or hyphens", name)
	}
	storeRoot, err := wm.WorktreeStoreRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(storeRoot, ".worktrees", ws.ID, name), nil
}

func (wm *workspaceManager) worktreeWorkspace(ws Workspace, name string) (Workspace, error) {
	dir, err := wm.worktreeDirFor(ws, name)
	if err != nil {
		return Workspace{}, err
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return Workspace{}, fmt.Errorf("worktree %q does not exist", name)
	}
	info, err := os.Stat(real)
	if err != nil || !info.IsDir() {
		return Workspace{}, fmt.Errorf("worktree %q does not exist", name)
	}
	return Workspace{
		ID:     ws.ID,
		Name:   ws.Name + "@" + name,
		Path:   dir,
		Source: "worktree",
		real:   real,
	}, nil
}

func hasGitPathSegment(slashPath string) bool {
	for _, seg := range strings.Split(slashPath, "/") {
		if strings.EqualFold(seg, ".git") {
			return true
		}
	}
	return false
}

func withinWorkspace(target, root string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// resolve maps a workspace-relative (or inside-workspace absolute) path to a
// symlink-verified absolute path inside ws, rejecting any .git segment.
func (ws Workspace) resolve(rel string) (string, error) {
	if strings.ContainsRune(rel, 0) {
		return "", errOutsideWorkspace
	}
	trimmed := strings.TrimSpace(rel)
	if trimmed != "" && filepath.IsAbs(trimmed) {
		if r, err := filepath.Rel(ws.Path, trimmed); err == nil && withinWorkspace(filepath.Join(ws.Path, r), ws.Path) {
			trimmed = r
		} else if r, err := filepath.Rel(ws.real, trimmed); err == nil && withinWorkspace(filepath.Join(ws.real, r), ws.real) {
			trimmed = r
		} else {
			return "", errOutsideWorkspace
		}
	}
	clean := path.Clean("/" + filepath.ToSlash(trimmed))
	if hasGitPathSegment(clean) {
		return "", errOutsideWorkspace
	}
	abs := filepath.Join(ws.Path, filepath.FromSlash(clean))
	real, err := filepath.EvalSymlinks(abs)
	if errors.Is(err, fs.ErrNotExist) {
		existing, missing := abs, ""
		for errors.Is(err, fs.ErrNotExist) && existing != filepath.Dir(existing) {
			missing = filepath.Join(filepath.Base(existing), missing)
			existing = filepath.Dir(existing)
			real, err = filepath.EvalSymlinks(existing)
		}
		if err != nil {
			return "", err
		}
		real = filepath.Join(real, missing)
	} else if err != nil {
		return "", err
	}
	if !withinWorkspace(real, ws.real) {
		return "", errOutsideWorkspace
	}
	if relReal, err := filepath.Rel(ws.real, real); err != nil || hasGitPathSegment(filepath.ToSlash(relReal)) {
		return "", errOutsideWorkspace
	}
	return real, nil
}

func (ws Workspace) relative(abs string) string {
	rel, err := filepath.Rel(ws.real, abs)
	if err != nil || rel == "." {
		return ""
	}
	return filepath.ToSlash(rel)
}

func (ws Workspace) checkedPath(rel string) (string, error) {
	abs, err := ws.resolve(rel)
	if err != nil {
		return "", err
	}
	if r := ws.relative(abs); r != "" {
		return r, nil
	}
	return ".", nil
}

var commonDeveloperBinDirs = []string{
	"/opt/homebrew/bin",
	"/opt/homebrew/sbin",
	"/usr/local/bin",
}

// commandEnv scrubs GBX_SERVE_* secrets and enriches minimal macOS GUI PATHs
// so toolchains (git, go, node, python, cargo) work when spawned by desktop apps.
func commandEnv(extra ...string) []string {
	inherited := os.Environ()
	out := make([]string, 0, len(inherited)+len(extra)+2)
	var pathVal string
	pathIdx := -1
	for _, entry := range inherited {
		key, val, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "GBX_SERVE_") {
			continue
		}
		if upper == "PATH" {
			pathIdx = len(out)
			pathVal = val
		}
		out = append(out, entry)
	}
	enriched := enrichDeveloperPath(pathVal)
	if pathIdx >= 0 {
		out[pathIdx] = "PATH=" + enriched
	} else if enriched != "" {
		out = append(out, "PATH="+enriched)
	}
	out = append(out, extra...)
	return out
}

func enrichDeveloperPath(current string) string {
	parts := filepath.SplitList(current)
	seen := make(map[string]bool, len(parts))
	for _, p := range parts {
		if p != "" {
			seen[filepath.Clean(p)] = true
		}
	}
	var candidates []string
	if runtime.GOOS != "windows" {
		candidates = append(candidates, commonDeveloperBinDirs...)
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			candidates = append(candidates,
				filepath.Join(home, "go", "bin"),
				filepath.Join(home, ".cargo", "bin"),
				filepath.Join(home, ".local", "bin"),
			)
		}
	}
	for _, cand := range candidates {
		clean := filepath.Clean(cand)
		if seen[clean] {
			continue
		}
		if info, err := os.Stat(clean); err == nil && info.IsDir() {
			seen[clean] = true
			parts = append(parts, clean)
		}
	}
	return strings.Join(parts, string(filepath.ListSeparator))
}
