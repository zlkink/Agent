// Package sidecar drives the Node agent-host over newline-delimited JSON-RPC.
// The sidecar owns the official Claude Agent SDK; the Go host only sees neutral
// events, so SDK changes stay inside the sidecar.
package sidecar

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
	"sync/atomic"
	"time"
)

type Client struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	events   chan json.RawMessage
	closed   chan struct{}
	onStderr func(string)
	seq      atomic.Int64
	once     sync.Once
}

type StartOptions struct {
	Node       string
	Script     string
	Dir        string
	Env        map[string]string
	OnStderr   func(string)
	EventQueue int
}

func Start(o StartOptions) (*Client, error) {
	if o.Node == "" {
		return nil, errors.New("node executable is empty")
	}
	if o.EventQueue <= 0 {
		o.EventQueue = 256
	}
	cmd := exec.Command(o.Node, o.Script)
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
		cmd:      cmd,
		stdin:    stdin,
		events:   make(chan json.RawMessage, o.EventQueue),
		closed:   make(chan struct{}),
		onStderr: o.OnStderr,
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
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		if msg.Method == "event" && len(msg.Params) > 0 {
			select {
			case c.events <- append(json.RawMessage(nil), msg.Params...):
			case <-c.closed:
				return
			}
		}
	}
}

func (c *Client) readStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		if c.onStderr != nil {
			c.onStderr(sc.Text())
		}
	}
}

// Prompt sends one user message and calls onEvent for every neutral event until
// the sidecar emits `done` or `error`. turnContext is injected through the
// sidecar's UserPromptSubmit hook.
func (c *Client) Prompt(ctx context.Context, text, turnContext string, onEvent func(json.RawMessage) error) error {
	return c.PromptWithImages(ctx, text, turnContext, nil, onEvent)
}

func (c *Client) PromptWithImages(ctx context.Context, text, turnContext string, images []any, onEvent func(json.RawMessage) error) error {
	id := c.seq.Add(1)
	params := map[string]any{"text": text}
	if turnContext != "" {
		params["context"] = turnContext
	}
	if len(images) > 0 {
		params["images"] = images
	}
	line, err := json.Marshal(map[string]any{
		"id":     id,
		"method": "prompt",
		"params": params,
	})
	if err != nil {
		return err
	}
	if _, err := c.stdin.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("sidecar stdin: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			_ = c.Interrupt()
			return ctx.Err()
		case <-c.closed:
			return errors.New("agent sidecar exited")
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
			switch ev.Type {
			case "done":
				return nil
			case "error":
				var e struct {
					Message string `json:"message"`
				}
				_ = json.Unmarshal(raw, &e)
				return errors.New(e.Message)
			}
		}
	}
}

func (c *Client) Interrupt() error {
	line, _ := json.Marshal(map[string]any{"method": "abort"})
	_, err := c.stdin.Write(append(line, '\n'))
	return err
}

// SetModel switches the running Claude Code session to another model served by
// the same gateway. Best effort: older sidecars ignore the control message.
func (c *Client) SetModel(model string) error {
	if model == "" {
		return nil
	}
	line, _ := json.Marshal(map[string]any{"method": "set_model", "params": map[string]any{"model": model}})
	_, err := c.stdin.Write(append(line, '\n'))
	return err
}

// RespondPermission answers a permission_request event emitted by the sidecar.
func (c *Client) RespondPermission(id string, allow bool) error {
	decision := "deny"
	if allow {
		decision = "allow"
	}
	line, _ := json.Marshal(map[string]any{"method": "permission", "id": id, "decision": decision})
	_, err := c.stdin.Write(append(line, '\n'))
	return err
}

func (c *Client) Close() {
	c.once.Do(func() {
		line, _ := json.Marshal(map[string]any{"method": "shutdown"})
		_, _ = c.stdin.Write(append(line, '\n'))
		_ = c.stdin.Close()
		select {
		case <-c.closed:
		case <-time.After(3 * time.Second):
			killTree(c.cmd)
		}
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
	})
}

func killTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := strconv.Itoa(cmd.Process.Pid)
	if runtime.GOOS == "windows" {
		_ = exec.Command("taskkill", "/PID", pid, "/T", "/F").Run()
		return
	}
	_ = cmd.Process.Kill()
}

func envSlice(over map[string]string) []string {
	merged := map[string]string{}
	for _, kv := range os.Environ() {
		if i := indexByte(kv, '='); i > 0 {
			merged[kv[:i]] = kv[i+1:]
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

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// Drain discards events that arrived after the host stopped reading a prompt
// (for example the tail of an interrupted turn). Without it, the next prompt
// could consume a stale done and finish without doing anything.
func (c *Client) Drain() {
	for {
		select {
		case <-c.events:
		default:
			return
		}
	}
}
