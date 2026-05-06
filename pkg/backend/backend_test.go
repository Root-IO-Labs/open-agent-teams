package backend

import (
	"testing"
)

func TestShellQuote(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"simple", "simple"},
		{"oat-agent", "oat-agent"},
		{"/usr/bin/oat-agent", "/usr/bin/oat-agent"},
		{"hello world", "'hello world'"},
		{`{"max_tokens":32000}`, `'{"max_tokens":32000}'`},
		{"it's", `'it'"'"'s'`},
		{"", "''"},
	}
	for _, tt := range tests {
		got := shellQuote(tt.input)
		if got != tt.want {
			t.Errorf("shellQuote(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
