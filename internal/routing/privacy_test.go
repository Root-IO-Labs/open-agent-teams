package routing

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestParsePrivacyMode_Defaults(t *testing.T) {
	tests := []struct {
		in   string
		want PrivacyMode
	}{
		{"", PrivacyModeLocal},
		{"unknown", PrivacyModeLocal},
		{"local", PrivacyModeLocal},
		{"LOCAL", PrivacyModeLocal},
		{"  Strict  ", PrivacyModeStrict},
		{"share-features", PrivacyModeShareFeatures},
		{"share-all", PrivacyModeShareAll},
	}
	for _, tt := range tests {
		if got := ParsePrivacyMode(tt.in); got != tt.want {
			t.Errorf("ParsePrivacyMode(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestIsLoggingDisabled_KillSwitch(t *testing.T) {
	tests := []struct {
		env  string
		want bool
	}{
		{"", false},
		{"on", false},
		{"local", false},
		{"off", true},
		{"OFF", true},
		{"  Off  ", true},
		{"0", true},
		{"false", true},
		{"disabled", true},
	}
	for _, tt := range tests {
		old := os.Getenv("OAT_OUTCOME_LOG")
		t.Setenv("OAT_OUTCOME_LOG", tt.env)
		got := IsLoggingDisabled()
		os.Setenv("OAT_OUTCOME_LOG", old) //nolint:errcheck
		if got != tt.want {
			t.Errorf("IsLoggingDisabled with OAT_OUTCOME_LOG=%q = %v, want %v", tt.env, got, tt.want)
		}
	}
}

func TestNewOutcomeLogger_KillSwitchReturnsNil(t *testing.T) {
	t.Setenv("OAT_OUTCOME_LOG", "off")
	l := NewOutcomeLogger("/tmp/anything.jsonl", nil)
	if l != nil {
		t.Errorf("OAT_OUTCOME_LOG=off should return nil logger; got %v", l)
	}
}

func TestApplyPrivacy_StrictRedactsText(t *testing.T) {
	rec := OutcomeRecord{
		TaskText:      "secret production code analysis details",
		Summary:       "I made the change you asked for",
		FailureReason: "syntax error in line 42 of secret_business_logic.py",
	}
	applyPrivacy(&rec, PrivacyModeStrict, "")
	if rec.TaskText != "" {
		t.Error("strict mode failed to redact task_text")
	}
	if rec.Summary != "" {
		t.Error("strict mode failed to redact summary")
	}
	if rec.FailureReason != "" {
		t.Error("strict mode failed to redact failure_reason")
	}
	if rec.Privacy == nil || rec.Privacy.UploadConsent != "strict" {
		t.Errorf("privacy metadata not stamped: %+v", rec.Privacy)
	}
	if rec.Privacy.TaskTextPresent {
		t.Error("strict mode should set task_text_present=false")
	}
}

func TestApplyPrivacy_LocalKeepsText(t *testing.T) {
	rec := OutcomeRecord{
		TaskText: "raw task description",
		Summary:  "the summary",
	}
	applyPrivacy(&rec, PrivacyModeLocal, "")
	if rec.TaskText != "raw task description" {
		t.Error("local mode should keep task_text")
	}
	if rec.Summary != "the summary" {
		t.Error("local mode should keep summary")
	}
	if !rec.Privacy.TaskTextPresent {
		t.Error("local mode should set task_text_present=true when text non-empty")
	}
	if rec.Privacy.UploadConsent != "local" {
		t.Errorf("upload_consent = %q, want local", rec.Privacy.UploadConsent)
	}
}

func TestApplyPrivacy_StampsUserID(t *testing.T) {
	rec := OutcomeRecord{TaskText: "x"}
	applyPrivacy(&rec, PrivacyModeShareAll, "user-uuid-abc")
	if rec.Privacy.UserID != "user-uuid-abc" {
		t.Errorf("user_id = %q, want user-uuid-abc", rec.Privacy.UserID)
	}
}

func TestOutcomeLogger_StrictModeOnDisk(t *testing.T) {
	t.Setenv("OAT_LOG_PRIVACY", "strict")
	tmp := t.TempDir()
	path := filepath.Join(tmp, "history.jsonl")
	l := NewOutcomeLogger(path, nil)
	if l == nil {
		t.Fatal("logger should not be nil in strict mode")
	}
	l.Log(OutcomeRecord{
		Repo:     "x",
		Worker:   "y",
		Model:    "z",
		TaskText: "PROPRIETARY BUSINESS LOGIC",
		Summary:  "PROPRIETARY SUMMARY",
	})

	f, _ := os.Open(path)
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		t.Fatal("no record written")
	}
	var got OutcomeRecord
	if err := json.Unmarshal(sc.Bytes(), &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.TaskText != "" {
		t.Errorf("strict mode wrote task_text to disk: %q", got.TaskText)
	}
	if got.Summary != "" {
		t.Errorf("strict mode wrote summary to disk: %q", got.Summary)
	}
	if got.Privacy == nil || got.Privacy.UploadConsent != "strict" {
		t.Errorf("privacy metadata not stamped in on-disk record: %+v", got.Privacy)
	}
}

func TestOutcomeLogger_LocalModeIsDefault(t *testing.T) {
	// No OAT_LOG_PRIVACY env set.
	t.Setenv("OAT_LOG_PRIVACY", "")
	tmp := t.TempDir()
	path := filepath.Join(tmp, "history.jsonl")
	l := NewOutcomeLogger(path, nil)
	if l == nil {
		t.Fatal("logger nil with empty env")
	}
	if l.PrivacyMode() != PrivacyModeLocal {
		t.Errorf("default privacy = %q, want local", l.PrivacyMode())
	}
}
