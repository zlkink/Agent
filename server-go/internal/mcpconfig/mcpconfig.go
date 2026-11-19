// Package mcpconfig loads the MCP servers configured in RamiCore and converts
// them to the shape each agent runtime expects.
package mcpconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

type Server map[string]any

// Load reads the MCP plugin store and PI's mcp.json. The internal prometheus
// tool bus is not included: every runtime adds that itself.
func Load(dataDir string) map[string]Server {
	out := map[string]Server{}
	readInto(out, filepath.Join(dataDir, "plugin-data", "mcp", "servers.json"), "servers")
	readInto(out, filepath.Join(dataDir, "pi-agent", "mcp.json"), "mcpServers")
	return out
}

func readInto(out map[string]Server, path, field string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(raw, &doc) != nil {
		return
	}
	var entries map[string]Server
	if json.Unmarshal(doc[field], &entries) != nil {
		return
	}
	for name, spec := range entries {
		if (name == "prometheus" || name == "ramicore") || out[name] != nil || spec["enabled"] == false {
			continue
		}
		if cfg := Normalize(spec); cfg != nil {
			out[name] = cfg
		}
	}
}

// Normalize keeps only the fields the SDKs understand.
func Normalize(spec Server) Server {
	typ, _ := spec["type"].(string)
	url, _ := spec["url"].(string)
	command, _ := spec["command"].(string)
	switch {
	case typ == "sse" || typ == "http" || url != "":
		if url == "" {
			return nil
		}
		cfg := Server{"type": typ, "url": url}
		if typ == "" {
			cfg["type"] = "http"
		}
		if h := spec["headers"]; h != nil {
			cfg["headers"] = h
		}
		return cfg
	case command != "":
		cfg := Server{"type": "stdio", "command": command}
		if a := spec["args"]; a != nil {
			cfg["args"] = a
		}
		if e := spec["env"]; e != nil {
			cfg["env"] = e
		}
		return cfg
	}
	return nil
}

func Expand(value any) any {
	switch v := value.(type) {
	case string:
		return os.Expand(v, func(key string) string {
			if strings.HasPrefix(key, "{") && strings.HasSuffix(key, "}") {
				return os.Getenv(strings.Trim(key, "{}"))
			}
			return os.Getenv(key)
		})
	case []any:
		for i := range v {
			v[i] = Expand(v[i])
		}
	case map[string]any:
		for k := range v {
			v[k] = Expand(v[k])
		}
	}
	return value
}

// Clone returns a deep-enough copy so callers can mutate safely.
func Clone(in map[string]Server) map[string]Server {
	out := map[string]Server{}
	for name, cfg := range in {
		c := Server{}
		for k, v := range cfg {
			c[k] = v
		}
		out[name] = c
	}
	return out
}
