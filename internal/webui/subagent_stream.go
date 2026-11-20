package webui

import (
	"encoding/json"
	"strings"

	acp "github.com/coder/acp-go-sdk"
)

type translatedEvent struct {
	kind    string
	payload any
}

// Each child has its own tool IDs and translator; only an announced descendant
// can feed the parent's stream. No child updates can change root usage or menus.
type acpChildSession struct {
	id         string
	translator *acpTranslator
	owner      *acpTranslator
	ownerScope *acpChildSession
	subagentID string
	pending    []translatedEvent
}

func (s *acpChildSession) toolID(id string) string {
	if id == "" {
		return ""
	}
	return "child:" + s.id + ":" + id
}

func (s *acpChildSession) parentID() string {
	sub := s.owner.subagents[s.subagentID]
	tool := s.owner.linkSubagentByDescription(sub)
	if tool == nil || !tool.emitted {
		return ""
	}
	if s.ownerScope != nil {
		return s.ownerScope.toolID(tool.id)
	}
	return tool.id
}

func (c *acpTurnClient) registerChildLocked(scope *acpChildSession, raw json.RawMessage) {
	var note struct {
		Kind   string `json:"sessionUpdate"`
		ID     string `json:"subagent_id"`
		Child  string `json:"child_session_id"`
		Parent string `json:"parent_session_id"`
	}
	if json.Unmarshal(raw, &note) != nil || (note.Kind != "subagent_spawned" && note.Kind != "subagent_finished") {
		return
	}
	note.ID, note.Child = strings.TrimSpace(note.ID), strings.TrimSpace(note.Child)
	if note.ID == "" || note.Child == "" || note.Child == string(c.sessionID) {
		return
	}
	owner := c.translator
	parentID := string(c.sessionID)
	if scope != nil {
		owner, parentID = scope.translator, scope.id
	}
	if note.Parent != "" && note.Parent != parentID {
		return
	}
	if c.children == nil {
		c.children = make(map[string]*acpChildSession)
	}
	if existing := c.children[note.Child]; existing != nil {
		// One session cannot be adopted by a different parent or spawn call.
		return
	}
	c.children[note.Child] = &acpChildSession{id: note.Child, translator: newACPTranslator(), owner: owner, ownerScope: scope, subagentID: note.ID}
}

func (c *acpTurnClient) translateChildLocked(child *acpChildSession, update acp.SessionUpdate) {
	child.translator.Handle(update, func(kind string, payload any) {
		switch kind {
		case EventThinking, EventToken, EventToolUse, EventToolResult, EventTask:
			child.pending = append(child.pending, translatedEvent{kind: kind, payload: payload})
		}
	})
}

// Flush parents first, including when a spawn's final input/result arrived after
// its child started streaming. The caller holds c.mu through persistence.
func (c *acpTurnClient) flushChildrenLocked() {
	for _, child := range c.children {
		c.flushChildLocked(child)
	}
}

func (c *acpTurnClient) flushChildLocked(child *acpChildSession) {
	if child.ownerScope != nil {
		c.flushChildLocked(child.ownerScope)
		if child.ownerScope.parentID() == "" {
			return
		}
	}
	parent := child.parentID()
	if parent == "" {
		return
	}
	for _, event := range child.pending {
		var payload any
		switch p := event.payload.(type) {
		case TokenPayload:
			p.Subagent, p.Parent = true, parent
			payload = p
		case ToolUsePayload:
			p.ID = child.toolID(p.ID)
			p.Subagent = true
			if p.Parent != "" {
				p.Parent = child.toolID(p.Parent)
			} else {
				p.Parent = parent
			}
			payload = p
		case ToolResultPayload:
			p.ID = child.toolID(p.ID)
			p.Subagent = true
			if p.Parent != "" {
				p.Parent = child.toolID(p.Parent)
			} else {
				p.Parent = parent
			}
			payload = p
		case TaskPayload:
			p.ID = child.toolID(p.ID)
			payload = p
		default:
			continue
		}
		_, _ = c.session.Append(event.kind, payload)
		if use, ok := payload.(ToolUsePayload); ok {
			if sub := child.owner.subagents[child.subagentID]; sub != nil && !sub.finished {
				_, _ = c.session.Append(EventTask, TaskPayload{ID: parent, Status: "running", LastTool: use.Name})
			}
		}
	}
	child.pending = nil
}
