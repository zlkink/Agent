package host

import (
	"errors"
	"strings"
	"testing"

	"prometheus-go/internal/store"
)

func TestSnapshotIncludesLiveAssistant(t *testing.T) {
	h := &Host{cfg: Config{}, sessions: map[string]*Session{}, network: map[string]any{}}
	s := &Session{ID: "s1", messages: []map[string]any{{"role": "user", "content": "hi"}}, Streaming: true}
	s.setLiveMessage(func() map[string]any {
		return map[string]any{
			"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "partial"}},
			"stopReason": "pending",
		}
	})
	h.sessions[s.ID] = s

	msgs, _ := h.snapshot(s)["messages"].([]map[string]any)
	if len(msgs) != 2 {
		t.Fatalf("want the partial assistant in the snapshot, got %d messages", len(msgs))
	}
	if msgs[1]["role"] != "assistant" {
		t.Fatalf("last snapshot message is %v, want assistant", msgs[1]["role"])
	}

	// Once the turn settles the live message must disappear (the final one is
	// already in s.messages).
	s.setLiveMessage(nil)
	s.Streaming = false
	msgs, _ = h.snapshot(s)["messages"].([]map[string]any)
	if len(msgs) != 1 {
		t.Fatalf("settled session still exposes a live message: %d messages", len(msgs))
	}
}

func TestHasVisible(t *testing.T) {
	invisible := []string{"", " ", "\n\t", "\u200b", "\ufeff", "\u200b\u200c\u200d\u2060", "\u00a0"}
	for _, s := range invisible {
		if hasVisible(s) {
			t.Fatalf("%q should be invisible", s)
		}
	}
	for _, s := range []string{"a", " 中 ", "\u200b思考"} {
		if !hasVisible(s) {
			t.Fatalf("%q should be visible", s)
		}
	}
}

func TestLiveMessageSurvivesPersist(t *testing.T) {
	dir := t.TempDir()
	h := New(Config{DataDir: dir, Cwd: dir})
	s := &Session{ID: "live", Cwd: dir, messages: []map[string]any{{"role": "user", "content": "hi"}}, Streaming: true}
	s.setLiveMessage(func() map[string]any {
		return map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "half"}}}
	})
	h.sessions[s.ID] = s
	h.order = append(h.order, s.ID)
	h.persist()

	again := New(Config{DataDir: dir, Cwd: dir})
	got := again.sessions[s.ID]
	if got == nil {
		t.Fatal("session not restored")
	}
	if len(got.messages) != 2 || got.messages[1]["stopReason"] != "aborted" {
		t.Fatalf("partial reply not restored: %#v", got.messages)
	}
}

func TestNeedsContinuation(t *testing.T) {
	tool := []any{map[string]any{"type": "text", "text": "我先看一下"}, map[string]any{"type": "toolCall", "id": "1"}}
	if !needsContinuation(tool) {
		t.Fatal("tool tail should need a continuation")
	}
	if !needsContinuation(nil) {
		t.Fatal("empty turn should need a continuation")
	}
	if !needsContinuation([]any{map[string]any{"type": "text", "text": "我先确认最新代码。"}}) {
		t.Fatal("planning-only text should need a continuation")
	}
	if needsContinuation([]any{map[string]any{"type": "text", "text": "结论：大厅展示当前期次，详情展示最近一期。"}}) {
		t.Fatal("a real answer must not be continued")
	}
}

func TestRetryableProviderError(t *testing.T) {
	for _, s := range []string{"429 Too Many Requests", "502 Bad Gateway", "connection reset by peer", "overloaded"} {
		if !retryableProviderError(errors.New(s)) {
			t.Fatalf("%q should be retryable", s)
		}
	}
	for _, s := range []string{"invalid api key", "model not found", "context length exceeded"} {
		if retryableProviderError(errors.New(s)) {
			t.Fatalf("%q should not be retried", s)
		}
	}
}

func TestTitleFromPrompt(t *testing.T) {
	cases := map[string]string{
		"排查当前 golang 版本，好像跑不出来今天的任务了\n\n更多": "排查当前 golang 版本，好像跑不出来今天的任务了",
		"# 帮我写一个脚本":                          "帮我写一个脚本",
		"<skill name=\"review\">请审查</skill>": "review",
		"短问题？":                               "短问题",
	}
	for in, want := range cases {
		if got := titleFromPrompt(in); got != want {
			t.Fatalf("titleFromPrompt(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExistingNewChatGetsTitle(t *testing.T) {
	dir := t.TempDir()
	seed := store.New(dir)
	if err := seed.Save(&store.State{Version: 1, Sessions: []store.Session{{
		ID: "old", Name: "New chat", Cwd: dir,
		Messages: []map[string]any{{"role": "user", "content": "帮我修复登录问题"}},
	}}}); err != nil {
		t.Fatal(err)
	}
	h := New(Config{DataDir: dir, Cwd: dir})
	if got := h.sessions["old"]; got == nil || got.Name != "帮我修复登录问题" {
		t.Fatalf("old New chat was not titled: %#v", got)
	}
}

func TestSnapshotCarriesPendingQuestion(t *testing.T) {
	h := &Host{cfg: Config{}, sessions: map[string]*Session{}, network: map[string]any{}, uiRequests: map[string]pendingUIRequest{}}
	s := &Session{ID: "s2"}
	h.sessions[s.ID] = s
	h.uiRequests["r1"] = pendingUIRequest{SessionID: s.ID, Request: map[string]any{"id": "r1", "method": "select", "title": "Q", "options": []any{"a", "b"}}}
	pending, _ := h.snapshot(s)["pendingUI"].([]any)
	if len(pending) != 1 {
		t.Fatalf("pendingUI = %#v, want the waiting question", pending)
	}
}

func TestHistoryContextKeepsRecentText(t *testing.T) {
	h := &Host{}
	s := &Session{messages: []map[string]any{
		{"role": "user", "content": "旧问题"},
		{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "旧回答"}}},
		{"role": "toolResult", "content": "ignored"},
	}}
	got := h.historyContext(s, 10, 1000)
	if got == "" || !strings.Contains(got, "旧问题") || !strings.Contains(got, "旧回答") || strings.Contains(got, "ignored") {
		t.Fatalf("history = %q", got)
	}
}

func TestSnapshotThinkingLevel(t *testing.T) {
	h := &Host{cfg: Config{}, sessions: map[string]*Session{}, network: map[string]any{}}
	s := &Session{ID: "think", Runtime: "claude-code", ThinkingLevel: "high"}
	snap := h.snapshot(s)
	if snap["thinkingLevel"] != "high" {
		t.Fatalf("thinkingLevel = %v", snap["thinkingLevel"])
	}
	levels, _ := snap["availableThinkingLevels"].([]any)
	if len(levels) < 2 {
		t.Fatalf("availableThinkingLevels = %#v", snap["availableThinkingLevels"])
	}
}
