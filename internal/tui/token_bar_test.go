package tui

import (
	"testing"
)

func TestFormatTokenCount(t *testing.T) {
	tests := []struct {
		input int64
		want  string
	}{
		{0, "0"},
		{500, "500"},
		{999, "999"},
		{1000, "1.0K"},
		{1500, "1.5K"},
		{12500, "12.5K"},
		{999999, "1000.0K"},
		{1000000, "1.0M"},
		{1500000, "1.5M"},
		{12500000, "12.5M"},
	}

	for _, tt := range tests {
		got := formatTokenCount(tt.input)
		if got != tt.want {
			t.Errorf("formatTokenCount(%d) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestAgentInfoHasTokenData(t *testing.T) {
	t.Run("no token data", func(t *testing.T) {
		info := AgentInfo{
			Name:         "worker-1",
			HasTokenData: false,
			TotalTokens:  0,
		}
		if info.HasTokenData {
			t.Error("HasTokenData should be false when no token events received")
		}
	})

	t.Run("has token data with zero spend", func(t *testing.T) {
		info := AgentInfo{
			Name:         "worker-1",
			HasTokenData: true,
			InputTokens:  0,
			OutputTokens: 0,
			TotalTokens:  0,
		}
		if !info.HasTokenData {
			t.Error("HasTokenData should be true even with zero spend (provider reported)")
		}
	})

	t.Run("has token data with spend", func(t *testing.T) {
		info := AgentInfo{
			Name:         "worker-1",
			HasTokenData: true,
			InputTokens:  5000,
			OutputTokens: 2000,
			TotalTokens:  7000,
		}
		if !info.HasTokenData {
			t.Error("HasTokenData should be true")
		}
		if info.InputTokens != 5000 {
			t.Errorf("InputTokens = %d, want 5000", info.InputTokens)
		}
	})
}
