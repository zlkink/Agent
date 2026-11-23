package host

import (
	"fmt"
	"os"
	"testing"

	"prometheus-go/internal/agents"
)

// The test binary doubles as a fake `codex exec --json` when the child env asks
// for it, so the adapter can be checked without a model call.
func TestMain(m *testing.M) {
	if os.Getenv("RAMICORE_FAKE_CODEX") == "1" {
		fmt.Print(`{"type":"item.started","item":{"id":"c1","type":"command_execution","command":"echo hi"}}
{"type":"item.completed","item":{"id":"c1","type":"command_execution","command":"echo hi","aggregated_output":"hi\n","exit_code":0}}
{"type":"item.started","item":{"id":"m1","type":"mcp_tool_call","server":"ramicore","tool":"read","arguments":{"path":"a.txt"}}}
{"type":"item.completed","item":{"id":"m1","type":"mcp_tool_call","server":"ramicore","tool":"read","arguments":{"path":"a.txt"},"result":{"content":[{"type":"text","text":"file body"}]}}}
{"type":"item.completed","item":{"type":"agent_message","phase":"final_answer","text":"done"}}
{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":2}}
`)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestCodexToolsBecomeMessageBlocks(t *testing.T) {
	t.Setenv("RAMICORE_FAKE_CODEX", "1")
	dir := t.TempDir()
	h := New(Config{Cwd: dir, DataDir: dir, Codex: agents.Agent{Path: os.Args[0]}, AllowWrite: true})
	s := &Session{ID: "codex-tools", Cwd: dir}
	h.sessions[s.ID] = s
	h.order = append(h.order, s.ID)

	h.runCodexPrompt(s, "review")

	var assistant map[string]any
	var results int
	for _, m := range s.messages {
		switch m["role"] {
		case "assistant":
			assistant = m
		case "toolResult":
			results++
			if content, _ := m["content"].([]any); len(content) == 0 || str(asMap(content[0])["text"]) == "" {
				t.Fatalf("empty tool result: %#v", m)
			}
		}
	}
	if results != 2 {
		t.Fatalf("tool results = %d, want 2", results)
	}
	if assistant == nil {
		t.Fatal("no assistant message")
	}
	blocks, _ := assistant["content"].([]any)
	var names []string
	for _, raw := range blocks {
		b := asMap(raw)
		if str(b["type"]) == "toolCall" {
			names = append(names, str(b["name"]))
		}
	}
	if len(names) != 2 || names[0] != "bash" || names[1] != "read" {
		t.Fatalf("tool blocks = %v, want [bash read]", names)
	}
	if text := messageText(blocks); text != "done" {
		t.Fatalf("text = %q", text)
	}
}
