package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const maxBatchSteps = 25

func (s *Server) registerBatchTool() {
	allowedNames := make([]string, 0, len(s.tools))
	for _, t := range s.tools {
		if t.Name != "run_batch" {
			allowedNames = append(allowedNames, t.Name)
		}
	}

	stepProps := withTargetProps(map[string]JSONSchema{
		"tool": {
			Type:        "string",
			Description: "Name of the GBX tool to invoke in this step.",
			Enum:        allowedNames,
		},
		"continue_on_error": {
			Type:        "boolean",
			Description: "Keep running subsequent steps even if this step errors or run_command exits non-zero (default false: halt pipeline on failure).",
		},
		"path": {
			Type:        "string",
			Description: "File or directory path argument (for read_file, edit_file, write_file, list_dir, search_code, git_diff, git_log, workspace_add/remove).",
		},
		"paths": {
			Type:        "array",
			Description: "Batch file paths (for read_file or git_commit).",
			Items:       &JSONSchema{Type: "string"},
		},
		"start_line": {
			Type:        "integer",
			Description: "1-based start line (for read_file).",
		},
		"end_line": {
			Type:        "integer",
			Description: "1-based end line (for read_file).",
		},
		"old_string": {
			Type:        "string",
			Description: "Exact existing text to replace (for edit_file).",
		},
		"new_string": {
			Type:        "string",
			Description: "Replacement text (for edit_file).",
		},
		"replace_all": {
			Type:        "boolean",
			Description: "Replace all occurrences (for edit_file).",
		},
		"expected_hash": {
			Type:        "string",
			Description: "Expected SHA-256 file hash (for edit_file or write_file).",
		},
		"content": {
			Type:        "string",
			Description: "Full file content (for write_file).",
		},
		"overwrite": {
			Type:        "boolean",
			Description: "Overwrite existing file when expected_hash is omitted (for write_file).",
		},
		"query": {
			Type:        "string",
			Description: "Search query (for search_code).",
		},
		"mode": {
			Type:        "string",
			Description: "Search mode: 'content' or 'files' (for search_code).",
			Enum:        []string{"content", "files"},
		},
		"regex": {
			Type:        "boolean",
			Description: "Treat query as regular expression (for search_code).",
		},
		"case_sensitive": {
			Type:        "boolean",
			Description: "Case-sensitive search (for search_code).",
		},
		"limit": {
			Type:        "integer",
			Description: "Result limit (for search_code or git_log).",
		},
		"staged": {
			Type:        "boolean",
			Description: "Diff staged changes (for git_diff).",
		},
		"base": {
			Type:        "string",
			Description: "Base commit/branch ref (for git_diff).",
		},
		"message": {
			Type:        "string",
			Description: "Commit message (for git_commit).",
		},
		"name": {
			Type:        "string",
			Description: "Worktree name (for worktree_create, worktree_merge, worktree_remove).",
		},
		"force": {
			Type:        "boolean",
			Description: "Force worktree removal (for worktree_remove).",
		},
		"as_root": {
			Type:        "boolean",
			Description: "Register parent directory of multiple repos (for workspace_add).",
		},
		"command": {
			Type:        "string",
			Description: "Shell command to execute (for run_command).",
		},
		"cwd": {
			Type:        "string",
			Description: "Workspace-relative working directory (for run_command).",
		},
		"timeout_seconds": {
			Type:        "integer",
			Description: "Command timeout in seconds (for run_command).",
		},
		"args_json": {
			Type:        "string",
			Description: "Optional JSON object string carrying extra tool arguments (for example nested 'edits' for edit_file).",
		},
	})

	s.addTool(ToolDef{
		Name:        "run_batch",
		Description: "Execute an ordered sequence of GBX tool calls in a single MCP round-trip (up to 25 steps) so the user only needs to approve once in hosts that prompt on every tool call. Automatically halts remaining steps if any step fails or if run_command returns a non-zero exit code (unless continue_on_error=true).",
		InputSchema: objectSchema(map[string]JSONSchema{
			"steps": {
				Type:        "array",
				Description: "Ordered list of tool invocations to execute sequentially.",
				Items: &JSONSchema{
					Type:       "object",
					Properties: stepProps,
					Required:   []string{"tool"},
				},
			},
		}, "steps"),
		Annotations: mutatingAnnotations("Run Batch Pipeline", !s.cfg.DisableExec, false),
	}, s.toolRunBatch)
}

func (s *Server) toolRunBatch(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		Steps []json.RawMessage `json:"steps"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", err
	}
	if len(args.Steps) == 0 {
		return "", errors.New("steps must contain at least 1 step")
	}
	if len(args.Steps) > maxBatchSteps {
		return "", fmt.Errorf("steps exceeds maximum batch size of %d", maxBatchSteps)
	}

	type stepOutcome struct {
		Index  int    `json:"index"`
		Tool   string `json:"tool"`
		OK     bool   `json:"ok"`
		Result any    `json:"result,omitempty"`
		Error  string `json:"error,omitempty"`
	}

	outcomes := make([]stepOutcome, 0, len(args.Steps))
	stoppedEarly := false

	for idx, rawStep := range args.Steps {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		var stepMap map[string]json.RawMessage
		if err := json.Unmarshal(rawStep, &stepMap); err != nil {
			return "", fmt.Errorf("step %d is not a JSON object: %w", idx, err)
		}

		var toolName string
		if rawTool, ok := stepMap["tool"]; ok {
			_ = json.Unmarshal(rawTool, &toolName)
		}
		toolName = strings.TrimSpace(toolName)
		if toolName == "" {
			return "", fmt.Errorf("step %d is missing 'tool'", idx)
		}
		if toolName == "run_batch" {
			return "", fmt.Errorf("step %d cannot recursively invoke run_batch", idx)
		}
		handler, ok := s.toolMap[toolName]
		if !ok {
			return "", fmt.Errorf("step %d references unknown or disabled tool %q", idx, toolName)
		}

		var continueOnErr bool
		if rawCont, ok := stepMap["continue_on_error"]; ok {
			_ = json.Unmarshal(rawCont, &continueOnErr)
		}

		// Merge optional nested "arguments" and "args_json" with flat step fields.
		merged := make(map[string]json.RawMessage, len(stepMap))
		if rawNested, ok := stepMap["arguments"]; ok {
			var nested map[string]json.RawMessage
			if json.Unmarshal(rawNested, &nested) == nil {
				for k, v := range nested {
					merged[k] = v
				}
			}
		}
		if rawArgsJSON, ok := stepMap["args_json"]; ok {
			var encoded string
			if json.Unmarshal(rawArgsJSON, &encoded) == nil && strings.TrimSpace(encoded) != "" {
				var parsed map[string]json.RawMessage
				if err := json.Unmarshal([]byte(encoded), &parsed); err != nil {
					return "", fmt.Errorf("step %d invalid args_json: %w", idx, err)
				}
				for k, v := range parsed {
					merged[k] = v
				}
			}
		}
		for k, v := range stepMap {
			if k == "tool" || k == "continue_on_error" || k == "arguments" || k == "args_json" {
				continue
			}
			merged[k] = v
		}
		mergedBytes, err := json.Marshal(merged)
		if err != nil {
			return "", err
		}

		outStr, callErr := handler(ctx, mergedBytes)
		if callErr != nil {
			outcomes = append(outcomes, stepOutcome{
				Index: idx,
				Tool:  toolName,
				OK:    false,
				Error: callErr.Error(),
			})
			if !continueOnErr {
				stoppedEarly = idx < len(args.Steps)-1
				break
			}
			continue
		}

		var parsedResult any = outStr
		var rawJSON json.RawMessage
		if json.Unmarshal([]byte(outStr), &rawJSON) == nil {
			parsedResult = rawJSON
		}

		commandFailed := false
		if toolName == "run_command" {
			var execRes struct {
				ExitCode int `json:"exit_code"`
			}
			if json.Unmarshal([]byte(outStr), &execRes) == nil && execRes.ExitCode != 0 {
				commandFailed = true
			}
		}

		outcomes = append(outcomes, stepOutcome{
			Index:  idx,
			Tool:   toolName,
			OK:     !commandFailed,
			Result: parsedResult,
		})
		if commandFailed && !continueOnErr {
			stoppedEarly = idx < len(args.Steps)-1
			break
		}
	}

	return marshalToolJSON(map[string]any{
		"completed_steps": len(outcomes),
		"total_steps":     len(args.Steps),
		"stopped_early":   stoppedEarly,
		"steps":           outcomes,
	})
}
