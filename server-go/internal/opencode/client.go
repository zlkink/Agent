// Package opencode drives a local `opencode serve` process over its REST API and
// /event SSE stream.
package opencode

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Client struct {
	cmd    *exec.Cmd
	base   string
	auth   string
	client *http.Client
	closed chan struct{}
	once   sync.Once
}

func Start(ctx context.Context, bin, cwd string) (*Client, error) {
	if bin == "" {
		return nil, errors.New("opencode executable is empty")
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	raw := make([]byte, 24)
	_, _ = rand.Read(raw)
	password := base64.RawURLEncoding.EncodeToString(raw)
	cmd := exec.Command(bin, "serve", "--port", strconv.Itoa(port), "--hostname", "127.0.0.1")
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "OPENCODE_SERVER_PASSWORD="+password)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &Client{
		cmd:    cmd,
		auth:   "Basic " + base64.StdEncoding.EncodeToString([]byte("opencode:"+password)),
		client: &http.Client{Transport: &http.Transport{Proxy: nil}},
		closed: make(chan struct{}),
	}
	ready := make(chan string, 1)
	go scanURL(stdout, ready)
	go scanURL(stderr, ready)
	go func() {
		_ = cmd.Wait()
		close(c.closed)
	}()
	select {
	case <-ctx.Done():
		c.Close()
		return nil, ctx.Err()
	case <-time.After(45 * time.Second):
		c.Close()
		return nil, fmt.Errorf("opencode server did not report a URL")
	case base := <-ready:
		c.base = strings.TrimRight(base, "/")
		return c, nil
	}
}

func scanURL(r io.Reader, ready chan<- string) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.Index(line, "http://127.0.0.1:"); i >= 0 {
			s := line[i:]
			if j := strings.IndexAny(s, " \t"); j >= 0 {
				s = s[:j]
			}
			select {
			case ready <- s:
			default:
			}
		}
	}
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func (c *Client) Request(ctx context.Context, method, path, cwd string, body any) (map[string]any, error) {
	u, err := url.Parse(c.base + path)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("directory", cwd)
	u.RawQuery = q.Encode()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = strings.NewReader(string(raw))
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", c.auth)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	var out map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		msg := strings.TrimSpace(string(raw))
		if out != nil {
			if m, ok := out["message"].(string); ok && m != "" {
				msg = m
			}
		}
		return nil, fmt.Errorf("%s %s: HTTP %d %s", method, path, res.StatusCode, msg)
	}
	return out, nil
}

// Events reads the SSE stream until ctx is cancelled.
func (c *Client) Events(ctx context.Context, cwd string, onEvent func(typ string, props map[string]any)) error {
	u, err := url.Parse(c.base + "/event")
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("directory", cwd)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.auth)
	req.Header.Set("Accept", "text/event-stream")
	res, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("event stream: HTTP %d", res.StatusCode)
	}
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if raw == "" {
			continue
		}
		var ev struct {
			Type       string         `json:"type"`
			Properties map[string]any `json:"properties"`
		}
		if json.Unmarshal([]byte(raw), &ev) != nil {
			continue
		}
		onEvent(ev.Type, ev.Properties)
	}
	return sc.Err()
}

func (c *Client) Close() {
	c.once.Do(func() {
		if c.cmd.Process != nil {
			if runtime.GOOS == "windows" {
				_ = exec.Command("taskkill", "/PID", strconv.Itoa(c.cmd.Process.Pid), "/T", "/F").Run()
			} else {
				_ = c.cmd.Process.Kill()
			}
		}
	})
}
