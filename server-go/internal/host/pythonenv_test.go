package host

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPythonEnvUsesProjectVenv(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, ".venv", "Scripts")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	env := pythonEnv(dir)
	if !strings.HasPrefix(env["PATH"], bin) {
		t.Fatalf("PATH does not start with venv: %q", env["PATH"])
	}
	if env["VIRTUAL_ENV"] != filepath.Join(dir, ".venv") {
		t.Fatalf("VIRTUAL_ENV = %q", env["VIRTUAL_ENV"])
	}
}
