package store

import (
	"testing"
	"time"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	now := time.Now().UTC().Truncate(time.Millisecond)
	in := &State{
		Version: 1,
		Sessions: []Session{{
			ID: "s1", Cwd: "C:/work", Name: "Chat", Runtime: "codex",
			Created: now, Modified: now, PermissionMode: "default",
			InputTokens: 12, OutputTokens: 3,
			Messages: []map[string]any{{"role": "user", "content": "hi"}},
		}},
		Workspaces: []map[string]any{{"id": "w1", "path": "C:/work", "name": "work"}},
		Settings:   map[string]any{"defaultPermission": "auto"},
	}
	if err := s.Save(in); err != nil {
		t.Fatalf("save: %v", err)
	}
	out, err := s.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(out.Sessions) != 1 || out.Sessions[0].ID != "s1" || out.Sessions[0].Runtime != "codex" {
		t.Fatalf("unexpected sessions: %+v", out.Sessions)
	}
	if out.Sessions[0].InputTokens != 12 || len(out.Sessions[0].Messages) != 1 {
		t.Fatalf("usage/messages not preserved: %+v", out.Sessions[0])
	}
	if len(out.Workspaces) != 1 || out.Settings["defaultPermission"] != "auto" {
		t.Fatalf("workspaces/settings not preserved: %+v %+v", out.Workspaces, out.Settings)
	}
}
