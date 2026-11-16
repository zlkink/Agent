package agents

import "testing"

func TestParseVersion(t *testing.T) {
	cases := map[string]string{
		"2.1.325 (Claude Code)": "2.1.325",
		"codex-cli 0.169.0":     "0.169.0",
		"v22.23.4":              "22.23.4",
		"no version here":       "",
	}
	for in, want := range cases {
		if got := parseVersion(in); got != want {
			t.Errorf("parseVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"2.1.326", "2.1.325", true},
		{"2.1.325", "2.1.325", false},
		{"2.0.999", "2.1.0", false},
		{"3.0.0", "2.99.99", true},
		{"bad", "2.1.0", false},
	}
	for _, c := range cases {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
