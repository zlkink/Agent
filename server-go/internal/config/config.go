// Package config reads the same config.json the Node backend uses, so the Go host
// can be dropped next to an existing Prometheus data directory.
package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type Auth struct {
	Type   string `json:"type"`
	Header string `json:"header"`
	Prefix string `json:"prefix"`
}

type Model struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextWindow int    `json:"contextWindow"`
	MaxTokens     int    `json:"maxTokens"`
	Reasoning     *bool  `json:"reasoning"`
	Vision        *bool  `json:"vision"`
}

type Gateway struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Format     string            `json:"format"`
	BaseURL    string            `json:"baseUrl"`
	APIKey     string            `json:"apiKey"`
	Auth       Auth              `json:"auth"`
	Headers    map[string]string `json:"headers"`
	Models     []Model           `json:"models"`
	Alternates map[string]string `json:"alternates"`
	Enabled    *bool             `json:"enabled"`
}

type Config struct {
	Gateways []Gateway `json:"gateways"`
}

// Home is the Prometheus data directory (~/.prometheus by default).
func Home() string {
	if v := os.Getenv("PROMETHEUS_HOME"); v != "" {
		return v
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return ".prometheus"
	}
	return filepath.Join(h, ".prometheus")
}

func Load() (*Config, error) {
	file := filepath.Join(Home(), "config.json")
	raw, err := os.ReadFile(file)
	// A fresh install has no config yet; an empty file and no file both mean "no gateways".
	if errors.Is(err, os.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func (g *Gateway) IsEnabled() bool {
	return g.Enabled == nil || *g.Enabled
}

// AnthropicBase returns the URL Claude Code should reach for the Anthropic Messages
// protocol: the gateway's own URL when it speaks that format, otherwise its
// anthropic-messages alternate.
func (g *Gateway) AnthropicBase() string {
	if g.Format == "anthropic-messages" {
		return strings.TrimRight(g.BaseURL, "/")
	}
	if v := strings.TrimSpace(g.Alternates["anthropic-messages"]); v != "" {
		return strings.TrimRight(v, "/")
	}
	return ""
}

// PickAnthropic finds a usable Anthropic-compatible gateway, mirroring the Node
// host's gatewayAs(g, "anthropic-messages") selection.
func (c *Config) PickAnthropic() (*Gateway, string, error) {
	for i := range c.Gateways {
		g := &c.Gateways[i]
		if !g.IsEnabled() {
			continue
		}
		if base := g.AnthropicBase(); base != "" {
			return g, base, nil
		}
	}
	return nil, "", errors.New("no enabled gateway offers anthropic-messages")
}

// PickModel prefers PROMETHEUS_MODEL, then a deepseek model, then the first model.
func (c *Config) PickModel(g *Gateway) string {
	if v := strings.TrimSpace(os.Getenv("PROMETHEUS_MODEL")); v != "" {
		return v
	}
	for _, m := range g.Models {
		if strings.Contains(strings.ToLower(m.ID), "deepseek") {
			return m.ID
		}
	}
	if len(g.Models) > 0 {
		return g.Models[0].ID
	}
	return ""
}

// AuthFor returns the header the relay must inject for the upstream protocol.
// It mirrors the earlier Node host relay.
func (g *Gateway) AuthFor(format string) (string, string) {
	key := g.APIKey
	switch g.Auth.Type {
	case "none":
		return "", ""
	case "header":
		return g.Auth.Header, g.Auth.Prefix + key
	case "bearer":
		return "Authorization", "Bearer " + key
	default: // native
		switch format {
		case "anthropic-messages":
			return "x-api-key", key
		case "google-generative-ai":
			return "x-goog-api-key", key
		default:
			return "Authorization", "Bearer " + key
		}
	}
}

// Masked returns gateway objects safe to send to the UI: the API key is
// replaced, everything else (including unknown fields we do not model) is kept.
func Masked(gws []Gateway) []any {
	out := make([]any, 0, len(gws))
	for _, g := range gws {
		auth := map[string]any{"type": g.Auth.Type}
		if g.Auth.Header != "" {
			auth["header"] = g.Auth.Header
		}
		if g.Auth.Prefix != "" {
			auth["prefix"] = g.Auth.Prefix
		}
		models := make([]any, 0, len(g.Models))
		for _, m := range g.Models {
			cw := m.ContextWindow
			if cw <= 0 {
				cw = 200_000
			}
			reasoning := true
			if m.Reasoning != nil {
				reasoning = *m.Reasoning
			}
			vision := false
			if m.Vision != nil {
				vision = *m.Vision
			}
			entry := map[string]any{
				"id": m.ID, "name": m.Name, "reasoning": reasoning, "vision": vision,
				"contextWindow": cw,
			}
			if m.MaxTokens > 0 {
				entry["maxTokens"] = m.MaxTokens
			}
			models = append(models, entry)
		}
		entry := map[string]any{
			"id": g.ID, "name": g.Name, "format": g.Format, "baseUrl": g.BaseURL,
			"auth": auth, "apiKey": "", "headers": g.Headers, "models": models,
			"enabled": g.IsEnabled(),
		}
		if len(g.Alternates) > 0 {
			entry["alternates"] = g.Alternates
		}
		out = append(out, entry)
	}
	return out
}

// MutateGateways edits the gateways array in config.json without dropping the
// rest of the file (agents, plugins, network, ...). The file is written atomically.
func MutateGateways(fn func(gws []map[string]any) ([]map[string]any, error)) ([]Gateway, error) {
	file := filepath.Join(Home(), "config.json")
	raw, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	root := map[string]json.RawMessage{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &root); err != nil {
			return nil, err
		}
	}
	var list []map[string]any
	if v, ok := root["gateways"]; ok && len(v) > 0 {
		var raws []json.RawMessage
		if err := json.Unmarshal(v, &raws); err != nil {
			return nil, err
		}
		for _, item := range raws {
			var m map[string]any
			if err := json.Unmarshal(item, &m); err != nil {
				return nil, err
			}
			list = append(list, m)
		}
	}
	next, err := fn(list)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return nil, err
	}
	root["gateways"] = encoded
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, err
	}
	tmp := file + ".tmp"
	if err := os.MkdirAll(Home(), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, file); err != nil {
		_ = os.Remove(file)
		if err := os.Rename(tmp, file); err != nil {
			return nil, err
		}
	}
	var cfg Config
	if err := json.Unmarshal(out, &cfg); err != nil {
		return nil, err
	}
	return cfg.Gateways, nil
}

// UpsertGateway stores one gateway. An empty or masked apiKey keeps the stored
// key, matching the UI's "••••" convention.
func UpsertGateway(in map[string]any) ([]Gateway, error) {
	id, _ := in["id"].(string)
	if id == "" || !IDPattern.MatchString(id) {
		return nil, errors.New("invalid gateway id")
	}
	return MutateGateways(func(gws []map[string]any) ([]map[string]any, error) {
		for i, g := range gws {
			if g["id"] != id {
				continue
			}
			key, _ := in["apiKey"].(string)
			if key == "" || strings.Contains(key, "•") || strings.Contains(key, "*") {
				if old, ok := g["apiKey"].(string); ok {
					in["apiKey"] = old
				}
			}
			gws[i] = in
			return gws, nil
		}
		return append(gws, in), nil
	})
}

func DeleteGateway(id string) ([]Gateway, error) {
	return MutateGateways(func(gws []map[string]any) ([]map[string]any, error) {
		out := gws[:0]
		for _, g := range gws {
			if g["id"] != id {
				out = append(out, g)
			}
		}
		return out, nil
	})
}

var IDPattern = regexpMust(`^[a-z0-9][a-z0-9-_]{0,47}$`)

func regexpMust(expr string) *regexp.Regexp {
	return regexp.MustCompile(expr)
}
