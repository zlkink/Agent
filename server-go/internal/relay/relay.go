// Package relay is a loopback reverse proxy that hides the real gateway key from
// the agent subprocess. It is the Go counterpart of GatewayRelay in
// packages/core/src/agent/relay/gateway-relay.ts.
package relay

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"prometheus-go/internal/config"
)

type Relay struct {
	URL    string
	Token  string
	server *http.Server
	base   *url.URL
	auth   [2]string
	extra  map[string]string
}

// Start listens on 127.0.0.1 with a random port and returns a relay whose URL is
// what the Claude CLI should receive as ANTHROPIC_BASE_URL.
func Start(gw *config.Gateway, baseURL string) (*Relay, error) {
	base, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return nil, err
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	r := &Relay{
		Token: "pmt_" + base64.RawURLEncoding.EncodeToString(raw),
		base:  base,
		extra: gw.Headers,
	}
	r.auth[0], r.auth[1] = gw.AuthFor("anthropic-messages")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	r.URL = "http://" + ln.Addr().String()
	r.server = &http.Server{Handler: r, ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = r.server.Serve(ln) }()
	return r, nil
}

func (r *Relay) Close() error {
	if r.server == nil {
		return nil
	}
	return r.server.Close()
}

func (r *Relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if !r.authorized(req) {
		http.Error(w, `{"error":{"message":"unknown or revoked relay token"}}`, http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, 64<<20))
	if err != nil {
		http.Error(w, `{"error":{"message":"body read failed"}}`, http.StatusBadRequest)
		return
	}

	target := upstreamURL(r.base, req.URL)
	out, err := http.NewRequestWithContext(req.Context(), req.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		http.Error(w, `{"error":{"message":"bad upstream request"}}`, http.StatusBadGateway)
		return
	}
	copyHeaders(out.Header, req.Header)
	out.Header.Set("Accept-Encoding", "identity")
	out.Header.Set("Host", target.Host)
	if r.auth[0] != "" {
		out.Header.Set(r.auth[0], r.auth[1])
	}
	for k, v := range r.extra {
		out.Header.Set(k, v)
	}
	if len(body) > 0 {
		out.ContentLength = int64(len(body))
		out.Header.Set("Content-Length", fmt.Sprint(len(body)))
	}

	client := &http.Client{
		Transport: &http.Transport{
			Proxy:               nil, // never send loopback traffic through a system proxy
			ForceAttemptHTTP2:   false,
			MaxIdleConnsPerHost: 8,
		},
	}
	res, err := client.Do(out)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`, err.Error()), http.StatusBadGateway)
		return
	}
	defer res.Body.Close()

	copyHeaders(w.Header(), res.Header)
	w.Header().Del("Content-Length")
	w.WriteHeader(res.StatusCode)
	// Flush every chunk so SSE events reach Claude as they arrive.
	_, _ = io.Copy(flushWriter{w}, res.Body)
}

func (r *Relay) authorized(req *http.Request) bool {
	given := ""
	if v := req.Header.Get("Authorization"); strings.HasPrefix(v, "Bearer ") {
		given = strings.TrimPrefix(v, "Bearer ")
	} else if v := req.Header.Get("x-api-key"); v != "" {
		given = v
	}
	return given != "" && given == r.Token
}

// upstreamURL joins the gateway base with the requested path. A base ending in
// /v1 absorbs the client's own /v1, exactly like the Node relay.
func upstreamURL(base *url.URL, requested *url.URL) *url.URL {
	path := requested.Path
	basePath := strings.TrimRight(base.Path, "/")
	if strings.HasSuffix(strings.ToLower(basePath), "/v1") &&
		(strings.HasPrefix(path, "/v1/") || path == "/v1") {
		path = strings.TrimPrefix(path, "/v1")
	}
	out := *base
	out.Path = basePath + path
	out.RawQuery = requested.RawQuery
	return &out
}

var hopByHop = map[string]bool{
	"Connection": true, "Proxy-Connection": true, "Keep-Alive": true,
	"Proxy-Authenticate": true, "Proxy-Authorization": true, "Te": true,
	"Trailer": true, "Transfer-Encoding": true, "Upgrade": true,
	"Authorization": true, "X-Api-Key": true, "Host": true, "Content-Length": true,
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		if hopByHop[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

type flushWriter struct{ w http.ResponseWriter }

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}
