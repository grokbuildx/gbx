package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	acp "github.com/coder/acp-go-sdk"
)

func (a *fakeACPAgent) promptBackgroundSubagents(ctx context.Context, sid acp.SessionId, method string) (acp.PromptResponse, error) {
	update := func(u acp.SessionUpdate) error {
		return a.conn.SessionUpdate(ctx, acp.SessionNotification{SessionId: sid, Update: u})
	}
	notify := func(payload map[string]any) error {
		return a.conn.NotifyExtension(ctx, method, map[string]any{"sessionId": sid, "update": payload})
	}
	for i := range 3 {
		id, subID := fmt.Sprintf("call_%d", i), fmt.Sprintf("sub_%d", i)
		desc := fmt.Sprintf("Check city %d weather", i)
		if err := update(acp.SessionUpdate{ToolCall: &acp.SessionUpdateToolCall{
			ToolCallId: acp.ToolCallId(id), Title: "spawn_subagent", Status: acp.ToolCallStatusPending,
			RawInput: map[string]any{"description": desc, "background": true},
			Meta:     map[string]any{"x.ai/tool": map[string]any{"name": "spawn_subagent", "kind": "task"}},
		}}); err != nil {
			return acp.PromptResponse{}, err
		}
		if err := notify(map[string]any{
			"sessionUpdate": "subagent_spawned", "subagent_id": subID, "child_session_id": subID,
			"subagent_type": "general-purpose", "description": desc,
		}); err != nil {
			return acp.PromptResponse{}, err
		}
		// Background launch returns text; completion arrives on the extension channel.
		completed := acp.ToolCallStatusCompleted
		if err := update(acp.SessionUpdate{ToolCallUpdate: &acp.SessionToolCallUpdate{
			ToolCallId: acp.ToolCallId(id), Status: &completed,
			RawOutput: "Subagent started in background.\nsubagent_id: " + subID,
		}}); err != nil {
			return acp.PromptResponse{}, err
		}
	}
	for _, i := range []int{2, 0, 1} {
		if err := notify(map[string]any{
			"sessionUpdate": "subagent_finished", "subagent_id": fmt.Sprintf("sub_%d", i),
			"status": "completed", "tool_calls": i + 1, "duration_ms": (i + 1) * 1000,
			"tokens_used": (i + 1) * 100, "output": fmt.Sprintf("City %d forecast", i),
		}); err != nil {
			return acp.PromptResponse{}, err
		}
	}
	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func TestRunnerBackgroundSubagentNotificationCarriers(t *testing.T) {
	for _, method := range []string{"_x.ai/session_notification", "_x.ai/session/update"} {
		t.Run(method, func(t *testing.T) {
			store := newTestStore(t)
			session, err := store.Create(Meta{Title: "Weather", Workspace: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			runner := newTestRunner(t, helperPrepareEnv(t, map[string]string{
				"GBX_WEBUI_HELPER_BACKGROUND_SUBAGENTS": method,
			}, false))
			handle, err := NewTimeline().Start(runner, session, LaunchSpec{
				Workspace: session.Meta().Workspace, Prompt: "Check three cities", PermissionMode: "bypassPermissions",
			})
			if err != nil {
				t.Fatal(err)
			}
			waitDone(t, handle.Done)
			events, err := session.Events(0)
			if err != nil {
				t.Fatal(err)
			}
			tasks := map[string]TaskPayload{}
			results := map[string]ToolResultPayload{}
			var completedOrder []string
			for _, event := range events {
				switch event.Type {
				case EventTask:
					var task TaskPayload
					if err := json.Unmarshal(event.Payload, &task); err != nil {
						t.Fatal(err)
					}
					tasks[task.ID] = task
					if task.Status == "completed" {
						completedOrder = append(completedOrder, task.ID)
					}
				case EventToolResult:
					var result ToolResultPayload
					if err := json.Unmarshal(event.Payload, &result); err != nil {
						t.Fatal(err)
					}
					results[result.ID] = result
				}
			}
			if fmt.Sprint(completedOrder) != "[call_2 call_0 call_1]" {
				t.Fatalf("completion order = %v, want all three background agents completed", completedOrder)
			}
			for i := range 3 {
				id := fmt.Sprintf("call_%d", i)
				task := tasks[id]
				if task.Status != "completed" || task.Agent != "general-purpose" || task.Summary != fmt.Sprintf("City %d forecast", i) {
					t.Fatalf("task %s = %+v", id, task)
				}
				wantUsage := TaskUsage{Tokens: int64((i + 1) * 100), ToolUses: int64(i + 1), DurationMs: int64((i + 1) * 1000)}
				if task.Usage == nil || *task.Usage != wantUsage {
					t.Fatalf("task %s usage = %+v, want %+v", id, task.Usage, wantUsage)
				}
				if result := results[id]; !result.OK || result.Output != task.Summary {
					t.Fatalf("result %s = %+v", id, result)
				}
			}
		})
	}
}

func TestSubagentExtensionTerminalStates(t *testing.T) {
	for _, status := range []string{"completed", "failed", "cancelled"} {
		for _, finishFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/finishFirst=%v", status, finishFirst), func(t *testing.T) {
				session, err := newTestStore(t).Create(Meta{Workspace: t.TempDir()})
				if err != nil {
					t.Fatal(err)
				}
				client := newACPTurnClient(nil, session, nil, "", "", "")
				client.setSession("parent", false)
				update := func(u acp.SessionUpdate) {
					t.Helper()
					if err := client.SessionUpdate(context.Background(), acp.SessionNotification{SessionId: "parent", Update: u}); err != nil {
						t.Fatal(err)
					}
				}
				notify := func(payload map[string]any) {
					t.Helper()
					raw, _ := json.Marshal(map[string]any{"sessionId": "parent", "update": payload})
					if _, err := client.HandleExtensionMethod(context.Background(), "_x.ai/session_notification", raw); err != nil {
						t.Fatal(err)
					}
				}
				update(acp.SessionUpdate{ToolCall: &acp.SessionUpdateToolCall{
					ToolCallId: "call", Title: "spawn_subagent", Status: acp.ToolCallStatusPending,
					RawInput: map[string]any{"description": "Weather", "background": true},
				}})
				notify(map[string]any{"sessionUpdate": "subagent_spawned", "subagent_id": "child", "description": "Weather"})
				finish := func() {
					notify(map[string]any{"sessionUpdate": "subagent_finished", "subagent_id": "child", "status": status, "output": "Final report"})
				}
				if finishFirst {
					finish()
				}
				completed := acp.ToolCallStatusCompleted
				update(acp.SessionUpdate{ToolCallUpdate: &acp.SessionToolCallUpdate{
					ToolCallId: "call", Status: &completed, RawOutput: "Subagent started in background.",
				}})
				if !finishFirst {
					finish()
				}
				events, err := session.Events(0)
				if err != nil {
					t.Fatal(err)
				}
				var last TaskPayload
				for _, event := range events {
					if event.Type == EventTask {
						if err := json.Unmarshal(event.Payload, &last); err != nil {
							t.Fatal(err)
						}
					}
				}
				want := status
				if status == "cancelled" {
					want = "stopped"
				}
				if last.ID != "call" || last.Status != want {
					t.Fatalf("last task = %+v, want status %s", last, want)
				}
			})
		}
	}
}

func TestSubagentExtensionSessionBoundary(t *testing.T) {
	for _, method := range []string{"_x.ai/session_notification", "_x.ai/session/update"} {
		for _, tc := range []struct {
			name, session string
			loading       bool
			want          bool
		}{
			{name: "live", session: "parent", want: true},
			{name: "other session", session: "other"},
			{name: "history replay", session: "parent", loading: true},
		} {
			t.Run(method+"/"+tc.name, func(t *testing.T) {
				client := &acpTurnClient{sessionID: "parent", loading: tc.loading, translator: newACPTranslator()}
				payload, _ := json.Marshal(map[string]any{
					"sessionId": tc.session,
					"update":    map[string]any{"sessionUpdate": "subagent_spawned", "subagent_id": "child"},
				})
				if _, err := client.HandleExtensionMethod(context.Background(), method, payload); err != nil {
					t.Fatal(err)
				}
				if got := client.translator.subagents["child"] != nil; got != tc.want {
					t.Fatalf("tracked child = %v, want %v", got, tc.want)
				}
			})
		}
	}
	client := &acpTurnClient{translator: newACPTranslator()}
	if _, err := client.HandleExtensionMethod(context.Background(), "_x.ai/unrelated", nil); err == nil {
		t.Fatal("unrecognized extension should return method not found")
	}
}
