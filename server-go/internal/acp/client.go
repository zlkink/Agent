// Package acp is a small Agent Client Protocol client: JSON-RPC 2.0 over the
// agent's stdio. The Go host uses it to drive claude-code-acp and similar agents.
package acp

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
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	mu         sync.Mutex
	seq        int64
	pending    map[int64]chan response
	closed     chan struct{}
	onNotify   func(method string, params json.RawMessage)
	onRequest  func(ctx context.Context, method string, params json.RawMessage) (any, error)
	onExit     func(code int, stderr string)
	stderrTail string
	closeOnce  sync.Once
}

type response struct {
	Result json.RawMessage
	Error  string
}

type StartOptions struct {
	Node      string
	Entry     string
	Dir       string
	Env       map[string]string
	OnNotify  func(method string, params json.RawMessage)
	OnRequest func(ctx context.Context, method string, params json.RawMessage) (any, error)
	OnExit    func(code int, stderr string)
}

func Start(o StartOptions) (*Client, error) {
	if o.Node == "" || o.Entry == "" {
		return nil, errors.New("ACP node/entry is empty")
	}
	cmd := exec.Command(o.Node, o.Entry)
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
		cmd: cmd, stdin: stdin, pending: map[int64]chan response{},
		closed: make(chan struct{}), onNotify: o.OnNotify, onRequest: o.OnRequest, onExit: o.OnExit,
	}
	go c.readStdout(stdout)
	go c.readStderr(stderr)
	go func() {
		err := cmd.Wait()
		code := 0
		if err != nil {
			code = -1
		}
		close(c.closed)
		if c.onExit != nil {
			c.onExit(code, c.stderrTail)
		}
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
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(line, &msg) != nil {
			continue
		}
		if len(msg.ID) > 0 && msg.Method == "" {
			id := parseID(msg.ID)
			c.mu.Lock()
			ch := c.pending[id]
			delete(c.pending, id)
			c.mu.Unlock()
			if ch != nil {
				if msg.Error != nil {
					ch <- response{Error: msg.Error.Message}
				} else {
					ch <- response{Result: msg.Result}
				}
			}
			continue
		}
		if msg.Method != "" && len(msg.ID) > 0 {
			go c.handleRequest(msg.ID, msg.Method, msg.Params)
			continue
		}
		if msg.Method != "" && c.onNotify != nil {
			c.onNotify(msg.Method, msg.Params)
		}
	}
}

func (c *Client) handleRequest(id json.RawMessage, method string, params json.RawMessage) {
	var result any
	var err error
	if c.onRequest != nil {
		result, err = c.onRequest(context.Background(), method, params)
	} else {
		err = fmt.Errorf("method not found: %s", method)
	}
	if err != nil {
		c.write(map[string]any{
			"jsonrpc": "2.0", "id": rawID(id),
			"error": map[string]any{"code": -32601, "message": err.Error()},
		})
		return
	}
	c.write(map[string]any{"jsonrpc": "2.0", "id": rawID(id), "result": result})
}

func (c *Client) readStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		c.mu.Lock()
		c.stderrTail = (c.stderrTail + sc.Text() + "\n")
		if len(c.stderrTail) > 8000 {
			c.stderrTail = c.stderrTail[len(c.stderrTail)-8000:]
		}
		c.mu.Unlock()
	}
}

func (c *Client) Request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := atomic.AddInt64(&c.seq, 1)
	ch := make(chan response, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.closed:
		return nil, errors.New("ACP agent exited")
	case resp := <-ch:
		if resp.Error != "" {
			return nil, errors.New(resp.Error)
		}
		return resp.Result, nil
	}
}

func (c *Client) Notify(method string, params any) error {
	return c.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *Client) write(v any) error {
	line, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = c.stdin.Write(append(line, '\n'))
	return err
}

func (c *Client) Close() {
	c.closeOnce.Do(func() {
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

func parseID(raw json.RawMessage) int64 {
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		n, _ = strconv.ParseInt(s, 10, 64)
	}
	return n
}

func rawID(raw json.RawMessage) any {
	var v any
	if json.Unmarshal(raw, &v) == nil {
		return v
	}
	return nil
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
