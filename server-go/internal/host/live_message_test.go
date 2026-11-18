package host

import "testing"

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
