package routing

import "testing"

func TestHashPromptText(t *testing.T) {
	if got := HashPromptText(""); got != "" {
		t.Errorf("empty input: got %q, want empty", got)
	}
	a := HashPromptText("hello")
	b := HashPromptText("hello")
	if a != b {
		t.Errorf("not deterministic: %s vs %s", a, b)
	}
	if len(a) != 64 {
		t.Errorf("hash length %d, want 64 (sha256 hex)", len(a))
	}
	if HashPromptText("hello") == HashPromptText("Hello") {
		t.Error("hash is case-insensitive — should distinguish")
	}
}

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"a", 1},                           // any non-empty input >= 1 token
		{"abcd", 1},                        // exactly 4 chars / 4 = 1
		{"abcdefgh", 2},                    // 8/4 = 2
		{"this is some sample text", 6},    // 24/4 = 6
		{string(make([]byte, 4000)), 1000}, // 4kb → 1k tokens
	}
	for _, tt := range tests {
		if got := EstimateTokens(tt.in); got != tt.want {
			t.Errorf("EstimateTokens(len=%d) = %d, want %d", len(tt.in), got, tt.want)
		}
	}
}
