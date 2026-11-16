// Package pi drives PI's own `--mode rpc` JSONL protocol. PI remains the Node
// process; the Go host sends commands and forwards the session events.
package pi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"time"
)

type Client struct {
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	events      chan json.RawMessage
	closed      chan struct{}
	mu          sync.Mutex
	seq         int64
	pending     map[string]chan Response
	onStderr    func(string)
	once        sync.Once
	autoApprove bool
}

type Response struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Command string          `json:"command"`
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   string          `json:"error"`
}

type StartOptions struct {
	Node        string
	Entry       string
	Dir         string
	Args        []string
	Env         map[string]string
	AutoApprove bool
	OnStderr    func(string)
}

func Start(o StartOptions) (*Client, error) {
	if o.Node == "" || o.Entry == "" {
		return nil, errors.New("PI node/entry is empty")
	}
	args := append([]string{o.Entry}, o.Args...)
	cmd := exec.Command(o.Node, args...)
	cmd.Dir = o.Dir
	cmd.Env = envSlice(o.Env)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &Client{
		cmd:         cmd,
		stdin:       stdin,
		events:      make(chan json.RawMessage, 4096),
		closed:      make(chan struct{}),
		pending:     map[string]chan Response{},
		onStderr:    o.OnStderr,
		autoApprove: o.AutoApprove,
	}
	go c.readStdout(stdout)
	go c.readStderr(stderr)
	go func() {
		_ = cmd.Wait()
		close(c.closed)
	}()
	return c, nil
}

func (c *Client) readStdout(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 32<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var head struct {
			Type   string `json:"type"`
			ID     string `json:"id"`
			Method string `json:"method"`
		}
		if json.Unmarshal(line, &head) != nil {
			continue
		}
		if head.Type == "response" {
			var resp Response
			if json.Unmarshal(line, &resp) == nil {
				c.mu.Lock()
				ch := c.pending[resp.ID]
				delete(c.pending, resp.ID)
				c.mu.Unlock()
				if ch != nil {
					select {
					case ch <- resp:
					default:
					}
				}
			}
			continue
		}
		if head.Type == "extension_ui_request" {
			// Keep PI from blocking on a dialog the POC has no UI for.
			c.respondExtension(head.ID, head.Method)
			continue
		}
		select {
		case c.events <- append(json.RawMessage(nil), line...):
		case <-c.closed:
			return
		}
	}
}

func (c *Client) readStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		if c.onStderr != nil {
			c.onStderr(sc.Text())
		}
	}
}

func (c *Client) respondExtension(id, method string) {
	if id == "" {
		return
	}
	switch method {
	case "notify", "setStatus", "setTitle", "setWidget", "set_editor_text":
		// Fire-and-forget in PI; no response expected.
		return
	}
	payload := map[string]any{"type": "extension_ui_response", "id": id, "cancelled": true}
	if c.autoApprove && method == "confirm" {
		payload = map[string]any{"type": "extension_ui_response", "id": id, "confirmed": true}
	}
	line, _ := json.Marshal(payload)
	_, _ = c.stdin.Write(append(line, '\n'))
}

// Request sends one PI RPC command and waits for its response.
func (c *Client) Request(ctx context.Context, typ string, fields map[string]any) (json.RawMessage, error) {
	c.mu.Lock()
	c.seq++
	id := "req_" + strconv.FormatInt(c.seq, 10)
	ch := make(chan Response, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	payload := map[string]any{"id": id, "type": typ}
	for k, v := range fields {
		payload[k] = v
	}
	line, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if _, err := c.stdin.Write(append(line, '\n')); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.closed:
		return nil, errors.New("PI process exited")
	case resp := <-ch:
		if !resp.Success {
			return nil, errors.New(firstNonEmpty(resp.Error, "PI command failed"))
		}
		return resp.Data, nil
	}
}

// Prompt runs one turn and calls onEvent for every PI session event until
// `agent_settled`.
func (c *Client) Prompt(ctx context.Context, text string, onEvent func(json.RawMessage) error) error {
	if _, err := c.Request(ctx, "prompt", map[string]any{"message": text}); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			_ = c.Abort()
			return ctx.Err()
		case <-c.closed:
			return errors.New("PI process exited")
		case raw := <-c.events:
			if onEvent != nil {
				if err := onEvent(raw); err != nil {
					return err
				}
			}
			var ev struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(raw, &ev)
			if ev.Type == "agent_settled" {
				return nil
			}
		}
	}
}

func (c *Client) Abort() error {
	line, _ := json.Marshal(map[string]any{"id": "abort", "type": "abort"})
	_, err := c.stdin.Write(append(line, '\n'))
	return err
}

func (c *Client) GetMessages(ctx context.Context) ([]map[string]any, error) {
	raw, err := c.Request(ctx, "get_messages", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out.Messages, nil
}

func (c *Client) GetStats(ctx context.Context) (map[string]any, error) {
	raw, err := c.Request(ctx, "get_session_stats", nil)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out, nil
}

func (c *Client) GetAvailableModels(ctx context.Context) ([]any, error) {
	raw, err := c.Request(ctx, "get_available_models", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Models []any `json:"models"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out.Models, nil
}

func (c *Client) Close() {
	c.once.Do(func() {
		_ = c.stdin.Close()
		select {
		case <-c.closed:
		case <-time.After(2 * time.Second):
			if c.cmd.Process != nil {
				if runtime.GOOS == "windows" {
					_ = exec.Command("taskkill", "/PID", strconv.Itoa(c.cmd.Process.Pid), "/T", "/F").Run()
				} else {
					_ = c.cmd.Process.Kill()
				}
			}
		}
	})
}

func envSlice(over map[string]string) []string {
	merged := map[string]string{}
	for _, kv := range os.Environ() {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				merged[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	for k, v := range over {
		if v == "" {
			delete(merged, k)
		} else {
			merged[k] = v
		}
	}
	out := make([]string, 0, len(merged))
	for k, v := range merged {
		out = append(out, k+"="+v)
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

var _ = fmt.Sprintf
