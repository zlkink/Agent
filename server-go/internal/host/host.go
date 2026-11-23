// Package host implements the Prometheus wire protocol v1 on top of the Go relay
// and the Node agent sidecar. It is deliberately a subset: enough for the real
// web UI to connect, list/open sessions and stream a Claude reply.
package host

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/gorilla/websocket"

	"prometheus-go/internal/acp"
	"prometheus-go/internal/agents"
	"prometheus-go/internal/config"
	"prometheus-go/internal/opencode"
	piclient "prometheus-go/internal/pi"
	"prometheus-go/internal/sidecar"
	"prometheus-go/internal/store"
	"prometheus-go/internal/toolbus"
)

const protocolVersion = 1

// Long tool-driven tasks legitimately run for hours; only a truly stuck turn
// should be cut off.
const promptTimeout = 0 // 0: wait indefinitely; the user can stop the turn with Esc

// titleFromPrompt makes the short sidebar title a new chat gets from its first
// message. It is deliberately local (no extra model call): first line, no
// markdown/list prefix, collapsed whitespace and a 40-character cap.
func titleFromPrompt(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if strings.HasPrefix(text, "<skill name=\"") {
		if rest := text[len("<skill name=\""):]; rest != "" {
			if end := strings.IndexByte(rest, '"'); end > 0 {
				return rest[:end]
			}
		}
	}
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = text[:i]
	}
	text = strings.TrimSpace(strings.TrimLeft(text, "#*-•> "))
	text = strings.Join(strings.Fields(text), " ")
	text = strings.TrimRight(text, "。．.!！?？;；:：,，")
	runes := []rune(text)
	if len(runes) > 40 {
		text = string(runes[:40]) + "…"
	}
	return text
}

// promptContext is the lifetime of one agent turn. By default there is no
// deadline: a slow model may think for a long time, and the user can always
// stop it manually. A positive promptTimeout restores the old behaviour.
func promptContext() (context.Context, context.CancelFunc) {
	if promptTimeout > 0 {
		return context.WithTimeout(context.Background(), promptTimeout)
	}
	return context.WithCancel(context.Background())
}

// hasVisible reports whether s contains anything besides whitespace and the
// zero-width characters some models emit as "thinking" between tool calls.
// Without this, every tool round produced a 1-character 思考过程 block.
func hasVisible(s string) bool {
	for _, r := range s {
		if unicode.IsSpace(r) || r == 0x00a0 || r == 0x200b || r == 0x200c || r == 0x200d || r == 0x2060 || r == 0xfeff {
			continue
		}
		return true
	}
	return false
}

type Config struct {
	Cwd          string
	DataDir      string
	Model        string
	Provider     string
	Gateway      string
	GatewayList  []any
	Claude       agents.Agent
	Codex        agents.Agent
	PINode       string
	PIEntry      string
	PIVersion    string
	ACPNode      string
	ACPEntry     string
	ACPVersion   string
	OpenCodePath string
	OpenCodeVer  string
	RelayURL     string
	RelayToken   string
	PluginURL    string
	Token        string
	AllowWrite   bool
	Offline      bool
	StartSidecar func(sessionID, cwd, mcpURL, mcpToken, staticContext, resume string) (*sidecar.Client, error)
}

type Host struct {
	cfg Config

	store       *store.Store
	bus         *toolbus.Server
	mu          sync.Mutex
	sessions    map[string]*Session
	order       []string
	clients     map[*client]struct{}
	settings    map[string]any
	workspaces  []map[string]any
	tickets     map[string]downloadTicket
	permPending map[string]chan bool
	uiPending   map[string]chan map[string]any
	uiRequests  map[string]pendingUIRequest
	kbBases     []map[string]any
	kbDocs      map[string][]map[string]any
	agents      []map[string]any
	network     map[string]any
	localPort   int
	applyNet    func(lan bool, port int) error
	tunnelProc  *exec.Cmd
	tunnelStop  context.CancelFunc
	tunnelGen   int
}

type downloadTicket struct {
	Path    string
	Name    string
	Expires time.Time
}

type client struct {
	conn    *websocket.Conn
	mu      sync.Mutex
	subs    map[string]bool
	authed  bool
	network bool
}

type Session struct {
	mu             sync.Mutex
	ID             string
	Cwd            string
	Name           string
	Runtime        string
	Provider       string
	ModelID        string
	nativeID       string
	Created        time.Time
	Modified       time.Time
	Archived       bool
	Streaming      bool
	interrupted    bool
	PermissionMode string
	ThinkingLevel  string
	sc             *sidecar.Client
	pi             *piclient.Client
	acpClient      *acp.Client
	acpSession     string
	acpState       *acpPromptState
	ocClient       *opencode.Client
	ocSession      string
	ocState        *ocPromptState
	ocCancel       context.CancelFunc
	messages       []map[string]any
	// liveMessage exposes the assistant message that is still being generated,
	// so opening a running conversation mid-turn does not look empty.
	liveMessage   func() map[string]any
	usage         usageTotals
	goal          map[string]any
	worktree      map[string]any
	contextTokens int
	contextWindow int
	steerQueue    []queuedPrompt
	followUps     []queuedPrompt
	// autoContinueLeft is the budget for "empty turn" continuations, reset on
	// each user prompt. continuation is started by endRun before the queues.
	autoContinueLeft int
	continuation     *queuedPrompt
	compacting       bool
	lastCompact      time.Time
}

type pendingUIRequest struct {
	SessionID string
	Request   map[string]any
}

type queuedPrompt struct {
	Text   string
	Images []any
}

type usageTotals struct {
	Input, Output, Reasoning, CacheRead, CacheWrite int
}

func New(cfg Config) *Host {
	if cfg.Provider == "" {
		cfg.Provider = "claude-code"
	}
	if cfg.DataDir == "" {
		cfg.DataDir = filepath.Join(config.Home(), "go-poc")
	}
	h := &Host{
		cfg:         cfg,
		store:       store.New(cfg.DataDir),
		sessions:    map[string]*Session{},
		clients:     map[*client]struct{}{},
		workspaces:  []map[string]any{},
		tickets:     map[string]downloadTicket{},
		permPending: map[string]chan bool{},
		uiPending:   map[string]chan map[string]any{},
		uiRequests:  map[string]pendingUIRequest{},
		kbDocs:      map[string][]map[string]any{},
		network:     map[string]any{"lan": false, "port": 9528, "allowedHosts": []any{}},
	}
	h.settings = map[string]any{
		"autoCompaction":      true,
		"workspaceRoot":       cfg.Cwd,
		"defaultAgent":        "claude-code",
		"defaultPermission":   "auto",
		"defaultThinking":     "",
		"goalTurns":           30,
		"compactPercent":      80,
		"worktreeMode":        "off",
		"worktreeDefault":     false,
		"worktreeStrategy":    "squash",
		"worktreeAutoPull":    false,
		"recycleDays":         0,
		"webSearchProvider":   "duckduckgo",
		"webSearchMaxResults": 5,
	}
	if bus, err := toolbus.New(filepath.Join(cfg.DataDir, "toolbus"), cfg.PluginURL); err == nil {
		h.bus = bus
		bus.SetSubagent(h.runSubagent)
		bus.SetAsk(h.askQuestion)
		bus.SetKnowledge(func(ctx context.Context, query string, topK int) ([]any, error) {
			return h.searchKnowledge(query, topK)
		})
		bus.SetSearch(h.webSearch)
	}
	if st, err := h.store.Load(); err == nil {
		for k, v := range st.Settings {
			h.settings[k] = v
		}
		h.workspaces = append(h.workspaces, st.Workspaces...)
		for i, w := range h.workspaces {
			if p := str(w["path"]); p != "" {
				h.workspaces[i]["path"] = normalizeDir(p)
			}
		}
		for _, rec := range st.Sessions {
			mode := rec.PermissionMode
			if mode == "" {
				mode = "auto"
			}
			runtimeID := rec.Runtime
			if runtimeID == "" {
				runtimeID = "claude-code"
			}
			messages := rec.Messages
			// A turn was cut off by a hard kill: keep what the model had
			// produced instead of showing only the user's prompt.
			if rec.LiveMessage != nil {
				live := cloneMap(rec.LiveMessage)
				live["stopReason"] = "aborted"
				live["errorMessage"] = "上次运行被中断，已保留部分回复"
				if blocks, ok := live["content"].([]any); ok && len(blocks) > 0 {
					messages = append(append([]map[string]any(nil), messages...), live)
				}
			}
			s := &Session{
				ID: rec.ID, Cwd: rec.Cwd, Name: rec.Name, Runtime: runtimeID,
				Created: rec.Created, Modified: rec.Modified,
				Archived: rec.Archived, PermissionMode: mode, ThinkingLevel: rec.ThinkingLevel,
				Provider: rec.Provider, ModelID: rec.Model, nativeID: rec.AgentSessionID,
				messages: messages,
				usage: usageTotals{
					Input: rec.InputTokens, Output: rec.OutputTokens, Reasoning: rec.ReasoningTokens,
					CacheRead: rec.CacheRead, CacheWrite: rec.CacheWrite,
				},
				worktree: rec.Worktree,
			}
			// Chats created before automatic titles existed still say "New chat";
			// derive their title from the first user message once.
			if s.Name == "" || s.Name == "New chat" {
				for _, m := range messages {
					if str(m["role"]) == "user" {
						if name := titleFromPrompt(messageText(m["content"])); name != "" {
							s.Name = name
						}
						break
					}
				}
			}
			h.sessions[s.ID] = s
			h.order = append(h.order, s.ID)
		}
	}
	h.addWorkspace(cfg.Cwd)
	h.loadNetwork()
	if len(h.order) == 0 {
		h.create(cfg.Cwd, "RamiCore", "claude-code")
	} else {
		h.persist()
	}
	go h.livePersistLoop()
	return h
}

// persist writes a consistent snapshot of sessions, workspaces and settings.
// It is called after structural changes and at the end of each turn.
func (h *Host) persist() {
	if h.store == nil {
		return
	}
	st := &store.State{Version: 1}
	h.mu.Lock()
	st.Settings = cloneMap(h.settings)
	order := append([]string(nil), h.order...)
	sessions := make([]*Session, 0, len(order))
	for _, id := range order {
		if s := h.sessions[id]; s != nil {
			sessions = append(sessions, s)
		}
	}
	st.Workspaces = append([]map[string]any(nil), h.workspaces...)
	h.mu.Unlock()
	for _, s := range sessions {
		s.mu.Lock()
		msgs := make([]map[string]any, len(s.messages))
		copy(msgs, s.messages)
		liveFn := s.liveMessage
		streaming := s.Streaming
		rec := store.Session{
			ID: s.ID, Cwd: s.Cwd, Name: s.Name, Runtime: s.Runtime,
			Created: s.Created, Modified: s.Modified, Archived: s.Archived,
			PermissionMode: s.PermissionMode, ThinkingLevel: s.ThinkingLevel,
			Provider: s.Provider, Model: s.ModelID,
			InputTokens: s.usage.Input, OutputTokens: s.usage.Output, ReasoningTokens: s.usage.Reasoning,
			CacheRead: s.usage.CacheRead, CacheWrite: s.usage.CacheWrite,
			Worktree: s.worktree,
			Messages: msgs,
		}
		s.mu.Unlock()
		// Building the live message locks the runtime state, never s.mu: doing it
		// while holding a lock the event path takes would deadlock.
		if streaming && liveFn != nil {
			rec.LiveMessage = liveFn()
		}
		st.Sessions = append(st.Sessions, rec)
	}
	_ = h.store.Save(st)
}

// livePersistLoop saves in-flight assistant content every few seconds so a
// hard kill (or a forced steer that restarts the runtime) keeps the partial
// reply instead of losing the whole turn.
func (h *Host) livePersistLoop() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for range t.C {
		h.mu.Lock()
		sessions := make([]*Session, 0, len(h.sessions))
		for _, s := range h.sessions {
			sessions = append(sessions, s)
		}
		h.mu.Unlock()
		active := false
		for _, s := range sessions {
			s.mu.Lock()
			if s.Streaming && s.liveMessage != nil {
				active = true
			}
			s.mu.Unlock()
			if active {
				break
			}
		}
		if active {
			h.persist()
		}
	}
}

// SetLocalPort lets /network.get report the real local listener port.
func (h *Host) SetLocalPort(port int) {
	h.mu.Lock()
	h.localPort = port
	h.mu.Unlock()
}

// SetNetworkApply installs the callback that starts/stops the LAN listener.
// Persisted LAN access is brought back up immediately so the setting survives a
// restart of the host.
func (h *Host) SetNetworkApply(fn func(lan bool, port int) error) {
	h.mu.Lock()
	h.applyNet = fn
	lan := h.network["lan"] == true && h.hasNetworkPasswordLocked()
	port := number(h.network["port"])
	h.mu.Unlock()
	if lan {
		_ = fn(true, port)
	}
}

// netTokenTTL matches the browser cookie's lifetime. Tokens are signed with a
// secret kept in network.json, so a restart does not sign every LAN client out.
const netTokenTTL = 30 * 24 * time.Hour

func randomHex(n int) string {
	raw := make([]byte, n)
	_, _ = rand.Read(raw)
	return hex.EncodeToString(raw)
}

// networkSecretLocked returns (creating it when needed) the per-install secret
// used to sign LAN session tokens. Callers must hold h.mu.
func (h *Host) networkSecretLocked() string {
	if s, _ := h.network["sessionSecret"].(string); s != "" {
		return s
	}
	s := randomHex(32)
	h.network["sessionSecret"] = s
	return s
}

// revokeNetworkSessionsLocked signs every existing token out without touching
// the password. Callers must hold h.mu.
func (h *Host) revokeNetworkSessionsLocked() {
	h.network["sessionSecret"] = randomHex(32)
}

// networkTokenKeyLocked binds tokens to both the signing secret and the current
// password hash: changing either one invalidates old sessions.
func (h *Host) networkTokenKeyLocked() []byte {
	saltHex, _ := h.network["passwordSalt"].(string)
	hashHex, _ := h.network["passwordHash"].(string)
	if saltHex == "" || hashHex == "" {
		return nil
	}
	sum := sha256.Sum256([]byte("ramicore-lan-session\x00" + h.networkSecretLocked() + "\x00" + saltHex + "\x00" + hashHex))
	return sum[:]
}

func (h *Host) issueNetworkTokenLocked(now time.Time) string {
	key := h.networkTokenKeyLocked()
	if key == nil {
		return ""
	}
	payload := "v1." + strconv.FormatInt(now.Add(netTokenTTL).Unix(), 10)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(payload))
	return payload + "." + hex.EncodeToString(mac.Sum(nil))
}

func (h *Host) validNetworkTokenLocked(token string) bool {
	key := h.networkTokenKeyLocked()
	if key == nil || token == "" {
		return false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return false
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(parts[0] + "." + parts[1]))
	want := hex.EncodeToString(mac.Sum(nil))
	return subtle.ConstantTimeCompare([]byte(want), []byte(parts[2])) == 1
}

func (h *Host) setNetworkPassword(password string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	// Password changes always sign existing clients out.
	h.revokeNetworkSessionsLocked()
	if password == "" {
		delete(h.network, "passwordSalt")
		delete(h.network, "passwordHash")
		return
	}
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	sum := sha256.Sum256(append(salt, []byte(password)...))
	h.network["passwordSalt"] = hex.EncodeToString(salt)
	h.network["passwordHash"] = hex.EncodeToString(sum[:])
}

// CheckNetworkPassword verifies the LAN password and returns a signed session
// token. The token is stateless: it still verifies after a host restart.
func (h *Host) CheckNetworkPassword(password string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	saltHex, _ := h.network["passwordSalt"].(string)
	hashHex, _ := h.network["passwordHash"].(string)
	if saltHex == "" || hashHex == "" {
		return "", false
	}
	salt, err := hex.DecodeString(saltHex)
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(append(salt, []byte(password)...))
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(hashHex)) != 1 {
		return "", false
	}
	token := h.issueNetworkTokenLocked(time.Now())
	if token == "" {
		return "", false
	}
	return token, true
}

func (h *Host) checkNetworkSession(token string) bool {
	if token == "" {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.validNetworkTokenLocked(token)
}

// CheckNetworkSession is the HTTP side of the LAN session check.
func (h *Host) CheckNetworkSession(token string) bool {
	return h.checkNetworkSession(token)
}

// settingInt reads a persisted chat setting with a default and bounds.
func (h *Host) settingInt(key string, def, min, max int) int {
	h.mu.Lock()
	v, ok := h.settings[key]
	h.mu.Unlock()
	if !ok {
		return def
	}
	n := int(number(v))
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}

func (h *Host) hasNetworkPassword() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	hash, _ := h.network["passwordHash"].(string)
	return hash != ""
}

func (h *Host) DefaultSessionID() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.order) == 0 {
		return ""
	}
	return h.order[0]
}

// ------------------------------------------------------------------ websocket

var upgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true },
}

type clientFrame struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params"`
	Token     string          `json:"token"`
	Password  string          `json:"password"`
	SessionID string          `json:"sessionId"`
	Plugin    string          `json:"plugin"`
	Topic     string          `json:"topic"`
}

func (h *Host) ServeWS(w http.ResponseWriter, r *http.Request) {
	h.serveWS(w, r, false)
}

// ServeWSNetwork is the LAN listener: clients sign in with the network password.
func (h *Host) ServeWSNetwork(w http.ResponseWriter, r *http.Request) {
	h.serveWS(w, r, true)
}

func (h *Host) serveWS(w http.ResponseWriter, r *http.Request, network bool) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	authRequired := network || h.cfg.Token != ""
	c := &client{conn: conn, subs: map[string]bool{}, authed: !authRequired, network: network}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.clients, c)
		h.mu.Unlock()
		_ = conn.Close()
	}()

	login := "token"
	if network {
		login = "password"
	}
	c.send(map[string]any{
		"type": "hello", "protocol": protocolVersion,
		"authRequired": authRequired, "login": login, "local": !network,
	})
	for {
		var f clientFrame
		if err := conn.ReadJSON(&f); err != nil {
			return
		}
		h.handleFrame(c, &f)
	}
}

func (c *client) send(v any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.WriteJSON(v)
}

// closeAuthFailed sends a proper close frame before dropping the socket. A bare
// Close() looks like an abnormal 1006 to browsers, which hides the reason from
// the UI; 4003 tells it to show the sign-in screen again.
func (c *client) closeAuthFailed(reason string) {
	deadline := time.Now().Add(time.Second)
	_ = c.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4003, reason), deadline)
	_ = c.conn.Close()
}

func (h *Host) handleFrame(c *client, f *clientFrame) {
	switch f.Type {
	case "ping":
		c.send(map[string]any{"type": "pong"})
	case "auth":
		if c.network {
			if h.checkNetworkSession(f.Token) {
				c.authed = true
				c.send(map[string]any{"type": "auth_ok"})
				return
			}
		} else if h.cfg.Token == "" || subtle.ConstantTimeCompare([]byte(f.Token), []byte(h.cfg.Token)) == 1 {
			c.authed = true
			c.send(map[string]any{"type": "auth_ok"})
			return
		}
		c.send(map[string]any{"type": "login_failed", "message": "invalid token"})
		c.closeAuthFailed("invalid token")
	case "login":
		if c.network {
			if token, ok := h.CheckNetworkPassword(f.Password); ok {
				c.authed = true
				c.send(map[string]any{"type": "auth_ok", "token": token})
				return
			}
		}
		c.send(map[string]any{"type": "login_failed", "message": "invalid password"})
	case "subscribe", "unsubscribe", "plugin.watch", "plugin.unwatch", "rpc":
		if !c.authed {
			c.send(map[string]any{"type": "error", "message": "authentication required"})
			c.closeAuthFailed("authentication required")
			return
		}
		switch f.Type {
		case "subscribe":
			c.subs[f.SessionID] = true
		case "unsubscribe":
			delete(c.subs, f.SessionID)
		case "plugin.watch", "plugin.unwatch":
			// no plugin topics
		case "rpc":
			data, err := h.dispatch(f.Method, f.Params)
			if err != nil {
				c.send(map[string]any{"type": "response", "id": f.ID, "ok": false, "error": err.Error()})
				return
			}
			c.send(map[string]any{"type": "response", "id": f.ID, "ok": true, "data": data})
		}
	default:
		c.send(map[string]any{"type": "error", "message": "unknown frame: " + f.Type})
	}
}

func (h *Host) event(sessionID string, event any) {
	h.mu.Lock()
	clients := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		if c.subs[sessionID] {
			clients = append(clients, c)
		}
	}
	h.mu.Unlock()
	frame := map[string]any{"type": "event", "sessionId": sessionID, "event": event}
	for _, c := range clients {
		c.send(frame)
	}
}

func (h *Host) broadcast(frame any) {
	h.mu.Lock()
	clients := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.Unlock()
	for _, c := range clients {
		c.send(frame)
	}
}

// ------------------------------------------------------------------ sessions

func (h *Host) create(cwd, name, runtimeID string) *Session {
	cwd = normalizeDir(cwd)
	if cwd == "" {
		cwd = h.cfg.Cwd
	}
	if runtimeID == "" {
		runtimeID = "claude-code"
	}
	now := time.Now()
	h.mu.Lock()
	thinking := str(h.settings["defaultThinking"])
	h.mu.Unlock()
	s := &Session{
		ID:            newID(),
		Cwd:           cwd,
		Name:          name,
		Runtime:       runtimeID,
		Provider:      h.cfg.Provider,
		ModelID:       h.cfg.Model,
		Created:       now,
		Modified:      now,
		ThinkingLevel: thinking,
	}
	h.mu.Lock()
	h.sessions[s.ID] = s
	h.order = append(h.order, s.ID)
	h.mu.Unlock()
	h.addWorkspace(cwd)
	h.persist()
	return s
}

func (h *Host) session(id string) *Session {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessions[id]
}

func (h *Host) addWorkspace(path string) {
	path = normalizeDir(path)
	if path == "" {
		return
	}
	id := workspaceID(path)
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, w := range h.workspaces {
		if w["id"] == id {
			return
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	h.workspaces = append(h.workspaces, map[string]any{
		"id":         id,
		"path":       path,
		"name":       baseName(path),
		"createdAt":  now,
		"lastUsedAt": now,
	})
}

func (s *Session) appendMessage(m map[string]any) {
	s.mu.Lock()
	s.messages = append(s.messages, m)
	s.Modified = time.Now()
	s.mu.Unlock()
}

// setLiveMessage registers (or clears, with nil) the builder for the assistant
// message currently being streamed into this session.
func (s *Session) setLiveMessage(fn func() map[string]any) {
	s.mu.Lock()
	s.liveMessage = fn
	s.mu.Unlock()
}

func (s *Session) clearLiveMessage() {
	s.setLiveMessage(nil)
}

func (s *Session) closeClient() {
	s.mu.Lock()
	s.interrupted = true
	s.continuation = nil
	sc := s.sc
	pc := s.pi
	ac := s.acpClient
	oc := s.ocClient
	ocCancel := s.ocCancel
	s.sc = nil
	s.pi = nil
	s.acpClient = nil
	s.acpSession = ""
	s.ocClient = nil
	s.ocSession = ""
	s.ocCancel = nil
	s.mu.Unlock()
	if sc != nil {
		sc.Close()
	}
	if pc != nil {
		pc.Close()
	}
	if ac != nil {
		ac.Close()
	}
	if ocCancel != nil {
		ocCancel()
	}
	if oc != nil {
		oc.Close()
	}
}

// CloseAll stops every session's sidecar (the relay is closed by main).
func (h *Host) CloseAll() {
	h.mu.Lock()
	sessions := make([]*Session, 0, len(h.sessions))
	for _, s := range h.sessions {
		sessions = append(sessions, s)
	}
	h.mu.Unlock()
	for _, s := range sessions {
		s.closeClient()
	}
	if h.bus != nil {
		h.bus.Close()
	}
}

func (h *Host) summary(s *Session) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	first := ""
	count := 0
	for _, m := range s.messages {
		if m["role"] == "user" {
			if v, ok := m["content"].(string); ok && first == "" {
				first = v
			}
		}
		count++
	}
	if len(first) > 200 {
		first = first[:200]
	}
	out := map[string]any{
		"id":           s.ID,
		"name":         s.Name,
		"cwd":          s.Cwd,
		"created":      s.Created.UTC().Format(time.RFC3339Nano),
		"modified":     s.Modified.UTC().Format(time.RFC3339Nano),
		"messageCount": count,
		"firstMessage": first,
		"live":         true,
		"running":      s.Streaming,
		"archived":     s.Archived,
		"workspaceId":  workspaceID(s.Cwd),
		"runtime":      s.Runtime,
	}
	if s.worktree != nil {
		out["worktree"] = s.worktree
	}
	return out
}

func queueTextsLocked(s *Session) ([]any, []any) {
	steering := make([]any, 0, len(s.steerQueue))
	for _, q := range s.steerQueue {
		if q.Text != "" {
			steering = append(steering, q.Text)
		}
	}
	followUp := make([]any, 0, len(s.followUps))
	for _, q := range s.followUps {
		if q.Text != "" {
			followUp = append(followUp, q.Text)
		}
	}
	return steering, followUp
}

func (h *Host) emitQueue(s *Session) {
	s.mu.Lock()
	steering, followUp := queueTextsLocked(s)
	s.mu.Unlock()
	h.event(s.ID, map[string]any{"type": "queue_update", "steering": steering, "followUp": followUp})
}

func (h *Host) snapshot(s *Session) map[string]any {
	s.mu.Lock()
	messages := make([]map[string]any, len(s.messages))
	copy(messages, s.messages)
	streaming := s.Streaming
	live := s.liveMessage
	steering, followUp := queueTextsLocked(s)
	sessionProvider, sessionModel := s.Provider, s.ModelID
	mode := s.PermissionMode
	thinkingLevel := s.ThinkingLevel
	if mode == "" {
		mode = "auto"
	}
	runtimeID := s.Runtime
	if runtimeID == "" {
		runtimeID = "claude-code"
	}
	s.mu.Unlock()
	if thinkingLevel == "" {
		h.mu.Lock()
		thinkingLevel = str(h.settings["defaultThinking"])
		h.mu.Unlock()
	}

	if streaming && live != nil {
		if m := live(); m != nil {
			messages = append(messages, m)
		}
	}

	provider, modelID := h.modelFor(runtimeID)
	if sessionModel != "" {
		provider, modelID = sessionProvider, sessionModel
	}
	caps := ccCapabilities()
	if runtimeID == "codex" {
		caps = codexCapabilities()
	} else if runtimeID == "pi" {
		caps = piCapabilities()
	} else if runtimeID == "acp" {
		caps = acpCapabilities()
	} else if runtimeID == "opencode" {
		caps = openCodeCapabilities()
	}
	snap := map[string]any{
		"id":                      s.ID,
		"name":                    s.Name,
		"cwd":                     s.Cwd,
		"workspaceId":             workspaceID(s.Cwd),
		"messages":                messages,
		"model":                   map[string]any{"provider": provider, "id": modelID},
		"thinkingLevel":           thinkingLevel,
		"availableThinkingLevels": caps["thinkingLevels"],
		"permissionMode":          mode,
		"isStreaming":             streaming,
		"queue":                   map[string]any{"steering": steering, "followUp": followUp},
		"pendingUI":               h.pendingUIFor(s.ID),
		"statuses":                map[string]any{},
		"widgets":                 map[string]any{},
		"subagents":               []any{},
		"runtime":                 runtimeID,
		"capabilities":            caps,
	}
	if runtimeID == "claude-code" && h.cfg.Gateway != "" && h.cfg.Gateway != "native" {
		snap["gateway"] = map[string]any{
			"mode": "prometheus", "gatewayId": h.cfg.Gateway,
			"model": map[string]any{"provider": h.cfg.Provider, "id": h.cfg.Model},
		}
	}
	if s.worktree != nil {
		snap["worktree"] = s.worktree
	}
	return snap
}

func (h *Host) modelFor(runtimeID string) (provider, modelID string) {
	if runtimeID == "codex" {
		return "codex", "default"
	}
	if runtimeID == "pi" {
		return "pi", "default"
	}
	if runtimeID == "acp" {
		return "acp", "default"
	}
	if runtimeID == "opencode" {
		return "opencode", "default"
	}
	return h.cfg.Provider, h.cfg.Model
}

func (h *Host) abortSession(s *Session, dropQueue bool) {
	if dropQueue {
		s.mu.Lock()
		s.steerQueue = nil
		s.followUps = nil
		s.mu.Unlock()
		h.emitQueue(s)
	}
	s.mu.Lock()
	sc := s.sc
	pc := s.pi
	ac := s.acpClient
	acSession := s.acpSession
	oc := s.ocClient
	ocSession := s.ocSession
	s.mu.Unlock()
	if oc != nil {
		_, _ = oc.Request(context.Background(), "POST", "/session/"+ocSession+"/abort", s.Cwd, nil)
	} else if ac != nil {
		_ = ac.Notify("session/cancel", map[string]any{"sessionId": acSession})
	} else if pc != nil {
		_ = pc.Abort()
	} else if sc != nil {
		_ = sc.Interrupt()
	}
}

// endRun closes a turn, runs auto-compaction when the context is full, and
// starts the next queued message. Queued messages only run after the runtime has
// fully settled, so they never cut into a reply.
func (h *Host) endRun(s *Session) {
	if h.shouldCompact(s) {
		h.runCompaction(s, "auto")
	}
	s.mu.Lock()
	s.Streaming = false
	s.liveMessage = nil
	s.Modified = time.Now()
	var next *queuedPrompt
	// A message the user queued or inserted while the turn ran always wins over
	// an automatic continuation.
	if len(s.steerQueue) > 0 {
		q := s.steerQueue[0]
		s.steerQueue = s.steerQueue[1:]
		next = &q
	} else if len(s.followUps) > 0 {
		q := s.followUps[0]
		s.followUps = s.followUps[1:]
		next = &q
	} else if s.continuation != nil {
		q := *s.continuation
		s.continuation = nil
		next = &q
	}
	s.mu.Unlock()
	h.emitQueue(s)
	h.broadcast(map[string]any{"type": "sessions.changed"})
	h.persist()
	if next != nil {
		go h.runPrompt(s, next.Text, next.Images)
	}
}

func (h *Host) shouldCompact(s *Session) bool {
	h.mu.Lock()
	auto := bool_(h.settings["autoCompaction"])
	percent := number(h.settings["compactPercent"])
	h.mu.Unlock()
	if !auto || percent <= 0 {
		return false
	}
	s.mu.Lock()
	runtimeID := s.Runtime
	sc := s.sc
	tokens := s.contextTokens
	window := s.contextWindow
	modelID := s.ModelID
	compacting := s.compacting
	last := s.lastCompact
	s.mu.Unlock()
	if runtimeID != "claude-code" || sc == nil || compacting || tokens <= 0 {
		return false
	}
	if window <= 0 {
		window = h.modelContextWindow(modelID)
	}
	if window <= 0 {
		window = h.modelContextWindow(h.cfg.Model)
	}
	if window <= 0 {
		return false
	}
	if !last.IsZero() && time.Since(last) < 2*time.Minute {
		return false
	}
	return float64(tokens)/float64(window)*100 >= float64(percent)
}

// runCompaction asks Claude Code to compact through its own `/compact` command.
// The compaction turn is hidden from the chat; only start/end events are sent.
func (h *Host) runCompaction(s *Session, reason string) {
	s.mu.Lock()
	if s.compacting {
		s.mu.Unlock()
		return
	}
	sc := s.sc
	if sc == nil {
		s.mu.Unlock()
		return
	}
	s.compacting = true
	s.lastCompact = time.Now()
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.compacting = false
		s.mu.Unlock()
	}()

	h.event(s.ID, map[string]any{"type": "compaction_start", "reason": reason})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	// Discard late events from the finished turn (background tasks, a closed query): a stale "done" would
	// otherwise be consumed as the answer to this prompt.
	sc.Drain()
	err := sc.Prompt(ctx, "/compact", "", func(raw json.RawMessage) error {
		var ev struct {
			Type   string `json:"type"`
			Tokens int    `json:"tokens"`
			Max    int    `json:"max"`
		}
		if json.Unmarshal(raw, &ev) != nil || ev.Type != "context_usage" || ev.Max <= 0 {
			return nil
		}
		window := ev.Max
		if configured := h.modelContextWindow(s.ModelID); configured > 0 && configured < window {
			window = configured
		}
		s.mu.Lock()
		s.contextTokens = ev.Tokens
		s.contextWindow = window
		s.mu.Unlock()
		return nil
	})
	end := map[string]any{"type": "compaction_end", "reason": reason, "aborted": false}
	if err != nil {
		end["errorMessage"] = err.Error()
	}
	h.event(s.ID, end)
}

func (h *Host) runPrompt(s *Session, text string, images ...[]any) {
	s.mu.Lock()
	if s.Streaming {
		s.mu.Unlock()
		return
	}
	s.Streaming = true
	s.interrupted = false
	runtimeID := s.Runtime
	sc := s.sc
	s.mu.Unlock()

	if runtimeID == "codex" {
		h.runCodexPrompt(s, text)
		return
	}
	if runtimeID == "pi" {
		h.runPiPrompt(s, text)
		return
	}
	if runtimeID == "acp" {
		h.runAcpPrompt(s, text)
		return
	}
	if runtimeID == "opencode" {
		h.runOpenCodePrompt(s, text)
		return
	}

	staticCtx, turnCtx := h.pluginContext(text, s)
	if sc == nil {
		var mcpURL, mcpToken string
		if h.bus != nil {
			mcpURL, mcpToken = h.bus.Open(s.ID, s.Cwd)
		}
		var err error
		s.mu.Lock()
		resume := s.nativeID
		s.mu.Unlock()
		if resume == "" {
			// Older chats have no native session id (or Claude cleaned it up):
			// give the fresh sidecar the recent transcript instead of starting
			// from nothing.
			if hist := h.historyContext(s, 24, 24000); hist != "" {
				staticCtx = "这是本会话此前的对话内容，请延续上下文：\n" + hist + "\n\n" + staticCtx
			}
		}
		sc, err = h.cfg.StartSidecar(s.ID, s.Cwd, mcpURL, mcpToken, staticCtx, resume)
		if err != nil {
			if h.bus != nil {
				h.bus.CloseSession(s.ID)
			}
			h.finishError(s, err)
			return
		}
		s.mu.Lock()
		s.sc = sc
		s.mu.Unlock()
	}
	s.mu.Lock()
	sessionProvider, sessionModel := s.Provider, s.ModelID
	thinkingLevel := s.ThinkingLevel
	s.mu.Unlock()
	if thinkingLevel == "" {
		h.mu.Lock()
		thinkingLevel = str(h.settings["defaultThinking"])
		h.mu.Unlock()
	}
	if sessionModel != "" {
		_ = sc.SetModel(sessionModel)
	}
	if thinkingLevel != "" {
		_ = sc.SetThinking(thinkingLevel)
	}
	if sessionProvider == "" {
		sessionProvider = h.cfg.Provider
	}
	if sessionModel == "" {
		sessionModel = h.cfg.Model
	}

	now := time.Now().UnixMilli()
	var media []any
	if len(images) > 0 {
		media = images[0]
	}
	userContent := any(text)
	if len(media) > 0 {
		parts := []any{}
		if text != "" {
			parts = append(parts, map[string]any{"type": "text", "text": text})
		}
		for _, raw := range media {
			m := asMap(raw)
			data, mime := str(m["data"]), str(m["mimeType"])
			if data != "" && mime != "" {
				parts = append(parts, map[string]any{"type": "image", "data": data, "mimeType": mime})
			}
		}
		userContent = parts
	}
	user := map[string]any{"role": "user", "content": userContent, "timestamp": now}
	s.appendMessage(user)
	h.event(s.ID, map[string]any{"type": "agent_start"})
	h.event(s.ID, map[string]any{"type": "turn_start"})
	h.event(s.ID, map[string]any{"type": "message_start", "message": user})

	assistant := map[string]any{
		"role": "assistant", "content": []any{}, "provider": sessionProvider,
		"model": sessionModel, "usage": zeroUsage(), "stopReason": "pending",
		"timestamp": time.Now().UnixMilli(),
	}
	h.event(s.ID, map[string]any{"type": "message_start", "message": assistant})

	var textBuf strings.Builder
	var usage map[string]any
	var resultText string
	content := []any{}
	thinkingAt, textAt := -1, -1
	toolAt := map[string]int{}
	toolBlocks := map[string]map[string]any{}
	// Deltas mutate the content blocks in place; the live-message snapshot used
	// by session.open takes the same lock so a reopen sees a consistent picture.
	var liveMu sync.Mutex
	s.setLiveMessage(func() map[string]any {
		liveMu.Lock()
		defer liveMu.Unlock()
		snap := cloneMap(assistant)
		snap["content"] = content
		raw, err := json.Marshal(snap)
		if err != nil {
			return nil
		}
		var out map[string]any
		if json.Unmarshal(raw, &out) != nil {
			return nil
		}
		return out
	})

	parseInput := func(raw json.RawMessage) any {
		if len(raw) == 0 {
			return map[string]any{}
		}
		var v any
		if json.Unmarshal(raw, &v) != nil || v == nil {
			return map[string]any{}
		}
		return v
	}
	appendThinking := func(delta string) {
		if !hasVisible(delta) {
			return
		}
		liveMu.Lock()
		if thinkingAt < 0 {
			content = append(content, map[string]any{"type": "thinking", "thinking": ""})
			thinkingAt = len(content) - 1
			textAt = -1
		}
		block := content[thinkingAt].(map[string]any)
		block["thinking"] = str(block["thinking"]) + delta
		idx := thinkingAt
		liveMu.Unlock()
		h.event(s.ID, map[string]any{
			"type":  "message_update",
			"delta": map[string]any{"type": "thinking_delta", "contentIndex": idx, "delta": delta},
		})
	}
	appendText := func(delta string) {
		if delta == "" {
			return
		}
		liveMu.Lock()
		if textAt < 0 {
			content = append(content, map[string]any{"type": "text", "text": ""})
			textAt = len(content) - 1
			thinkingAt = -1
		}
		block := content[textAt].(map[string]any)
		block["text"] = str(block["text"]) + delta
		textBuf.WriteString(delta)
		idx := textAt
		liveMu.Unlock()
		h.event(s.ID, map[string]any{
			"type":  "message_update",
			"delta": map[string]any{"type": "text_delta", "contentIndex": idx, "delta": delta},
		})
	}
	startTool := func(id, name string, input json.RawMessage, complete bool) {
		if id == "" {
			return
		}
		args := parseInput(input)
		liveMu.Lock()
		if idx, ok := toolAt[id]; ok {
			block := toolBlocks[id]
			if name != "" {
				block["name"] = name
			}
			if complete {
				block["arguments"] = args
			}
			liveMu.Unlock()
			if complete {
				h.event(s.ID, map[string]any{
					"type":  "message_update",
					"delta": map[string]any{"type": "toolcall_end", "contentIndex": idx, "toolCall": block},
				})
			}
			return
		}
		idx := len(content)
		block := map[string]any{"type": "toolCall", "id": id, "name": name, "arguments": args}
		content = append(content, block)
		toolAt[id] = idx
		toolBlocks[id] = block
		thinkingAt, textAt = -1, -1
		liveMu.Unlock()
		h.event(s.ID, map[string]any{
			"type":  "message_update",
			"delta": map[string]any{"type": "toolcall_start", "contentIndex": idx, "id": id, "name": name},
		})
		h.event(s.ID, map[string]any{
			"type": "tool_execution_start", "toolCallId": id,
			"toolName": name, "args": args,
		})
	}
	toolResultContent := func(raw json.RawMessage) any {
		if len(raw) == 0 {
			return []any{}
		}
		var v any
		if json.Unmarshal(raw, &v) != nil || v == nil {
			return []any{}
		}
		if text, ok := v.(string); ok {
			return []any{map[string]any{"type": "text", "text": text}}
		}
		return v
	}

	ctx, cancel := promptContext()
	defer cancel()
	handle := func(raw json.RawMessage) error {
		var ev struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			Name      string          `json:"name"`
			Tool      string          `json:"tool"`
			ID        string          `json:"id"`
			Message   string          `json:"message"`
			Result    json.RawMessage `json:"result"`
			IsError   bool            `json:"isError"`
			Usage     json.RawMessage `json:"usage"`
			Input     json.RawMessage `json:"input"`
			Tokens    int             `json:"tokens"`
			Max       int             `json:"max"`
			SessionID string          `json:"sessionId"`
		}
		if json.Unmarshal(raw, &ev) != nil {
			return nil
		}
		switch ev.Type {
		case "init":
			if ev.SessionID != "" {
				s.mu.Lock()
				s.nativeID = ev.SessionID
				s.mu.Unlock()
			}
		case "thinking_delta":
			appendThinking(ev.Text)
		case "text_delta":
			appendText(ev.Text)
		case "tool_start":
			startTool(ev.ID, ev.Name, ev.Input, false)
		case "tool_input":
			startTool(ev.ID, ev.Name, ev.Input, true)
		case "tool_result":
			if idx, ok := toolAt[ev.ID]; ok {
				h.event(s.ID, map[string]any{
					"type":  "message_update",
					"delta": map[string]any{"type": "toolcall_end", "contentIndex": idx, "toolCall": toolBlocks[ev.ID]},
				})
			}
			content := toolResultContent(ev.Result)
			h.event(s.ID, map[string]any{
				"type": "tool_execution_end", "toolCallId": ev.ID,
				"toolName": "tool", "result": map[string]any{"content": content}, "isError": ev.IsError,
			})
			s.appendMessage(map[string]any{
				"role": "toolResult", "toolCallId": ev.ID, "toolName": "tool",
				"content": content, "isError": ev.IsError, "timestamp": time.Now().UnixMilli(),
			})
		case "permission_request":
			go h.handlePermission(s, sc, ev.ID, ev.Tool, ev.Input)
		case "permission":
			h.event(s.ID, map[string]any{
				"type": "ui.request",
				"request": map[string]any{
					"id": newID(), "method": "notify",
					"message": fmt.Sprintf("tool %s", ev.Tool),
					"level":   "info",
				},
			})
		case "context_usage":
			if ev.Max > 0 {
				s.mu.Lock()
				configured := h.modelContextWindow(s.ModelID)
				s.contextTokens = ev.Tokens
				if configured > 0 && configured < ev.Max {
					s.contextWindow = configured
				} else {
					s.contextWindow = ev.Max
				}
				s.mu.Unlock()
			}
		case "done":
			if len(ev.Usage) > 0 {
				_ = json.Unmarshal(ev.Usage, &usage)
			}
			if len(ev.Result) > 0 && textBuf.Len() == 0 {
				var text string
				if json.Unmarshal(ev.Result, &text) == nil {
					resultText = text
				}
			}
		case "error":
			return errors.New(ev.Message)
		}
		return nil
	}

	hasOutput := func() bool {
		liveMu.Lock()
		defer liveMu.Unlock()
		return len(content) > 0
	}
	err := h.promptWithRetry(ctx, s, sc, text, turnCtx, media, handle, hasOutput)

	if resultText != "" && textBuf.Len() == 0 {
		content = append(content, map[string]any{"type": "text", "text": resultText})
	}
	if err != nil {
		// The sidecar may still consider its turn active; tell it to stop so the
		// next prompt is not answered with "a prompt is already running".
		_ = sc.Interrupt()
		if errors.Is(err, context.DeadlineExceeded) {
			err = errors.New("本轮运行超时，已自动中断；可以直接继续")
		}
	}
	stop := "stop"
	if err != nil {
		stop = "error"
	}
	final := map[string]any{
		"role": "assistant", "content": content, "provider": sessionProvider,
		"model": sessionModel, "usage": normalizeUsage(usage), "stopReason": stop,
		"timestamp": time.Now().UnixMilli(),
	}
	if err != nil {
		final["errorMessage"] = err.Error()
	}
	s.clearLiveMessage()
	s.appendMessage(final)
	h.event(s.ID, map[string]any{"type": "message_end", "message": final})
	h.event(s.ID, map[string]any{"type": "turn_end"})
	h.event(s.ID, map[string]any{"type": "agent_settled", "aborted": false})

	// A turn that ends after tools with only a progress line is not an answer.
	// Queue one continuation before the user's queued prompts.
	if err == nil && needsContinuation(content) {
		s.mu.Lock()
		// An interrupt, a forced steer or any queued user message means the user
		// is taking over: never inject an automatic continuation on top.
		if s.autoContinueLeft > 0 && !s.interrupted && len(s.steerQueue) == 0 && len(s.followUps) == 0 {
			s.autoContinueLeft--
			s.continuation = &queuedPrompt{Text: autoContinuePrompt}
		}
		s.mu.Unlock()
	}

	s.mu.Lock()
	s.usage.Input += number(usage["input_tokens"])
	s.usage.Output += number(usage["output_tokens"])
	s.usage.Reasoning += number(usage["reasoning_output_tokens"]) + number(usage["reasoning"])
	s.usage.CacheRead += number(usage["cache_read_input_tokens"])
	s.usage.CacheWrite += number(usage["cache_creation_input_tokens"])
	s.mu.Unlock()
	h.endRun(s)
}

// promptWithRetry runs one user prompt, retrying only transient provider
// failures and only before the model produced any content. Retries are shown
// in the chat through auto_retry_start/end so the wait is not mistaken for a
// hang. Settings → Chat controls the attempt count.
func (h *Host) promptWithRetry(ctx context.Context, s *Session, sc *sidecar.Client, text, turnCtx string, media []any, handle func(json.RawMessage) error, hasOutput func() bool) error {
	// A late "done" left by the previous turn (background command, closed query) would end this prompt
	// immediately with an empty reply before the model is called. Start from a clean channel.
	sc.Drain()
	attempts := h.settingInt("retryAttempts", 3, 0, 10)
	err := sc.PromptWithImages(ctx, text, turnCtx, media, handle)
	for attempt := 1; err != nil && attempt <= attempts && ctx.Err() == nil && !hasOutput() && retryableProviderError(err); attempt++ {
		delay := time.Duration(1<<uint(attempt-1)) * time.Second
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
		h.event(s.ID, map[string]any{
			"type": "auto_retry_start", "delayMs": delay.Milliseconds(),
			"attempt": attempt, "maxAttempts": attempts, "errorMessage": err.Error(),
		})
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			h.event(s.ID, map[string]any{"type": "auto_retry_end", "success": false, "finalError": ctx.Err().Error()})
			return ctx.Err()
		case <-timer.C:
		}
		_ = sc.Interrupt()
		time.Sleep(150 * time.Millisecond)
		sc.Drain()
		err = sc.PromptWithImages(ctx, text, turnCtx, media, handle)
		finalError := ""
		if err != nil {
			finalError = err.Error()
		}
		h.event(s.ID, map[string]any{"type": "auto_retry_end", "success": err == nil, "finalError": finalError})
	}
	return err
}

// retryableProviderError matches the failures worth retrying. Authentication,
// bad requests and model errors are not here: repeating them only wastes time.
func retryableProviderError(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"rate limit", "rate_limit", "too many requests", "429",
		"overloaded", "529", "500", "502", "503", "504",
		"server error", "internal server error", "bad gateway", "service unavailable",
		"timed out", "timeout", "connection reset", "connection refused",
		"connection closed", "network error", "network is unreachable",
		"temporarily unavailable", "try again", "econnreset", "econnrefused",
		"socket hang up", "stream ended", "unexpected end",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// needsContinuation spots a turn that stopped after tool calls without a real
// answer (the last block is a tool call, or only a short "let me check" line).
func needsContinuation(content []any) bool {
	if len(content) == 0 {
		return true
	}
	last, _ := content[len(content)-1].(map[string]any)
	if last == nil {
		return false
	}
	switch str(last["type"]) {
	case "toolCall":
		return true
	case "text":
		text := strings.TrimSpace(str(last["text"]))
		if text == "" {
			return true
		}
		if len([]rune(text)) > 160 {
			return false
		}
		lower := strings.ToLower(text)
		for _, prefix := range []string{"我先", "我会", "我将", "继续", "接下来", "现在", "让我", "下一步", "i'll", "i will", "let me", "next", "first,"} {
			if strings.HasPrefix(lower, prefix) {
				return true
			}
		}
	}
	return false
}

const autoContinuePrompt = "（自动续跑）请基于已有结果直接回答用户，不要重复计划或再次确认；如果还有必须执行的操作，先完成再回答。"

// handlePermission resolves one sidecar permission_request according to the
// session's permission mode, asking the UI when the mode requires it.
func (h *Host) handlePermission(s *Session, sc *sidecar.Client, id, tool string, input json.RawMessage) {
	s.mu.Lock()
	mode := s.PermissionMode
	s.mu.Unlock()
	if mode == "" {
		mode = "auto"
	}
	allow, ask := decidePermission(mode, tool)
	if ask {
		allow = h.askUI(s, tool, input)
	}
	_ = sc.RespondPermission(id, allow)
}

// askQuestion asks the user directly in the chat. Unlike the assistant plugin's
// delegated-run restriction, this works in any open conversation.
// pendingUIFor returns the dialogs a session is still waiting on, so opening
// or reconnecting to it shows them instead of leaving the agent blocked.
func (h *Host) pendingUIFor(sessionID string) []any {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []any{}
	for _, p := range h.uiRequests {
		if p.SessionID == sessionID {
			out = append(out, p.Request)
		}
	}
	return out
}

func (h *Host) askQuestion(ctx context.Context, sessionID, cwd, question string, options []string) (string, error) {
	s := h.session(sessionID)
	if s == nil {
		return "", fmt.Errorf("sessionNotFound")
	}
	if strings.TrimSpace(question) == "" {
		return "", fmt.Errorf("question is empty")
	}
	requestID := newID()
	ch := make(chan map[string]any, 1)
	h.mu.Lock()
	h.uiPending[requestID] = ch
	h.mu.Unlock()
	deadline := time.Now().Add(5 * time.Minute)
	req := map[string]any{"id": requestID, "method": "input", "title": question, "placeholder": "", "question": true, "expiresAt": deadline.UnixMilli()}
	if len(options) > 0 {
		req["method"] = "select"
		req["options"] = options
	}
	h.mu.Lock()
	h.uiRequests[requestID] = pendingUIRequest{SessionID: sessionID, Request: req}
	h.mu.Unlock()
	// A question must be seen even when this chat is in the background: send it
	// to every connected client instead of only this session's subscribers.
	h.broadcast(map[string]any{"type": "ui.request", "sessionId": sessionID, "request": req})
	if ctx == nil {
		ctx = context.Background()
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		h.mu.Lock()
		delete(h.uiPending, requestID)
		delete(h.uiRequests, requestID)
		h.mu.Unlock()
		h.broadcast(map[string]any{"type": "ui.resolved", "sessionId": sessionID, "requestId": requestID})
		return "", ctx.Err()
	case <-timer.C:
		h.mu.Lock()
		delete(h.uiPending, requestID)
		delete(h.uiRequests, requestID)
		h.mu.Unlock()
		h.broadcast(map[string]any{"type": "ui.resolved", "sessionId": sessionID, "requestId": requestID})
		// No answer after five minutes: take the recommended option (the first
		// one, by convention) so the agent can continue instead of hanging.
		if len(options) > 0 {
			return options[0], nil
		}
		return "", fmt.Errorf("the user did not answer in time; decide yourself and say what you assumed")
	case resp := <-ch:
		if cancelled, _ := resp["cancelled"].(bool); cancelled {
			return "", fmt.Errorf("the user cancelled the question; decide yourself and say what you assumed")
		}
		answer := str(resp["value"])
		if answer == "" {
			return "", fmt.Errorf("the user sent an empty answer")
		}
		return answer, nil
	}
}

func (h *Host) askUI(s *Session, tool string, input json.RawMessage) bool {
	requestID := newID()
	ch := make(chan bool, 1)
	h.mu.Lock()
	h.permPending[requestID] = ch
	h.mu.Unlock()

	detail := ""
	if len(input) > 0 {
		detail = string(input)
		if len(detail) > 2000 {
			detail = detail[:2000]
		}
	}
	req := map[string]any{
		"id": requestID, "method": "permission", "kind": "tool",
		"toolName": tool, "summary": "Allow " + tool + "?", "detail": detail,
	}
	h.mu.Lock()
	h.uiRequests[requestID] = pendingUIRequest{SessionID: s.ID, Request: req}
	h.mu.Unlock()
	h.broadcast(map[string]any{"type": "ui.request", "sessionId": s.ID, "request": req})
	select {
	case allow := <-ch:
		return allow
	case <-time.After(120 * time.Second):
		h.mu.Lock()
		delete(h.permPending, requestID)
		delete(h.uiRequests, requestID)
		h.mu.Unlock()
		return false
	}
}

func (h *Host) sendFrame(sessionID string, frame map[string]any) {
	h.mu.Lock()
	clients := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		if c.subs[sessionID] {
			clients = append(clients, c)
		}
	}
	h.mu.Unlock()
	for _, c := range clients {
		c.send(frame)
	}
}

func decidePermission(mode, tool string) (allow bool, ask bool) {
	readOnly := tool == "Read" || tool == "Grep" || tool == "Glob" || tool == "WebFetch" || tool == "WebSearch"
	edits := tool == "Edit" || tool == "MultiEdit" || tool == "Write" || tool == "NotebookEdit"
	switch mode {
	case "auto", "bypassPermissions":
		return true, false
	case "plan":
		return readOnly, false
	case "acceptEdits":
		if readOnly || edits {
			return true, false
		}
		return false, true
	default: // default
		return false, true
	}
}

func setSessionModel(s *Session, m map[string]any) {
	if s == nil || m == nil {
		return
	}
	provider, modelID := str(m["provider"]), str(m["id"])
	if modelID == "" {
		return
	}
	s.mu.Lock()
	s.Provider = provider
	s.ModelID = modelID
	s.contextWindow = 0
	s.contextTokens = 0
	s.mu.Unlock()
}

func (h *Host) finishError(s *Session, err error) {
	s.clearLiveMessage()
	msg := map[string]any{
		"role": "assistant", "content": []any{}, "provider": h.cfg.Provider,
		"model": h.cfg.Model, "usage": zeroUsage(), "stopReason": "error",
		"errorMessage": err.Error(), "timestamp": time.Now().UnixMilli(),
	}
	s.appendMessage(msg)
	h.event(s.ID, map[string]any{"type": "message_end", "message": msg})
	h.event(s.ID, map[string]any{"type": "agent_settled", "aborted": false})
	h.endRun(s)
}

// ------------------------------------------------------------------ rpc

func (h *Host) dispatch(method string, raw json.RawMessage) (any, error) {
	var p map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &p)
	}
	switch method {
	case "host.info":
		return map[string]any{
			"version": "0.30.44-ramicore", "piVersion": h.cfg.PIVersion, "protocol": protocolVersion,
			"cwd": h.cfg.Cwd, "platform": runtime.GOOS,
		}, nil

	case "session.list":
		h.mu.Lock()
		ids := append([]string(nil), h.order...)
		days := number(h.settings["recycleDays"])
		h.mu.Unlock()
		cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
		out := make([]any, 0, len(ids))
		for _, id := range ids {
			s := h.session(id)
			if s == nil {
				continue
			}
			if days >= 1 {
				s.mu.Lock()
				stale := !s.Streaming && s.Modified.Before(cutoff)
				s.mu.Unlock()
				if stale {
					continue
				}
			}
			out = append(out, h.summary(s))
		}
		return out, nil

	case "session.create":
		cwd := str(p["cwd"])
		if cwd == "" {
			cwd = h.cfg.Cwd
		}
		name := str(p["name"])
		if name == "" {
			name = "New chat"
		}
		runtimeID := str(p["runtime"])
		if runtimeID == "" {
			runtimeID = "claude-code"
		}
		if runtimeID == "codex" && h.cfg.Codex.Path == "" {
			return nil, fmt.Errorf("Codex is not installed")
		}
		if runtimeID == "pi" && h.cfg.PIEntry == "" {
			return nil, fmt.Errorf("PI is not installed")
		}
		if runtimeID == "acp" && h.cfg.ACPEntry == "" {
			return nil, fmt.Errorf("ACP adapter is not installed")
		}
		if runtimeID == "opencode" && h.cfg.OpenCodePath == "" {
			return nil, fmt.Errorf("OpenCode is not installed")
		}
		if runtimeID != "codex" && runtimeID != "pi" && runtimeID != "acp" && runtimeID != "opencode" {
			runtimeID = "claude-code"
		}
		s := h.create(cwd, name, runtimeID)
		if m, ok := p["model"].(map[string]any); ok {
			setSessionModel(s, m)
		}
		wantWorktree := bool_(p["worktree"]) || str(h.settings["worktreeMode"]) == "on"
		if wantWorktree {
			if err := h.setupWorktree(s); err != nil {
				s.closeClient()
				h.mu.Lock()
				delete(h.sessions, s.ID)
				for i, id := range h.order {
					if id == s.ID {
						h.order = append(h.order[:i], h.order[i+1:]...)
						break
					}
				}
				h.mu.Unlock()
				h.persist()
				return nil, err
			}
			h.persist()
		}
		h.broadcast(map[string]any{"type": "sessions.changed"})
		return h.snapshot(s), nil

	case "session.open":
		s := h.session(str(p["sessionId"]))
		if s == nil {
			return nil, fmt.Errorf("sessionNotFound")
		}
		return h.snapshot(s), nil

	case "session.messages":
		s := h.session(str(p["sessionId"]))
		if s == nil {
			return nil, fmt.Errorf("sessionNotFound")
		}
		s.mu.Lock()
		all := make([]any, len(s.messages))
		for i, m := range s.messages {
			all[i] = m
		}
		s.mu.Unlock()
		before := number(p["before"])
		if before <= 0 || before > len(all) {
			before = len(all)
		}
		turns := number(p["turns"])
		if turns <= 0 {
			turns = 12
		}
		start := before - turns*2
		if start < 0 {
			start = 0
		}
		return map[string]any{"messages": all[start:before], "offset": start}, nil

	case "session.toolResult":
		return map[string]any{"content": []any{}}, nil

	case "file.stat":
		s := h.session(str(p["sessionId"]))
		path, err := h.resolvePath(s, str(p["path"]))
		if err != nil {
			return map[string]any{"exists": false}, nil
		}
		fi, err := os.Stat(path)
		if err != nil {
			return map[string]any{"exists": false}, nil
		}
		return map[string]any{"exists": true, "name": filepath.Base(path), "size": fi.Size()}, nil

	case "file.read":
		s := h.session(str(p["sessionId"]))
		path, err := h.resolvePath(s, str(p["path"]))
		if err != nil {
			return nil, err
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			return nil, err
		}
		offset := int64(number(p["offset"]))
		length := int64(number(p["length"]))
		if length <= 0 || length > 4<<20 {
			length = 4 << 20
		}
		if offset < 0 {
			offset = 0
		}
		if offset > fi.Size() {
			offset = fi.Size()
		}
		buf := make([]byte, min64(length, fi.Size()-offset))
		n, err := f.ReadAt(buf, offset)
		if err != nil && n == 0 {
			return nil, err
		}
		return map[string]any{
			"data": base64.StdEncoding.EncodeToString(buf[:n]),
			"size": fi.Size(), "eof": offset+int64(n) >= fi.Size(),
		}, nil

	case "file.link":
		s := h.session(str(p["sessionId"]))
		path, err := h.resolvePath(s, str(p["path"]))
		if err != nil {
			return nil, err
		}
		id := newID()
		h.mu.Lock()
		h.tickets[id] = downloadTicket{Path: path, Name: filepath.Base(path), Expires: time.Now().Add(time.Minute)}
		h.mu.Unlock()
		return map[string]any{"id": id, "name": filepath.Base(path)}, nil

	case "session.rename":
		s := h.session(str(p["sessionId"]))
		if s == nil {
			return nil, fmt.Errorf("sessionNotFound")
		}
		s.mu.Lock()
		s.Name = str(p["name"])
		s.mu.Unlock()
		h.broadcast(map[string]any{"type": "sessions.changed"})
		h.persist()
		return nil, nil

	case "session.archive":
		s := h.session(str(p["sessionId"]))
		if s == nil {
			return nil, fmt.Errorf("sessionNotFound")
		}
		s.mu.Lock()
		s.Archived = bool_(p["archived"])
		s.mu.Unlock()
		h.broadcast(map[string]any{"type": "sessions.changed"})
		h.persist()
		return nil, nil

	case "session.close":
		s := h.session(str(p["sessionId"]))
		if s != nil {
			s.closeClient()
			if h.bus != nil {
				h.bus.CloseSession(s.ID)
			}
		}
		return nil, nil

	case "session.delete":
		id := str(p["sessionId"])
		s := h.session(id)
		if s != nil {
			s.mu.Lock()
			wt := s.worktree
			s.mu.Unlock()
			if wt != nil && str(wt["state"]) == "active" {
				_ = h.discardWorktree(s)
			}
			s.closeClient()
			if h.bus != nil {
				h.bus.CloseSession(id)
			}
			h.mu.Lock()
			delete(h.sessions, id)
			for i, x := range h.order {
				if x == id {
					h.order = append(h.order[:i], h.order[i+1:]...)
					break
				}
			}
			h.mu.Unlock()
		}
		h.broadcast(map[string]any{"type": "sessions.changed"})
		h.persist()
		return nil, nil

	case "session.switchRuntime":
		s := h.session(str(p["sessionId"]))
		if s == nil {
			return nil, fmt.Errorf("sessionNotFound")
		}
		runtimeID := str(p["runtime"])
		if runtimeID == "codex" && h.cfg.Codex.Path == "" {
			return nil, fmt.Errorf("Codex is not installed")
		}
		if runtimeID == "acp" && h.cfg.ACPEntry == "" {
			return nil, fmt.Errorf("ACP adapter is not installed")
		}
		if runtimeID == "opencode" && h.cfg.OpenCodePath == "" {
			return nil, fmt.Errorf("OpenCode is not installed")
		}
		if runtimeID == "" {
			runtimeID = "claude-code"
		}
		s.mu.Lock()
		old := s.sc
		oldPI := s.pi
		oldACP := s.acpClient
		oldOC := s.ocClient
		oldOCCancel := s.ocCancel
		s.sc = nil
		s.pi = nil
		s.acpClient = nil
		s.acpSession = ""
		s.ocClient = nil
		s.ocSession = ""
		s.ocCancel = nil
		s.Runtime = runtimeID
		s.mu.Unlock()
		if m, ok := p["model"].(map[string]any); ok {
			setSessionModel(s, m)
		}
		if old != nil {
			old.Close()
		}
		if oldPI != nil {
			oldPI.Close()
		}
		if oldACP != nil {
			oldACP.Close()
		}
		if oldOCCancel != nil {
			oldOCCancel()
		}
		if oldOC != nil {
			oldOC.Close()
		}
		if h.bus != nil {
			h.bus.CloseSession(s.ID)
		}
		h.persist()
		return h.snapshot(s), nil

	case "session.stats":
		s := h.session(str(p["sessionId"]))
		if s == nil {
			return nil, fmt.Errorf("sessionNotFound")
		}
		s.mu.Lock()
		in, out, reasoning, cr, cw := s.usage.Input, s.usage.Output, s.usage.Reasoning, s.usage.CacheRead, s.usage.CacheWrite
		ctxTokens, ctxWindow := s.contextTokens, s.contextWindow
		sessionModel := s.ModelID
		tokens := 0
		for _, m := range s.messages {
			if b, ok := m["content"].(string); ok {
				tokens += len(b) / 4
			}
		}
		s.mu.Unlock()
		estimated := true
		if ctxTokens > 0 {
			tokens = ctxTokens
			estimated = false
		} else if tokens == 0 && in+out+reasoning+cr+cw > 0 {
			tokens = in + out + reasoning + cr + cw
		}
		window := ctxWindow
		if window <= 0 {
			window = h.modelContextWindow(sessionModel)
		}
		if window <= 0 {
			window = h.modelContextWindow(h.cfg.Model)
		}
		if window <= 0 {
			window = 200_000
		}
		percent := float64(tokens) / float64(window) * 100
		if percent > 100 {
			percent = 100
		}
		return map[string]any{
			"contextTokens": tokens, "contextEstimated": estimated, "contextWindow": window,
			"contextPercent": percent,
			"breakdown":      map[string]any{"system": 0, "tools": 0, "user": 0, "assistant": 0, "toolResults": 0, "other": 0},
			"totalCost":      0, "inputTokens": in, "outputTokens": out, "reasoningTokens": reasoning,
			"cacheReadTokens": cr, "cacheWriteTokens": cw,
		}, nil

	case "chat.prompt":
		s := h.session(str(p["sessionId"]))
		if s == nil {
			return nil, fmt.Errorf("sessionNotFound")
		}
		text := str(p["text"])
		images := asList(p["images"])
		if strings.TrimSpace(text) == "" && len(images) == 0 {
			return nil, fmt.Errorf("empty prompt")
		}
		delivery := str(p["delivery"])
		budget := h.settingInt("autoContinue", 0, 0, 3)
		s.mu.Lock()
		busy := s.Streaming
		renamed := false
		renamedName := ""
		if !busy {
			s.autoContinueLeft = budget
			if name := titleFromPrompt(text); name != "" && (s.Name == "" || s.Name == "New chat") {
				first := true
				for _, m := range s.messages {
					if str(m["role"]) == "user" {
						first = false
						break
					}
				}
				if first {
					s.Name = name
					renamedName = name
					renamed = true
				}
			}
		}
		if busy {
			q := queuedPrompt{Text: text, Images: images}
			if delivery == "steer" {
				s.steerQueue = append(s.steerQueue, q)
			} else {
				s.followUps = append(s.followUps, q)
			}
		}
		s.mu.Unlock()
		if renamed {
			h.event(s.ID, map[string]any{"type": "session_info_changed", "name": renamedName})
		}
		if busy {
			h.emitQueue(s)
			if delivery == "steer" {
				// "Send now": stop the current turn; the queued message starts
				// as soon as the runtime has settled.
				h.abortSession(s, false)
			}
			return map[string]any{"disposition": "queued"}, nil
		}
		go h.runPrompt(s, text, images)
		return map[string]any{"disposition": "started"}, nil

	case "chat.abort":
		if s := h.session(str(p["sessionId"])); s != nil {
			// Interrupting stops the reply that is running, not the conversation.
			// Messages already queued stay queued and start as soon as this turn
			// has settled, so an interrupt never silently drops the user's input.
			h.abortSession(s, false)
		}
		return nil, nil

	case "chat.compact":
		s := h.session(str(p["sessionId"]))
		if s == nil {
			return nil, fmt.Errorf("sessionNotFound")
		}
		s.mu.Lock()
		busy := s.Streaming
		s.mu.Unlock()
		if busy {
			return nil, fmt.Errorf("waitForReply")
		}
		go func() {
			s.mu.Lock()
			s.Streaming = true
			s.mu.Unlock()
			h.runCompaction(s, "manual")
			h.endRun(s)
		}()
		return nil, nil

	case "chat.unqueue":
		s := h.session(str(p["sessionId"]))
		if s == nil {
			return nil, fmt.Errorf("sessionNotFound")
		}
		kind := str(p["kind"])
		index := int(number(p["index"]))
		guard := str(p["text"])
		s.mu.Lock()
		queue := &s.followUps
		if kind == "steer" {
			queue = &s.steerQueue
		}
		var taken queuedPrompt
		ok := index >= 0 && index < len(*queue)
		if ok {
			if guard != "" && (*queue)[index].Text != guard {
				ok = false
			} else {
				taken = (*queue)[index]
				*queue = append((*queue)[:index], (*queue)[index+1:]...)
			}
		}
		s.mu.Unlock()
		if !ok {
			return map[string]any{"text": "", "images": []any{}}, nil
		}
		h.emitQueue(s)
		return map[string]any{"text": taken.Text, "images": taken.Images}, nil

	case "message.delete":
		s := h.session(str(p["sessionId"]))
		if s == nil {
			return nil, fmt.Errorf("sessionNotFound")
		}
		idx := number(asMap(p["ref"])["index"])
		s.mu.Lock()
		if idx >= 0 && idx < len(s.messages) {
			s.messages = append(s.messages[:idx], s.messages[idx+1:]...)
		}
		s.mu.Unlock()
		h.persist()
		return nil, nil

	case "message.edit":
		s := h.session(str(p["sessionId"]))
		if s == nil {
			return nil, fmt.Errorf("sessionNotFound")
		}
		idx := number(asMap(p["ref"])["index"])
		text := str(p["text"])
		s.mu.Lock()
		if idx < 0 || idx >= len(s.messages) {
			s.mu.Unlock()
			return nil, fmt.Errorf("message not found")
		}
		if s.Streaming {
			s.mu.Unlock()
			return nil, fmt.Errorf("上一条回复还在运行，请先停止或稍后再试")
		}
		s.messages[idx] = map[string]any{"role": "user", "content": text, "timestamp": time.Now().UnixMilli()}
		s.messages = s.messages[:idx+1]
		s.mu.Unlock()
		go h.runPrompt(s, text)
		return nil, nil

	case "message.retry":
		s := h.session(str(p["sessionId"]))
		if s == nil {
			return nil, fmt.Errorf("sessionNotFound")
		}
		idx := number(asMap(p["ref"])["index"])
		s.mu.Lock()
		if idx < 0 || idx >= len(s.messages) {
			s.mu.Unlock()
			return nil, fmt.Errorf("message not found")
		}
		text := ""
		for i := idx; i >= 0; i-- {
			if str(s.messages[i]["role"]) == "user" {
				text = messageText(s.messages[i]["content"])
				idx = i
				break
			}
		}
		if text == "" {
			s.mu.Unlock()
			return nil, fmt.Errorf("no user message to retry")
		}
		if s.Streaming {
			s.mu.Unlock()
			return nil, fmt.Errorf("上一条回复还在运行，请先停止或稍后再试")
		}
		s.messages = s.messages[:idx]
		s.autoContinueLeft = h.settingInt("autoContinue", 0, 0, 3)
		s.mu.Unlock()
		go h.runPrompt(s, text)
		return nil, nil

	case "message.fork":
		s := h.session(str(p["sessionId"]))
		if s == nil {
			return nil, fmt.Errorf("sessionNotFound")
		}
		idx := number(asMap(p["ref"])["index"])
		s.mu.Lock()
		editor := ""
		if idx >= 0 && idx < len(s.messages) {
			editor = messageText(s.messages[idx]["content"])
		}
		s.mu.Unlock()
		return map[string]any{"snapshot": h.snapshot(s), "editorText": editor}, nil

	case "goal.set":
		s := h.session(str(p["sessionId"]))
		if s == nil {
			return nil, fmt.Errorf("sessionNotFound")
		}
		maxTurns := number(p["maxTurns"])
		if maxTurns <= 0 {
			maxTurns = 30
		}
		goal := map[string]any{
			"objective": str(p["objective"]), "status": "active",
			"turn": 0, "maxTurns": maxTurns,
		}
		s.mu.Lock()
		s.goal = goal
		s.mu.Unlock()
		h.event(s.ID, map[string]any{"type": "goal_update", "goal": goal})
		return goal, nil

	case "goal.control":
		s := h.session(str(p["sessionId"]))
		if s == nil {
			return nil, fmt.Errorf("sessionNotFound")
		}
		action := str(p["action"])
		s.mu.Lock()
		goal := s.goal
		switch action {
		case "clear":
			s.goal = nil
			goal = nil
		case "pause":
			if goal != nil {
				goal["status"] = "paused"
			}
		case "resume":
			if goal != nil {
				goal["status"] = "active"
			}
		}
		s.mu.Unlock()
		h.event(s.ID, map[string]any{"type": "goal_update", "goal": goal})
		return goal, nil

	case "model.list":
		if h.cfg.PIEntry != "" {
			return h.piModelList(), nil
		}
		return h.modelList(), nil

	case "model.set":
		if s := h.session(str(p["sessionId"])); s != nil {
			if m, ok := p["model"].(map[string]any); ok {
				setSessionModel(s, m)
				h.event(s.ID, map[string]any{"type": "model_changed", "model": m})
				h.persist()
			}
		}
		return nil, nil

	case "thinking.set":
		if s := h.session(str(p["sessionId"])); s != nil {
			level := str(p["level"])
			s.mu.Lock()
			s.ThinkingLevel = level
			s.mu.Unlock()
			h.event(s.ID, map[string]any{"type": "thinking_level_changed", "level": level})
			h.persist()
		}
		return nil, nil

	case "permission.set":
		s := h.session(str(p["sessionId"]))
		if s == nil {
			return nil, fmt.Errorf("sessionNotFound")
		}
		mode := str(p["mode"])
		if mode == "" {
			mode = "default"
		}
		s.mu.Lock()
		s.PermissionMode = mode
		s.mu.Unlock()
		h.event(s.ID, map[string]any{"type": "permission_mode_changed", "mode": mode})
		h.persist()
		return nil, nil

	case "runtime.list":
		entry := func(id, name, kind string, available bool, version string, caps map[string]any) map[string]any {
			health := map[string]any{"available": available, "version": version}
			if !available {
				health["missing"] = true
			}
			return map[string]any{
				"id": id, "name": name, "kind": kind,
				"health": health, "capabilities": caps,
			}
		}
		claudeOK := h.cfg.Claude.Path != ""
		claude := entry("claude-code", "Claude Code", "claude-code", claudeOK, h.cfg.Claude.Version, ccCapabilities())
		claudeHealth := claude["health"].(map[string]any)
		claudeHealth["loggedIn"] = claudeOK
		claudeHealth["installed"] = h.cfg.Claude.Managed
		out := []any{
			claude,
			entry("codex", "Codex", "codex", h.cfg.Codex.Path != "", h.cfg.Codex.Version, codexCapabilities()),
			entry("pi", "PI", "pi", h.cfg.PIEntry != "" && h.cfg.PINode != "", h.cfg.PIVersion, piCapabilities()),
			entry("opencode", "OpenCode", "opencode", h.cfg.OpenCodePath != "", h.cfg.OpenCodeVer, openCodeCapabilities()),
		}
		return out, nil

	case "runtime.models":
		switch str(p["runtime"]) {
		case "codex":
			if models := h.gatewayModelList(); len(models) > 0 {
				return models, nil
			}
			return []any{map[string]any{
				"provider": "codex", "id": "default", "name": "Codex (account default)",
				"reasoning": true, "vision": true, "contextWindow": 200_000,
				"thinkingLevels": []any{"minimal", "medium", "high", "xhigh", "max"},
			}}, nil
		case "pi":
			return h.piModelList(), nil
		case "acp":
			return []any{map[string]any{
				"provider": "acp", "id": "default", "name": "ACP (agent default)",
				"reasoning": true, "vision": true, "contextWindow": 200_000,
			}}, nil
		case "opencode":
			return []any{map[string]any{
				"provider": "opencode", "id": "default", "name": "OpenCode (agent default)",
				"reasoning": true, "vision": true, "contextWindow": 200_000,
			}}, nil
		default:
			return h.modelList(), nil
		}

	case "agent.list":
		out := []any{
			map[string]any{"id": "claude-code", "name": "Claude Code", "kind": "claude-code", "enabled": true, "builtin": true},
			map[string]any{"id": "codex", "name": "Codex", "kind": "codex", "enabled": true, "builtin": true},
		}
		h.mu.Lock()
		for _, a := range h.agents {
			out = append(out, a)
		}
		h.mu.Unlock()
		return out, nil

	case "agent.save":
		a, _ := p["agent"].(map[string]any)
		if a == nil || str(a["id"]) == "" {
			return nil, fmt.Errorf("invalid agent profile")
		}
		h.mu.Lock()
		replaced := false
		for i, old := range h.agents {
			if str(old["id"]) == str(a["id"]) {
				h.agents[i] = a
				replaced = true
			}
		}
		if !replaced {
			h.agents = append(h.agents, a)
		}
		h.mu.Unlock()
		return a, nil

	case "agent.delete":
		id := str(p["id"])
		h.mu.Lock()
		for i, a := range h.agents {
			if str(a["id"]) == id {
				h.agents = append(h.agents[:i], h.agents[i+1:]...)
				break
			}
		}
		h.mu.Unlock()
		return nil, nil

	case "agent.syncSessions":
		return map[string]any{"found": 0, "added": 0, "workspaces": 0}, nil

	case "agent.install":
		id := str(p["agent"])
		kind := str(p["kind"])
		if id == "" {
			id = kind
		}
		if kind == "" {
			kind = id
		}
		if kind == "claude-code" {
			a, err := agents.Resolve(id)
			if err != nil || a.Path == "" {
				return nil, fmt.Errorf("Claude Code 未安装：请先用官方安装器或 npm 安装")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, a.Path, "update")
			out, err := cmd.CombinedOutput()
			text := strings.TrimSpace(string(out))
			if err != nil {
				if text == "" {
					text = err.Error()
				}
				return nil, fmt.Errorf("Claude Code 更新失败：%s", text)
			}
			if v, err := agents.Version(ctx, a.Path); err == nil {
				a.Version = v
			}
			h.refreshAgentCfg("claude-code")
			return map[string]any{"ok": true, "version": a.Version, "output": text}, nil
		}
		if kind == "codex" {
			a, err := agents.Resolve(id)
			if err != nil || a.Path == "" {
				return nil, fmt.Errorf("Codex 未安装：请先用官方安装器或 npm 安装")
			}
			method := agents.InstallMethod(a.Path)
			argv := agents.UpdateCommand(a.Path, method)
			if len(argv) == 0 {
				return nil, fmt.Errorf("无法判断 Codex 的安装方式（%s），请用它安装时的工具更新", a.Path)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
			text := strings.TrimSpace(string(out))
			if err != nil {
				if text == "" {
					text = err.Error()
				}
				return nil, fmt.Errorf("Codex 更新失败：%s", text)
			}
			if again, err := agents.Resolve(id); err == nil {
				a = again
			}
			if v, err := agents.Version(ctx, a.Path); err == nil {
				a.Version = v
			}
			h.refreshAgentCfg("codex")
			return map[string]any{"ok": true, "version": a.Version, "output": text}, nil
		}
		return nil, fmt.Errorf("installing this agent CLI is not implemented; install it with its official installer")
	case "agent.uninstall":
		return nil, fmt.Errorf("uninstalling agent CLIs is not implemented in the Go host")

	case "agent.checkUpdates":
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		list := agents.Detect(ctx, !h.cfg.Offline)
		out := make([]any, 0, len(list))
		for _, a := range list {
			src := "path"
			switch a.Source {
			case "managed":
				src = "prometheus"
			case "missing":
				src = "missing"
			}
			item := map[string]any{
				"id": a.ID, "current": a.Version, "latest": a.Latest,
				"updateAvailable": a.UpdateAvailable, "source": src,
				// Claude Code can update itself (`claude update`) no matter how it
				// was installed, including the native installer. Other CLIs are
				// only updatable when the managed npm copy is the active one.
				"canUpdate": a.Source == "managed" || (a.ID == "claude-code" && a.Path != "" && a.Source != "missing") || (a.ID == "codex" && a.Path != "" && agents.InstallMethod(a.Path) != ""),
			}
			if a.Error != "" {
				item["error"] = a.Error
			}
			out = append(out, item)
		}
		return out, nil

	case "workspace.list":
		h.mu.Lock()
		out := append([]any(nil), toAny(h.workspaces)...)
		h.mu.Unlock()
		return out, nil

	case "workspace.add":
		path := normalizeDir(str(p["path"]))
		h.addWorkspace(path)
		h.persist()
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, w := range h.workspaces {
			if w["path"] == path {
				return w, nil
			}
		}
		return nil, fmt.Errorf("workspace add failed")

	case "workspace.remove":
		id := str(p["id"])
		h.mu.Lock()
		for i, w := range h.workspaces {
			if w["id"] == id {
				h.workspaces = append(h.workspaces[:i], h.workspaces[i+1:]...)
				break
			}
		}
		h.mu.Unlock()
		h.persist()
		// Tell the UI to refetch the sidebar; without this the removed workspace
		// stayed visible until a manual reload.
		h.broadcast(map[string]any{"type": "sessions.changed"})
		return nil, nil

	case "workspace.setFolders", "workspace.setWorktree":
		return nil, nil

	case "workspace.open":
		id := str(p["id"])
		h.mu.Lock()
		for _, w := range h.workspaces {
			if w["id"] == id {
				path := str(w["path"])
				h.mu.Unlock()
				_ = openTarget(path)
				return nil, nil
			}
		}
		h.mu.Unlock()
		return nil, nil

	case "workspace.rename":
		id := str(p["id"])
		name := str(p["name"])
		h.mu.Lock()
		var found any
		for _, w := range h.workspaces {
			if w["id"] == id {
				w["name"] = name
				found = w
			}
		}
		h.mu.Unlock()
		h.persist()
		return found, nil

	case "settings.get":
		h.mu.Lock()
		out := cloneMap(h.settings)
		h.mu.Unlock()
		return out, nil

	case "settings.set":
		h.mu.Lock()
		for k, v := range p {
			h.settings[k] = v
		}
		if v, ok := h.settings["worktreeAutoPull"]; ok {
			h.settings["worktreeAutoPull"] = bool_(v)
		}
		if v, ok := h.settings["recycleDays"]; ok {
			days := number(v)
			if days < 1 {
				days = 0
			}
			if days > 3650 {
				days = 3650
			}
			h.settings["recycleDays"] = days
		}
		out := cloneMap(h.settings)
		h.mu.Unlock()
		h.persist()
		return out, nil

	case "gateway.list":
		return h.cfg.GatewayList, nil

	case "gateway.save":
		gw, _ := p["gateway"].(map[string]any)
		list, err := config.UpsertGateway(gw)
		if err != nil {
			return nil, err
		}
		h.cfg.GatewayList = config.Masked(list)
		id := str(gw["id"])
		for i := range list {
			if list[i].ID == id {
				return config.Masked(list[i : i+1])[0], nil
			}
		}
		return nil, nil

	case "gateway.delete":
		list, err := config.DeleteGateway(str(p["id"]))
		if err != nil {
			return nil, err
		}
		h.cfg.GatewayList = config.Masked(list)
		return nil, nil

	case "gateway.test":
		g := h.gateway(str(p["id"]))
		if g == nil {
			return nil, fmt.Errorf("gateway not found")
		}
		ok, latency, message := probeGateway(g)
		return map[string]any{"ok": ok, "latencyMs": latency, "message": message}, nil

	case "gateway.models":
		if raw, ok := p["gateway"].(map[string]any); ok {
			if g := gatewayFromMap(raw); g != nil {
				models, err := fetchModels(g)
				if err != nil {
					return nil, err
				}
				return h.applyModelWindows(models), nil
			}
		}
		g := h.gateway(str(p["id"]))
		if g == nil {
			return nil, fmt.Errorf("gateway not found")
		}
		models, err := fetchModels(g)
		if err != nil {
			return nil, err
		}
		return h.applyModelWindows(models), nil

	case "gateway.pricing":
		return map[string]any{}, nil

	case "gateway.defaults":
		return map[string]any{"source": "", "defaults": map[string]any{}}, nil

	case "gateway.piList":
		return []any{}, nil

	case "gateway.piImport":
		return map[string]any{"imported": []any{}, "skipped": []any{}}, nil

	case "kb.list":
		h.mu.Lock()
		out := make([]any, 0, len(h.kbBases))
		for _, b := range h.kbBases {
			out = append(out, h.kbPublic(b))
		}
		h.mu.Unlock()
		return out, nil

	case "kb.stores":
		out := []any{"memory"}
		if h.cfg.PluginURL != "" {
			client := &http.Client{Timeout: 5 * time.Second}
			if res, err := client.Get(strings.TrimRight(h.cfg.PluginURL, "/") + "/kb/stores"); err == nil {
				var list []string
				_ = json.NewDecoder(res.Body).Decode(&list)
				res.Body.Close()
				for _, id := range list {
					out = append(out, id)
				}
			}
		}
		return out, nil

	case "kb.create":
		id := newID()
		base := map[string]any{
			"id": id, "name": str(p["name"]), "description": str(p["description"]),
			"store": str(p["store"]), "embedding": p["embedding"],
			"createdAt": time.Now().UTC().Format(time.RFC3339),
		}
		if str(base["store"]) == "" {
			base["store"] = "memory"
		}
		h.mu.Lock()
		h.kbBases = append(h.kbBases, base)
		h.kbDocs[id] = []map[string]any{}
		h.mu.Unlock()
		return h.kbPublic(base), nil

	case "kb.update":
		id := str(p["id"])
		h.mu.Lock()
		var out any
		for _, b := range h.kbBases {
			if str(b["id"]) != id {
				continue
			}
			if v, ok := p["name"]; ok {
				b["name"] = v
			}
			if v, ok := p["description"]; ok {
				b["description"] = v
			}
			if v, ok := p["embedding"]; ok {
				b["embedding"] = v
			}
			out = h.kbPublic(b)
		}
		h.mu.Unlock()
		if out == nil {
			return nil, fmt.Errorf("knowledge base not found")
		}
		return out, nil

	case "kb.delete":
		id := str(p["id"])
		base := h.kbBase(id)
		if base != nil && str(base["store"]) != "memory" && h.cfg.PluginURL != "" {
			_, _ = h.pluginKB("drop", map[string]any{
				"store": str(base["store"]), "baseId": id, "dataDir": h.kbDataDir(id),
			})
		}
		h.mu.Lock()
		for i, b := range h.kbBases {
			if str(b["id"]) == id {
				h.kbBases = append(h.kbBases[:i], h.kbBases[i+1:]...)
				break
			}
		}
		delete(h.kbDocs, id)
		h.mu.Unlock()
		return nil, nil

	case "kb.documents":
		id := str(p["id"])
		h.mu.Lock()
		docs := h.kbDocs[id]
		out := make([]any, 0, len(docs))
		for _, d := range docs {
			out = append(out, publicDoc(d))
		}
		h.mu.Unlock()
		return out, nil

	case "kb.ingest":
		id := str(p["id"])
		base := h.kbBase(id)
		if base == nil {
			return nil, fmt.Errorf("knowledge base not found")
		}
		docID := newID()
		text := str(p["text"])
		title := str(p["title"])
		if str(base["store"]) != "memory" && h.cfg.PluginURL != "" {
			vector, err := h.embed(base, text)
			if err != nil {
				return nil, err
			}
			chunk := map[string]any{
				"id": docID, "documentId": docID, "title": title,
				"text": text, "vector": vector,
			}
			if _, err := h.pluginKB("upsert", map[string]any{
				"store": str(base["store"]), "baseId": id, "dataDir": h.kbDataDir(id),
				"dimensions": len(vector), "chunks": []any{chunk},
			}); err != nil {
				return nil, err
			}
		}
		doc := map[string]any{
			"id": docID, "title": title, "text": text,
			"addedAt": time.Now().UTC().Format(time.RFC3339), "chunks": 1,
		}
		h.mu.Lock()
		h.kbDocs[id] = append(h.kbDocs[id], doc)
		h.mu.Unlock()
		return publicDoc(doc), nil

	case "kb.removeDocument":
		id := str(p["id"])
		docID := str(p["documentId"])
		base := h.kbBase(id)
		if base != nil && str(base["store"]) != "memory" && h.cfg.PluginURL != "" {
			_, _ = h.pluginKB("delete", map[string]any{
				"store": str(base["store"]), "baseId": id, "dataDir": h.kbDataDir(id),
				"documentId": docID,
			})
		}
		h.mu.Lock()
		docs := h.kbDocs[id]
		out := docs[:0]
		for _, d := range docs {
			if str(d["id"]) != docID {
				out = append(out, d)
			}
		}
		h.kbDocs[id] = out
		h.mu.Unlock()
		return nil, nil

	case "knowledge_search":
		return h.searchKnowledge(str(p["query"]), int(number(p["topK"])))
	case "kb.search":
		id := str(p["id"])
		q := strings.ToLower(strings.TrimSpace(str(p["query"])))
		topK := number(p["topK"])
		if topK <= 0 {
			topK = 6
		}
		base := h.kbBase(id)
		if base != nil && str(base["store"]) != "memory" && h.cfg.PluginURL != "" {
			vector, err := h.embed(base, q)
			if err != nil {
				return nil, err
			}
			raw, err := h.pluginKB("search", map[string]any{
				"store": str(base["store"]), "baseId": id, "dataDir": h.kbDataDir(id),
				"dimensions": len(vector), "vector": vector, "topK": topK,
			})
			if err != nil {
				return nil, err
			}
			hits := []any{}
			for _, item := range asList(raw) {
				m := asMap(item)
				hits = append(hits, map[string]any{
					"documentId": str(m["documentId"]), "title": str(m["title"]),
					"text": str(m["text"]), "score": m["score"],
				})
			}
			return hits, nil
		}
		h.mu.Lock()
		docs := append([]map[string]any(nil), h.kbDocs[id]...)
		h.mu.Unlock()
		hits := []any{}
		for _, d := range docs {
			text := str(d["text"])
			if q != "" && !strings.Contains(strings.ToLower(text), q) {
				continue
			}
			hits = append(hits, map[string]any{
				"documentId": str(d["id"]), "title": str(d["title"]),
				"text": text, "score": 1.0,
			})
			if len(hits) >= topK {
				break
			}
		}
		return hits, nil

	case "plugin.list":
		return h.pluginList(), nil

	case "plugin.call":
		if h.cfg.PluginURL != "" {
			result, err := h.pluginRPC(str(p["plugin"]), str(p["method"]), asMap(p["params"]))
			if err == nil {
				return result, nil
			}
			return nil, err
		}
		return nil, fmt.Errorf("plugin not loaded: %s", str(p["plugin"]))

	case "plugin.compatibility":
		return []any{}, nil

	case "plugin.scan":
		return h.pluginList(), nil

	case "plugin.install", "plugin.setEnabled", "plugin.reload", "plugin.remove":
		return nil, fmt.Errorf("plugin management is not available in this build")

	case "commands.list":
		return []any{
			map[string]any{"name": "compact", "description": "Compact this conversation", "source": "builtin"},
		}, nil

	case "tools.list":
		return []any{
			map[string]any{"name": "todo_write", "description": "Replace the todo list", "active": true},
			map[string]any{"name": "memory_save", "description": "Save a memory", "active": true},
			map[string]any{"name": "memory_search", "description": "Search memories", "active": true},
			map[string]any{"name": "knowledge_search", "description": "Search knowledge bases", "active": true},
			map[string]any{"name": "open_file", "description": "Show a file in the side viewer", "active": true},
			map[string]any{"name": "bash", "description": "Run a shell command", "active": h.cfg.AllowWrite},
			map[string]any{"name": "read", "description": "Read a file", "active": true},
			map[string]any{"name": "edit", "description": "Edit a file", "active": h.cfg.AllowWrite},
		}, nil

	case "usage.stats":
		h.mu.Lock()
		ids := append([]string(nil), h.order...)
		h.mu.Unlock()
		in, out, reasoning, cr, cw, sessions := 0, 0, 0, 0, 0, 0
		for _, id := range ids {
			s := h.session(id)
			if s == nil {
				continue
			}
			s.mu.Lock()
			if len(s.messages) > 0 {
				sessions++
			}
			in += s.usage.Input
			out += s.usage.Output
			reasoning += s.usage.Reasoning
			cr += s.usage.CacheRead
			cw += s.usage.CacheWrite
			s.mu.Unlock()
		}
		return map[string]any{
			"days": number(p["days"]), "sessions": sessions,
			"totals": map[string]any{
				"input": in, "output": out, "reasoning": reasoning, "cacheRead": cr, "cacheWrite": cw,
				"totalTokens": in + out + reasoning + cr + cw, "cost": 0,
			},
			"byModel": []any{}, "byDay": []any{},
		}, nil

	case "package.optionalList":
		return []any{}, nil

	case "package.optionalInstall", "package.optionalUninstall":
		return nil, fmt.Errorf("optional packages are managed by the Node app")

	case "network.get":
		return h.networkStatus(), nil
	case "network.set":
		return h.applyNetworkPatch(p)

	case "runtime.env":
		return nil, nil

	case "session.warm":
		return nil, nil

	case "subagent.detail", "subagent.stop":
		return nil, fmt.Errorf("subagents are not available")

	case "panel.link":
		return map[string]any{"id": newID()}, nil

	case "worktree.status":
		s := h.session(str(p["sessionId"]))
		if s == nil {
			return nil, fmt.Errorf("sessionNotFound")
		}
		return h.worktreeStatus(s)
	case "worktree.sync":
		s := h.session(str(p["sessionId"]))
		if s == nil {
			return nil, fmt.Errorf("sessionNotFound")
		}
		return h.syncWorktree(s)
	case "worktree.pullBase":
		s := h.session(str(p["sessionId"]))
		if s == nil {
			return nil, fmt.Errorf("sessionNotFound")
		}
		return h.pullBase(s)
	case "worktree.merge":
		s := h.session(str(p["sessionId"]))
		if s == nil {
			return nil, fmt.Errorf("sessionNotFound")
		}
		return h.mergeWorktree(s, str(p["strategy"]), str(p["message"]), str(p["resolve"]))
	case "worktree.restoreBackup":
		s := h.session(str(p["sessionId"]))
		if s == nil {
			return nil, fmt.Errorf("sessionNotFound")
		}
		return h.restoreBackup(s, bool_(p["drop"]))
	case "worktree.discard":
		s := h.session(str(p["sessionId"]))
		if s == nil {
			return nil, fmt.Errorf("sessionNotFound")
		}
		return nil, h.discardWorktree(s)
	case "worktree.diff":
		s := h.session(str(p["sessionId"]))
		if s == nil {
			return nil, fmt.Errorf("sessionNotFound")
		}
		return h.worktreeDiff(s, str(p["path"]))
	case "worktree.pending":
		return h.pendingWorktrees(), nil

	case "ui.respond":
		sid := str(p["sessionId"])
		rid := str(p["requestId"])
		resp, _ := p["response"].(map[string]any)
		h.mu.Lock()
		permCh := h.permPending[rid]
		uiCh := h.uiPending[rid]
		delete(h.permPending, rid)
		delete(h.uiPending, rid)
		delete(h.uiRequests, rid)
		h.mu.Unlock()
		if permCh != nil {
			v := str(resp["value"])
			cancelled, _ := resp["cancelled"].(bool)
			allow := !cancelled && (v == "allow" || v == "always")
			select {
			case permCh <- allow:
			default:
			}
		}
		if uiCh != nil {
			select {
			case uiCh <- resp:
			default:
			}
		}
		h.broadcast(map[string]any{"type": "ui.resolved", "sessionId": sid, "requestId": rid})
		return nil, nil

	case "fs.dirs":
		path := str(p["path"])
		if path == "" {
			path = h.cfg.Cwd
		}
		return listDirs(path, h.cfg.Cwd), nil

	case "fs.mkdir":
		parent := str(p["parent"])
		name := str(p["name"])
		if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
			return nil, fmt.Errorf("invalid folder name")
		}
		next := filepath.Join(parent, name)
		if err := os.MkdirAll(next, 0o755); err != nil {
			return nil, err
		}
		return listDirs(parent, h.cfg.Cwd), nil

	case "fs.pick":
		path, err := pickFolder(str(p["start"]), str(p["title"]))
		if err != nil {
			return nil, err
		}
		if path == "" {
			return map[string]any{"path": nil}, nil
		}
		return map[string]any{"path": path}, nil

	case "system.apps":
		return listExternalApps(), nil

	case "system.openIn":
		if err := openInExternalApp(str(p["app"]), str(p["path"])); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil

	case "system.open":
		url := str(p["url"])
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			return nil, fmt.Errorf("only http(s) links can be opened")
		}
		_ = openTarget(url)
		return map[string]any{"ok": true}, nil

	default:
		return nil, fmt.Errorf("unsupported rpc: %s", method)
	}
}

// ------------------------------------------------------------------ helpers

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

func (h *Host) searchKnowledge(query string, topK int) ([]any, error) {
	if topK <= 0 {
		topK = 6
	}
	h.mu.Lock()
	bases := make([]map[string]any, len(h.kbBases))
	copy(bases, h.kbBases)
	h.mu.Unlock()
	hits := []any{}
	for _, base := range bases {
		id := str(base["id"])
		raw, _ := json.Marshal(map[string]any{"id": id, "query": query, "topK": topK})
		res, err := h.dispatch("kb.search", raw)
		if err != nil {
			continue
		}
		for _, item := range asList(res) {
			hit := asMap(item)
			hit["baseId"] = id
			hit["base"] = str(base["name"])
			hits = append(hits, hit)
		}
	}
	if len(hits) > topK {
		hits = hits[:topK]
	}
	return hits, nil
}

func (h *Host) kbBase(id string) map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, b := range h.kbBases {
		if str(b["id"]) == id {
			return b
		}
	}
	return nil
}

func (h *Host) kbDataDir(id string) string {
	return filepath.Join(h.cfg.DataDir, "knowledge", id)
}

func (h *Host) pluginKB(action string, body map[string]any) (any, error) {
	raw, _ := json.Marshal(body)
	client := &http.Client{Timeout: 120 * time.Second, Transport: &http.Transport{Proxy: nil}}
	res, err := client.Post(strings.TrimRight(h.cfg.PluginURL, "/")+"/kb/"+action, "application/json", strings.NewReader(string(raw)))
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var out struct {
		Result any    `json:"result"`
		Error  string `json:"error"`
	}
	if json.NewDecoder(res.Body).Decode(&out) != nil {
		return nil, fmt.Errorf("knowledge store returned invalid JSON")
	}
	if out.Error != "" {
		return nil, fmt.Errorf("%s", out.Error)
	}
	return out.Result, nil
}

func asList(v any) []any {
	if list, ok := v.([]any); ok {
		return list
	}
	return nil
}

func (h *Host) embed(base map[string]any, text string) ([]float64, error) {
	emb := asMap(base["embedding"])
	gatewayID := str(emb["gatewayId"])
	model := str(emb["model"])
	if gatewayID == "" || model == "" {
		return nil, fmt.Errorf("knowledge base has no embedding model")
	}
	g := h.gateway(gatewayID)
	if g == nil {
		return nil, fmt.Errorf("embedding gateway not found: %s", gatewayID)
	}
	if g.Format != "openai-completions" {
		return nil, fmt.Errorf("gateway %s does not support embeddings", gatewayID)
	}
	baseURL := strings.TrimRight(g.BaseURL, "/")
	candidates := []string{baseURL + "/embeddings"}
	if !strings.HasSuffix(strings.ToLower(baseURL), "/v1") {
		candidates = append(candidates, baseURL+"/v1/embeddings")
	}
	body, _ := json.Marshal(map[string]any{"model": model, "input": []string{text}})
	client := &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{Proxy: nil}}
	last := "no embedding response"
	for _, url := range candidates {
		req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
		if err != nil {
			last = err.Error()
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		if k, v := g.AuthFor(g.Format); k != "" {
			req.Header.Set(k, v)
		}
		for k, v := range g.Headers {
			req.Header.Set(k, v)
		}
		res, err := client.Do(req)
		if err != nil {
			last = err.Error()
			continue
		}
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			last = fmt.Sprintf("HTTP %d", res.StatusCode)
			res.Body.Close()
			continue
		}
		var out struct {
			Data []struct {
				Embedding []float64 `json:"embedding"`
			} `json:"data"`
		}
		err = json.NewDecoder(res.Body).Decode(&out)
		res.Body.Close()
		if err != nil {
			last = err.Error()
			continue
		}
		if len(out.Data) == 0 || len(out.Data[0].Embedding) == 0 {
			last = "empty embedding"
			continue
		}
		return out.Data[0].Embedding, nil
	}
	return nil, fmt.Errorf("embedding failed: %s", last)
}

func (h *Host) kbPublic(b map[string]any) map[string]any {
	id := str(b["id"])
	docs := h.kbDocs[id]
	chunks := 0
	for _, d := range docs {
		chunks += number(d["chunks"])
	}
	out := cloneMap(b)
	out["documentCount"] = len(docs)
	out["chunkCount"] = chunks
	return out
}

func publicDoc(d map[string]any) map[string]any {
	return map[string]any{
		"id": str(d["id"]), "title": str(d["title"]),
		"chunks": number(d["chunks"]), "addedAt": str(d["addedAt"]),
	}
}

func (h *Host) resolvePath(s *Session, p string) (string, error) {
	base := h.cfg.Cwd
	if s != nil && s.Cwd != "" {
		base = s.Cwd
	}
	if p == "" {
		p = base
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	return filepath.Abs(p)
}

// TakeDownload consumes a one-minute download ticket created by file.link.
func (h *Host) TakeDownload(id string) (path, name string, ok bool) {
	h.mu.Lock()
	t, exists := h.tickets[id]
	if exists {
		delete(h.tickets, id)
	}
	h.mu.Unlock()
	if !exists || time.Now().After(t.Expires) {
		return "", "", false
	}
	return t.Path, t.Name, true
}

func listDirs(path, workspaceRoot string) map[string]any {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	dirs := []any{}
	if entries, err := os.ReadDir(abs); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				dirs = append(dirs, map[string]any{
					"name": e.Name(), "path": filepath.Join(abs, e.Name()),
				})
			}
		}
	}
	var parent any
	if p := filepath.Dir(abs); p != abs {
		parent = p
	}
	return map[string]any{
		"path": abs, "parent": parent, "dirs": dirs,
		"roots": roots(), "home": homeDir(), "workspaceRoot": workspaceRoot,
	}
}

func openTarget(target string) error {
	if target == "" {
		return nil
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
			cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
		} else {
			cmd = exec.Command("explorer", target)
		}
	case "darwin":
		cmd = exec.Command("open", target)
	default:
		if p, err := exec.LookPath("xdg-open"); err == nil {
			cmd = exec.Command(p, target)
		} else if p, err := exec.LookPath("gio"); err == nil {
			cmd = exec.Command(p, "open", target)
		} else {
			return fmt.Errorf("找不到 xdg-open 或 gio，无法打开文件管理器")
		}
	}
	return cmd.Start()
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func (h *Host) workspaceRefs() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]map[string]any, 0, len(h.workspaces))
	for _, w := range h.workspaces {
		out = append(out, map[string]any{
			"id": str(w["id"]), "path": str(w["path"]), "name": str(w["name"]),
		})
	}
	return out
}

const memoryGuidance = "# Long-term memory\nBefore saving or searching durable facts, inspect the available tools. If this session has a memory tool from another MCP server (for example mcp__Magpie__magpie-memory__memory_save / memory_search), use that server's memory tools, because that store is shared with the other agents. Only when no such MCP memory tool is available, use mcp__ramicore__memory_save and mcp__ramicore__memory_search. RamiCore's Memory page shows only its own store; entries written to Magpie memory stay in Magpie."

func withMemoryGuidance(static string) string {
	if strings.TrimSpace(static) == "" {
		return memoryGuidance
	}
	return static + "\n\n" + memoryGuidance
}

func (h *Host) pluginContext(prompt string, s *Session) (static, turn string) {
	if h.cfg.PluginURL == "" {
		return withMemoryGuidance(h.knowledgeContext()), ""
	}
	body, _ := json.Marshal(map[string]any{
		"prompt": prompt, "cwd": s.Cwd, "sessionId": s.ID,
		"workspaces": h.workspaceRefs(),
	})
	client := &http.Client{Timeout: 6 * time.Second}
	res, err := client.Post(strings.TrimRight(h.cfg.PluginURL, "/")+"/context", "application/json", strings.NewReader(string(body)))
	if err != nil {
		return withMemoryGuidance(h.knowledgeContext()), ""
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return withMemoryGuidance(h.knowledgeContext()), ""
	}
	var out struct {
		Static struct {
			Sections   map[string]string `json:"sections"`
			Guidelines []string          `json:"guidelines"`
		} `json:"static"`
		Turn []string `json:"turn"`
	}
	if json.NewDecoder(res.Body).Decode(&out) != nil {
		return withMemoryGuidance(h.knowledgeContext()), ""
	}
	var b strings.Builder
	keys := make([]string, 0, len(out.Static.Sections))
	for k := range out.Static.Sections {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString("# " + k + "\n" + out.Static.Sections[k] + "\n\n")
	}
	if len(out.Static.Guidelines) > 0 {
		b.WriteString("# Guidelines\n")
		for _, g := range out.Static.Guidelines {
			b.WriteString("- " + g + "\n")
		}
	}
	if kb := h.knowledgeContext(); kb != "" {
		b.WriteString("\n" + kb + "\n")
	}
	return strings.TrimSpace(b.String()), strings.TrimSpace(strings.Join(out.Turn, "\n\n"))
}

func (h *Host) knowledgeContext() string {
	h.mu.Lock()
	bases := make([]map[string]any, len(h.kbBases))
	copy(bases, h.kbBases)
	h.mu.Unlock()
	if len(bases) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Knowledge bases\nSearch these with the knowledge_search tool when the answer may be in the user's documents.\n")
	for _, base := range bases {
		b.WriteString("- " + str(base["name"]) + " (id " + str(base["id"]) + ")")
		if d := str(base["description"]); d != "" {
			b.WriteString(": " + d)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func withContext(static, turn, text string) string {
	if static == "" && turn == "" {
		return text
	}
	var b strings.Builder
	b.WriteString("<prometheus_context>\n")
	if static != "" {
		b.WriteString(static)
		b.WriteString("\n")
	}
	if turn != "" {
		b.WriteString(turn)
		b.WriteString("\n")
	}
	b.WriteString("</prometheus_context>\n\n")
	b.WriteString(text)
	return b.String()
}

func (h *Host) modelContextWindow(modelID string) int {
	if modelID == "" {
		return 0
	}
	for _, raw := range h.cfg.GatewayList {
		g := asMap(raw)
		if enabled, ok := g["enabled"].(bool); ok && !enabled {
			continue
		}
		for _, mr := range asList(g["models"]) {
			m := asMap(mr)
			if str(m["id"]) == modelID {
				if cw := number(m["contextWindow"]); cw > 0 {
					return cw
				}
			}
		}
	}
	if cfg, err := config.Load(); err == nil {
		for _, g := range cfg.Gateways {
			if !g.IsEnabled() {
				continue
			}
			for _, m := range g.Models {
				if m.ID == modelID && m.ContextWindow > 0 {
					return m.ContextWindow
				}
			}
		}
	}
	return 0
}

func (h *Host) networkFile() string {
	return filepath.Join(h.cfg.DataDir, "network.json")
}

// loadNetwork restores LAN settings from the previous run. Sessions are not
// persisted: clients sign in again after a restart.
func (h *Host) loadNetwork() {
	raw, err := os.ReadFile(h.networkFile())
	if err != nil {
		return
	}
	var saved map[string]any
	if json.Unmarshal(raw, &saved) != nil || saved == nil {
		return
	}
	h.mu.Lock()
	migrated := false
	if v, ok := saved["lan"]; ok {
		h.network["lan"] = bool_(v)
	}
	if v, ok := saved["port"]; ok {
		if p := number(v); p >= 1024 && p <= 65535 {
			// 7421 was the old default; move saved configurations to the new one.
			if p == 7421 {
				p = 9528
				migrated = true
			}
			h.network["port"] = p
		}
	}
	if v, ok := saved["allowedHosts"]; ok {
		h.network["allowedHosts"] = v
	}
	if v, ok := saved["passwordSalt"].(string); ok && v != "" {
		if hash, _ := saved["passwordHash"].(string); hash != "" {
			h.network["passwordSalt"] = v
			h.network["passwordHash"] = hash
		}
	}
	if v, ok := saved["sessionSecret"].(string); ok && v != "" {
		h.network["sessionSecret"] = v
	} else if _, ok := h.network["passwordHash"].(string); ok {
		// First run of the signed-token version: mint a persistent secret so
		// tokens issued now survive the next restart.
		h.networkSecretLocked()
		migrated = true
	}
	h.mu.Unlock()
	if migrated {
		// Persist the new default so an older Prometheus sharing this data
		// directory cannot keep advertising the retired port.
		h.saveNetwork()
	}
}

func (h *Host) saveNetwork() {
	h.mu.Lock()
	saved := map[string]any{}
	for _, k := range []string{"lan", "port", "allowedHosts", "passwordSalt", "passwordHash", "sessionSecret"} {
		if v, ok := h.network[k]; ok {
			saved[k] = v
		}
	}
	h.mu.Unlock()
	if err := os.MkdirAll(h.cfg.DataDir, 0o755); err != nil {
		return
	}
	raw, err := json.MarshalIndent(saved, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(h.networkFile(), raw, 0o600)
}

func tunnelActive(m map[string]any) bool {
	t, _ := m["tunnel"].(map[string]any)
	if t == nil {
		return false
	}
	switch str(t["state"]) {
	case "downloading", "starting", "on":
		return true
	}
	return false
}

func (h *Host) applyNetworkPatch(p map[string]any) (any, error) {
	if pw, ok := p["password"]; ok {
		h.setNetworkPassword(str(pw))
		h.saveNetwork()
	}

	h.mu.Lock()
	oldLan := h.network["lan"] == true
	oldPort := number(h.network["port"])
	oldActive := tunnelActive(h.network)

	if v, ok := p["allowedHosts"]; ok {
		switch list := v.(type) {
		case []any:
			h.network["allowedHosts"] = list
		case []string:
			out := make([]any, len(list))
			for i, s := range list {
				out[i] = s
			}
			h.network["allowedHosts"] = out
		case string:
			out := []any{}
			for _, line := range strings.Split(list, "\n") {
				if s := strings.TrimSpace(line); s != "" {
					out = append(out, s)
				}
			}
			h.network["allowedHosts"] = out
		}
	}
	portChanged := false
	if v, ok := p["port"]; ok {
		port := number(v)
		if port < 1024 || port > 65535 {
			h.mu.Unlock()
			return nil, fmt.Errorf("端口需在 1024–65535 之间")
		}
		if port != oldPort {
			h.network["port"] = port
			portChanged = true
		}
	}
	if v, ok := p["lan"]; ok {
		h.network["lan"] = bool_(v)
	}
	if v, ok := p["revokeSessions"]; ok && bool_(v) {
		h.revokeNetworkSessionsLocked()
	}

	lan := h.network["lan"] == true
	port := number(h.network["port"])
	if port == 0 {
		port = 9528
	}
	wantTunnel := oldActive
	if v, ok := p["tunnel"]; ok {
		wantTunnel = bool_(v)
	}
	hasPassword := h.hasNetworkPasswordLocked()
	h.mu.Unlock()

	if (lan || wantTunnel) && !hasPassword {
		if wantTunnel && !oldActive {
			h.tunnelState("off", "", "", false)
		}
		return nil, fmt.Errorf("请先设置登录密码，才能开启局域网访问")
	}

	if oldActive && !wantTunnel {
		h.stopTunnel()
	}
	restartTunnel := oldActive && wantTunnel && portChanged
	if restartTunnel {
		h.stopTunnel()
	}
	startTunnel := wantTunnel && (!oldActive || restartTunnel)
	if startTunnel {
		h.tunnelState("downloading", "", "", false)
	}

	newNeed := lan || wantTunnel
	oldNeed := oldLan || oldActive
	if apply := h.applyNet; apply != nil && (oldNeed != newNeed || (portChanged && newNeed)) {
		if newNeed {
			if err := apply(true, port); err != nil {
				if wantTunnel {
					h.tunnelState("error", "", err.Error(), true)
				}
				return nil, err
			}
		} else {
			_ = apply(false, port)
		}
	}
	h.saveNetwork()

	if startTunnel || restartTunnel {
		h.startTunnel(port)
	}
	return h.networkStatus(), nil
}

func (h *Host) tunnelState(state, url, msg string, network bool) {
	h.setTunnelState(0, state, url, msg, network)
}

// setTunnelState updates the tunnel snapshot. A non-zero gen only applies while
// it is still the current tunnel process, so a stopped run cannot overwrite a
// newer one.
func (h *Host) setTunnelState(gen int, state, url, msg string, network bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if gen != 0 && gen != h.tunnelGen {
		return
	}
	if gen != 0 && (state == "on" || state == "error") {
		h.tunnelProc = nil
		h.tunnelStop = nil
	}
	t := map[string]any{"state": state}
	if url != "" {
		t["url"] = url
	}
	if msg != "" {
		t["error"] = strings.TrimSpace(msg)
		t["network"] = network
	}
	h.network["tunnel"] = t
}

func (h *Host) stopTunnel() {
	h.mu.Lock()
	h.tunnelGen++
	h.mu.Unlock()
	h.terminateTunnel()
	h.setTunnelState(0, "off", "", "", false)
}

// terminateTunnel stops the process tree without touching the reported state;
// callers decide whether that becomes "off" or an error.
func (h *Host) terminateTunnel() {
	h.mu.Lock()
	cancel := h.tunnelStop
	proc := h.tunnelProc
	h.tunnelStop = nil
	h.tunnelProc = nil
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if proc != nil && proc.Process != nil {
		if runtime.GOOS == "windows" {
			_ = exec.Command("taskkill", "/PID", fmt.Sprint(proc.Process.Pid), "/T", "/F").Run()
		} else {
			_ = proc.Process.Kill()
		}
	}
}

func (h *Host) startTunnel(port int) {
	h.mu.Lock()
	if h.tunnelStop != nil {
		h.mu.Unlock()
		return
	}
	h.tunnelGen++
	gen := h.tunnelGen
	ctx, cancel := context.WithCancel(context.Background())
	h.tunnelStop = cancel
	h.mu.Unlock()
	go h.runTunnel(ctx, gen, port)
}

var trycloudURL = regexp.MustCompile(`https://[a-zA-Z0-9-]+\.trycloudflare\.com`)

func (h *Host) runTunnel(ctx context.Context, gen, port int) {
	bin := h.cloudflaredPath()
	if _, err := os.Stat(bin); err != nil {
		if err := downloadCloudflared(bin); err != nil {
			if ctx.Err() == nil {
				h.setTunnelState(gen, "error", "", err.Error(), tunnelNetworkError(err.Error()))
			}
			return
		}
	}
	if ctx.Err() != nil {
		return
	}
	h.setTunnelState(gen, "starting", "", "", false)

	cmd := exec.CommandContext(ctx, bin, "tunnel", "--url", fmt.Sprintf("http://127.0.0.1:%d", port), "--no-autoupdate")
	// Never run the tunnel with the install folder as its working directory:
	// a leftover cloudflared would keep that folder locked after a crash.
	if home := config.Home(); home != "" {
		cmd.Dir = home
	}
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		if ctx.Err() == nil {
			h.setTunnelState(gen, "error", "", "启动 cloudflared 失败："+err.Error(), tunnelNetworkError(err.Error()))
		}
		return
	}
	h.mu.Lock()
	if h.tunnelGen == gen {
		h.tunnelProc = cmd
	}
	h.mu.Unlock()

	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		_ = pw.Close()
		close(done)
	}()

	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	var tail []string
	pendingURL := ""
	timer := time.AfterFunc(90*time.Second, func() {
		if ctx.Err() != nil {
			return
		}
		h.setTunnelState(gen, "error", "", "等待 Cloudflare 分配地址超时", true)
		h.terminateTunnel()
	})
	defer timer.Stop()
	for sc.Scan() {
		line := sc.Text()
		if m := trycloudURL.FindString(line); m != "" && !strings.Contains(strings.ToLower(m), "api.trycloudflare.com") {
			pendingURL = m
		}
		// cloudflared prints the address well before the edge is ready; only
		// report "on" once a connection has actually been registered.
		if pendingURL != "" && strings.Contains(line, "Registered tunnel connection") {
			if ctx.Err() == nil {
				timer.Stop()
				h.setTunnelState(gen, "on", pendingURL, "", false)
			}
			continue
		}
		if s := strings.TrimSpace(line); s != "" {
			tail = append(tail, s)
			if len(tail) > 6 {
				tail = tail[1:]
			}
		}
	}
	<-done
	if ctx.Err() != nil {
		return
	}
	msg := strings.Join(tail, "\n")
	if msg == "" {
		msg = "cloudflared 已退出"
	}
	h.setTunnelState(gen, "error", "", msg, tunnelNetworkError(msg))
}

func tunnelNetworkError(msg string) bool {
	low := strings.ToLower(msg)
	for _, s := range []string{
		"dial tcp", "no such host", "connection refused", "i/o timeout", "timeout",
		"tls handshake", "proxyconnect", "network is unreachable", "failed to request quick tunnel",
		"failed to dial", "context deadline exceeded", "certificate",
	} {
		if strings.Contains(low, s) {
			return true
		}
	}
	return false
}

func (h *Host) cloudflaredPath() string {
	name := "cloudflared"
	if runtime.GOOS == "windows" {
		name = "cloudflared.exe"
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	// Same place the original Prometheus server keeps it: <home>/bin.
	for _, dir := range []string{filepath.Join(config.Home(), "bin"), filepath.Join(h.cfg.DataDir, "bin")} {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return filepath.Join(config.Home(), "bin", name)
}

func cloudflaredAsset() (string, bool, error) {
	goos, arch := runtime.GOOS, runtime.GOARCH
	switch goos {
	case "windows":
		switch arch {
		case "amd64", "arm64":
			return "cloudflared-windows-" + arch + ".exe", false, nil
		case "386":
			return "cloudflared-windows-386.exe", false, nil
		}
	case "linux":
		switch arch {
		case "amd64", "arm64", "arm", "386", "ppc64le", "s390x":
			return "cloudflared-linux-" + arch, false, nil
		}
	case "darwin":
		switch arch {
		case "amd64", "arm64":
			return "cloudflared-darwin-" + arch + ".tgz", true, nil
		}
	}
	return "", false, fmt.Errorf("当前平台 %s/%s 暂不支持自动下载 cloudflared，请手动安装后重试", goos, arch)
}

// proxyURL mirrors the original server: environment first, then the Windows
// system proxy. curl honours both; Go's HTTP client only reads the environment.
func proxyURL() string {
	for _, key := range []string{"HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy", "HTTP_PROXY", "http_proxy"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	if runtime.GOOS != "windows" {
		return ""
	}
	out, err := exec.Command("reg", "query", `HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings`, "/v", "ProxyServer").Output()
	if err != nil {
		return ""
	}
	m := regexp.MustCompile(`ProxyServer\s+REG_SZ\s+(\S+)`).FindStringSubmatch(string(out))
	if len(m) < 2 || m[1] == "" {
		return ""
	}
	v := m[1]
	if strings.Contains(v, "=") {
		if hm := regexp.MustCompile(`https=([^;]+)`).FindStringSubmatch(v); len(hm) > 1 {
			v = hm[1]
		} else if hm := regexp.MustCompile(`http=([^;]+)`).FindStringSubmatch(v); len(hm) > 1 {
			v = hm[1]
		}
	}
	if v != "" && !strings.Contains(v, "://") {
		v = "http://" + v
	}
	return v
}

func fetchCloudflared(url, dest string) error {
	proxy := proxyURL()
	if curl, err := exec.LookPath("curl"); err == nil {
		args := []string{"-fL", "--retry", "2", "--connect-timeout", "20", "-o", dest}
		if proxy != "" {
			args = append(args, "-x", proxy)
		}
		args = append(args, url)
		out, err := exec.Command(curl, args...).CombinedOutput()
		if err != nil {
			msg := lastLine(strings.TrimSpace(string(out)))
			if msg == "" {
				msg = err.Error()
			}
			hint := "（未检测到代理）"
			if proxy != "" {
				hint = "（代理 " + proxy + "）"
			}
			return fmt.Errorf("下载 cloudflared 失败%s：%s。可手动安装 cloudflared 并加入 PATH 后重试", hint, msg)
		}
		return nil
	}
	// curl is missing (minimal Linux): fall back to Go's HTTP client.
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("下载 cloudflared 失败：%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载 cloudflared 失败：HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		_ = f.Close()
		return fmt.Errorf("下载 cloudflared 失败：%v", err)
	}
	return f.Close()
}

func extractCloudflaredTarGz(archive, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("解压 cloudflared 失败：%v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = out.Close()
		if !ok {
			_ = os.Remove(dest)
		}
	}()
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("解压 cloudflared 失败：%v", err)
		}
		if hdr.Typeflag == tar.TypeReg && filepath.Base(hdr.Name) == "cloudflared" {
			if _, err := io.Copy(out, tr); err != nil {
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
			ok = true
			return nil
		}
	}
	return fmt.Errorf("cloudflared 压缩包中没有找到可执行文件")
}

func downloadCloudflared(dest string) error {
	asset, tarred, err := cloudflaredAsset()
	if err != nil {
		return err
	}
	url := "https://github.com/cloudflare/cloudflared/releases/latest/download/" + asset
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	archive := dest + ".download"
	if tarred {
		archive += ".tgz"
	}
	defer os.Remove(archive)
	if err := fetchCloudflared(url, archive); err != nil {
		return err
	}
	if tarred {
		if err := extractCloudflaredTarGz(archive, dest); err != nil {
			return err
		}
	} else if err := os.Rename(archive, dest); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(dest, 0o755)
	}
	return nil
}

func (h *Host) networkStatus() map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	port := number(h.network["port"])
	if port == 0 {
		port = 9528
	}
	hosts := h.network["allowedHosts"]
	if hosts == nil {
		hosts = []any{}
	}
	tunnel, _ := h.network["tunnel"].(map[string]any)
	if tunnel == nil {
		tunnel = map[string]any{"state": "off"}
	}
	tunnel = cloneMap(tunnel)
	lan := h.network["lan"] == true
	local := h.localPort
	if local == 0 {
		local = 9528
	}
	addresses := []any{}
	if lan {
		for _, ip := range lanAddresses() {
			addresses = append(addresses, ip)
		}
	}
	return map[string]any{
		"tunnel": tunnel, "managed": false, "lan": lan,
		"port": port, "allowedHosts": hosts,
		"passwordSet": h.hasNetworkPasswordLocked(),
		"listening":   lan || tunnelActive(h.network),
		"addresses":   addresses, "localPort": local, "canEdit": true,
	}
}

func (h *Host) hasNetworkPasswordLocked() bool {
	hash, _ := h.network["passwordHash"].(string)
	return hash != ""
}

// AllowsHost enforces the domain allow-list for LAN requests. IP literals and
// localhost are always allowed; the temporary Cloudflare hostname is allowed
// while the tunnel is running.
func (h *Host) AllowsHost(hostport string) bool {
	host := hostport
	if hh, _, err := net.SplitHostPort(hostport); err == nil {
		host = hh
	}
	host = strings.Trim(strings.ToLower(host), "[]")
	host = strings.TrimSuffix(host, ".")
	if host == "" || host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return true
	}
	h.mu.Lock()
	hosts := h.network["allowedHosts"]
	tunnel := tunnelActive(h.network)
	h.mu.Unlock()
	if tunnel && (host == "trycloudflare.com" || strings.HasSuffix(host, ".trycloudflare.com")) {
		return true
	}
	var list []string
	switch v := hosts.(type) {
	case []any:
		for _, x := range v {
			list = append(list, str(x))
		}
	case []string:
		list = v
	case string:
		list = strings.Split(v, "\n")
	}
	for _, pat := range list {
		pat = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(pat)), ".")
		if pat == "" {
			continue
		}
		if strings.HasPrefix(pat, "*.") {
			suffix := pat[1:]
			if strings.HasSuffix(host, suffix) || host == strings.TrimPrefix(suffix, ".") {
				return true
			}
		} else if host == pat {
			return true
		}
	}
	return false
}

func lanAddresses() []string {
	out := []string{}
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			var ip net.IP
			switch a := addr.(type) {
			case *net.IPNet:
				ip = a.IP
			case *net.IPAddr:
				ip = a.IP
			}
			if ip == nil || ip.IsLoopback() || ip.To4() == nil {
				continue
			}
			out = append(out, ip.String())
		}
	}
	return out
}

func (h *Host) pluginList() []any {
	if h.cfg.PluginURL == "" {
		return []any{}
	}
	client := &http.Client{Timeout: 5 * time.Second}
	res, err := client.Get(strings.TrimRight(h.cfg.PluginURL, "/") + "/plugins")
	if err != nil {
		return []any{}
	}
	defer res.Body.Close()
	var list []map[string]any
	if res.StatusCode != http.StatusOK || json.NewDecoder(res.Body).Decode(&list) != nil {
		return []any{}
	}
	out := make([]any, 0, len(list))
	for _, item := range list {
		panels := item["panels"]
		if panels == nil {
			panels = []any{}
		}
		out = append(out, map[string]any{
			"id": str(item["id"]), "name": str(item["name"]),
			"source": "builtin", "status": "loaded",
			"panels": panels, "toggleable": false, "removable": false, "notes": []any{},
		})
	}
	return out
}

func (h *Host) pluginRPC(plugin, method string, params map[string]any) (any, error) {
	if plugin == "" || method == "" {
		return nil, fmt.Errorf("plugin and method are required")
	}
	body, _ := json.Marshal(map[string]any{
		"plugin": plugin, "method": method, "params": params,
		"workspaces": h.workspaceRefs(),
	})
	client := &http.Client{Timeout: 120 * time.Second}
	res, err := client.Post(strings.TrimRight(h.cfg.PluginURL, "/")+"/rpc", "application/json", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var out struct {
		Result any    `json:"result"`
		Error  string `json:"error"`
	}
	if json.NewDecoder(res.Body).Decode(&out) != nil {
		return nil, fmt.Errorf("plugin rpc returned invalid JSON")
	}
	if out.Error != "" {
		return nil, fmt.Errorf("%s", out.Error)
	}
	return out.Result, nil
}

func (h *Host) gateway(id string) *config.Gateway {
	cfg, err := config.Load()
	if err != nil {
		return nil
	}
	for i := range cfg.Gateways {
		if cfg.Gateways[i].ID == id {
			return &cfg.Gateways[i]
		}
	}
	return nil
}

func gatewayFromMap(m map[string]any) *config.Gateway {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	var g config.Gateway
	if json.Unmarshal(raw, &g) != nil {
		return nil
	}
	return &g
}

func probeGateway(g *config.Gateway) (bool, int, string) {
	start := time.Now()
	base := strings.TrimRight(g.BaseURL, "/")
	candidates := []string{base + "/models"}
	if !strings.HasSuffix(strings.ToLower(base), "/v1") {
		candidates = append(candidates, base+"/v1/models")
	}
	client := &http.Client{Timeout: 6 * time.Second, Transport: &http.Transport{Proxy: nil}}
	last := "no response"
	for _, u := range candidates {
		req, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			last = err.Error()
			continue
		}
		if k, v := g.AuthFor(g.Format); k != "" {
			req.Header.Set(k, v)
		}
		for k, v := range g.Headers {
			req.Header.Set(k, v)
		}
		if g.Format == "anthropic-messages" {
			req.Header.Set("anthropic-version", "2023-06-01")
		}
		res, err := client.Do(req)
		if err != nil {
			last = err.Error()
			continue
		}
		_ = res.Body.Close()
		ms := int(time.Since(start).Milliseconds())
		if res.StatusCode >= 200 && res.StatusCode < 300 {
			return true, ms, fmt.Sprintf("HTTP %d", res.StatusCode)
		}
		last = fmt.Sprintf("HTTP %d", res.StatusCode)
	}
	return false, int(time.Since(start).Milliseconds()), last
}

func fetchModels(g *config.Gateway) ([]any, error) {
	base := strings.TrimRight(g.BaseURL, "/")
	candidates := []string{base + "/models"}
	if !strings.HasSuffix(strings.ToLower(base), "/v1") {
		candidates = append(candidates, base+"/v1/models")
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}
	last := "no response"
	for _, u := range candidates {
		req, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			last = err.Error()
			continue
		}
		if k, v := g.AuthFor(g.Format); k != "" {
			req.Header.Set(k, v)
		}
		for k, v := range g.Headers {
			req.Header.Set(k, v)
		}
		if g.Format == "anthropic-messages" {
			req.Header.Set("anthropic-version", "2023-06-01")
		}
		res, err := client.Do(req)
		if err != nil {
			last = err.Error()
			continue
		}
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			last = fmt.Sprintf("HTTP %d", res.StatusCode)
			_ = res.Body.Close()
			continue
		}
		var payload struct {
			Data []struct {
				ID              string          `json:"id"`
				Name            string          `json:"name"`
				DisplayName     string          `json:"display_name"`
				ContextWindow   int             `json:"context_window"`
				ContextLength   int             `json:"context_length"`
				MaxInput        int             `json:"max_input_tokens"`
				SupportsVision  json.RawMessage `json:"supports_vision"`
				Modalities      json.RawMessage `json:"modalities"`
				InputModalities json.RawMessage `json:"input_modalities"`
				Architecture    struct {
					Modality        string          `json:"modality"`
					InputModalities json.RawMessage `json:"input_modalities"`
				} `json:"architecture"`
				Capabilities json.RawMessage `json:"capabilities"`
			} `json:"data"`
		}
		err = json.NewDecoder(res.Body).Decode(&payload)
		_ = res.Body.Close()
		if err != nil {
			last = err.Error()
			continue
		}
		out := make([]any, 0, len(payload.Data))
		for _, m := range payload.Data {
			name := m.Name
			if name == "" {
				name = m.DisplayName
			}
			if name == "" {
				name = m.ID
			}
			cw := m.ContextWindow
			if cw <= 0 {
				cw = m.ContextLength
			}
			if cw <= 0 {
				cw = m.MaxInput
			}
			vision := anyHasVision(m.SupportsVision) || anyHasVision(m.Capabilities) ||
				anyHasVision(m.Modalities) || anyHasVision(m.InputModalities) ||
				anyHasVision(m.Architecture.InputModalities) ||
				strings.Contains(strings.ToLower(m.Architecture.Modality), "image")
			if !vision {
				vision = modelLooksVision(m.ID)
			}
			entry := map[string]any{
				"id": m.ID, "name": name, "reasoning": true,
				"vision": vision, "contextWindow": cw,
			}
			out = append(out, entry)
		}
		return out, nil
	}
	return nil, fmt.Errorf("gateway models: %s", last)
}

func (h *Host) applyModelWindows(models []any) []any {
	for _, raw := range models {
		m := asMap(raw)
		cw := h.modelContextWindow(str(m["id"]))
		if cw <= 0 {
			cw = number(m["contextWindow"])
		}
		if cw <= 0 {
			cw = 200_000
		}
		m["contextWindow"] = cw
	}
	return models
}

func boolDefault(v any, def bool) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return def
}

// gatewayModelList flattens every model of the configured gateway(s) into the
// shape the model picker expects, so all configured models are selectable.
func (h *Host) gatewayModelList() []any {
	out := []any{}
	seen := map[string]bool{}
	for _, raw := range h.cfg.GatewayList {
		g := asMap(raw)
		if enabled, ok := g["enabled"].(bool); ok && !enabled {
			continue
		}
		gid := str(g["id"])
		if gid == "" {
			gid = h.cfg.Provider
		}
		for _, mr := range asList(g["models"]) {
			m := asMap(mr)
			id := str(m["id"])
			if id == "" {
				continue
			}
			key := gid + "\x00" + id
			if seen[key] {
				continue
			}
			seen[key] = true
			name := str(m["name"])
			if name == "" {
				name = id
			}
			cw := number(m["contextWindow"])
			if cw <= 0 {
				cw = 200_000
			}
			out = append(out, map[string]any{
				"provider": gid, "id": id, "name": name,
				"reasoning":     boolDefault(m["reasoning"], true),
				"vision":        boolDefault(m["vision"], false),
				"contextWindow": cw,
			})
		}
	}
	return out
}

func (h *Host) modelList() []any {
	if models := h.gatewayModelList(); len(models) > 0 {
		return models
	}
	window := h.modelContextWindow(h.cfg.Model)
	if window <= 0 {
		window = 200_000
	}
	return []any{map[string]any{
		"provider": h.cfg.Provider, "id": h.cfg.Model, "name": h.cfg.Model,
		"reasoning": true, "vision": false, "contextWindow": window,
	}}
}

func (h *Host) piModelList() []any {
	if models := h.gatewayModelList(); len(models) > 0 {
		for _, raw := range models {
			asMap(raw)["thinkingLevels"] = []any{"off", "minimal", "low", "medium", "high", "xhigh"}
		}
		return models
	}
	return []any{map[string]any{
		"provider": "pi", "id": "default", "name": "PI (configured model)",
		"reasoning": true, "vision": true, "contextWindow": 200_000,
		"thinkingLevels": []any{"off", "minimal", "low", "medium", "high", "xhigh"},
	}}
}

func piCapabilities() map[string]any {
	return map[string]any{
		"steering": true, "followUpQueue": true, "compaction": "manual+auto",
		"fork": true, "treeNavigation": true,
		"thinkingLevels": []any{"off", "minimal", "low", "medium", "high", "xhigh"},
		"images":         true, "liveToolListChange": true, "systemPromptReplace": "live",
		"contextBreakdown": true,
		"permissionModes":  []any{"default", "acceptEdits", "plan", "bypassPermissions", "auto"},
		"nativeTools":      []any{"exec", "read", "edit", "write", "search"},
	}
}

func openCodeCapabilities() map[string]any {
	return map[string]any{
		"steering": true, "followUpQueue": true, "compaction": "manual+auto",
		"fork": false, "treeNavigation": false, "thinkingLevels": []any{},
		"images": true, "liveToolListChange": false, "systemPromptReplace": "none",
		"contextBreakdown": false,
		"permissionModes":  []any{"default", "acceptEdits", "plan", "bypassPermissions", "auto"},
		"nativeTools":      []any{"exec", "read", "edit", "write", "search", "fetch", "todo", "task"},
	}
}

func acpCapabilities() map[string]any {
	return map[string]any{
		"steering": false, "followUpQueue": true, "compaction": "none",
		"fork": false, "treeNavigation": false, "thinkingLevels": []any{},
		"images": true, "liveToolListChange": false, "systemPromptReplace": "none",
		"contextBreakdown": false,
		"permissionModes":  []any{"default", "acceptEdits", "plan", "bypassPermissions", "auto"},
		"nativeTools":      []any{"exec", "read", "edit", "write", "search", "fetch", "todo"},
	}
}

func codexCapabilities() map[string]any {
	return map[string]any{
		"steering": true, "followUpQueue": true, "compaction": "manual+auto",
		"fork": false, "treeNavigation": false,
		"thinkingLevels": []any{"minimal", "medium", "high", "xhigh", "max"},
		"images":         true, "liveToolListChange": false, "systemPromptReplace": "none",
		"contextBreakdown": false,
		"permissionModes":  []any{"default", "acceptEdits", "plan", "bypassPermissions", "auto"},
		"nativeTools":      []any{"exec", "edit", "write", "fetch", "todo"},
	}
}

func ccCapabilities() map[string]any {
	return map[string]any{
		"steering": true, "followUpQueue": true, "compaction": "manual+auto",
		"fork": false, "treeNavigation": false, "thinkingLevels": []any{"off", "low", "medium", "high", "xhigh", "max"},
		"images": true, "liveToolListChange": true, "systemPromptReplace": "restart",
		"contextBreakdown": false,
		"permissionModes":  []any{"default", "acceptEdits", "plan", "bypassPermissions", "auto"},
		"nativeTools":      []any{"exec", "read", "edit", "write", "search", "fetch", "plan_exit"},
	}
}

func zeroUsage() map[string]any {
	return map[string]any{
		"input": 0, "output": 0, "reasoning": 0, "cacheRead": 0, "cacheWrite": 0, "totalTokens": 0,
		"cost": map[string]any{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0, "total": 0},
	}
}

func normalizeUsage(raw map[string]any) map[string]any {
	in := number(raw["input_tokens"])
	out := number(raw["output_tokens"])
	reasoning := number(raw["reasoning_output_tokens"])
	if reasoning == 0 {
		reasoning = number(raw["reasoning"])
	}
	cr := number(raw["cache_read_input_tokens"])
	cw := number(raw["cache_creation_input_tokens"])
	if in+out+reasoning+cr+cw == 0 {
		return zeroUsage()
	}
	return map[string]any{
		"input": in, "output": out, "reasoning": reasoning, "cacheRead": cr, "cacheWrite": cw,
		"totalTokens": in + out + reasoning + cr + cw,
		"cost":        map[string]any{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0, "total": 0},
	}
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func workspaceID(path string) string {
	key := strings.TrimRight(path, `\/`)
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	sum := sha1.Sum([]byte(key))
	return hex.EncodeToString(sum[:])[:12]
}

func normalizeDir(p string) string {
	if p == "" {
		return p
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return filepath.Clean(p)
}

func baseName(path string) string {
	path = strings.TrimRight(path, `\/`)
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			return path[i+1:]
		}
	}
	return path
}

func parentDir(path string) string {
	path = strings.TrimRight(path, `\/`)
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			return path[:i]
		}
	}
	return path
}

func homeDir() string {
	if v := os.Getenv("USERPROFILE"); v != "" {
		return v
	}
	return os.Getenv("HOME")
}

func roots() []any {
	if runtime.GOOS == "windows" {
		return []any{"C:\\", "D:\\"}
	}
	return []any{"/"}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func bool_(v any) bool {
	b, _ := v.(bool)
	return b
}

func number(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func toAny(list []map[string]any) []any {
	out := make([]any, len(list))
	for i, v := range list {
		out[i] = v
	}
	return out
}

// historyContext renders the recent text of a conversation for runtimes that
// start without a native session (for example after an app restart, or every
// Codex turn). Tool traffic and huge outputs are omitted.
func (h *Host) historyContext(s *Session, maxMessages, maxChars int) string {
	s.mu.Lock()
	msgs := append([]map[string]any(nil), s.messages...)
	s.mu.Unlock()
	start := len(msgs) - maxMessages
	if start < 0 {
		start = 0
	}
	var b strings.Builder
	for _, m := range msgs[start:] {
		role := str(m["role"])
		if role != "user" && role != "assistant" {
			continue
		}
		text := strings.TrimSpace(messageText(m["content"]))
		if text == "" {
			continue
		}
		if r := []rune(text); len(r) > 1500 {
			text = string(r[:1500]) + "…"
		}
		b.WriteString(role)
		b.WriteString(": ")
		b.WriteString(text)
		b.WriteString("\n")
		if b.Len() >= maxChars {
			break
		}
	}
	out := b.String()
	if len(out) > maxChars {
		out = out[:maxChars]
	}
	return strings.TrimSpace(out)
}

func anyHasVision(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return false
	}
	return valueHasVision(v)
}

func valueHasVision(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		s := strings.ToLower(t)
		return s == "true" || s == "1" || strings.Contains(s, "image") || strings.Contains(s, "vision")
	case []any:
		for _, x := range t {
			if valueHasVision(x) {
				return true
			}
		}
	case map[string]any:
		for _, x := range t {
			if valueHasVision(x) {
				return true
			}
		}
	}
	return false
}

// modelLooksVision is the fallback for providers whose /models endpoint has no
// modality metadata (plain OpenAI-compatible APIs). It only catches well-known
// vision families; the per-model checkbox in the gateway editor remains the
// override for anything else.
func modelLooksVision(id string) bool {
	id = strings.ToLower(id)
	for _, needle := range []string{
		"vision", "-vl", "/vl", "gpt-4o", "gpt-4.1", "gpt-5", "o3", "o4",
		"claude-3", "claude-4", "claude-sonnet", "claude-opus", "gemini",
		"pixtral", "llava", "qwen2-vl", "qwen2.5-vl", "qwen3-vl", "glm-4v",
		"glm-4.5v", "internvl", "minicpm-v", "llama-3.2-11b", "llama-3.2-90b",
		"command-a-vision", "phi-3-vision", "phi-4-multimodal",
	} {
		if strings.Contains(id, needle) {
			return true
		}
	}
	return false
}

// refreshAgentCfg replaces the cached runtime details after an agent CLI was
// updated in place: runtime.list otherwise keeps showing the version seen at
// host startup.
func (h *Host) refreshAgentCfg(id string) {
	a, err := agents.Resolve(id)
	if err != nil || a.Path == "" {
		return
	}
	h.mu.Lock()
	switch id {
	case "claude-code":
		h.cfg.Claude = a
	case "codex":
		h.cfg.Codex = a
	}
	h.mu.Unlock()
}
