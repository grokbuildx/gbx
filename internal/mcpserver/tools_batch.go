package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const maxBatchSteps = 25

func (s *Server) registerBatchTool() {
	allowedNames := make([]string, 0, len(s.tools))
	for _, t := range s.tools {
		if t.Name != "run_batch" {
			allowedNames = append(allowedNames, t.Name)
		}
	}

	// Keep the schema compact (3 scalar fields per step) so LLM constrained
	// decoding compilers (such as Gemini's FunctionDeclaration FSA) compile
	// tools/list instantaneously without combinatorial state explosion.
	desc := `Execute an ordered pipeline of GBX coding operations in ONE call (up to 25 steps), halting automatically on failure or non-zero command exit unless continue_on_error=true.
Available step tools and their args_json fields (all tools accept optional "workspace" and "worktree"):
• workspace_list: {}
• workspace_add: {"path":"/abs/dir","as_root":true}
• list_dir: {"path":"subdir"}
• search_code: {"query":"text","mode":"content|files","path":"subdir","regex":false,"limit":50}
• read_file: {"path":"file.go","start_line":1,"end_line":200} or {"paths":["a.go","b.go"]}
• edit_file: {"path":"file.go","old_string":"...","new_string":"...","replace_all":false,"expected_hash":"..."}
• write_file: {"path":"file.go","content":"...","overwrite":true,"expected_hash":"..."}
• git_status: {}
• git_diff: {"staged":false,"base":"HEAD","path":"file.go"}
• git_log: {"limit":20,"path":"file.go"}
• git_commit: {"message":"feat: ...","paths":["a.go"]}
• worktree_create / worktree_merge / worktree_remove: {"name":"fix-slug","force":false}
• worktree_list: {}
• run_command: {"command":"go test ./...","cwd":"subdir","timeout_seconds":60}`

	s.addTool(ToolDef{
		Name:        "run_batch",
		Description: desc,
		InputSchema: objectSchema(map[string]JSONSchema{
			"steps": {
				Type:        "array",
				Description: "Ordered list of tool steps to execute sequentially.",
				Items: &JSONSchema{
					Type: "object",
					Properties: map[string]JSONSchema{
						"tool": {
							Type:        "string",
							Description: "Tool name to execute.",
							Enum:        allowedNames,
						},
						"args_json": {
							Type:        "string",
							Description: "JSON object string of arguments for this tool (use \"{}\" when no arguments are needed).",
						},
						"continue_on_error": {
							Type:        "boolean",
							Description: "Keep running remaining steps even if this step fails (default false).",
						},
					},
					Required: []string{"tool"},
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

		// Accept args_json string, nested "arguments" object, and/or flat step properties.
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
			if json.Unmarshal(rawArgsJSON, &encoded) == nil {
				encoded = strings.TrimSpace(encoded)
				if encoded != "" {
					var parsed map[string]json.RawMessage
					if err := json.Unmarshal([]byte(encoded), &parsed); err != nil {
						return "", fmt.Errorf("step %d invalid args_json: %w", idx, err)
					}
					for k, v := range parsed {
						merged[k] = v
					}
				}
			} else {
				// Gracefully tolerate an LLM passing an inline JSON object instead of a string for args_json.
				var inlineObj map[string]json.RawMessage
				if json.Unmarshal(rawArgsJSON, &inlineObj) == nil {
					for k, v := range inlineObj {
						merged[k] = v
					}
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

		stepStart := time.Now()
		outStr, callErr := handler(ctx, mergedBytes)
		stepMs := time.Since(stepStart).Milliseconds()
		if callErr != nil {
			s.logf("  batch step %d/%d %s -> error (%dms): %s", idx+1, len(args.Steps), toolName, stepMs, callErr.Error())
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
		s.logf("  batch step %d/%d %s -> ok=%v (%dms)", idx+1, len(args.Steps), toolName, !commandFailed, stepMs)

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
