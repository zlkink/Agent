// Package agents detects the external agent CLIs (Claude Code, Codex) and checks
// whether a newer version is published. It is the Go counterpart of
// earlier Node host.
package agents

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"prometheus-go/internal/config"
)

type Agent struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Package         string `json:"package"`
	Path            string `json:"path,omitempty"`
	Version         string `json:"version,omitempty"`
	Latest          string `json:"latest,omitempty"`
	Source          string `json:"source"` // path | managed | missing
	UpdateAvailable bool   `json:"updateAvailable"`
	Managed         string `json:"managed,omitempty"`
	Error           string `json:"error,omitempty"`
}

type def struct {
	id, name, pkg, bin string
}

var defs = []def{
	{"claude-code", "Claude Code", "@anthropic-ai/claude-code", "claude"},
	{"codex", "Codex", "@openai/codex", "codex"},
}

func Definition(id string) (def, bool) {
	for _, d := range defs {
		if d.id == id {
			return d, true
		}
	}
	return def{}, false
}

// Resolve returns the executable a runtime should use, in the same order the Node
// host uses: PATH first, then the Prometheus-managed copy under <home>/agents.
func Resolve(id string) (Agent, error) {
	d, ok := Definition(id)
	if !ok {
		return Agent{}, os.ErrNotExist
	}
	a := Agent{ID: d.id, Name: d.name, Package: d.pkg, Source: "missing"}
	if p, err := exec.LookPath(d.bin); err == nil {
		a.Path = p
		a.Source = "path"
	}
	if a.Path == "" {
		// GUI launches on macOS get a minimal PATH and the native Claude
		// installer uses ~/.claude/local, so check the usual places directly.
		for _, p := range commonPaths(d.bin) {
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
				a.Path = p
				a.Source = "path"
				break
			}
		}
	}
	if managed := managedPath(d.bin); managed != "" {
		a.Managed = managed
		if a.Path == "" {
			a.Path = managed
			a.Source = "managed"
		}
	}
	if a.Path == "" {
		return a, os.ErrNotExist
	}
	return a, nil
}

func commonPaths(bin string) []string {
	home := config.Home()
	userHome, _ := os.UserHomeDir()
	names := []string{bin, bin + ".cmd", bin + ".exe"}
	var out []string
	dirs := []string{
		filepath.Join(userHome, ".local", "bin"),
		filepath.Join(userHome, ".claude", "local"),
		filepath.Join(home, "bin"),
		filepath.Join(home, "agents"),
		filepath.Join(home, "agents", "bin"),
	}
	if runtime.GOOS != "windows" {
		dirs = append(dirs, "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin")
	}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		for _, name := range names {
			out = append(out, filepath.Join(dir, name))
		}
	}
	return out
}

func managedPath(bin string) string {
	root := filepath.Join(config.Home(), "agents")
	names := []string{bin, bin + ".exe", bin + ".cmd"}
	for _, dir := range []string{root, filepath.Join(root, "bin")} {
		for _, name := range names {
			p := filepath.Join(dir, name)
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
				return p
			}
		}
	}
	return ""
}

// Detect reports both agents. checkLatest runs `npm view`, which needs network.
func Detect(ctx context.Context, checkLatest bool) []Agent {
	out := make([]Agent, 0, len(defs))
	for _, d := range defs {
		a, err := Resolve(d.id)
		if err != nil {
			a = Agent{ID: d.id, Name: d.name, Package: d.pkg, Source: "missing", Error: "not found on PATH or in ~/.prometheus/agents"}
			out = append(out, a)
			continue
		}
		if v, err := Version(ctx, a.Path); err != nil {
			a.Error = err.Error()
		} else {
			a.Version = v
		}
		if checkLatest {
			if v, err := LatestVersion(ctx, d.pkg); err != nil {
				if a.Error == "" {
					a.Error = err.Error()
				}
			} else {
				a.Latest = v
				a.UpdateAvailable = Newer(a.Latest, a.Version)
			}
		}
		out = append(out, a)
	}
	return out
}

// Version runs `<bin> --version` and returns the first x.y.z it prints.
func Version(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := command(ctx, path, "--version").CombinedOutput()
	if err != nil && len(out) == 0 {
		return "", err
	}
	if v := parseVersion(string(out)); v != "" {
		return v, nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// LatestVersion asks npm for the newest published version (through the user's
// registry / mirror), the same call the Node host makes.
func LatestVersion(ctx context.Context, pkg string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/c", "npm", "view", pkg, "version")
	} else {
		cmd = exec.CommandContext(ctx, "npm", "view", pkg, "version")
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if v := parseVersion(lines[i]); v != "" {
			return v, nil
		}
	}
	return "", os.ErrNotExist
}

// command builds a command, routing .cmd/.bat shims through cmd.exe on Windows.
func command(ctx context.Context, path string, args ...string) *exec.Cmd {
	lower := strings.ToLower(path)
	if runtime.GOOS == "windows" && (strings.HasSuffix(lower, ".cmd") || strings.HasSuffix(lower, ".bat")) {
		return exec.CommandContext(ctx, "cmd", append([]string{"/c", path}, args...)...)
	}
	return exec.CommandContext(ctx, path, args...)
}

var versionRe = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

func parseVersion(s string) string {
	return versionRe.FindString(s)
}

// Newer reports whether a is a strictly newer x.y.z than b.
func Newer(a, b string) bool {
	av, okA := nums(a)
	bv, okB := nums(b)
	if !okA || !okB {
		return false
	}
	for i := 0; i < 3; i++ {
		if av[i] != bv[i] {
			return av[i] > bv[i]
		}
	}
	return false
}

func nums(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".")
	if len(parts) < 3 {
		return out, false
	}
	for i := 0; i < 3; i++ {
		n, err := strconv.Atoi(strings.TrimFunc(parts[i], func(r rune) bool { return r < '0' || r > '9' }))
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
