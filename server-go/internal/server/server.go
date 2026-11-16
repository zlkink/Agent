// Package server wires the Go host to HTTP: the Prometheus WebSocket protocol at
// /ws, the health endpoint the desktop shell probes, and the web UI assets.
package server

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"prometheus-go/internal/agents"
	"prometheus-go/internal/host"
)

//go:embed index.html
var indexHTML []byte

type Server struct {
	Host      *host.Host
	WebRoot   string
	PluginURL string
	Token     string
	Cwd       string
	Model     string
	Gateway   string
	Claude    agents.Agent
	Offline   bool

	lanMu   sync.Mutex
	lanSrv  *http.Server
	lanPort int
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", s.Host.ServeWS)
	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("GET /api/download/", s.auth(s.download))
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /agents", s.agents)
	mux.HandleFunc("GET /plugins/", s.auth(s.plugins))
	mux.HandleFunc("POST /plugins/", s.auth(s.plugins))
	mux.HandleFunc("GET /", s.static)
	return mux
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"model":   s.Model,
		"gateway": s.Gateway,
		"claude":  s.Claude.Version,
		"agent":   s.Claude.Path,
		"host":    "ramicore",
	})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if s.Token == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body)
	if subtle.ConstantTimeCompare([]byte(body.Token), []byte(s.Token)) != 1 {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "invalid token"})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: "prometheus_token", Value: s.Token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: 86400,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Token == "" || s.authorized(r) {
			next(w, r)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
}

const lanCookie = "helixagent_session"

// ApplyNetwork starts or stops the LAN listener. The local listener always stays
// on loopback; LAN clients get password-authenticated WebSocket and HTTP access.
func (s *Server) ApplyNetwork(lan bool, port int) error {
	s.lanMu.Lock()
	defer s.lanMu.Unlock()
	if s.lanSrv != nil {
		_ = s.lanSrv.Close()
		s.lanSrv = nil
	}
	if !lan {
		return nil
	}
	if port < 1024 || port > 65535 {
		return fmt.Errorf("端口需在 1024–65535 之间")
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("监听端口 %d 失败：%v", port, err)
	}
	srv := &http.Server{Handler: s.lanHandler(), ReadHeaderTimeout: 30 * time.Second}
	s.lanSrv = srv
	s.lanPort = port
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("LAN listener stopped: %v", err)
		}
	}()
	return nil
}

func (s *Server) lanHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("POST /api/login", s.lanHost(s.lanLogin))
	mux.HandleFunc("GET /ws", s.lanHost(s.lanWS))
	mux.HandleFunc("GET /api/download/", s.lanHost(s.lanAuth(s.download)))
	mux.HandleFunc("GET /plugins/", s.lanHost(s.lanAuth(s.plugins)))
	mux.HandleFunc("POST /plugins/", s.lanHost(s.lanAuth(s.plugins)))
	mux.HandleFunc("GET /", s.lanHost(s.static))
	return mux
}

// lanHost enforces the domain allow-list. IP addresses and localhost are always
// accepted; the tunnel's own trycloudflare.com hostname is accepted while the
// tunnel is running.
func (s *Server) lanHost(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.Host.AllowsHost(r.Host) {
			http.Error(w, "host not allowed", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func (s *Server) lanLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
		Token    string `json:"token"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body)
	token := ""
	switch {
	case body.Password != "":
		if t, ok := s.Host.CheckNetworkPassword(body.Password); ok {
			token = t
		}
	case body.Token != "" && s.Host.CheckNetworkSession(body.Token):
		token = body.Token
	}
	if token == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "invalid password"})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: lanCookie, Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: 30 * 86400,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "token": token})
}

func (s *Server) lanSession(r *http.Request) string {
	if c, err := r.Cookie(lanCookie); err == nil && s.Host.CheckNetworkSession(c.Value) {
		return c.Value
	}
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		tok := strings.TrimPrefix(auth, "Bearer ")
		if s.Host.CheckNetworkSession(tok) {
			return tok
		}
	}
	// Non-browser clients (CLI, native apps) have no cookie jar; allow the
	// session token in the query string as well.
	if tok := r.URL.Query().Get("token"); tok != "" && s.Host.CheckNetworkSession(tok) {
		return tok
	}
	return ""
}

func (s *Server) lanAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.lanSession(r) == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) lanWS(w http.ResponseWriter, r *http.Request) {
	// No cookie gate here: the WebSocket handshake is where a LAN client signs
	// in (hello → login frame → auth_ok). HTTP assets use the session cookie.
	s.Host.ServeWSNetwork(w, r)
}

func (s *Server) authorized(r *http.Request) bool {
	if c, err := r.Cookie("prometheus_token"); err == nil &&
		subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.Token)) == 1 {
		return true
	}
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") &&
		subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(auth, "Bearer ")), []byte(s.Token)) == 1 {
		return true
	}
	return false
}

func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/download/")
	path, name, ok := s.Host.TakeDownload(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(name, `"`, "")+`"`)
	http.ServeFile(w, r, path)
}

func (s *Server) agents(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, agents.Detect(ctx, !s.Offline))
}

func (s *Server) plugins(w http.ResponseWriter, r *http.Request) {
	if s.PluginURL == "" {
		http.NotFound(w, r)
		return
	}
	target, err := url.Parse(s.PluginURL)
	if err != nil {
		http.Error(w, "plugin host URL is invalid", http.StatusBadGateway)
		return
	}
	httputil.NewSingleHostReverseProxy(target).ServeHTTP(w, r)
}

func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	root := s.WebRoot
	if root == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/plugins/") {
		http.NotFound(w, r)
		return
	}
	rel := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	file := filepath.Join(root, filepath.FromSlash(rel))
	if fi, err := os.Stat(file); err == nil && !fi.IsDir() {
		http.ServeFile(w, r, file)
		return
	}
	index := filepath.Join(root, "index.html")
	if fi, err := os.Stat(index); err == nil && !fi.IsDir() {
		http.ServeFile(w, r, index)
		return
	}
	http.NotFound(w, r)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write json: %v", err)
	}
}

// Printer renders neutral sidecar events for the CLI.
type Printer struct {
	sawText bool
}

func (p *Printer) Print(raw json.RawMessage) error {
	var ev struct {
		Type    string          `json:"type"`
		Text    string          `json:"text"`
		Name    string          `json:"name"`
		Tool    string          `json:"tool"`
		Message string          `json:"message"`
		Result  string          `json:"result"`
		IsError bool            `json:"isError"`
		Usage   json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		fmt.Printf("\n[raw] %s\n", raw)
		return nil
	}
	switch ev.Type {
	case "text_delta":
		p.sawText = true
		fmt.Print(ev.Text)
	case "thinking_delta":
		// Keep the CLI quiet; uncomment to inspect reasoning.
	case "tool_start":
		fmt.Printf("\n[tool] %s\n", ev.Name)
	case "permission":
		fmt.Printf("\n[permission] %s\n", ev.Tool)
	case "log":
		if !strings.Contains(ev.Message, "unrecognized_model") {
			fmt.Printf("\n[sidecar] %s\n", ev.Message)
		}
	case "done":
		if !p.sawText && ev.Result != "" {
			fmt.Println(ev.Result)
		} else {
			fmt.Println()
		}
		if len(ev.Usage) > 0 {
			fmt.Printf("[usage] %s\n", ev.Usage)
		}
	case "error":
		fmt.Printf("\n[error] %s\n", ev.Message)
	}
	return nil
}
