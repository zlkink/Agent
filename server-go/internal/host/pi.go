package host

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"prometheus-go/internal/mcpconfig"
	piclient "prometheus-go/internal/pi"
)

// runPiPrompt drives PI's own RPC mode. PI emits the same SessionEvent shape the
// rest of Prometheus uses, so the events are forwarded almost unchanged; after
// the turn the host asks PI for the canonical message list.
func (h *Host) runPiPrompt(s *Session, text string) {
	s.mu.Lock()
	c := s.pi
	modelID := s.ModelID
	if modelID == "" {
		modelID = h.cfg.Model
	}
	s.mu.Unlock()
	if c == nil {
		env := map[string]string{}
		args := []string{}
		agentDir := filepath.Join(h.cfg.DataDir, "pi-agent")
		_ = os.MkdirAll(agentDir, 0o755)
		env["PI_CODING_AGENT_DIR"] = agentDir
		env["PI_CODING_AGENT_SESSION_DIR"] = filepath.Join(h.cfg.DataDir, "pi-sessions")
		if h.bus != nil {
			mcpURL, mcpToken := h.bus.Open(s.ID, s.Cwd)
			_ = writePIMcp(agentDir, mcpURL, mcpToken, mcpconfig.Load(h.cfg.DataDir))
		}
		// The host's own Claude session credentials must not leak into PI.
		env["ANTHROPIC_BASE_URL"] = ""
		env["ANTHROPIC_AUTH_TOKEN"] = ""
		env["ANTHROPIC_MODEL"] = ""
		env["ANTHROPIC_SMALL_FAST_MODEL"] = ""
		env["ANTHROPIC_API_KEY"] = ""
		if h.cfg.Gateway != "" && h.cfg.Gateway != "native" && h.cfg.RelayURL != "" {
			models := h.gatewayModelList()
			if len(models) == 0 {
				models = h.modelList()
			}
			_ = writePIModels(agentDir, h.cfg.RelayURL, h.cfg.RelayToken, h.cfg.Gateway, models)
			args = append(args, "--provider", h.cfg.Gateway, "--model", modelID)
		}
		var err error
		c, err = piclient.Start(piclient.StartOptions{
			Node:        h.cfg.PINode,
			Entry:       h.cfg.PIEntry,
			Dir:         s.Cwd,
			Args:        args,
			Env:         env,
			AutoApprove: h.cfg.AllowWrite,
			OnStderr: func(line string) {
				// PI writes diagnostics to stderr; keep them out of the chat.
			},
		})
		if err != nil {
			h.finishError(s, fmt.Errorf("start PI: %w", err))
			return
		}
		s.mu.Lock()
		s.pi = c
		s.mu.Unlock()
	}

	ctx, cancel := context.WithTimeout(context.Background(), promptTimeout)
	defer cancel()
	settled := false
	staticCtx, turnCtx := h.pluginContext(text, s)
	err := c.Prompt(ctx, withContext(staticCtx, turnCtx, text), func(raw json.RawMessage) error {
		var ev map[string]any
		if json.Unmarshal(raw, &ev) != nil {
			return nil
		}
		switch str(ev["type"]) {
		case "extension_error":
			return nil
		case "agent_end":
			return nil
		case "message_update":
			delta := ev["assistantMessageEvent"]
			if delta == nil {
				return nil
			}
			h.event(s.ID, map[string]any{"type": "message_update", "delta": delta})
			return nil
		case "message_start", "message_end":
			if msg, ok := ev["message"].(map[string]any); ok {
				if str(msg["role"]) == "system" {
					return nil
				}
			}
		case "agent_settled":
			settled = true
		}
		h.event(s.ID, ev)
		return nil
	})

	readCtx, readCancel := context.WithTimeout(context.Background(), 15*time.Second)
	if msgs, err2 := c.GetMessages(readCtx); err2 == nil && len(msgs) > 0 {
		s.mu.Lock()
		s.messages = msgs
		s.mu.Unlock()
	}
	if stats, err2 := c.GetStats(readCtx); err2 == nil {
		in, out, cr, cw := usageFromStats(stats)
		s.mu.Lock()
		s.usage.Input += in
		s.usage.Output += out
		s.usage.CacheRead += cr
		s.usage.CacheWrite += cw
		s.mu.Unlock()
	}
	readCancel()

	if !settled {
		h.event(s.ID, map[string]any{"type": "agent_settled", "aborted": false})
	}
	if err != nil && ctx.Err() == nil {
		h.event(s.ID, map[string]any{"type": "auto_retry_end", "success": false, "finalError": err.Error()})
	}
	h.endRun(s)
}

func usageFromStats(stats map[string]any) (in, out, cr, cw int) {
	pick := func(m map[string]any) {
		in += firstNum(m, "inputTokens", "input_tokens")
		out += firstNum(m, "outputTokens", "output_tokens")
		cr += firstNum(m, "cacheReadTokens", "cache_read_input_tokens")
		cw += firstNum(m, "cacheWriteTokens", "cache_creation_input_tokens")
	}
	pick(stats)
	for _, key := range []string{"usage", "tokens", "totals"} {
		if m, ok := stats[key].(map[string]any); ok {
			pick(m)
		}
	}
	return
}

func firstNum(m map[string]any, keys ...string) int {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			return number(v)
		}
	}
	return 0
}

// writePIModels points PI at the Go relay through a per-run models.json, so the
// real gateway key never reaches the PI process.
// writePIMcp exposes the same Go tool bus to PI through its mcp.json.
func writePIMcp(agentDir, busURL, busToken string, extra map[string]mcpconfig.Server) error {
	servers := map[string]any{}
	if busURL != "" {
		servers["prometheus"] = map[string]any{
			"url":         busURL,
			"headers":     map[string]any{"Authorization": "Bearer " + busToken},
			"description": "RamiCore tools: todo, memory and open_file",
		}
	}
	for name, cfg := range extra {
		servers[name] = piMCPConfig(cfg)
	}
	if len(servers) == 0 {
		return nil
	}
	payload := map[string]any{"mcpServers": servers}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(agentDir, "mcp.json"), append(raw, '\n'), 0o600)
}

func piMCPConfig(cfg mcpconfig.Server) map[string]any {
	cfg = mcpconfig.Expand(cfg).(map[string]any)
	if str(cfg["command"]) != "" {
		out := map[string]any{"command": cfg["command"]}
		if a := cfg["args"]; a != nil {
			out["args"] = a
		}
		if e := cfg["env"]; e != nil {
			out["env"] = e
		}
		return out
	}
	out := map[string]any{"url": cfg["url"]}
	if h := cfg["headers"]; h != nil {
		out["headers"] = h
	}
	return out
}

func writePIModels(agentDir, relayURL, relayToken, provider string, models []any) error {
	if provider == "" {
		provider = "prometheus"
	}
	entries := []any{}
	for _, raw := range models {
		m := asMap(raw)
		id := str(m["id"])
		if id == "" {
			continue
		}
		name := str(m["name"])
		if name == "" {
			name = id
		}
		cw := number(m["contextWindow"])
		if cw <= 0 {
			cw = 200_000
		}
		entries = append(entries, map[string]any{
			"id": id, "name": name, "reasoning": boolDefault(m["reasoning"], true),
			"input": []any{"text", "image"}, "contextWindow": cw,
			"maxTokens": 128_000,
			"cost":      map[string]any{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0},
		})
	}
	payload := map[string]any{
		"providers": map[string]any{
			provider: map[string]any{
				"baseUrl": relayURL,
				"api":     "anthropic-messages",
				"apiKey":  relayToken,
				"models":  entries,
			},
		},
	}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(agentDir, "models.json"), append(raw, '\n'), 0o600)
}
