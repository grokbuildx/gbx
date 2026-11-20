package webui

import (
	"context"
	"encoding/json"
	"testing"

	acp "github.com/coder/acp-go-sdk"
)

// Grok Build carries the running context size on every session notification's
// outer _meta; usage_update alone only arrives for some providers.
func TestContextFromNotificationMetadata(t *testing.T) {
	session, err := newTestStore(t).Create(Meta{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	client := newACPTurnClient(nil, session, nil, "", "", "")
	client.setSession("root", false)
	client.setContextWindow(256000)

	note := acp.SessionNotification{
		SessionId: "root",
		Update:    acp.UpdateAgentMessageText("hi"),
		Meta:      map[string]any{"totalTokens": float64(230039)},
	}
	if err := client.SessionUpdate(context.Background(), note); err != nil {
		t.Fatal(err)
	}
	events, err := session.Events(0)
	if err != nil {
		t.Fatal(err)
	}
	var got []ContextPayload
	for _, event := range events {
		if event.Type == EventContext {
			var payload ContextPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			got = append(got, payload)
		}
	}
	want := ContextPayload{Tokens: 230039, Window: 256000}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("context = %+v, want %+v", got, want)
	}

	// Extension notifications carry it in their own outer envelope.
	raw, _ := json.Marshal(map[string]any{
		"sessionId": "root",
		"_meta":     map[string]any{"totalTokens": 240000},
		"update":    map[string]any{"sessionUpdate": "subagent_progress", "subagent_id": "x"},
	})
	if _, err := client.HandleExtensionMethod(context.Background(), "_x.ai/session_notification", raw); err != nil {
		t.Fatal(err)
	}
	events, _ = session.Events(0)
	last := events[len(events)-1]
	var after ContextPayload
	if last.Type != EventContext || json.Unmarshal(last.Payload, &after) != nil || after.Tokens != 240000 {
		t.Fatalf("last event = %s %s", last.Type, last.Payload)
	}
}

// The window arrives after the first notification. Its measurement must be
// re-published with the window so the page does not keep waiting.
func TestContextWindowBackfillsEarlierMeasurement(t *testing.T) {
	session, err := newTestStore(t).Create(Meta{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	client := newACPTurnClient(nil, session, nil, "", "", "")
	client.setSession("root", false)
	_ = client.SessionUpdate(context.Background(), acp.SessionNotification{
		SessionId: "root",
		Update:    acp.UpdateAgentMessageText("hi"),
		Meta:      map[string]any{"totalTokens": float64(1329)},
	})
	client.setContextWindow(256000)
	events, err := session.Events(0)
	if err != nil {
		t.Fatal(err)
	}
	var contexts []ContextPayload
	for _, event := range events {
		if event.Type == EventContext {
			var payload ContextPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			contexts = append(contexts, payload)
		}
	}
	if len(contexts) != 2 || contexts[1] != (ContextPayload{Tokens: 1329, Window: 256000}) {
		t.Fatalf("contexts = %+v", contexts)
	}
	// A repeated or absent window must not duplicate the measurement.
	client.setContextWindow(256000)
	if events, _ = session.Events(0); len(events) != 3 {
		t.Fatalf("duplicate backfill: %+v", events)
	}
}

func TestContextMetadataIgnoresReplayAndForeignSessions(t *testing.T) {
	session, err := newTestStore(t).Create(Meta{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	client := newACPTurnClient(nil, session, nil, "", "", "")
	client.setSession("root", false)
	client.setLoading(true)
	_ = client.SessionUpdate(context.Background(), acp.SessionNotification{
		SessionId: "root",
		Update:    acp.UpdateAgentMessageText("replayed"),
		Meta:      map[string]any{"totalTokens": float64(5000)},
	})
	client.setLoading(false)
	_ = client.SessionUpdate(context.Background(), acp.SessionNotification{
		SessionId: "other",
		Update:    acp.UpdateAgentMessageText("foreign"),
		Meta:      map[string]any{"totalTokens": float64(7000)},
	})
	if events, _ := session.Events(0); len(events) != 0 {
		t.Fatalf("replay or foreign context leaked: %+v", events)
	}
}

func TestSessionInfoWindow(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want int64
	}{
		{"result envelope", `{"result":{"context":{"total":256000,"used":10}}}`, 256000},
		{"missing", `{"result":{"context":{}}}`, 0},
		{"zero", `{"result":{"context":{"total":0}}}`, 0},
		{"invalid", `{`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseSessionInfoWindow([]byte(tc.raw)); got != tc.want {
				t.Fatalf("window = %d, want %d", got, tc.want)
			}
		})
	}
}
