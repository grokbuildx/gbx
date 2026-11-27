package mcpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/grokbuildx/gbx/internal/fsutil"
)

const (
	maxReadBytes       = 2 << 20 // 2 MiB
	maxWriteBytes      = 5 << 20 // 5 MiB
	defaultReadLines   = 800
	defaultSearchLimit = 50
	maxSearchLimit     = 200
	maxWalkEntries     = 50000
)

func (s *Server) registerTools() {
	s.registerFSTools()
	s.registerGitTools()
	if !s.cfg.DisableExec {
		s.registerWorktreeTools()
		s.registerExecTools()
	}
	s.registerBatchTool()
}

func workspaceTargetProps() map[string]JSONSchema {
	return map[string]JSONSchema{
		"workspace": {
			Type:        "string",
			Description: "Optional workspace ID, folder name, or path when multiple workspaces are connected.",
		},
		"worktree": {
			Type:        "string",
			Description: "Optional managed worktree name (from worktree_create) to operate inside an isolated branch.",
		},
	}
}

func withTargetProps(props map[string]JSONSchema) map[string]JSONSchema {
	out := workspaceTargetProps()
	for k, v := range props {
		out[k] = v
	}
	return out
}

func marshalToolJSON(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (s *Server) resolveWorkspace(ctx context.Context, workspaceSelector, worktreeName string) (Workspace, error) {
	s.ensureInitialRoots(ctx)
	return s.workspaces.Resolve(workspaceSelector, worktreeName)
}

func (s *Server) resolveBaseWorkspace(ctx context.Context, workspaceSelector string) (Workspace, error) {
	s.ensureInitialRoots(ctx)
	return s.workspaces.ResolveBase(workspaceSelector)
}

func (s *Server) registerFSTools() {
	s.addTool(ToolDef{
		Name:        "workspace_list",
		Description: "List all local project workspaces currently available to GBX (from CLI flags, ~/.gbx/mcp.json, workspace_add, and/or MCP connected folders).",
		InputSchema: objectSchema(nil),
		Annotations: readOnlyAnnotations("List Workspaces"),
	}, s.toolWorkspaceList)

	if !s.workspaces.Locked() {
		s.addTool(ToolDef{
			Name:        "workspace_add",
			Description: "Register a local project directory (or a parent directory of multiple Git repositories when as_root=true, automatically discovering 1- and 2-level nested Git repos) and optionally persist it to ~/.gbx/mcp.json.",
			InputSchema: objectSchema(map[string]JSONSchema{
				"path": {
					Type:        "string",
					Description: "Absolute local path of the repository or parent code directory (e.g. '/Users/you/Space/GitHub').",
				},
				"as_root": {
					Type:        "boolean",
					Description: "Set true when path is a parent directory containing multiple repositories/sub-groups rather than a single repository.",
				},
				"persist": {
					Type:        "boolean",
					Description: "Persist this directory in ~/.gbx/mcp.json for future sessions (default true).",
				},
			}, "path"),
			Annotations: mutatingAnnotations("Add Workspace", false, true),
		}, s.toolWorkspaceAdd)

		s.addTool(ToolDef{
			Name:        "workspace_remove",
			Description: "Unregister a previously added workspace or workspace-root path from the server and ~/.gbx/mcp.json.",
			InputSchema: objectSchema(map[string]JSONSchema{
				"path": {
					Type:        "string",
					Description: "Absolute path to remove.",
				},
			}, "path"),
			Annotations: mutatingAnnotations("Remove Workspace", false, true),
		}, s.toolWorkspaceRemove)
	}

	s.addTool(ToolDef{
		Name:        "list_dir",
		Description: "List files and subdirectories at a relative directory path inside the workspace.",
		InputSchema: objectSchema(withTargetProps(map[string]JSONSchema{
			"path": {
				Type:        "string",
				Description: "Workspace-relative directory path (empty or '.' for workspace root).",
			},
		})),
		Annotations: readOnlyAnnotations("List Directory"),
	}, s.toolListDir)

	s.addTool(ToolDef{
		Name:        "read_file",
		Description: "Read one UTF-8 text file (via 'path') or multiple files in a single batch call (via 'paths') inside the workspace, returning SHA-256 hashes for safe editing.",
		InputSchema: objectSchema(withTargetProps(map[string]JSONSchema{
			"path": {
				Type:        "string",
				Description: "Workspace-relative path of a single file to read.",
			},
			"paths": {
				Type:        "array",
				Description: "Optional list of workspace-relative file paths to read together in one call (up to 20 files).",
				Items:       &JSONSchema{Type: "string"},
			},
			"start_line": {
				Type:        "integer",
				Description: "Optional 1-based starting line number (inclusive, applies when reading a single path).",
			},
			"end_line": {
				Type:        "integer",
				Description: "Optional 1-based ending line number (inclusive, applies when reading a single path).",
			},
		})),
		Annotations: readOnlyAnnotations("Read File"),
	}, s.toolReadFile)

	s.addTool(ToolDef{
		Name:        "edit_file",
		Description: "Replace exact text blocks in one file (via path/old_string/new_string) or apply multiple edits atomically in a single call (via 'edits'), checking expected_hash when provided.",
		InputSchema: objectSchema(withTargetProps(map[string]JSONSchema{
			"path": {
				Type:        "string",
				Description: "Workspace-relative path of the file to modify (when applying a single edit).",
			},
			"old_string": {
				Type:        "string",
				Description: "Exact existing text to replace.",
			},
			"new_string": {
				Type:        "string",
				Description: "Replacement text (may be empty to delete old_string).",
			},
			"replace_all": {
				Type:        "boolean",
				Description: "Set true to replace every occurrence of old_string in the file.",
			},
			"expected_hash": {
				Type:        "string",
				Description: "Optional SHA-256 hash returned by read_file; refuses the edit if the file changed on disk.",
			},
			"edits": {
				Type:        "array",
				Description: "Optional batch of edits across one or more files in the workspace, validated before writing.",
				Items: &JSONSchema{
					Type: "object",
					Properties: map[string]JSONSchema{
						"path":          {Type: "string", Description: "Workspace-relative file path."},
						"old_string":    {Type: "string", Description: "Exact existing text to replace."},
						"new_string":    {Type: "string", Description: "Replacement text."},
						"replace_all":   {Type: "boolean", Description: "Replace all occurrences."},
						"expected_hash": {Type: "string", Description: "Optional SHA-256 hash from read_file."},
					},
					Required: []string{"path", "old_string", "new_string"},
				},
			},
		})),
		Annotations: mutatingAnnotations("Edit File", false, false),
	}, s.toolEditFile)

	s.addTool(ToolDef{
		Name:        "write_file",
		Description: "Create a new file or replace an entire file inside the workspace atomically.",
		InputSchema: objectSchema(withTargetProps(map[string]JSONSchema{
			"path": {
				Type:        "string",
				Description: "Workspace-relative path of the file to create or overwrite.",
			},
			"content": {
				Type:        "string",
				Description: "Full UTF-8 text content to write.",
			},
			"expected_hash": {
				Type:        "string",
				Description: "Optional SHA-256 hash of the existing file when updating; leave empty for new files.",
			},
			"overwrite": {
				Type:        "boolean",
				Description: "Allow overwriting an existing file when expected_hash is omitted.",
			},
		}), "path", "content"),
		Annotations: mutatingAnnotations("Write File", false, true),
	}, s.toolWriteFile)

	s.addTool(ToolDef{
		Name:        "search_code",
		Description: "Search file contents (like git grep) or file names inside the workspace.",
		InputSchema: objectSchema(withTargetProps(map[string]JSONSchema{
			"query": {
				Type:        "string",
				Description: "Search string, regular expression, or file name pattern.",
			},
			"mode": {
				Type:        "string",
				Description: "Search 'content' (default) for lines inside files, or 'files' for matching file paths.",
				Enum:        []string{"content", "files"},
			},
			"path": {
				Type:        "string",
				Description: "Optional workspace-relative directory or file to scope the search.",
			},
			"regex": {
				Type:        "boolean",
				Description: "Treat query as a regular expression in content mode (default false: literal substring).",
			},
			"case_sensitive": {
				Type:        "boolean",
				Description: "Match case-sensitively in content mode (default false: case-insensitive).",
			},
			"limit": {
				Type:        "integer",
				Description: "Maximum matches to return (default 50, max 200).",
			},
		}), "query"),
		Annotations: readOnlyAnnotations("Search Code"),
	}, s.toolSearchCode)
}

func (s *Server) toolWorkspaceList(ctx context.Context, _ json.RawMessage) (string, error) {
	s.ensureInitialRoots(ctx)
	list := s.workspaces.List()
	type item struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Path   string `json:"path"`
		Source string `json:"source"`
		Branch string `json:"branch,omitempty"`
	}
	items := make([]item, 0, len(list))
	for _, ws := range list {
		branch, _ := gitText(ctx, ws, "symbolic-ref", "--quiet", "--short", "HEAD")
		items = append(items, item{
			ID:     ws.ID,
			Name:   ws.Name,
			Path:   ws.Path,
			Source: ws.Source,
			Branch: branch,
		})
	}
	return marshalToolJSON(map[string]any{
		"locked":     s.workspaces.Locked(),
		"workspaces": items,
	})
}

func (s *Server) toolWorkspaceAdd(_ context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		Path    string `json:"path"`
		AsRoot  bool   `json:"as_root"`
		Persist *bool  `json:"persist"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", err
	}
	persist := true
	if args.Persist != nil {
		persist = *args.Persist
	}
	list, persisted, err := s.workspaces.AddDynamic(args.Path, args.AsRoot, persist)
	if err != nil {
		return "", err
	}
	return marshalToolJSON(map[string]any{
		"added":      args.Path,
		"as_root":    args.AsRoot,
		"persisted":  persisted,
		"workspaces": list,
	})
}

func (s *Server) toolWorkspaceRemove(_ context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", err
	}
	removed, err := s.workspaces.RemoveDynamic(args.Path)
	if err != nil {
		return "", err
	}
	return marshalToolJSON(map[string]any{
		"removed":    removed,
		"workspaces": s.workspaces.List(),
	})
}

type dirEntryInfo struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"`
	Type     string    `json:"type"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

func (s *Server) toolListDir(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		Workspace string `json:"workspace"`
		Worktree  string `json:"worktree"`
		Path      string `json:"path"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", err
	}
	ws, err := s.resolveWorkspace(ctx, args.Workspace, args.Worktree)
	if err != nil {
		return "", err
	}
	dir, err := ws.resolve(args.Path)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	out := make([]dirEntryInfo, 0, len(entries))
	for _, entry := range entries {
		if strings.EqualFold(entry.Name(), ".git") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		kind := "file"
		switch {
		case entry.Type()&fs.ModeSymlink != 0:
			kind = "symlink"
			if target, err := ws.resolve(ws.relative(filepath.Join(dir, entry.Name()))); err == nil {
				if st, err := os.Stat(target); err == nil && st.IsDir() {
					kind = "dir"
				}
			}
		case entry.IsDir():
			kind = "dir"
		}
		out = append(out, dirEntryInfo{
			Name:     entry.Name(),
			Path:     ws.relative(filepath.Join(dir, entry.Name())),
			Type:     kind,
			Size:     info.Size(),
			Modified: info.ModTime().UTC(),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Type == "dir") != (out[j].Type == "dir") {
			return out[i].Type == "dir"
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return marshalToolJSON(map[string]any{
		"workspace": ws.Name,
		"path":      ws.relative(dir),
		"entries":   out,
	})
}

func isBinaryBytes(data []byte) bool {
	sample := data
	if len(sample) > 8192 {
		sample = sample[:8192]
	}
	if bytes.IndexByte(sample, 0) >= 0 {
		return true
	}
	return !utf8.Valid(data)
}

func readOneFileSlice(ws Workspace, relPath string, startLine, endLine int) (map[string]any, error) {
	file, err := ws.resolve(relPath)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(file)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%q is a directory; use list_dir instead", ws.relative(file))
	}
	data, err := fsutil.ReadRegularFileLimited(file, maxReadBytes)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	if isBinaryBytes(data) {
		return nil, fmt.Errorf("%q is a binary or non-UTF8 file (%d bytes, sha256 %s)", ws.relative(file), len(data), hash)
	}

	text := string(data)
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" && strings.HasSuffix(text, "\n") {
		lines = lines[:len(lines)-1]
	}
	totalLines := len(lines)
	if totalLines == 0 {
		return map[string]any{
			"path":        ws.relative(file),
			"hash":        hash,
			"size":        len(data),
			"total_lines": 0,
			"start_line":  0,
			"end_line":    0,
			"truncated":   false,
			"content":     "",
		}, nil
	}

	start := startLine
	if start <= 0 {
		start = 1
	}
	if start > totalLines {
		return nil, fmt.Errorf("start_line %d exceeds total_lines %d", start, totalLines)
	}
	end := endLine
	truncated := false
	if end <= 0 {
		end = start + defaultReadLines - 1
		if end < totalLines {
			truncated = true
		} else {
			end = totalLines
		}
	} else if end > totalLines {
		end = totalLines
	}
	if end < start {
		return nil, fmt.Errorf("end_line %d is before start_line %d", end, start)
	}

	slice := strings.Join(lines[start-1:end], "\n")
	if start == 1 && end == totalLines && strings.HasSuffix(text, "\n") {
		slice += "\n"
	}
	return map[string]any{
		"path":        ws.relative(file),
		"hash":        hash,
		"size":        len(data),
		"total_lines": totalLines,
		"start_line":  start,
		"end_line":    end,
		"truncated":   truncated,
		"content":     slice,
	}, nil
}

func (s *Server) toolReadFile(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		Workspace string   `json:"workspace"`
		Worktree  string   `json:"worktree"`
		Path      string   `json:"path"`
		Paths     []string `json:"paths"`
		StartLine int      `json:"start_line"`
		EndLine   int      `json:"end_line"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", err
	}
	ws, err := s.resolveWorkspace(ctx, args.Workspace, args.Worktree)
	if err != nil {
		return "", err
	}
	if len(args.Paths) > 0 {
		if len(args.Paths) > 20 {
			return "", errors.New("paths batch limit is 20 files per read_file call")
		}
		results := make([]map[string]any, 0, len(args.Paths))
		for _, p := range args.Paths {
			if strings.TrimSpace(p) == "" {
				continue
			}
			res, rErr := readOneFileSlice(ws, p, 0, 0)
			if rErr != nil {
				results = append(results, map[string]any{
					"path":  p,
					"error": rErr.Error(),
				})
				continue
			}
			results = append(results, res)
		}
		return marshalToolJSON(map[string]any{"files": results})
	}
	if strings.TrimSpace(args.Path) == "" {
		return "", errors.New("path (or paths) is required")
	}
	res, err := readOneFileSlice(ws, args.Path, args.StartLine, args.EndLine)
	if err != nil {
		return "", err
	}
	return marshalToolJSON(res)
}

func verifyAndReadExisting(file string, expectedHash string) ([]byte, string, fs.FileMode, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, "", 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, "", 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, "", 0, fmt.Errorf("%q is not a regular file", file)
	}
	if info.Size() > maxWriteBytes {
		return nil, "", 0, fmt.Errorf("file exceeds %d MiB safety limit", maxWriteBytes>>20)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxWriteBytes+1))
	if err != nil {
		return nil, "", 0, err
	}
	if int64(len(data)) > maxWriteBytes {
		return nil, "", 0, fmt.Errorf("file exceeds %d MiB safety limit", maxWriteBytes>>20)
	}
	sum := sha256.Sum256(data)
	currentHash := hex.EncodeToString(sum[:])
	if expectedHash != "" && !strings.EqualFold(currentHash, expectedHash) {
		return nil, currentHash, 0, fmt.Errorf("file changed on disk since read_file (expected hash %s, current hash %s); read_file again before editing", expectedHash, currentHash)
	}
	return data, currentHash, info.Mode().Perm(), nil
}

type singleEditSpec struct {
	Path         string `json:"path"`
	OldString    string `json:"old_string"`
	NewString    string `json:"new_string"`
	ReplaceAll   bool   `json:"replace_all"`
	ExpectedHash string `json:"expected_hash"`
}

func applyOneReplacement(relFile, content, oldStr, newStr string, replaceAll bool) (string, int, error) {
	if oldStr == "" {
		return "", 0, errors.New("old_string must not be empty; use write_file to create or replace an entire file")
	}
	if oldStr == newStr {
		return "", 0, errors.New("old_string and new_string are identical")
	}
	if strings.Contains(content, "\r\n") && !strings.Contains(oldStr, "\r\n") && strings.Contains(oldStr, "\n") {
		crlfOld := strings.ReplaceAll(oldStr, "\n", "\r\n")
		if strings.Contains(content, crlfOld) {
			oldStr = crlfOld
			if !strings.Contains(newStr, "\r\n") {
				newStr = strings.ReplaceAll(newStr, "\n", "\r\n")
			}
		}
	}
	count := strings.Count(content, oldStr)
	if count == 0 {
		return "", 0, fmt.Errorf("old_string not found in %s; read_file to inspect exact text and whitespace", relFile)
	}
	if count > 1 && !replaceAll {
		return "", 0, fmt.Errorf("old_string appears %d times in %s; include more surrounding context lines or set replace_all=true", count, relFile)
	}
	var updated string
	replaced := 1
	if replaceAll {
		updated = strings.ReplaceAll(content, oldStr, newStr)
		replaced = count
	} else {
		updated = strings.Replace(content, oldStr, newStr, 1)
	}
	if len(updated) > maxWriteBytes {
		return "", 0, fmt.Errorf("updated file %s exceeds %d MiB limit", relFile, maxWriteBytes>>20)
	}
	return updated, replaced, nil
}

func (s *Server) toolEditFile(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		Workspace string           `json:"workspace"`
		Worktree  string           `json:"worktree"`
		Edits     []singleEditSpec `json:"edits"`
		singleEditSpec
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", err
	}
	ws, err := s.resolveWorkspace(ctx, args.Workspace, args.Worktree)
	if err != nil {
		return "", err
	}

	specs := args.Edits
	batchMode := len(specs) > 0
	if !batchMode {
		if strings.Trim(path.Clean("/"+args.Path), "/") == "" {
			return "", errors.New("path (or edits) is required")
		}
		specs = []singleEditSpec{args.singleEditSpec}
	} else if len(specs) > 50 {
		return "", errors.New("edits batch limit is 50 operations per edit_file call")
	}

	type stagedFile struct {
		absPath      string
		relPath      string
		content      string
		mode         fs.FileMode
		replacements int
	}
	staged := make(map[string]*stagedFile)
	var order []string

	// Validate all edits in memory before writing any file on disk.
	for _, spec := range specs {
		if strings.Trim(path.Clean("/"+spec.Path), "/") == "" {
			return "", errors.New("each edit requires a non-empty path")
		}
		absFile, err := ws.resolve(spec.Path)
		if err != nil {
			return "", err
		}
		entry, exists := staged[absFile]
		if !exists {
			data, _, mode, err := verifyAndReadExisting(absFile, strings.TrimSpace(spec.ExpectedHash))
			if err != nil {
				return "", err
			}
			if isBinaryBytes(data) {
				return "", fmt.Errorf("cannot edit binary file %s", ws.relative(absFile))
			}
			entry = &stagedFile{
				absPath: absFile,
				relPath: ws.relative(absFile),
				content: string(data),
				mode:    mode,
			}
			staged[absFile] = entry
			order = append(order, absFile)
		}
		updated, nRep, err := applyOneReplacement(entry.relPath, entry.content, spec.OldString, spec.NewString, spec.ReplaceAll)
		if err != nil {
			return "", err
		}
		entry.content = updated
		entry.replacements += nRep
	}

	results := make([]map[string]any, 0, len(order))
	for _, absFile := range order {
		entry := staged[absFile]
		if err := fsutil.WriteFileAtomic(entry.absPath, []byte(entry.content), entry.mode); err != nil {
			return "", err
		}
		sum := sha256.Sum256([]byte(entry.content))
		results = append(results, map[string]any{
			"path":         entry.relPath,
			"hash":         hex.EncodeToString(sum[:]),
			"replacements": entry.replacements,
		})
	}
	if !batchMode && len(results) == 1 {
		return marshalToolJSON(results[0])
	}
	return marshalToolJSON(map[string]any{"edited": results})
}

func (s *Server) toolWriteFile(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		Workspace    string `json:"workspace"`
		Worktree     string `json:"worktree"`
		Path         string `json:"path"`
		Content      string `json:"content"`
		ExpectedHash string `json:"expected_hash"`
		Overwrite    bool   `json:"overwrite"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", err
	}
	if strings.Trim(path.Clean("/"+args.Path), "/") == "" {
		return "", errors.New("path is required")
	}
	if len(args.Content) > maxWriteBytes {
		return "", fmt.Errorf("content exceeds %d MiB limit", maxWriteBytes>>20)
	}
	ws, err := s.resolveWorkspace(ctx, args.Workspace, args.Worktree)
	if err != nil {
		return "", err
	}
	file, err := ws.resolve(args.Path)
	if err != nil {
		return "", err
	}

	expected := strings.TrimSpace(args.ExpectedHash)
	mode := fs.FileMode(0o644)
	info, statErr := os.Lstat(file)
	switch {
	case statErr == nil:
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("%q is not a regular file", ws.relative(file))
		}
		if expected == "" && !args.Overwrite {
			return "", fmt.Errorf("%q already exists; pass expected_hash from read_file or set overwrite=true", ws.relative(file))
		}
		if expected != "" {
			if _, _, existingMode, verifyErr := verifyAndReadExisting(file, expected); verifyErr != nil {
				return "", verifyErr
			} else {
				mode = existingMode
			}
		} else {
			mode = info.Mode().Perm()
		}
	case errors.Is(statErr, fs.ErrNotExist):
		if expected != "" {
			return "", fmt.Errorf("%q does not exist on disk, but expected_hash was provided", ws.relative(file))
		}
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			return "", err
		}
	default:
		return "", statErr
	}

	rawBytes := []byte(args.Content)
	if err := fsutil.WriteFileAtomic(file, rawBytes, mode); err != nil {
		return "", err
	}
	sum := sha256.Sum256(rawBytes)
	return marshalToolJSON(map[string]any{
		"path":          ws.relative(file),
		"hash":          hex.EncodeToString(sum[:]),
		"bytes_written": len(rawBytes),
	})
}

type contentMatch struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

func (s *Server) toolSearchCode(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		Workspace     string `json:"workspace"`
		Worktree      string `json:"worktree"`
		Query         string `json:"query"`
		Mode          string `json:"mode"`
		Path          string `json:"path"`
		Regex         bool   `json:"regex"`
		CaseSensitive bool   `json:"case_sensitive"`
		Limit         int    `json:"limit"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", err
	}
	query := strings.TrimSpace(args.Query)
	if query == "" {
		return "", errors.New("query is required")
	}
	limit := args.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	} else if limit > maxSearchLimit {
		limit = maxSearchLimit
	}
	ws, err := s.resolveWorkspace(ctx, args.Workspace, args.Worktree)
	if err != nil {
		return "", err
	}
	scopeRel := "."
	if strings.TrimSpace(args.Path) != "" {
		checked, err := ws.checkedPath(args.Path)
		if err != nil {
			return "", err
		}
		scopeRel = checked
	}

	if strings.EqualFold(strings.TrimSpace(args.Mode), "files") {
		files, err := listWorkspaceFiles(ctx, ws, scopeRel)
		if err != nil {
			return "", err
		}
		qLower := strings.ToLower(query)
		var matched []string
		for _, f := range files {
			fLower := strings.ToLower(f)
			base := path.Base(fLower)
			ok, _ := path.Match(qLower, base)
			if ok || strings.Contains(fLower, qLower) {
				matched = append(matched, f)
				if len(matched) >= limit {
					break
				}
			}
		}
		return marshalToolJSON(map[string]any{
			"mode":  "files",
			"files": matched,
		})
	}

	matches, truncated, err := searchWorkspaceContent(ctx, ws, scopeRel, query, args.Regex, args.CaseSensitive, limit)
	if err != nil {
		return "", err
	}
	return marshalToolJSON(map[string]any{
		"mode":      "content",
		"truncated": truncated,
		"matches":   matches,
	})
}

func listWorkspaceFiles(ctx context.Context, ws Workspace, scopeRel string) ([]string, error) {
	gitArgs := []string{"ls-files", "-z", "--cached", "--others", "--exclude-standard"}
	if scopeRel != "." {
		gitArgs = append(gitArgs, "--", scopeRel)
	}
	if res, err := runGit(ctx, ws, nil, gitArgs...); err == nil && res.code == 0 {
		var out []string
		for _, item := range bytes.Split(res.stdout, []byte{0}) {
			if len(item) > 0 {
				out = append(out, string(item))
			}
		}
		return out, nil
	}
	return walkWorkspaceFiles(ctx, ws, scopeRel)
}

func walkWorkspaceFiles(ctx context.Context, ws Workspace, scopeRel string) ([]string, error) {
	startDir, err := ws.resolve(scopeRel)
	if err != nil {
		return nil, err
	}
	var out []string
	visited := 0
	err = filepath.WalkDir(startDir, func(cur string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		visited++
		if visited > maxWalkEntries {
			return fs.SkipAll
		}
		if strings.EqualFold(d.Name(), ".git") || d.Name() == "node_modules" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if !d.IsDir() && d.Type().IsRegular() {
			if rel := ws.relative(cur); rel != "" {
				out = append(out, rel)
			}
		}
		return nil
	})
	return out, err
}

func searchWorkspaceContent(ctx context.Context, ws Workspace, scopeRel, query string, isRegex, caseSensitive bool, limit int) ([]contentMatch, bool, error) {
	// Fast path: git grep inside tracked + untracked non-ignored files when git is present.
	if inside, err := gitText(ctx, ws, "rev-parse", "--is-inside-work-tree"); err == nil && inside == "true" {
		matches, truncated, ok, err := gitGrepContent(ctx, ws, scopeRel, query, isRegex, caseSensitive, limit)
		if err != nil {
			return nil, false, err
		}
		if ok {
			return matches, truncated, nil
		}
	}

	// Fallback: walk workspace files in Go.
	files, err := walkWorkspaceFiles(ctx, ws, scopeRel)
	if err != nil {
		return nil, false, err
	}
	needle := query
	if !caseSensitive {
		needle = strings.ToLower(needle)
	}
	matches := []contentMatch{}
	for _, rel := range files {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		abs, err := ws.resolve(rel)
		if err != nil {
			continue
		}
		data, err := fsutil.ReadRegularFileLimited(abs, maxReadBytes)
		if err != nil || isBinaryBytes(data) {
			continue
		}
		lines := strings.Split(string(data), "\n")
		for idx, line := range lines {
			hay := line
			if !caseSensitive {
				hay = strings.ToLower(hay)
			}
			if strings.Contains(hay, needle) {
				snippet := strings.TrimRight(line, "\r")
				if len(snippet) > 300 {
					snippet = snippet[:300] + "..."
				}
				matches = append(matches, contentMatch{
					Path: rel,
					Line: idx + 1,
					Text: snippet,
				})
				if len(matches) >= limit {
					return matches, true, nil
				}
			}
		}
	}
	return matches, false, nil
}

func gitGrepContent(ctx context.Context, ws Workspace, scopeRel, query string, isRegex, caseSensitive bool, limit int) ([]contentMatch, bool, bool, error) {
	grepCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// Search both tracked working tree and untracked non-ignored files.
	collect := func(extraFlag string, maxNeeded int) ([]contentMatch, bool, error) {
		args := []string{
			"-c", "core.quotepath=off",
			"-c", "color.ui=false",
			"-c", "core.fsmonitor=false",
			"-c", "core.hooksPath=/dev/null",
			"grep", "-n", "-I", "--no-color", "-z",
		}
		if extraFlag != "" {
			args = append(args, extraFlag)
		}
		if isRegex {
			args = append(args, "-E")
		} else {
			args = append(args, "-F")
		}
		if !caseSensitive {
			args = append(args, "-i")
		}
		args = append(args, "-e", query, "--")
		if scopeRel != "." {
			args = append(args, scopeRel)
		}
		cmd := exec.CommandContext(grepCtx, "git", args...)
		cmd.Dir = ws.Path
		cmd.Env = commandEnv("GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
		var stdout limitedBuffer
		stdout.limit = 4 << 20
		var stderr limitedBuffer
		stderr.limit = 64 << 10
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				if exitErr.ExitCode() == 1 {
					return nil, false, nil // no matches
				}
				if exitErr.ExitCode() == 2 && isRegex {
					return nil, false, fmt.Errorf("invalid regex %q: %s", query, strings.TrimSpace(stderr.String()))
				}
			}
			return nil, false, err
		}
		return parseGitGrepNull(stdout.Bytes(), maxNeeded)
	}

	tracked, trunc1, err := collect("", limit)
	if err != nil {
		if isRegex && strings.Contains(err.Error(), "invalid regex") {
			return nil, false, false, err
		}
		return nil, false, false, nil // fall back to Go walker if git grep failed unexpectedly
	}
	if len(tracked) >= limit {
		return tracked[:limit], true, true, nil
	}
	untracked, trunc2, err := collect("--untracked", limit-len(tracked))
	if err == nil {
		seen := make(map[string]bool, len(tracked))
		for _, m := range tracked {
			seen[fmt.Sprintf("%s:%d", m.Path, m.Line)] = true
		}
		for _, m := range untracked {
			key := fmt.Sprintf("%s:%d", m.Path, m.Line)
			if !seen[key] {
				tracked = append(tracked, m)
				if len(tracked) >= limit {
					return tracked, true, true, nil
				}
			}
		}
	}
	if tracked == nil {
		tracked = []contentMatch{}
	}
	return tracked, trunc1 || trunc2, true, nil
}

func parseGitGrepNull(raw []byte, limit int) ([]contentMatch, bool, error) {
	// With `git grep -n -z`, each matching line has format:
	// path \x00 lineNo \x00 text \n
	var out []contentMatch
	lines := bytes.Split(raw, []byte{'\n'})
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		parts := bytes.SplitN(line, []byte{0}, 3)
		if len(parts) < 3 {
			continue
		}
		var lineNum int
		for _, ch := range parts[1] {
			if ch >= '0' && ch <= '9' {
				lineNum = lineNum*10 + int(ch-'0')
			}
		}
		snippet := strings.TrimRight(string(parts[2]), "\r")
		if len(snippet) > 300 {
			snippet = snippet[:300] + "..."
		}
		out = append(out, contentMatch{
			Path: string(parts[0]),
			Line: lineNum,
			Text: snippet,
		})
		if len(out) >= limit {
			return out, true, nil
		}
	}
	return out, false, nil
}
