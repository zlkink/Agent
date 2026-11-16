package relay

import (
	"net/url"
	"testing"
)

func TestUpstreamURL(t *testing.T) {
	cases := []struct {
		base, requested, want string
	}{
		{"http://127.0.0.1:3425/v1", "/v1/messages", "http://127.0.0.1:3425/v1/messages"},
		{"http://127.0.0.1:3425/v1", "/v1/messages/count_tokens", "http://127.0.0.1:3425/v1/messages/count_tokens"},
		{"http://127.0.0.1:3425", "/v1/messages", "http://127.0.0.1:3425/v1/messages"},
		{"https://api.example.com/anthropic/v1", "/v1/messages?beta=1", "https://api.example.com/anthropic/v1/messages?beta=1"},
	}
	for _, c := range cases {
		base, _ := url.Parse(c.base)
		req, _ := url.Parse(c.requested)
		if got := upstreamURL(base, req).String(); got != c.want {
			t.Errorf("upstreamURL(%q, %q) = %q, want %q", c.base, c.requested, got, c.want)
		}
	}
}
