package webui

import (
	"fmt"
	"strings"
)

const (
	SandboxWorkspace = "workspace"
	SandboxOff       = "off"
)

// NormalizeSandbox validates the web launcher policy. Empty retains the safe default.
func NormalizeSandbox(mode string) (string, error) {
	switch mode = strings.TrimSpace(mode); mode {
	case "", SandboxWorkspace:
		return SandboxWorkspace, nil
	case SandboxOff:
		return SandboxOff, nil
	default:
		return "", fmt.Errorf("sandbox: expected workspace or off, got %q", mode)
	}
}

// SandboxMode is the default captured by newly created conversations.
// Existing conversations keep their stored mode so ACP resumes in the same profile.
func (s *Server) SandboxMode() string {
	s.sandboxMu.RLock()
	defer s.sandboxMu.RUnlock()
	return s.sandbox
}

func sandboxProfile(workspace, mode string) string {
	if mode == SandboxOff {
		return SandboxOff
	}
	if worktreeUsesCustomSandbox(workspace) {
		return worktreeSandboxProfile
	}
	return SandboxWorkspace
}

// Pin before launching so a crash or workspace change cannot alter a resumed profile.
func pinSessionSandbox(session *Session, spec *LaunchSpec) error {
	var updatedMeta *Meta
	err := func() error {
		session.mu.Lock()
		defer session.mu.Unlock()
		if session.deleted {
			return ErrNotFound
		}
		if session.deleting {
			return ErrBusy
		}
		meta := session.meta
		mode, err := NormalizeSandbox(sessionSandbox(meta))
		if err != nil {
			return err
		}
		if spec.Sandbox != "" && spec.Sandbox != mode {
			return fmt.Errorf("a conversation keeps its original sandbox; start a new conversation to change it")
		}
		spec.Sandbox = mode
		profile := sandboxProfile(spec.Workspace, mode)
		if meta.SandboxProfile != "" && meta.SandboxProfile != profile {
			return fmt.Errorf("the conversation's sandbox profile changed; restore its workspace sandbox configuration before continuing")
		}
		if meta.SandboxProfile == "" {
			if meta.Worktree != nil && mode == SandboxWorkspace && profile != worktreeSandboxProfile {
				return fmt.Errorf("the conversation's worktree sandbox profile is missing; restore its sandbox configuration before continuing")
			}
			meta.Sandbox = mode
			meta.SandboxProfile = profile
			if err := writeMeta(session.dir, meta); err != nil {
				return fmt.Errorf("save conversation sandbox: %w", err)
			}
			session.meta.Sandbox = mode
			session.meta.SandboxProfile = profile
			copied := session.meta
			updatedMeta = &copied
		}
		spec.sandboxProfile = profile
		return nil
	}()
	if err != nil {
		return err
	}
	if updatedMeta != nil && session.store != nil {
		session.store.recordMeta(session, *updatedMeta)
	}
	return nil
}

func sessionSandbox(meta Meta) string {
	if meta.Sandbox == "" {
		return SandboxWorkspace
	}
	return meta.Sandbox
}
