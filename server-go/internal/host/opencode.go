package host

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"prometheus-go/internal/mcpconfig"
	"prometheus-go/internal/opencode"
)

type ocPromptState struct {
	mu         sync.Mutex
	started    bool
	text       strings.Builder
	content    []any
	thinkingAt int
	textAt     int
	parts      map[string]string
	tools      map[string]bool
	ended      map[string]bool
	done       chan struct{}
	doneOnce   sync.Once
	err        error
}

func (st *ocPromptState) finish(err error) {
	st.mu.Lock()
	if st.err == nil && err != nil {
		st.err = err
	}
	st.mu.Unlock()
	st.doneOnce.Do(func() { close(st.done) })
}

func (h *Host) runOpenCodePrompt(s *Session, text string) {
	c, sessionID, err := h.ensureOpenCode(s)
	if err != nil {
		h.finishError(s, err)
		return
	}
	state := &ocPromptState{parts: map[string]string{}, tools: map[string]bool{}, ended: map[string]bool{}, done: make(chan struct{}), thinkingAt: -1, textAt: -1}
	s.mu.Lock()
	s.ocState = state
	s.mu.Unlock()

	now := time.Now().UnixMilli()
	user := map[string]any{"role": "user", "content": text, "timestamp": now}
	s.appendMessage(user)
	h.event(s.ID, map[string]any{"type": "agent_start"})
	h.event(s.ID, map[string]any{"type": "turn_start"})
	h.event(s.ID, map[string]any{"type": "message_start", "message": user})

	staticCtx, turnCtx := h.pluginContext(text, s)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	body := map[string]any{
		"parts": []any{map[string]any{"type": "text", "text": text}},
		"agent": "build",
	}
	if staticCtx != "" || turnCtx != "" {
		body["system"] = withContext(staticCtx, turnCtx, "")
	}
	if _, err := c.Request(ctx, "POST", "/session/"+sessionID+"/prompt_async", s.Cwd, body); err != nil {
		state.finish(err)
	} else {
		select {
		case <-ctx.Done():
			state.finish(ctx.Err())
		case <-state.done:
		}
	}
	state.mu.Lock()
	runErr := state.err
	state.mu.Unlock()
	h.finishOpenCode(s, state, runErr)
}

func (h *Host) ensureOpenCode(s *Session) (*opencode.Client, string, error) {
	s.mu.Lock()
	c := s.ocClient
	sessionID := s.ocSession
	s.mu.Unlock()
	if c != nil && sessionID != "" {
		return c, sessionID, nil
	}
	if h.cfg.OpenCodePath == "" {
		return nil, "", fmt.Errorf("OpenCode is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, err := opencode.Start(ctx, h.cfg.OpenCodePath, s.Cwd)
	if err != nil {
		return nil, "", err
	}
	created, err := c.Request(ctx, "POST", "/session", s.Cwd, map[string]any{"title": s.Name})
	if err != nil {
		c.Close()
		return nil, "", fmt.Errorf("opencode session: %w", err)
	}
	sessionID = str(created["id"])
	if sessionID == "" {
		c.Close()
		return nil, "", fmt.Errorf("opencode returned no session id")
	}
	if h.bus != nil {
		url, token := h.bus.Open(s.ID, s.Cwd)
		name := "prometheus-" + strings.ReplaceAll(s.ID[:8], "-", "")
		_, _ = c.Request(ctx, "POST", "/mcp", s.Cwd, map[string]any{
			"name": name,
			"config": map[string]any{
				"type": "remote", "url": url, "enabled": true, "oauth": false,
				"headers": map[string]any{"Authorization": "Bearer " + token},
			},
		})
	}
	for name, cfg := range mcpconfig.Load(h.cfg.DataDir) {
		mc := map[string]any{"enabled": true}
		if str(cfg["command"]) != "" {
			mc["type"] = "local"
			cmd := []any{cfg["command"]}
			if a, ok := cfg["args"].([]any); ok {
				cmd = append(cmd, a...)
			}
			mc["command"] = cmd
			if e, ok := cfg["env"].(map[string]any); ok {
				mc["environment"] = e
			}
		} else {
			mc["type"] = "remote"
			mc["url"] = cfg["url"]
			mc["oauth"] = false
			if hs, ok := cfg["headers"].(map[string]any); ok {
				mc["headers"] = hs
			}
		}
		_, _ = c.Request(ctx, "POST", "/mcp", s.Cwd, map[string]any{"name": name, "config": mc})
	}
	eventCtx, eventCancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.ocClient = c
	s.ocSession = sessionID
	s.ocCancel = eventCancel
	s.mu.Unlock()
	go func() {
		_ = c.Events(eventCtx, s.Cwd, func(typ string, props map[string]any) {
			h.ocEvent(s, typ, props)
		})
	}()
	return c, sessionID, nil
}

func (h *Host) ocEvent(s *Session, typ string, p map[string]any) {
	if sid := str(p["sessionID"]); sid != "" {
		s.mu.Lock()
		current := s.ocSession
		s.mu.Unlock()
		if current != "" && sid != current {
			return
		}
	}
	s.mu.Lock()
	state := s.ocState
	s.mu.Unlock()
	if state == nil {
		return
	}
	switch typ {
	case "message.updated":
		info, _ := p["info"].(map[string]any)
		if str(info["role"]) == "assistant" {
			h.ocStartMessage(s, state)
		}
	case "message.part.delta":
		if str(p["field"]) != "text" {
			return
		}
		delta := str(p["delta"])
		if delta == "" {
			return
		}
		partID := str(p["partID"])
		state.mu.Lock()
		state.parts[partID] += delta
		state.mu.Unlock()
		h.ocText(s, state, delta)
	case "message.part.updated":
		part, _ := p["part"].(map[string]any)
		if part == nil {
			return
		}
		if part["synthetic"] == true || part["ignored"] == true {
			return
		}
		switch str(part["type"]) {
		case "text", "reasoning", "thinking":
			full := str(part["text"])
			partID := str(part["id"])
			state.mu.Lock()
			have := state.parts[partID]
			if len(full) > len(have) && strings.HasPrefix(full, have) {
				delta := full[len(have):]
				state.parts[partID] = full
				state.mu.Unlock()
				if str(part["type"]) == "reasoning" || str(part["type"]) == "thinking" {
					h.ocThinking(s, state, delta)
				} else {
					h.ocText(s, state, delta)
				}
			} else {
				state.mu.Unlock()
			}
			if t, ok := part["time"].(map[string]any); ok && t["end"] != nil {
				state.mu.Lock()
				state.ended[partID] = true
				state.mu.Unlock()
			}
		case "tool":
			h.ocTool(s, state, part)
		}
	case "session.idle":
		state.finish(nil)
	case "session.status":
		status, _ := p["status"].(map[string]any)
		if str(status["type"]) == "idle" {
			state.finish(nil)
		}
	case "session.error":
		state.finish(fmt.Errorf("%s", ocErrorText(p["error"])))
	}
}

func (h *Host) ocStartMessage(s *Session, state *ocPromptState) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.started {
		return
	}
	state.started = true
	msg := map[string]any{
		"role": "assistant", "content": []any{}, "provider": "opencode",
		"model": "default", "usage": zeroUsage(), "stopReason": "pending",
		"timestamp": time.Now().UnixMilli(),
	}
	h.event(s.ID, map[string]any{"type": "message_start", "message": msg})
}

func (h *Host) ocText(s *Session, state *ocPromptState, delta string) {
	if delta == "" {
		return
	}
	h.ocStartMessage(s, state)
	state.mu.Lock()
	if state.textAt < 0 {
		state.content = append(state.content, map[string]any{"type": "text", "text": ""})
		state.textAt = len(state.content) - 1
		state.thinkingAt = -1
	}
	block := state.content[state.textAt].(map[string]any)
	block["text"] = str(block["text"]) + delta
	state.text.WriteString(delta)
	idx := state.textAt
	state.mu.Unlock()
	h.event(s.ID, map[string]any{"type": "message_update", "delta": map[string]any{"type": "text_delta", "contentIndex": idx, "delta": delta}})
}

func (h *Host) ocThinking(s *Session, state *ocPromptState, delta string) {
	if delta == "" {
		return
	}
	h.ocStartMessage(s, state)
	state.mu.Lock()
	if state.thinkingAt < 0 {
		state.content = append(state.content, map[string]any{"type": "thinking", "thinking": ""})
		state.thinkingAt = len(state.content) - 1
		state.textAt = -1
	}
	block := state.content[state.thinkingAt].(map[string]any)
	block["thinking"] = str(block["thinking"]) + delta
	idx := state.thinkingAt
	state.mu.Unlock()
	h.event(s.ID, map[string]any{"type": "message_update", "delta": map[string]any{"type": "thinking_delta", "contentIndex": idx, "delta": delta}})
}

func (h *Host) ocTool(s *Session, state *ocPromptState, part map[string]any) {
	id := str(part["callID"])
	if id == "" {
		id = str(part["id"])
	}
	if id == "" {
		return
	}
	stateInfo, _ := part["state"].(map[string]any)
	status := str(stateInfo["status"])
	name := str(part["tool"])
	if name == "" {
		name = "tool"
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if status == "" || status == "pending" {
		return
	}
	if !state.tools[id] {
		state.tools[id] = true
		h.event(s.ID, map[string]any{
			"type": "tool_execution_start", "toolCallId": id, "toolName": name,
			"args": stateInfo["input"],
		})
	}
	if (status == "completed" || status == "error") && !state.ended[id] {
		state.ended[id] = true
		h.event(s.ID, map[string]any{
			"type": "tool_execution_end", "toolCallId": id, "toolName": name,
			"result": stateInfo["output"], "isError": status == "error",
		})
	}
}

func (h *Host) finishOpenCode(s *Session, state *ocPromptState, err error) {
	state.mu.Lock()
	text := state.text.String()
	content := append([]any(nil), state.content...)
	state.mu.Unlock()
	if !state.started && text == "" {
		// The model produced no assistant message; still close the turn.
		h.ocStartMessage(s, state)
	}
	if len(content) == 0 && text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	stop := "stop"
	if err != nil {
		stop = "error"
	}
	msg := map[string]any{
		"role": "assistant", "content": content, "provider": "opencode",
		"model": "default", "usage": zeroUsage(), "stopReason": stop,
		"timestamp": time.Now().UnixMilli(),
	}
	if err != nil {
		msg["errorMessage"] = err.Error()
	}
	s.appendMessage(msg)
	h.event(s.ID, map[string]any{"type": "message_end", "message": msg})
	h.event(s.ID, map[string]any{"type": "turn_end"})
	h.event(s.ID, map[string]any{"type": "agent_settled", "aborted": false})
	s.mu.Lock()
	s.ocState = nil
	s.mu.Unlock()
	h.endRun(s)
}

func ocErrorText(v any) string {
	switch e := v.(type) {
	case string:
		return e
	case map[string]any:
		if m := str(e["message"]); m != "" {
			return m
		}
		if d := e["data"]; d != nil {
			raw, _ := json.Marshal(d)
			return string(raw)
		}
	}
	return "OpenCode error"
}
