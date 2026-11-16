// Package store persists the Go host's sessions, workspaces and settings.
// It deliberately lives in its own directory (default ~/.prometheus/go-poc)
// so the proof of concept never rewrites the Node backend's data.
package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Session struct {
	ID             string           `json:"id"`
	Cwd            string           `json:"cwd"`
	Name           string           `json:"name"`
	Runtime        string           `json:"runtime"`
	Created        time.Time        `json:"created"`
	Modified       time.Time        `json:"modified"`
	Archived       bool             `json:"archived"`
	PermissionMode string           `json:"permissionMode,omitempty"`
	Provider       string           `json:"provider,omitempty"`
	Model          string           `json:"model,omitempty"`
	InputTokens    int              `json:"inputTokens,omitempty"`
	OutputTokens   int              `json:"outputTokens,omitempty"`
	CacheRead      int              `json:"cacheRead,omitempty"`
	CacheWrite     int              `json:"cacheWrite,omitempty"`
	Worktree       map[string]any   `json:"worktree,omitempty"`
	Messages       []map[string]any `json:"messages"`
}

type State struct {
	Version    int              `json:"version"`
	Sessions   []Session        `json:"sessions"`
	Workspaces []map[string]any `json:"workspaces"`
	Settings   map[string]any   `json:"settings,omitempty"`
}

type Store struct {
	mu   sync.Mutex
	dir  string
	file string
}

func New(dir string) *Store {
	return &Store{dir: dir, file: filepath.Join(dir, "state.json")}
}

func (s *Store) Load() (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(s.file)
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, err
	}
	if st.Sessions == nil {
		st.Sessions = []Session{}
	}
	if st.Workspaces == nil {
		st.Workspaces = []map[string]any{}
	}
	if st.Settings == nil {
		st.Settings = map[string]any{}
	}
	return &st, nil
}

func (s *Store) Save(st *State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	st.Version = 1
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.file + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.file); err == nil {
		return nil
	}
	// Windows refuses to replace an existing file with Rename.
	_ = os.Remove(s.file)
	return os.Rename(tmp, s.file)
}
