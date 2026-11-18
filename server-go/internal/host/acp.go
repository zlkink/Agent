package host

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"prometheus-go/internal/acp"
	"prometheus-go/internal/mcpconfig"
)

type acpPromptState struct {
	mu           sync.Mutex
	started      bool
	text         strings.Builder
	content      []any
	thinkingAt   int
	textAt       int
	usage        map[string]any
	toolsStarted map[string]bool
	toolsEnded   map[string]bool
}

func envMap(items []string) map[string]string {
	out := map[string]string{}
	for _, kv := range items {
		if i := strings.Index(kv, "="); i > 0 {
			out[kv[:i]] = kv[i+1:]
		}
	}
	return out
}

func (h *Host) runAcpPrompt(s *Session, text string) {
	c, sessionID, err := h.ensureAcp(s)
	if err != nil {
		h.finishError(s, err)
		return
	}
	state := &acpPromptState{toolsStarted: map[string]bool{}, toolsEnded: map[string]bool{}, thinkingAt: -1, textAt: -1}
	s.mu.Lock()
	s.acpState = state
	s.mu.Unlock()
	s.setLiveMessage(func() map[string]any {
		state.mu.Lock()
		defer state.mu.Unlock()
		if !state.started || len(state.content) == 0 {
			return nil
		}
		raw, err := json.Marshal(map[string]any{
			"role": "assistant", "content": state.content, "provider": "acp",
			"model": "default", "usage": zeroUsage(), "stopReason": "pending",
			"timestamp": time.Now().UnixMilli(),
		})
		if err != nil {
			return nil
		}
		var out map[string]any
		if json.Unmarshal(raw, &out) != nil {
			return nil
		}
		return out
	})

	staticCtx, turnCtx := h.pluginContext(text, s)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	params := map[string]any{
		"sessionId": sessionID,
		"prompt": []any{map[string]any{
			"type": "text", "text": withContext(staticCtx, turnCtx, text),
		}},
	}
	raw, err := c.Request(ctx, "session/prompt", params)
	var result struct {
		StopReason string `json:"stopReason"`
	}
	_ = json.Unmarshal(raw, &result)
	if err != nil {
		h.finishAcp(s, state, err)
		return
	}
	if result.StopReason == "cancelled" {
		h.finishAcp(s, state, context.Canceled)
		return
	}
	h.finishAcp(s, state, nil)
}

func (h *Host) ensureAcp(s *Session) (*acp.Client, string, error) {
	s.mu.Lock()
	c := s.acpClient
	sessionID := s.acpSession
	s.mu.Unlock()
	if c != nil && sessionID != "" {
		return c, sessionID, nil
	}
	if h.cfg.ACPEntry == "" {
		return nil, "", fmt.Errorf("ACP adapter is not installed")
	}
	c, err := acp.Start(acp.StartOptions{
		Node:  h.cfg.ACPNode,
		Entry: h.cfg.ACPEntry,
		Dir:   s.Cwd,
		Env:   envMap(h.subagentEnv()),
		OnNotify: func(method string, params json.RawMessage) {
			h.acpNotify(s, method, params)
		},
		OnRequest: func(ctx context.Context, method string, params json.RawMessage) (any, error) {
			return h.acpRequest(s, method, params)
		},
		OnExit: func(code int, stderr string) {
			s.mu.Lock()
			streaming := s.Streaming
			s.acpClient = nil
			s.acpSession = ""
			s.mu.Unlock()
			if streaming {
				h.event(s.ID, map[string]any{"type": "agent_settled", "aborted": false})
			}
		},
	})
	if err != nil {
		return nil, "", err
	}
	initCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := c.Request(initCtx, "initialize", map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
		"clientInfo": map[string]any{"name": "prometheus", "version": "1"},
	}); err != nil {
		c.Close()
		return nil, "", fmt.Errorf("ACP initialize: %w", err)
	}
	mcp := []any{}
	if h.bus != nil {
		url, token := h.bus.Open(s.ID, s.Cwd)
		mcp = append(mcp, map[string]any{
			"type": "http", "name": "prometheus", "url": url,
			"headers": []any{map[string]any{"name": "Authorization", "value": "Bearer " + token}},
		})
	}
	for name, cfg := range mcpconfig.Load(h.cfg.DataDir) {
		if str(cfg["command"]) != "" {
			args := []any{}
			if a, ok := cfg["args"].([]any); ok {
				args = a
			}
			mcp = append(mcp, map[string]any{"type": "stdio", "name": name, "command": cfg["command"], "args": args})
		} else if url := str(cfg["url"]); url != "" {
			headers := []any{}
			if hs, ok := cfg["headers"].(map[string]any); ok {
				for k, v := range hs {
					headers = append(headers, map[string]any{"name": k, "value": str(v)})
				}
			}
			mcp = append(mcp, map[string]any{"type": "http", "name": name, "url": url, "headers": headers})
		}
	}
	raw, err := c.Request(initCtx, "session/new", map[string]any{"cwd": s.Cwd, "mcpServers": mcp})
	if err != nil {
		c.Close()
		return nil, "", fmt.Errorf("ACP session/new: %w", err)
	}
	var opened struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(raw, &opened)
	if opened.SessionID == "" {
		c.Close()
		return nil, "", fmt.Errorf("ACP session/new returned no session id")
	}
	s.mu.Lock()
	s.acpClient = c
	s.acpSession = opened.SessionID
	s.mu.Unlock()
	return c, opened.SessionID, nil
}

func (h *Host) acpNotify(s *Session, method string, params json.RawMessage) {
	if method != "session/update" {
		return
	}
	var p struct {
		Update map[string]any `json:"update"`
	}
	if json.Unmarshal(params, &p) != nil || p.Update == nil {
		return
	}
	s.mu.Lock()
	state := s.acpState
	s.mu.Unlock()
	if state == nil {
		return
	}
	kind := str(p.Update["sessionUpdate"])
	switch kind {
	case "agent_message_chunk":
		content, _ := p.Update["content"].(map[string]any)
		if str(content["type"]) == "text" {
			h.acpText(s, state, str(content["text"]), "text")
		}
	case "agent_thought_chunk":
		content, _ := p.Update["content"].(map[string]any)
		if str(content["type"]) == "text" {
			h.acpText(s, state, str(content["text"]), "thinking")
		}
	case "tool_call":
		h.acpTool(s, state, p.Update, false)
	case "tool_call_update":
		h.acpTool(s, state, p.Update, true)
	case "usage_update":
		if used, ok := p.Update["used"].(float64); ok {
			state.mu.Lock()
			state.usage = map[string]any{"contextUsed": int(used)}
			state.mu.Unlock()
		}
	}
}

func (h *Host) acpText(s *Session, state *acpPromptState, text, kind string) {
	if text == "" {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.started {
		state.started = true
		h.event(s.ID, map[string]any{"type": "agent_start"})
		h.event(s.ID, map[string]any{"type": "turn_start"})
		msg := map[string]any{
			"role": "assistant", "content": []any{}, "provider": "acp",
			"model": "default", "usage": zeroUsage(), "stopReason": "pending",
			"timestamp": time.Now().UnixMilli(),
		}
		h.event(s.ID, map[string]any{"type": "message_start", "message": msg})
	}
	if kind == "thinking" {
		if state.thinkingAt < 0 {
			state.content = append(state.content, map[string]any{"type": "thinking", "thinking": ""})
			state.thinkingAt = len(state.content) - 1
			state.textAt = -1
		}
		block := state.content[state.thinkingAt].(map[string]any)
		block["thinking"] = str(block["thinking"]) + text
		h.event(s.ID, map[string]any{"type": "message_update", "delta": map[string]any{"type": "thinking_delta", "contentIndex": state.thinkingAt, "delta": text}})
		return
	}
	if state.textAt < 0 {
		state.content = append(state.content, map[string]any{"type": "text", "text": ""})
		state.textAt = len(state.content) - 1
		state.thinkingAt = -1
	}
	block := state.content[state.textAt].(map[string]any)
	block["text"] = str(block["text"]) + text
	state.text.WriteString(text)
	h.event(s.ID, map[string]any{"type": "message_update", "delta": map[string]any{"type": "text_delta", "contentIndex": state.textAt, "delta": text}})
}

func (h *Host) acpTool(s *Session, state *acpPromptState, u map[string]any, update bool) {
	id := str(u["toolCallId"])
	if id == "" {
		return
	}
	status := str(u["status"])
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.toolsStarted[id] {
		state.toolsStarted[id] = true
		name := str(u["title"])
		if name == "" {
			name = str(u["kind"])
		}
		h.event(s.ID, map[string]any{
			"type": "tool_execution_start", "toolCallId": id, "toolName": name,
			"args": u["rawInput"],
		})
	}
	if (status == "completed" || status == "failed") && !state.toolsEnded[id] {
		state.toolsEnded[id] = true
		name := str(u["title"])
		if name == "" {
			name = str(u["kind"])
		}
		h.event(s.ID, map[string]any{
			"type": "tool_execution_end", "toolCallId": id, "toolName": name,
			"result": u["rawOutput"], "isError": status == "failed",
		})
	}
}

func (h *Host) acpRequest(s *Session, method string, params json.RawMessage) (any, error) {
	if method != "session/request_permission" {
		return nil, fmt.Errorf("method not found: %s", method)
	}
	var p struct {
		ToolCall map[string]any `json:"toolCall"`
		Options  []struct {
			OptionID string `json:"optionId"`
			Kind     string `json:"kind"`
			Name     string `json:"name"`
		} `json:"options"`
	}
	if json.Unmarshal(params, &p) != nil {
		return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}, nil
	}
	allowOpt := ""
	rejectOpt := ""
	for _, o := range p.Options {
		switch o.Kind {
		case "allow_once", "allow_always":
			if allowOpt == "" {
				allowOpt = o.OptionID
			}
		case "reject_once", "reject_always":
			if rejectOpt == "" {
				rejectOpt = o.OptionID
			}
		}
	}
	allow := false
	s.mu.Lock()
	mode := s.PermissionMode
	s.mu.Unlock()
	if mode == "" {
		mode = "auto"
	}
	title := str(p.ToolCall["title"])
	if title == "" {
		title = str(p.ToolCall["kind"])
	}
	switch mode {
	case "auto", "bypassPermissions":
		allow = true
	case "plan":
		kind := str(p.ToolCall["kind"])
		allow = kind == "read" || kind == "search" || kind == "think"
	default:
		raw, _ := json.Marshal(p.ToolCall["rawInput"])
		allow = h.askUI(s, title, raw)
	}
	if allow && allowOpt != "" {
		return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": allowOpt}}, nil
	}
	if rejectOpt != "" {
		return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": rejectOpt}}, nil
	}
	return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}, nil
}

func (h *Host) finishAcp(s *Session, state *acpPromptState, err error) {
	state.mu.Lock()
	text := state.text.String()
	usage := state.usage
	content := append([]any(nil), state.content...)
	state.mu.Unlock()
	if len(content) == 0 && text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	stop := "stop"
	if err != nil {
		stop = "error"
	}
	msg := map[string]any{
		"role": "assistant", "content": content, "provider": "acp",
		"model": "default", "usage": zeroUsage(), "stopReason": stop,
		"timestamp": time.Now().UnixMilli(),
	}
	if err != nil {
		msg["errorMessage"] = err.Error()
	}
	s.clearLiveMessage()
	s.appendMessage(msg)
	h.event(s.ID, map[string]any{"type": "message_end", "message": msg})
	h.event(s.ID, map[string]any{"type": "turn_end"})
	h.event(s.ID, map[string]any{"type": "agent_settled", "aborted": false})
	s.mu.Lock()
	s.acpState = nil
	if usage != nil {
		// context usage is reported by ACP as used/size, not token totals
	}
	s.mu.Unlock()
	h.endRun(s)
}
