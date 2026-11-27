package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

var worktreeLocalFiles = []string{
	"AGENTS.local.md",
	"CLAUDE.local.md",
	".mcp.json",
	filepath.Join(".grok", "settings.local.json"),
	filepath.Join(".grok", "config.local.toml"),
	filepath.Join(".grok", "sandbox.toml"),
	filepath.Join(".claude", "settings.local.json"),
}

var worktreeLocalDirs = []string{
	filepath.Join(".grok", "skills"),
	filepath.Join(".grok", "agents"),
	filepath.Join(".grok", "commands"),
	filepath.Join(".agents", "skills"),
}

type worktreeMeta struct {
	Name       string `json:"name"`
	Branch     string `json:"branch"`
	Base       string `json:"base,omitempty"`
	BaseCommit string `json:"base_commit"`
	Path       string `json:"path"`
}

func (s *Server) registerWorktreeTools() {
	s.addTool(ToolDef{
		Name:        "worktree_create",
		Description: "Create an isolated Git worktree on branch gbx/<name> at HEAD so edits and tests do not disturb the main working tree. Pass worktree='<name>' to subsequent file/git/command tools.",
		InputSchema: objectSchema(map[string]JSONSchema{
			"workspace": {
				Type:        "string",
				Description: "Optional workspace ID, folder name, or path when multiple workspaces are connected.",
			},
			"name": {
				Type:        "string",
				Description: "Short worktree slug (1-40 lowercase letters, digits, or hyphens, e.g. 'fix-auth-bug').",
			},
		}, "name"),
		Annotations: mutatingAnnotations("Create Worktree", false, false),
	}, s.toolWorktreeCreate)

	s.addTool(ToolDef{
		Name:        "worktree_list",
		Description: "List managed GBX Git worktrees for the workspace along with ahead/behind/dirty counts.",
		InputSchema: objectSchema(map[string]JSONSchema{
			"workspace": {
				Type:        "string",
				Description: "Optional workspace ID, folder name, or path when multiple workspaces are connected.",
			},
		}),
		Annotations: readOnlyAnnotations("List Worktrees"),
	}, s.toolWorktreeList)

	s.addTool(ToolDef{
		Name:        "worktree_merge",
		Description: "Merge a clean, committed GBX worktree branch (gbx/<name>) back into its base branch in the main workspace (--no-ff, aborts cleanly on conflict).",
		InputSchema: objectSchema(map[string]JSONSchema{
			"workspace": {
				Type:        "string",
				Description: "Optional workspace ID, folder name, or path when multiple workspaces are connected.",
			},
			"name": {
				Type:        "string",
				Description: "Worktree name created by worktree_create.",
			},
		}, "name"),
		Annotations: mutatingAnnotations("Merge Worktree", false, false),
	}, s.toolWorktreeMerge)

	s.addTool(ToolDef{
		Name:        "worktree_remove",
		Description: "Remove a managed GBX Git worktree after merging or discarding work.",
		InputSchema: objectSchema(map[string]JSONSchema{
			"workspace": {
				Type:        "string",
				Description: "Optional workspace ID, folder name, or path when multiple workspaces are connected.",
			},
			"name": {
				Type:        "string",
				Description: "Worktree name created by worktree_create.",
			},
			"force": {
				Type:        "boolean",
				Description: "Discard uncommitted changes inside the worktree when removing.",
			},
		}, "name"),
		Annotations: mutatingAnnotations("Remove Worktree", true, true),
	}, s.toolWorktreeRemove)
}

func (s *Server) worktreeManifestDir(ws Workspace) (string, error) {
	storeRoot, err := s.workspaces.WorktreeStoreRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(storeRoot, ".worktree-overlays", ws.ID), nil
}

func (s *Server) worktreeMetaPath(ws Workspace, name string) (string, error) {
	dir, err := s.worktreeManifestDir(ws)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".json"), nil
}

func (s *Server) worktreeOverlaysPath(ws Workspace, name string) (string, error) {
	dir, err := s.worktreeManifestDir(ws)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

func validateOverlayRel(raw string) (string, bool) {
	rel := strings.TrimSpace(filepath.ToSlash(raw))
	if rel == "" || strings.ContainsRune(rel, 0) || strings.HasPrefix(rel, "/") || filepath.IsAbs(raw) {
		return "", false
	}
	for i, seg := range strings.Split(rel, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.EqualFold(seg, ".git") {
			return "", false
		}
		if i == 0 && strings.Contains(seg, ":") {
			return "", false
		}
	}
	return rel, true
}

func copyFileIfMissing(from, to string) bool {
	info, err := os.Lstat(from)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	if _, err := os.Lstat(to); !errors.Is(err, fs.ErrNotExist) {
		return false
	}
	data, err := os.ReadFile(from)
	if err != nil {
		return false
	}
	if os.MkdirAll(filepath.Dir(to), 0o755) != nil {
		return false
	}
	return os.WriteFile(to, data, info.Mode().Perm()) == nil
}

func copyUntrackedSubdir(srcRoot, dstRoot, relDir string) []string {
	srcDir := filepath.Join(srcRoot, relDir)
	info, err := os.Lstat(srcDir)
	if err != nil || !info.IsDir() {
		return nil
	}
	var copied []string
	_ = filepath.WalkDir(srcDir, func(cur string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(srcRoot, cur)
		if err != nil || strings.HasPrefix(rel, "..") {
			return nil
		}
		if copyFileIfMissing(cur, filepath.Join(dstRoot, rel)) {
			copied = append(copied, filepath.ToSlash(rel))
		}
		return nil
	})
	return copied
}

func (s *Server) writeOverlaysManifest(ws Workspace, name string, overlays []string) {
	manifest, err := s.worktreeOverlaysPath(ws, name)
	if err != nil || os.MkdirAll(filepath.Dir(manifest), 0o700) != nil {
		return
	}
	seen := make(map[string]bool, len(overlays))
	var lines []string
	for _, raw := range overlays {
		rel, ok := validateOverlayRel(raw)
		if ok && !seen[rel] {
			seen[rel] = true
			lines = append(lines, rel)
		}
	}
	content := ""
	if len(lines) > 0 {
		content = strings.Join(lines, "\n") + "\n"
	}
	_ = os.WriteFile(manifest, []byte(content), 0o600)
}

func (s *Server) readOverlaysManifest(ws Workspace, name string) map[string]struct{} {
	out := make(map[string]struct{}, len(worktreeLocalFiles))
	for _, rel := range worktreeLocalFiles {
		if valid, ok := validateOverlayRel(rel); ok {
			out[valid] = struct{}{}
		}
	}
	manifest, err := s.worktreeOverlaysPath(ws, name)
	if err != nil {
		return out
	}
	raw, err := os.ReadFile(manifest)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if valid, ok := validateOverlayRel(line); ok {
			out[valid] = struct{}{}
		}
	}
	return out
}

func countWorktreeDirty(porcelain []byte, overlays map[string]struct{}) int {
	count := 0
	entries := bytes.Split(porcelain, []byte{0})
	for i := 0; i < len(entries); i++ {
		entry := string(entries[i])
		if len(entry) < 4 {
			continue
		}
		if entry[0] == 'R' || entry[0] == 'C' {
			i++
		}
		if entry[:3] == "?? " {
			rel := entry[3:]
			if _, ok := overlays[rel]; ok {
				continue
			}
		}
		count++
	}
	return count
}

func (s *Server) loadOrInferWorktreeMeta(ctx context.Context, ws Workspace, name string) (worktreeMeta, error) {
	dir, err := s.workspaces.worktreeDirFor(ws, name)
	if err != nil {
		return worktreeMeta{}, err
	}
	if metaPath, err := s.worktreeMetaPath(ws, name); err == nil {
		if raw, err := os.ReadFile(metaPath); err == nil {
			var m worktreeMeta
			if json.Unmarshal(raw, &m) == nil && m.Name == name {
				m.Path = dir
				return m, nil
			}
		}
	}
	tree, err := s.workspaces.worktreeWorkspace(ws, name)
	if err != nil {
		return worktreeMeta{}, err
	}
	branch, _ := gitText(ctx, tree, "symbolic-ref", "--quiet", "--short", "HEAD")
	if branch == "" {
		branch = "gbx/" + name
	}
	base, _ := gitText(ctx, ws, "symbolic-ref", "--quiet", "--short", "HEAD")
	baseCommit := ""
	if base != "" {
		baseCommit, _ = gitText(ctx, tree, "merge-base", base, "HEAD")
	}
	return worktreeMeta{
		Name:       name,
		Branch:     branch,
		Base:       base,
		BaseCommit: baseCommit,
		Path:       dir,
	}, nil
}

func (s *Server) toolWorktreeCreate(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		Workspace string `json:"workspace"`
		Name      string `json:"name"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", err
	}
	name := strings.TrimSpace(args.Name)
	ws, err := s.resolveBaseWorkspace(ctx, args.Workspace)
	if err != nil {
		return "", err
	}
	dir, err := s.workspaces.worktreeDirFor(ws, name)
	if err != nil {
		return "", err
	}
	if inside, err := gitText(ctx, ws, "rev-parse", "--is-inside-work-tree"); err != nil || inside != "true" {
		return "", errors.New("workspace is not a git repository")
	}
	commit, err := gitText(ctx, ws, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if err != nil || commit == "" {
		return "", errors.New("repository has no commits yet")
	}
	base, _ := gitText(ctx, ws, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return "", fmt.Errorf("create worktree directory (if running in a sandboxed desktop app, enable full file access or pass --worktree-dir): %w", err)
	}
	meta := worktreeMeta{
		Name:       name,
		Branch:     "gbx/" + name,
		Base:       base,
		BaseCommit: commit,
		Path:       dir,
	}
	if _, err := gitText(ctx, ws, "worktree", "add", "-b", meta.Branch, "--", dir, commit); err != nil {
		return "", err
	}
	var overlays []string
	for _, rel := range worktreeLocalFiles {
		if copyFileIfMissing(filepath.Join(ws.Path, rel), filepath.Join(dir, rel)) {
			overlays = append(overlays, filepath.ToSlash(rel))
		}
	}
	for _, relDir := range worktreeLocalDirs {
		overlays = append(overlays, copyUntrackedSubdir(ws.Path, dir, relDir)...)
	}
	s.writeOverlaysManifest(ws, name, overlays)
	if metaPath, err := s.worktreeMetaPath(ws, name); err == nil {
		if data, mErr := json.Marshal(meta); mErr == nil {
			_ = os.MkdirAll(filepath.Dir(metaPath), 0o700)
			_ = os.WriteFile(metaPath, data, 0o600)
		}
	}
	return marshalToolJSON(meta)
}

func (s *Server) toolWorktreeList(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		Workspace string `json:"workspace"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", err
	}
	ws, err := s.resolveBaseWorkspace(ctx, args.Workspace)
	if err != nil {
		return "", err
	}
	storeRoot, err := s.workspaces.WorktreeStoreRoot()
	if err != nil {
		return "", err
	}
	wtRoot := filepath.Join(storeRoot, ".worktrees", ws.ID)
	entries, err := os.ReadDir(wtRoot)
	type wtItem struct {
		worktreeMeta
		Ahead  int `json:"ahead"`
		Behind int `json:"behind"`
		Dirty  int `json:"dirty"`
	}
	items := []wtItem{}
	if err == nil {
		for _, entry := range entries {
			if !entry.IsDir() || !worktreeNamePattern.MatchString(entry.Name()) {
				continue
			}
			tree, tErr := s.workspaces.worktreeWorkspace(ws, entry.Name())
			if tErr != nil {
				continue
			}
			meta, mErr := s.loadOrInferWorktreeMeta(ctx, ws, entry.Name())
			if mErr != nil {
				continue
			}
			item := wtItem{worktreeMeta: meta}
			compareBase := meta.Base
			if compareBase == "" {
				compareBase = meta.BaseCommit
			}
			if compareBase != "" {
				if counts, err := gitText(ctx, tree, "rev-list", "--left-right", "--count", compareBase+"...HEAD"); err == nil {
					if fields := strings.Fields(counts); len(fields) == 2 {
						item.Behind, _ = strconv.Atoi(fields[0])
						item.Ahead, _ = strconv.Atoi(fields[1])
					}
				}
			}
			if stRes, err := runGit(ctx, tree, nil, "status", "--porcelain", "-z", "-uall"); err == nil && stRes.code == 0 {
				item.Dirty = countWorktreeDirty(stRes.stdout, s.readOverlaysManifest(ws, entry.Name()))
			}
			items = append(items, item)
		}
	}
	return marshalToolJSON(map[string]any{"worktrees": items})
}

func (s *Server) toolWorktreeMerge(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		Workspace string `json:"workspace"`
		Name      string `json:"name"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", err
	}
	name := strings.TrimSpace(args.Name)
	ws, err := s.resolveBaseWorkspace(ctx, args.Workspace)
	if err != nil {
		return "", err
	}
	tree, err := s.workspaces.worktreeWorkspace(ws, name)
	if err != nil {
		return "", err
	}
	meta, err := s.loadOrInferWorktreeMeta(ctx, ws, name)
	if err != nil {
		return "", err
	}
	if meta.Base == "" {
		return "", errors.New("worktree started from a detached HEAD; merge its branch manually")
	}
	current, _ := gitText(ctx, ws, "symbolic-ref", "--quiet", "--short", "HEAD")
	if current != meta.Base {
		return "", fmt.Errorf("main workspace currently has %q checked out; switch to %q before merging", current, meta.Base)
	}
	if stRes, err := runGit(ctx, tree, nil, "status", "--porcelain", "-z", "-uall"); err == nil && stRes.code == 0 {
		if countWorktreeDirty(stRes.stdout, s.readOverlaysManifest(ws, name)) > 0 {
			return "", errors.New("worktree has uncommitted changes; git_commit or discard them before merging")
		}
	}
	ahead := 0
	if counts, err := gitText(ctx, tree, "rev-list", "--left-right", "--count", meta.Base+"...HEAD"); err == nil {
		if fields := strings.Fields(counts); len(fields) == 2 {
			ahead, _ = strconv.Atoi(fields[1])
		}
	}
	if ahead == 0 {
		return "", errors.New("worktree branch has no commits ahead of " + meta.Base)
	}
	if changes, err := gitText(ctx, ws, "status", "--porcelain", "--untracked-files=no"); err != nil || changes != "" {
		return "", errors.New("main workspace has uncommitted changes; commit or stash them before merging")
	}
	res, err := runGit(ctx, ws, nil, "merge", "--no-ff", "--no-edit", meta.Branch)
	if err != nil {
		return "", err
	}
	output := strings.TrimSpace(string(res.stdout) + "\n" + res.stderr)
	if res.code != 0 {
		_, _ = runGit(ctx, ws, nil, "merge", "--abort")
		return "", fmt.Errorf("merge conflict (aborted cleanly): %s", output)
	}
	head, _ := gitText(ctx, ws, "rev-parse", "HEAD")
	return marshalToolJSON(map[string]any{
		"merged_branch": meta.Branch,
		"into":          meta.Base,
		"merge_commit":  head,
		"output":        output,
	})
}

func pruneEmptyOverlayParents(root *os.Root, rel string) {
	if root == nil {
		return
	}
	for parent := filepath.Dir(filepath.FromSlash(rel)); parent != "." && parent != "" && parent != string(filepath.Separator); parent = filepath.Dir(parent) {
		if root.Remove(parent) != nil {
			break
		}
	}
}

func (s *Server) toolWorktreeRemove(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		Workspace string `json:"workspace"`
		Name      string `json:"name"`
		Force     bool   `json:"force"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", err
	}
	name := strings.TrimSpace(args.Name)
	ws, err := s.resolveBaseWorkspace(ctx, args.Workspace)
	if err != nil {
		return "", err
	}
	dir, err := s.workspaces.worktreeDirFor(ws, name)
	if err != nil {
		return "", err
	}
	meta, _ := s.loadOrInferWorktreeMeta(ctx, ws, name)
	if meta.Branch == "" {
		meta.Branch = "gbx/" + name
	}

	if _, statErr := os.Stat(dir); statErr == nil {
		tree := Workspace{ID: ws.ID, Name: ws.Name, Path: dir, real: dir}
		if real, rErr := filepath.EvalSymlinks(dir); rErr == nil {
			tree.real = real
		}
		overlays := s.readOverlaysManifest(ws, name)
		if !args.Force {
			stRes, stErr := runGit(ctx, tree, nil, "status", "--porcelain", "-z", "-uall")
			if stErr != nil {
				return "", stErr
			}
			if stRes.code == 0 && countWorktreeDirty(stRes.stdout, overlays) > 0 {
				return "", errors.New("worktree has uncommitted changes; commit them or pass force=true")
			}
		}
		type overlayBackup struct {
			rel  string
			data []byte
			perm fs.FileMode
		}
		var backups []overlayBackup
		if root, rootErr := os.OpenRoot(dir); rootErr == nil {
			for rel := range overlays {
				validRel, ok := validateOverlayRel(rel)
				if !ok {
					continue
				}
				nativeRel := filepath.FromSlash(validRel)
				if _, err := gitText(ctx, tree, "ls-files", "--error-unmatch", "--", nativeRel); err == nil {
					continue
				}
				fi, fiErr := root.Lstat(nativeRel)
				if fiErr != nil || !fi.Mode().IsRegular() {
					continue
				}
				if !args.Force {
					if d, rErr := root.ReadFile(nativeRel); rErr == nil {
						backups = append(backups, overlayBackup{rel: nativeRel, data: d, perm: fi.Mode().Perm()})
					}
				}
				if root.Remove(nativeRel) == nil {
					pruneEmptyOverlayParents(root, validRel)
				}
			}
			_ = root.Close()
		}
		rmArgs := []string{"worktree", "remove"}
		if args.Force {
			rmArgs = append(rmArgs, "--force")
		}
		if _, err := gitText(ctx, ws, append(rmArgs, "--", dir)...); err != nil {
			if !args.Force {
				for _, b := range backups {
					target := filepath.Join(dir, b.rel)
					_ = os.MkdirAll(filepath.Dir(target), 0o755)
					_ = os.WriteFile(target, b.data, b.perm)
				}
			}
			return "", err
		}
	}
	if manifest, err := s.worktreeOverlaysPath(ws, name); err == nil {
		_ = os.Remove(manifest)
	}
	if metaPath, err := s.worktreeMetaPath(ws, name); err == nil {
		_ = os.Remove(metaPath)
	}
	_, _ = gitText(ctx, ws, "worktree", "prune")
	_, _ = gitText(ctx, ws, "branch", "-d", "--", meta.Branch)
	return marshalToolJSON(map[string]any{
		"removed": name,
		"branch":  meta.Branch,
	})
}
