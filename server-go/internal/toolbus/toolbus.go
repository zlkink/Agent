// Package toolbus is a minimal MCP Streamable HTTP server that gives Claude
// Code the same kind of Prometheus tools the Node host exposes. It is the
// first slice of the tool bus described in docs/agent-runtime.md §4.
package toolbus

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Server struct {
	ln        net.Listener
	http      *http.Server
	dir       string
	pluginURL string
	mu        sync.Mutex
	sessions  map[string]*session
	memMu     sync.Mutex
	subagent  func(ctx context.Context, cwd, agent, task string) (string, error)
	ask       func(ctx context.Context, sessionID, cwd, question string, options []string) (string, error)
}

// SetSubagent installs the runner behind the `subagent` tool.
func (s *Server) SetSubagent(fn func(ctx context.Context, cwd, agent, task string) (string, error)) {
	s.mu.Lock()
	s.subagent = fn
	s.mu.Unlock()
}

// SetAsk installs the interactive question runner behind `assistant_ask`.
func (s *Server) SetAsk(fn func(ctx context.Context, sessionID, cwd, question string, options []string) (string, error)) {
	s.mu.Lock()
	s.ask = fn
	s.mu.Unlock()
}

type session struct {
	ID    string
	Token string
	Cwd   string
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func New(dir, pluginURL string) (*Server, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &Server{ln: ln, dir: dir, pluginURL: pluginURL, sessions: map[string]*session{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp/", s.handle)
	s.http = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = s.http.Serve(ln) }()
	return s, nil
}

func (s *Server) Close() {
	_ = s.http.Close()
}

// Open returns the endpoint and bearer token for one session.
func (s *Server) Open(sessionID, cwd string) (url, token string) {
	raw := make([]byte, 24)
	_, _ = rand.Read(raw)
	token = "pmt_" + base64.RawURLEncoding.EncodeToString(raw)
	s.mu.Lock()
	s.sessions[sessionID] = &session{ID: sessionID, Token: token, Cwd: cwd}
	s.mu.Unlock()
	return fmt.Sprintf("http://%s/mcp/%s", s.ln.Addr().String(), sessionID), token
}

func (s *Server) CloseSession(sessionID string) {
	s.mu.Lock()
	delete(s.sessions, sessionID)
	s.mu.Unlock()
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	sessionID := strings.TrimPrefix(r.URL.Path, "/mcp/")
	s.mu.Lock()
	sess := s.sessions[sessionID]
	s.mu.Unlock()
	if sess == nil {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}
	if !authorized(r, sess.Token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodGet {
		w.Header().Set("Allow", "POST")
		http.Error(w, "SSE stream not implemented", http.StatusMethodNotAllowed)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, "body", http.StatusBadRequest)
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPC(w, nil, nil, &rpcError{Code: -32700, Message: "parse error"})
		return
	}
	if strings.HasPrefix(req.Method, "notifications/") {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	result, rerr := s.dispatch(sess.ID, &req)
	writeRPC(w, req.ID, result, rerr)
}

func authorized(r *http.Request, token string) bool {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") && strings.TrimPrefix(auth, "Bearer ") == token {
		return true
	}
	return r.Header.Get("x-api-key") == token
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func writeRPC(w http.ResponseWriter, id json.RawMessage, result any, rerr *rpcError) {
	w.Header().Set("Content-Type", "application/json")
	if rerr != nil {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": rawID(id), "error": rerr,
		})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": rawID(id), "result": result,
	})
}

func rawID(id json.RawMessage) any {
	if len(id) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(id, &v) != nil {
		return nil
	}
	return v
}

func (s *Server) dispatch(sessionID string, req *rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		return map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "prometheus-go", "version": "0.1.0"},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": s.toolDefinitions()}, nil
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		return s.call(sessionID, p.Name, p.Arguments)
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
}

func localToolDefinitions() []any {
	return []any{
		map[string]any{
			"name":        "todo_write",
			"description": "Replace the todo list for this chat. Use it to plan and track multi-step work.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"todos": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"content": map[string]any{"type": "string"},
								"status":  map[string]any{"type": "string", "enum": []any{"pending", "in_progress", "completed"}},
							},
							"required": []any{"content"},
						},
					},
				},
				"required": []any{"todos"},
			},
		},
		map[string]any{
			"name":        "memory_save",
			"description": "Save one durable fact to long-term memory.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text": map[string]any{"type": "string"},
					"tags": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				},
				"required": []any{"text"},
			},
		},
		map[string]any{
			"name":        "memory_search",
			"description": "Search saved memories by keyword.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string"},
					"limit": map[string]any{"type": "number"},
				},
				"required": []any{"query"},
			},
		},
		map[string]any{
			"name":        "open_file",
			"description": "Show a file in the user's side viewer. Returns the path.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string"},
					"line": map[string]any{"type": "number"},
				},
				"required": []any{"path"},
			},
		},
		map[string]any{
			"name":        "assistant_ask",
			"description": "Ask the user a question in this chat and wait for the answer. Works in any conversation, not only delegated runs. Use it when you cannot tell what the user wants and a wrong guess would waste work.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"question": map[string]any{"type": "string", "description": "One clear question"},
					"options":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Up to 6 short choices; the user may also type something else"},
				},
				"required": []any{"question"},
			},
		},
		map[string]any{
			"name":        "subagent",
			"description": "Delegate a bounded task to a Claude Code or Codex subagent. Read-only unless the agent name ends in -writer.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"agent": map[string]any{
						"type": "string",
						"enum": []any{"claude-code", "claude-code-writer", "codex", "codex-writer"},
					},
					"task":  map[string]any{"type": "string"},
					"model": map[string]any{"type": "string"},
				},
				"required": []any{"agent", "task"},
			},
		},
	}
}

func (s *Server) toolDefinitions() []any {
	local := localToolDefinitions()
	if s.pluginURL == "" {
		return local
	}
	plugin := s.fetchPluginTools()
	if len(plugin) == 0 {
		return local
	}
	out := append([]any(nil), local...)
	index := map[string]int{}
	for i, raw := range out {
		if m, ok := raw.(map[string]any); ok {
			index[str(m["name"])] = i
		}
	}
	for _, raw := range plugin {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name := str(m["name"])
		// The assistant plugin's version only works in delegated runs; the Go
		// host provides a version that works in any chat.
		if name == "assistant_ask" {
			continue
		}
		if i, exists := index[name]; exists {
			out[i] = m
			continue
		}
		index[name] = len(out)
		out = append(out, m)
	}
	return out
}

func (s *Server) fetchPluginTools() []any {
	client := &http.Client{Timeout: 5 * time.Second}
	res, err := client.Get(strings.TrimRight(s.pluginURL, "/") + "/tools")
	if err != nil {
		return nil
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil
	}
	var list []map[string]any
	if json.NewDecoder(res.Body).Decode(&list) != nil {
		return nil
	}
	out := make([]any, 0, len(list))
	for _, item := range list {
		out = append(out, map[string]any{
			"name":        str(item["name"]),
			"description": str(item["description"]),
			"inputSchema": item["inputSchema"],
		})
	}
	return out
}

func (s *Server) callPlugin(cwd, sessionID, name string, args map[string]any) (any, bool) {
	body, _ := json.Marshal(map[string]any{"name": name, "args": args, "cwd": cwd, "sessionId": sessionID})
	client := &http.Client{Timeout: 120 * time.Second}
	res, err := client.Post(strings.TrimRight(s.pluginURL, "/")+"/call", "application/json", strings.NewReader(string(body)))
	if err != nil {
		return nil, false
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, false
	}
	var out struct {
		Content []any `json:"content"`
		Details any   `json:"details"`
	}
	if json.NewDecoder(res.Body).Decode(&out) != nil {
		return nil, false
	}
	result := map[string]any{"content": out.Content, "isError": false}
	if !emptyDetails(out.Details) {
		result["structuredContent"] = out.Details
	}
	return result, true
}

func asList(v any) []any {
	if list, ok := v.([]any); ok {
		return list
	}
	return nil
}

func emptyDetails(v any) bool {
	switch d := v.(type) {
	case nil:
		return true
	case map[string]any:
		return len(d) == 0
	case []any:
		return len(d) == 0
	}
	return false
}

func (s *Server) call(sessionID, name string, args map[string]any) (any, *rpcError) {
	s.mu.Lock()
	ask := s.ask
	cwd := ""
	if sess := s.sessions[sessionID]; sess != nil {
		cwd = sess.Cwd
	}
	s.mu.Unlock()
	if name == "assistant_ask" {
		if ask == nil {
			return nil, &rpcError{Code: -32603, Message: "interactive questions are not available"}
		}
		options := []string{}
		for _, raw := range asList(args["options"]) {
			if v := str(raw); v != "" {
				options = append(options, v)
			}
		}
		answer, err := ask(context.Background(), sessionID, cwd, str(args["question"]), options)
		if err != nil {
			return nil, &rpcError{Code: -32603, Message: err.Error()}
		}
		return map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "The user answered: " + answer}},
			"isError": false,
		}, nil
	}
	if s.pluginURL != "" {
		if result, ok := s.callPlugin(cwd, sessionID, name, args); ok {
			return result, nil
		}
	}
	switch name {
	case "todo_write":
		todos, _ := args["todos"].([]any)
		if todos == nil {
			todos = []any{}
		}
		file := filepath.Join(s.dir, "todos", safe(sessionID)+".json")
		_ = os.MkdirAll(filepath.Dir(file), 0o755)
		raw, _ := json.MarshalIndent(todos, "", "  ")
		if err := os.WriteFile(file, raw, 0o600); err != nil {
			return nil, &rpcError{Code: -32603, Message: err.Error()}
		}
		return toolResult(fmt.Sprintf("Saved %d todos", len(todos)), map[string]any{"todos": todos}), nil

	case "memory_save":
		text := strings.TrimSpace(str(args["text"]))
		if text == "" {
			return nil, &rpcError{Code: -32602, Message: "text is required"}
		}
		entry := map[string]any{
			"id": newID(), "text": text, "tags": args["tags"],
			"sessionId": sessionID, "createdAt": time.Now().UTC().Format(time.RFC3339),
		}
		s.memMu.Lock()
		items := s.readMemory()
		items = append(items, entry)
		err := s.writeMemory(items)
		s.memMu.Unlock()
		if err != nil {
			return nil, &rpcError{Code: -32603, Message: err.Error()}
		}
		return toolResult("Saved memory "+str(entry["id"]), entry), nil

	case "memory_search":
		q := strings.ToLower(strings.TrimSpace(str(args["query"])))
		limit := 5
		if n, ok := args["limit"].(float64); ok && n > 0 {
			limit = int(n)
		}
		s.memMu.Lock()
		items := s.readMemory()
		s.memMu.Unlock()
		hits := []any{}
		for _, raw := range items {
			item, _ := raw.(map[string]any)
			if item == nil {
				continue
			}
			if q == "" || strings.Contains(strings.ToLower(str(item["text"])), q) {
				hits = append(hits, item)
				if len(hits) >= limit {
					break
				}
			}
		}
		return toolResult(fmt.Sprintf("Found %d memories", len(hits)), map[string]any{"hits": hits}), nil

	case "open_file":
		return toolResult("Opened "+str(args["path"]), map[string]any{"path": str(args["path"])}), nil

	case "subagent":
		s.mu.Lock()
		runner := s.subagent
		cwd := ""
		if sess := s.sessions[sessionID]; sess != nil {
			cwd = sess.Cwd
		}
		s.mu.Unlock()
		if runner == nil {
			return nil, &rpcError{Code: -32603, Message: "subagent runner is not available"}
		}
		agent := str(args["agent"])
		task := str(args["task"])
		if agent == "" || task == "" {
			return nil, &rpcError{Code: -32602, Message: "agent and task are required"}
		}
		out, err := runner(context.Background(), cwd, agent, task)
		if err != nil {
			return nil, &rpcError{Code: -32603, Message: err.Error()}
		}
		return map[string]any{
			"content": []any{map[string]any{"type": "text", "text": out}},
			"isError": false,
		}, nil
	}
	return nil, &rpcError{Code: -32602, Message: "unknown tool: " + name}
}

func (s *Server) readMemory() []any {
	raw, err := os.ReadFile(filepath.Join(s.dir, "memory.json"))
	if err != nil {
		return []any{}
	}
	var items []any
	if json.Unmarshal(raw, &items) != nil {
		return []any{}
	}
	return items
}

func (s *Server) writeMemory(items []any) error {
	raw, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.dir, "memory.json"), raw, 0o600)
}

func toolResult(text string, structured any) map[string]any {
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": text}},
		"structuredContent": structured,
		"isError":           false,
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func safe(v string) string {
	var b strings.Builder
	for _, r := range v {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "session"
	}
	return b.String()
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
