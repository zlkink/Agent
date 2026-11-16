package host

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// runSubagent is the `subagent` MCP tool: a bounded one-shot Claude Code or
// Codex run in the session's directory.
func (h *Host) runSubagent(_ context.Context, cwd, agent, task string) (string, error) {
	if strings.TrimSpace(task) == "" {
		return "", fmt.Errorf("task is empty")
	}
	writable := strings.HasSuffix(agent, "-writer")
	if strings.HasPrefix(agent, "codex") {
		return h.runCodexSubagent(cwd, writable, task)
	}
	return h.runClaudeSubagent(cwd, writable, task)
}

func (h *Host) subagentEnv() []string {
	blocked := map[string]bool{
		"CLAUDE_CODE_SESSION_ID": true, "CLAUDE_CODE_ENTRYPOINT": true,
		"CLAUDE_CODE_SSE_PORT": true, "CLAUDE_CODE_CHILD_SESSION": true,
		"CLAUDE_PID": true, "CLAUDECODE": true, "CLAUDE_EFFORT": true,
		"ANTHROPIC_BASE_URL": true, "ANTHROPIC_AUTH_TOKEN": true,
		"ANTHROPIC_MODEL": true, "ANTHROPIC_SMALL_FAST_MODEL": true,
	}
	out := make([]string, 0, len(os.Environ())+6)
	for _, kv := range os.Environ() {
		key := kv
		if i := strings.Index(kv, "="); i > 0 {
			key = kv[:i]
		}
		if blocked[key] {
			continue
		}
		out = append(out, kv)
	}
	if h.cfg.Gateway != "native" && h.cfg.RelayURL != "" {
		out = append(out,
			"ANTHROPIC_BASE_URL="+h.cfg.RelayURL,
			"ANTHROPIC_AUTH_TOKEN="+h.cfg.RelayToken,
			"ANTHROPIC_MODEL="+h.cfg.Model,
			"ANTHROPIC_SMALL_FAST_MODEL="+h.cfg.Model,
			"ANTHROPIC_API_KEY=",
		)
	}
	return out
}

func (h *Host) runClaudeSubagent(cwd string, writable bool, task string) (string, error) {
	if h.cfg.Claude.Path == "" {
		return "", fmt.Errorf("Claude Code is not installed")
	}
	mode := "plan"
	tools := "Read,Grep,Glob"
	if writable {
		mode = "acceptEdits"
		tools = "Read,Grep,Glob,Edit,Write,Bash"
	}
	args := []string{
		"-p", "--input-format", "text", "--output-format", "stream-json",
		"--verbose", "--no-session-persistence", "--disable-slash-commands",
		"--permission-mode", mode, "--tools", tools, "--allowedTools", tools,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.cfg.Claude.Path, args...)
	cmd.Dir = cwd
	cmd.Env = h.subagentEnv()
	cmd.Stdin = strings.NewReader(task)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return "", err
	}
	var errText strings.Builder
	go func() {
		if stderr != nil {
			sc := bufio.NewScanner(stderr)
			for sc.Scan() {
				errText.WriteString(sc.Text() + "\n")
			}
		}
	}()
	var text strings.Builder
	var runErr error
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), 32<<20)
	for sc.Scan() {
		var ev struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
			Result  string `json:"result"`
			IsError bool   `json:"is_error"`
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "assistant":
			for _, b := range ev.Message.Content {
				if b.Type == "text" {
					text.WriteString(b.Text)
				}
			}
		case "result":
			if ev.Result != "" {
				text.Reset()
				text.WriteString(ev.Result)
			}
			if ev.IsError || (ev.Subtype != "" && ev.Subtype != "success") {
				runErr = fmt.Errorf("%s", firstNonEmpty(ev.Result, ev.Subtype, "Claude subagent failed"))
			}
		}
	}
	waitErr := cmd.Wait()
	if runErr == nil && waitErr != nil {
		runErr = fmt.Errorf("%s", firstNonEmpty(lastLine(errText.String()), waitErr.Error()))
	}
	if runErr != nil {
		return "", runErr
	}
	out := strings.TrimSpace(text.String())
	if out == "" {
		return "", fmt.Errorf("Claude subagent returned no text")
	}
	return out, nil
}

func (h *Host) runCodexSubagent(cwd string, writable bool, task string) (string, error) {
	if h.cfg.Codex.Path == "" {
		return "", fmt.Errorf("Codex is not installed")
	}
	sandbox := "read-only"
	if writable {
		sandbox = "workspace-write"
	}
	args := []string{
		"exec", "--json", "--color", "never", "--skip-git-repo-check", "--ephemeral",
		"-C", cwd, "-s", sandbox, "-c", `approval_policy="never"`,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.cfg.Codex.Path, args...)
	cmd.Dir = cwd
	cmd.Stdin = strings.NewReader(task)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return "", err
	}
	var errText strings.Builder
	go func() {
		if stderr != nil {
			sc := bufio.NewScanner(stderr)
			for sc.Scan() {
				errText.WriteString(sc.Text() + "\n")
			}
		}
	}()
	var text strings.Builder
	var runErr error
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), 32<<20)
	for sc.Scan() {
		var ev struct {
			Type string `json:"type"`
			Item struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
			Message string `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "item.completed":
			if ev.Item.Type == "agent_message" && strings.TrimSpace(ev.Item.Text) != "" {
				text.WriteString(ev.Item.Text)
			}
		case "turn.failed":
			runErr = fmt.Errorf("%s", firstNonEmpty(ev.Error.Message, "Codex subagent failed"))
		case "error":
			runErr = fmt.Errorf("%s", firstNonEmpty(ev.Message, "Codex subagent failed"))
		}
	}
	waitErr := cmd.Wait()
	if runErr == nil && waitErr != nil {
		runErr = fmt.Errorf("%s", firstNonEmpty(lastLine(errText.String()), waitErr.Error()))
	}
	if runErr != nil {
		return "", runErr
	}
	out := strings.TrimSpace(text.String())
	if out == "" {
		return "", fmt.Errorf("Codex subagent returned no text")
	}
	return out, nil
}
