package toolbus

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func post(t *testing.T, url, token, body string) map[string]any {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestMcpInitializeListAndCall(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	url, token := s.Open("s1", ".")

	init := post(t, url, token, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	result, _ := init["result"].(map[string]any)
	if result["protocolVersion"] != "2025-06-18" {
		t.Fatalf("unexpected initialize: %+v", init)
	}

	list := post(t, url, token, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	lr, _ := list["result"].(map[string]any)
	tools, _ := lr["tools"].([]any)
	if len(tools) < 3 {
		t.Fatalf("expected tools, got %+v", list)
	}

	call := post(t, url, token, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"todo_write","arguments":{"todos":[{"content":"write tests","status":"in_progress"}]}}}`)
	cr, _ := call["result"].(map[string]any)
	if cr["isError"] == true {
		t.Fatalf("todo_write failed: %+v", call)
	}
	file := filepath.Join(dir, "todos", "s1.json")
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("todo file: %v", err)
	}
	if !bytes.Contains(raw, []byte("write tests")) {
		t.Fatalf("todo file missing content: %s", raw)
	}

	bad := post(t, url, token, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"memory_search","arguments":{"query":"x"}}}`)
	if bad["error"] != nil {
		t.Fatalf("unexpected error: %+v", bad)
	}
}
