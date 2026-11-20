package webui

import (
	"context"
	"encoding/json"
	"testing"

	acp "github.com/coder/acp-go-sdk"
)

func childUpdate(t *testing.T, client *acpTurnClient, sid string, update acp.SessionUpdate) {
	t.Helper()
	if err := client.SessionUpdate(context.Background(), acp.SessionNotification{SessionId: acp.SessionId(sid), Update: update}); err != nil {
		t.Fatal(err)
	}
}

func childExtension(t *testing.T, client *acpTurnClient, sid string, update map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"sessionId": sid, "update": update})
	if _, err := client.HandleExtensionMethod(context.Background(), "_x.ai/session_notification", raw); err != nil {
		t.Fatal(err)
	}
}

func spawnUpdate(id, description string) acp.SessionUpdate {
	return acp.SessionUpdate{ToolCall: &acp.SessionUpdateToolCall{ToolCallId: acp.ToolCallId(id), Title: "spawn_subagent", Status: acp.ToolCallStatusPending, RawInput: map[string]any{"description": description, "prompt": "Inspect the fixture", "background": true}}}
}

func spawnChild(t *testing.T, client *acpTurnClient, parent, id string) {
	t.Helper()
	childUpdate(t, client, parent, spawnUpdate("spawn-"+id, "Inspect "+id))
	childExtension(t, client, parent, map[string]any{"sessionUpdate": "subagent_spawned", "subagent_id": id, "child_session_id": id, "parent_session_id": parent, "description": "Inspect " + id, "subagent_type": "general-purpose"})
}

func TestChildSessionThinkingToolsAndNestedIsolation(t *testing.T) {
	session, err := newTestStore(t).Create(Meta{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	client := newACPTurnClient(nil, session, nil, "", "", "")
	client.setSession("root", false)
	spawnChild(t, client, "root", "a")
	spawnChild(t, client, "root", "b")
	spawnChild(t, client, "a", "nested")
	for _, sid := range []string{"root", "a", "b", "nested", "unrelated"} {
		childUpdate(t, client, sid, acp.UpdateAgentThoughtText(sid+" thinking"))
		childUpdate(t, client, sid, acp.SessionUpdate{ToolCall: &acp.SessionUpdateToolCall{ToolCallId: "same-id", Title: "read_file", Status: acp.ToolCallStatusPending, RawInput: map[string]any{"target_file": "fixture.txt"}}})
		done := acp.ToolCallStatusCompleted
		childUpdate(t, client, sid, acp.SessionUpdate{ToolCallUpdate: &acp.SessionToolCallUpdate{ToolCallId: "same-id", Status: &done, RawOutput: sid + " file contents"}})
		childUpdate(t, client, sid, acp.UpdateAgentMessageText(sid+" final"))
	}
	events, err := session.Events(0)
	if err != nil {
		t.Fatal(err)
	}
	parents := map[string]string{"a": "spawn-a", "b": "spawn-b", "nested": "child:a:spawn-nested"}
	thoughts := map[string]string{}
	tools := map[string]string{}
	results := map[string]string{}
	for _, e := range events {
		switch e.Type {
		case EventThinking:
			var p TokenPayload
			_ = json.Unmarshal(e.Payload, &p)
			thoughts[p.Text] = p.Parent
		case EventToolUse:
			var p ToolUsePayload
			_ = json.Unmarshal(e.Payload, &p)
			tools[p.ID] = p.Parent
		case EventToolResult:
			var p ToolResultPayload
			_ = json.Unmarshal(e.Payload, &p)
			results[p.ID] = p.Parent
		}
	}
	if len(thoughts) != 4 {
		t.Fatalf("thoughts=%v", thoughts)
	}
	for sid, parent := range parents {
		if thoughts[sid+" thinking"] != parent || tools["child:"+sid+":same-id"] != parent || results["child:"+sid+":same-id"] != parent {
			t.Fatalf("%s routed incorrectly: thoughts=%v tools=%v results=%v", sid, thoughts, tools, results)
		}
	}
	if tools["same-id"] != "" || results["same-id"] != "" {
		t.Fatal("root tools were moved into a child")
	}
}

func TestChildSessionBuffersUntilSpawnToolIsKnown(t *testing.T) {
	session, err := newTestStore(t).Create(Meta{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	client := newACPTurnClient(nil, session, nil, "", "", "")
	client.setSession("root", false)
	childExtension(t, client, "root", map[string]any{"sessionUpdate": "subagent_spawned", "subagent_id": "child", "child_session_id": "child", "description": "late input"})
	childUpdate(t, client, "child", acp.UpdateAgentThoughtText("child progress"))
	if events, _ := session.Events(0); len(events) != 0 {
		t.Fatalf("unowned child content leaked: %+v", events)
	}
	childUpdate(t, client, "root", spawnUpdate("spawn", "late input"))
	events, _ := session.Events(0)
	if len(events) != 3 || events[0].Type != EventToolUse || events[2].Type != EventThinking {
		t.Fatalf("spawn and child order=%+v", events)
	}
	var thought TokenPayload
	_ = json.Unmarshal(events[2].Payload, &thought)
	if thought.Parent != "spawn" {
		t.Fatalf("thought=%+v", thought)
	}
}

func TestChildSessionCannotChangeRootOrEscapeReplayGate(t *testing.T) {
	session, err := newTestStore(t).Create(Meta{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	client := newACPTurnClient(nil, session, nil, "", "", "")
	client.setSession("root", false)
	spawnChild(t, client, "root", "child")
	seq := session.LastSeq()
	childUpdate(t, client, "child", acp.SessionUpdate{UsageUpdate: &acp.SessionUsageUpdate{Used: 123, Size: 456, Cost: &acp.Cost{Amount: 12, Currency: "USD"}}})
	childUpdate(t, client, "child", acp.SessionUpdate{AvailableCommandsUpdate: &acp.SessionAvailableCommandsUpdate{AvailableCommands: []acp.AvailableCommand{{Name: "foreign"}}}})
	childUpdate(t, client, "unknown", acp.UpdateAgentThoughtText("unrelated"))
	childExtension(t, client, "unknown", map[string]any{"sessionUpdate": "subagent_spawned", "subagent_id": "evil", "child_session_id": "evil"})
	childExtension(t, client, "child", map[string]any{"sessionUpdate": "subagent_spawned", "subagent_id": "bad-parent", "child_session_id": "bad-parent", "parent_session_id": "other"})
	childUpdate(t, client, "bad-parent", acp.UpdateAgentThoughtText("wrong parent"))
	client.setLoading(true)
	childUpdate(t, client, "child", acp.UpdateAgentThoughtText("replayed"))
	if session.LastSeq() != seq || client.sessionCostUSD() != 0 || len(client.slashCommands()) != 0 {
		t.Fatal("child modified root state or replay escaped")
	}
}

func TestChildProgressAndLateStepsKeepTerminalState(t *testing.T) {
	session, err := newTestStore(t).Create(Meta{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	client := newACPTurnClient(nil, session, nil, "", "", "")
	client.setSession("root", false)
	spawnChild(t, client, "root", "child")
	done := acp.ToolCallStatusCompleted
	childUpdate(t, client, "root", acp.SessionUpdate{ToolCallUpdate: &acp.SessionToolCallUpdate{ToolCallId: "spawn-child", Status: &done, RawOutput: map[string]any{"type": "SubagentStarted", "subagent_id": "child"}}})
	childExtension(t, client, "root", map[string]any{"sessionUpdate": "subagent_progress", "subagent_id": "child", "tool_call_count": 4, "tokens_used": 3000, "duration_ms": 1000})
	events, _ := session.Events(0)
	var progress TaskPayload
	_ = json.Unmarshal(events[len(events)-1].Payload, &progress)
	if progress.Usage == nil || progress.Usage.ToolUses != 4 || progress.Usage.Tokens != 3000 {
		t.Fatalf("progress=%+v", progress)
	}
	childExtension(t, client, "root", map[string]any{"sessionUpdate": "subagent_finished", "subagent_id": "child", "child_session_id": "child", "status": "completed", "output": "final", "tool_calls": 4})
	seq := session.LastSeq()
	childExtension(t, client, "root", map[string]any{"sessionUpdate": "subagent_progress", "subagent_id": "child", "tool_call_count": 5})
	childUpdate(t, client, "child", acp.UpdateAgentMessageText("late final chunk"))
	events, _ = session.Events(seq)
	if len(events) != 1 || events[0].Type != EventToken {
		t.Fatalf("late events=%+v", events)
	}
}
