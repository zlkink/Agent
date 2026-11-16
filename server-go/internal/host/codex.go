package host

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// runCodexPrompt drives `codex exec --json` for one turn. Codex has no
// interactive permission callback in exec mode, so the sandbox (read-only or
// workspace-write) is the gate; Prometheus still records the tool cards.
func (h *Host) runCodexPrompt(s *Session, text string) {
	if h.cfg.Codex.Path == "" {
		h.finishError(s, fmt.Errorf("Codex is not installed"))
		return
	}
	s.mu.Lock()
	history := make([]map[string]any, len(s.messages))
	copy(history, s.messages)
	s.mu.Unlock()

	now := time.Now().UnixMilli()
	user := map[string]any{"role": "user", "content": text, "timestamp": now}
	s.appendMessage(user)
	h.event(s.ID, map[string]any{"type": "agent_start"})
	h.event(s.ID, map[string]any{"type": "turn_start"})
	h.event(s.ID, map[string]any{"type": "message_start", "message": user})

	assistant := map[string]any{
		"role": "assistant", "content": []any{}, "provider": "codex",
		"model": "default", "usage": zeroUsage(), "stopReason": "pending",
		"timestamp": time.Now().UnixMilli(),
	}
	h.event(s.ID, map[string]any{"type": "message_start", "message": assistant})

	sandbox := "read-only"
	if h.cfg.AllowWrite {
		sandbox = "workspace-write"
	}
	args := []string{
		"exec", "--json", "--color", "never", "--skip-git-repo-check", "--ephemeral",
		"-C", s.Cwd, "-s", sandbox, "-c", `approval_policy="never"`,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.cfg.Codex.Path, args...)
	cmd.Dir = s.Cwd
	staticCtx, turnCtx := h.pluginContext(text, s)
	cmd.Stdin = strings.NewReader(codexPromptWithHistory(history, withContext(staticCtx, turnCtx, text)))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		h.finishError(s, err)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		h.finishError(s, err)
		return
	}
	if err := cmd.Start(); err != nil {
		h.finishError(s, err)
		return
	}
	var stderrText strings.Builder
	stderrDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(&stderrText, stderr)
		close(stderrDone)
	}()

	var textBuf strings.Builder
	var usage map[string]any
	var runErr error
	toolNames := map[string]string{}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev struct {
			Type  string         `json:"type"`
			Item  codexItem      `json:"item"`
			Usage map[string]any `json:"usage"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
			Message string `json:"message"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		item := ev.Item
		switch ev.Type {
		case "item.started":
			switch item.Type {
			case "command_execution":
				toolNames[item.ID] = "bash"
				h.event(s.ID, map[string]any{
					"type": "tool_execution_start", "toolCallId": item.ID,
					"toolName": "bash", "args": map[string]any{"command": item.Command},
				})
			case "file_change":
				toolNames[item.ID] = "edit"
				h.event(s.ID, map[string]any{
					"type": "tool_execution_start", "toolCallId": item.ID,
					"toolName": "edit", "args": map[string]any{"changes": item.Changes},
				})
			}
		case "item.completed":
			switch item.Type {
			case "agent_message":
				if strings.TrimSpace(item.Text) != "" {
					textBuf.WriteString(item.Text)
					h.event(s.ID, map[string]any{
						"type":  "message_update",
						"delta": map[string]any{"type": "text_delta", "contentIndex": 0, "delta": item.Text},
					})
				}
			case "reasoning":
				if strings.TrimSpace(item.Text) != "" {
					h.event(s.ID, map[string]any{
						"type":  "message_update",
						"delta": map[string]any{"type": "thinking_delta", "contentIndex": 0, "delta": item.Text},
					})
				}
			case "command_execution":
				isErr := item.ExitCode != nil && *item.ExitCode != 0
				h.event(s.ID, map[string]any{
					"type": "tool_execution_end", "toolCallId": item.ID, "toolName": "bash",
					"result": map[string]any{"output": item.AggregatedOutput}, "isError": isErr,
				})
			case "file_change":
				h.event(s.ID, map[string]any{
					"type": "tool_execution_end", "toolCallId": item.ID, "toolName": "edit",
					"result": map[string]any{"changes": item.Changes}, "isError": false,
				})
			}
		case "turn.completed":
			usage = ev.Usage
		case "turn.failed":
			runErr = fmt.Errorf("%s", firstNonEmpty(ev.Error.Message, "Codex turn failed"))
		case "error":
			runErr = fmt.Errorf("%s", firstNonEmpty(ev.Message, "Codex error"))
		}
	}
	waitErr := cmd.Wait()
	<-stderrDone
	if runErr == nil && waitErr != nil {
		if ctx.Err() != nil {
			runErr = fmt.Errorf("Codex timed out")
		} else {
			msg := strings.TrimSpace(stderrText.String())
			if msg == "" {
				msg = waitErr.Error()
			}
			runErr = fmt.Errorf("%s", lastLine(msg))
		}
	}

	content := []any{}
	if textBuf.Len() > 0 {
		content = append(content, map[string]any{"type": "text", "text": textBuf.String()})
	}
	stop := "stop"
	if runErr != nil {
		stop = "error"
	}
	final := map[string]any{
		"role": "assistant", "content": content, "provider": "codex",
		"model": "default", "usage": normalizeUsage(usage), "stopReason": stop,
		"timestamp": time.Now().UnixMilli(),
	}
	if runErr != nil {
		final["errorMessage"] = runErr.Error()
	}
	s.appendMessage(final)
	h.event(s.ID, map[string]any{"type": "message_end", "message": final})
	h.event(s.ID, map[string]any{"type": "turn_end"})
	h.event(s.ID, map[string]any{"type": "agent_settled", "aborted": false})

	s.mu.Lock()
	s.usage.Input += number(usage["input_tokens"])
	s.usage.Output += number(usage["output_tokens"])
	s.usage.CacheRead += number(usage["cache_read_input_tokens"])
	s.usage.CacheWrite += number(usage["cache_creation_input_tokens"])
	s.mu.Unlock()
	h.endRun(s)
}

type codexItem struct {
	ID               string `json:"id"`
	Type             string `json:"type"`
	Text             string `json:"text"`
	Command          string `json:"command"`
	AggregatedOutput string `json:"aggregated_output"`
	ExitCode         *int   `json:"exit_code"`
	Changes          any    `json:"changes"`
}

func codexPromptWithHistory(history []map[string]any, text string) string {
	var b strings.Builder
	start := 0
	if len(history) > 10 {
		start = len(history) - 10
	}
	if start < len(history) {
		b.WriteString("Conversation so far:\n")
		for _, m := range history[start:] {
			role := str(m["role"])
			if role != "user" && role != "assistant" {
				continue
			}
			line := messageText(m["content"])
			if line == "" {
				continue
			}
			if len(line) > 4000 {
				line = line[:4000]
			}
			b.WriteString(role + ": " + line + "\n")
		}
		b.WriteString("\nNew message:\n")
	}
	b.WriteString(text)
	return b.String()
}

func messageText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, raw := range v {
			block, _ := raw.(map[string]any)
			if block == nil {
				continue
			}
			if str(block["type"]) == "text" {
				b.WriteString(str(block["text"]))
			}
		}
		return b.String()
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) == 0 {
		return s
	}
	return strings.TrimSpace(lines[len(lines)-1])
}
