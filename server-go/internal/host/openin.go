package host

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// Editors the title bar can hand a workspace folder to. This mirrors the
// original Node host: only apps that are
// actually installed are offered, and labels stay stable across languages.
type externalEditor struct {
	id    string
	label string
	win   []string
	mac   string
	bin   string
}

var externalEditors = []externalEditor{
	{
		id:    "vscode",
		label: "VS Code",
		win: []string{
			"%LOCALAPPDATA%/Programs/Microsoft VS Code/Code.exe",
			"%ProgramFiles%/Microsoft VS Code/Code.exe",
			"%ProgramFiles(x86)%/Microsoft VS Code/Code.exe",
		},
		mac: "Visual Studio Code",
		bin: "code",
	},
	{
		id:    "cursor",
		label: "Cursor",
		win: []string{
			"%LOCALAPPDATA%/Programs/cursor/Cursor.exe",
			"%ProgramFiles%/Cursor/Cursor.exe",
		},
		mac: "Cursor",
		bin: "cursor",
	},
	{
		id:    "windsurf",
		label: "Windsurf",
		win:   []string{"%LOCALAPPDATA%/Programs/Windsurf/Windsurf.exe"},
		mac:   "Windsurf",
		bin:   "windsurf",
	},
}

var winEnvRe = regexp.MustCompile(`%([^%]+)%`)

func expandWinPath(p string) string {
	return winEnvRe.ReplaceAllStringFunc(p, func(m string) string {
		key := strings.Trim(m, "%")
		if v, ok := os.LookupEnv(key); ok {
			return v
		}
		return ""
	})
}

// editorPath returns the launcher for an installed editor, or "" when it is
// not installed. The file manager has no path: it is the OS itself.
func editorPath(e externalEditor) string {
	if runtime.GOOS == "windows" {
		for _, raw := range e.win {
			p := filepath.FromSlash(expandWinPath(raw))
			if p != "" && filepath.IsAbs(p) {
				if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
					return p
				}
			}
		}
		// `code.cmd` lives in <install>/bin; the real executable is one level up.
		if cmd, err := exec.LookPath(e.bin); err == nil {
			base := ""
			if len(e.win) > 0 {
				base = filepath.Base(filepath.FromSlash(e.win[0]))
			}
			if base != "" {
				exe := filepath.Join(filepath.Dir(filepath.Dir(cmd)), base)
				if fi, err := os.Stat(exe); err == nil && !fi.IsDir() {
					return exe
				}
			}
		}
		return ""
	}
	if runtime.GOOS == "darwin" {
		home, _ := os.UserHomeDir()
		for _, dir := range []string{"/Applications", filepath.Join(home, "Applications")} {
			app := filepath.Join(dir, e.mac+".app")
			if fi, err := os.Stat(app); err == nil && fi.IsDir() {
				return app
			}
		}
		return ""
	}
	if p, err := exec.LookPath(e.bin); err == nil {
		return p
	}
	return ""
}

func listExternalApps() []any {
	manager := "i18n:fileManagerName"
	if runtime.GOOS == "darwin" {
		manager = "Finder"
	}
	apps := []any{map[string]any{"id": "files", "label": manager}}
	for _, e := range externalEditors {
		if editorPath(e) != "" {
			apps = append(apps, map[string]any{"id": e.id, "label": e.label})
		}
	}
	return apps
}

// launchDetached starts a GUI app without keeping a child handle: the folder
// must be usable even after RamiCore quits.
func launchDetached(cmd string, args ...string) {
	c := exec.Command(cmd, args...)
	c.Stdout = nil
	c.Stderr = nil
	if err := c.Start(); err == nil {
		_ = c.Process.Release()
	}
}

func openInExternalApp(app, path string) error {
	if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
		return fmt.Errorf("i18n:openPathMissing")
	}
	if app == "files" {
		switch runtime.GOOS {
		case "windows":
			launchDetached("explorer.exe", path)
		case "darwin":
			launchDetached("open", path)
		default:
			launchDetached("xdg-open", path)
		}
		return nil
	}
	var editor *externalEditor
	for i := range externalEditors {
		if externalEditors[i].id == app {
			editor = &externalEditors[i]
			break
		}
	}
	if editor == nil {
		return fmt.Errorf("i18n:openAppUnsupported")
	}
	found := editorPath(*editor)
	if found == "" {
		return fmt.Errorf("i18n:openAppNotFound|label=%s", editor.label)
	}
	if runtime.GOOS == "darwin" {
		launchDetached("open", "-a", found, path)
	} else {
		launchDetached(found, path)
	}
	return nil
}
