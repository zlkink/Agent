package agents

import "testing"

func TestInstallMethod(t *testing.T) {
	cases := map[string]string{
		`C:\Users\me\.codex\packages\standalone\releases\0.169.1\bin\codex.exe`: "self",
		`D:\nvm4w\nodejs\codex.cmd`:                    "npm",
		`/opt/homebrew/Cellar/codex/0.169.1/bin/codex`: "brew",
		`C:\Users\me\.local\bin\claude.exe`:            "self",
		`/usr/local/bin/somethingelse`:                 "",
	}
	for path, want := range cases {
		if got := InstallMethod(path); got != want {
			t.Fatalf("InstallMethod(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestUpdateCommand(t *testing.T) {
	if got := UpdateCommand(`C:\x\.codex\packages\standalone\releases\1\bin\codex.exe`, "self"); len(got) != 2 || got[1] != "update" {
		t.Fatalf("self command = %#v", got)
	}
	if got := UpdateCommand(`D:\node\codex.cmd`, "npm"); len(got) < 4 {
		t.Fatalf("npm command = %#v", got)
	}
}
