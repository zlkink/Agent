package host

import (
	"testing"
	"time"
)

func newTokenHost(port int) *Host {
	return &Host{network: map[string]any{"port": port}}
}

func copyNetwork(src map[string]any) map[string]any {
	dst := map[string]any{}
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func TestNetworkTokenSurvivesRestart(t *testing.T) {
	a := newTokenHost(9528)
	a.setNetworkPassword("hunter2")
	token, ok := a.CheckNetworkPassword("hunter2")
	if !ok || token == "" {
		t.Fatal("password login did not issue a token")
	}
	if !a.checkNetworkSession(token) {
		t.Fatal("fresh token rejected")
	}
	// A restart rebuilds the host from network.json; the signed token must still work.
	b := newTokenHost(9528)
	b.network = copyNetwork(a.network)
	if !b.checkNetworkSession(token) {
		t.Fatal("token stopped working after restart")
	}
	if b.checkNetworkSession(token + "x") {
		t.Fatal("tampered token accepted")
	}
}

func TestNetworkTokenRevokedByPasswordAndRevoke(t *testing.T) {
	a := newTokenHost(9528)
	a.setNetworkPassword("hunter2")
	token, _ := a.CheckNetworkPassword("hunter2")

	b := newTokenHost(9528)
	b.network = copyNetwork(a.network)
	b.setNetworkPassword("new-password")
	if b.checkNetworkSession(token) {
		t.Fatal("token survived a password change")
	}

	c := newTokenHost(9528)
	c.network = copyNetwork(a.network)
	c.mu.Lock()
	c.revokeNetworkSessionsLocked()
	c.mu.Unlock()
	if c.checkNetworkSession(token) {
		t.Fatal("token survived revokeSessions")
	}
}

func TestNetworkTokenExpires(t *testing.T) {
	a := newTokenHost(9528)
	a.setNetworkPassword("hunter2")
	a.mu.Lock()
	expired := a.issueNetworkTokenLocked(time.Now().Add(-netTokenTTL - time.Minute))
	a.mu.Unlock()
	if expired == "" {
		t.Fatal("could not build a token")
	}
	if a.checkNetworkSession(expired) {
		t.Fatal("expired token accepted")
	}
}
