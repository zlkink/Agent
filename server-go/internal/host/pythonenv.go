package host

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// pythonEnv puts the project's virtualenv and uv-managed interpreters ahead of
// PATH. Every runtime gets it: without this only the Claude sidecar knew about
// a venv, while Codex/ACP/OpenCode ran `python` against a PATH with none.
// PythonEnv is the environment for agent processes in cwd: its virtualenv,
// uv-managed interpreters and uv itself ahead of PATH.
func PythonEnv(cwd string) map[string]string { return pythonEnv(cwd) }

func pythonEnv(cwd string) map[string]string {
	env := map[string]string{}
	var paths []string
	venv := filepath.Join(cwd, ".venv")
	if fi, err := os.Stat(venv); err == nil && fi.IsDir() {
		env["VIRTUAL_ENV"] = venv
		for _, sub := range []string{"Scripts", "bin"} {
			dir := filepath.Join(venv, sub)
			if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
				paths = append(paths, dir)
				break
			}
		}
	} else if v := os.Getenv("VIRTUAL_ENV"); v != "" {
		for _, sub := range []string{"Scripts", "bin"} {
			dir := filepath.Join(v, sub)
			if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
				paths = append(paths, dir)
				env["VIRTUAL_ENV"] = v
				break
			}
		}
	}
	paths = append(paths, managedPythonDirs()...)
	if home, err := os.UserHomeDir(); err == nil {
		// uv and its versioned shims live here; Windows installs uv here too.
		paths = append(paths, filepath.Join(home, ".local", "bin"))
	}
	if len(paths) > 0 {
		path := os.Getenv("PATH")
		env["PATH"] = strings.Join(paths, string(os.PathListSeparator)) + string(os.PathListSeparator) + path
	}
	return env
}

func managedPythonDirs() []string {
	home, _ := os.UserHomeDir()
	var roots []string
	if runtime.GOOS == "windows" {
		if d := os.Getenv("APPDATA"); d != "" {
			roots = append(roots, filepath.Join(d, "uv", "python"))
		}
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			roots = append(roots, filepath.Join(d, "uv", "python"))
		}
	} else {
		roots = append(roots, filepath.Join(home, ".local", "share", "uv", "python"))
		if d := os.Getenv("XDG_DATA_HOME"); d != "" {
			roots = append(roots, filepath.Join(d, "uv", "python"))
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, root := range roots {
		matches, _ := filepath.Glob(filepath.Join(root, "*"))
		sort.Sort(sort.Reverse(sort.StringSlice(matches)))
		for _, dir := range matches {
			if seen[dir] {
				continue
			}
			for _, exe := range []string{"python.exe", "python3", "python"} {
				if fi, err := os.Stat(filepath.Join(dir, exe)); err == nil && !fi.IsDir() {
					seen[dir] = true
					out = append(out, dir)
					break
				}
			}
		}
	}
	return out
}

// envWith applies extra over the current process environment, replacing keys
// case-insensitively on Windows.
func envWith(extra map[string]string) []string {
	if len(extra) == 0 {
		return os.Environ()
	}
	base := make(map[string]string, len(extra))
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		base[k] = v
	}
	for k, v := range extra {
		base[k] = v
	}
	out := make([]string, 0, len(base))
	for k, v := range base {
		out = append(out, k+"="+v)
	}
	return out
}
