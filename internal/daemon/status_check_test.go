package daemon

import "testing"

func TestClassifySidePanelUserMessage_PureStatus(t *testing.T) {
	cases := []string{
		"are you stuck?",
		"are you still working?",
		"you've been stuck again for several minutes",
		"ping",
		"hello?",
		"what's causing you to get stuck though?",
	}
	for _, c := range cases {
		if got := classifySidePanelUserMessage(c); got != sidePanelMsgPureStatus {
			t.Errorf("%q: want pureStatus, got %v", c, got)
		}
	}
}

func TestClassifySidePanelUserMessage_NewWork(t *testing.T) {
	cases := []string{
		"Map the Aikido app UI",
		"are you stuck? also please open the dashboard and document it",
		"continue with the Code Audit section",
		"try again",
		"",
	}
	for _, c := range cases {
		if got := classifySidePanelUserMessage(c); got != sidePanelMsgNewWork {
			t.Errorf("%q: want newWork, got %v", c, got)
		}
	}
}

func TestClassifySidePanelUserMessage_StripsSentinel(t *testing.T) {
	raw := sidePanelInputSentinel + "[active-tab-id: 42] are you stuck?"
	if got := classifySidePanelUserMessage(raw); got != sidePanelMsgPureStatus {
		t.Fatalf("decorated pure status: got %v", got)
	}
}

func TestLooksLikeStatusAsk(t *testing.T) {
	if !looksLikeStatusAsk("are you stuck? also map the page") {
		t.Fatal("expected status+task to look like status ask")
	}
	if looksLikeStatusAsk("Map the Aikido app") {
		t.Fatal("plain task should not look like status ask")
	}
}
