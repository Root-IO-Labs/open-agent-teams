package routing

import (
	"errors"
	"testing"
)

func TestBuildSharePayload_RequiresShareMode(t *testing.T) {
	for _, mode := range []PrivacyMode{PrivacyModeStrict, PrivacyModeLocal, PrivacyMode("garbage")} {
		_, err := BuildSharePayload(nil, mode, "uid", "v0.1.0")
		if !errors.Is(err, ErrShareNotPermitted) {
			t.Errorf("mode %q: want ErrShareNotPermitted, got %v", mode, err)
		}
	}
}

func TestBuildSharePayload_ShareFeaturesStripsText(t *testing.T) {
	rec := OutcomeRecord{
		RecordID:      "rec-1",
		Model:         "openai:gpt-5.4-mini",
		TaskText:      "PROPRIETARY TASK DESCRIPTION",
		Summary:       "PROPRIETARY SUMMARY",
		FailureReason: "PROPRIETARY ERROR",
		Repo:          "PROPRIETARY-REPO-NAME",
	}
	payload, err := BuildSharePayload([]OutcomeRecord{rec}, PrivacyModeShareFeatures, "uid", "v0.1.0")
	if err != nil {
		t.Fatalf("share-features: %v", err)
	}
	if len(payload.Records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(payload.Records))
	}
	got := payload.Records[0]
	if got.TaskText != "" {
		t.Error("share-features should strip task_text")
	}
	if got.Summary != "" {
		t.Error("share-features should strip summary")
	}
	if got.FailureReason != "" {
		t.Error("share-features should strip failure_reason")
	}
	if got.Repo != "" {
		t.Error("share-features should strip repo (could leak project identity)")
	}
}

func TestBuildSharePayload_ShareAllIncludesText(t *testing.T) {
	rec := OutcomeRecord{
		RecordID: "rec-1",
		Model:    "openai:gpt-5.4-mini",
		TaskText: "Fix the typo",
		Summary:  "Did it",
		Repo:     "my-project",
	}
	payload, err := BuildSharePayload([]OutcomeRecord{rec}, PrivacyModeShareAll, "uid", "v0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	got := payload.Records[0]
	if got.TaskText != "Fix the typo" {
		t.Errorf("share-all dropped task_text: %q", got.TaskText)
	}
	if got.Summary != "Did it" {
		t.Error("share-all dropped summary")
	}
	if got.Repo != "my-project" {
		t.Error("share-all dropped repo")
	}
}

func TestBuildSharePayload_StampsConsent(t *testing.T) {
	payload, _ := BuildSharePayload(nil, PrivacyModeShareFeatures, "u", "v")
	if payload.Consent != "share-features" {
		t.Errorf("Consent = %q, want share-features", payload.Consent)
	}
	payload2, _ := BuildSharePayload(nil, PrivacyModeShareAll, "u", "v")
	if payload2.Consent != "share-all" {
		t.Errorf("Consent = %q, want share-all", payload2.Consent)
	}
}

func TestBuildSharePayload_DerivesSuccessScore(t *testing.T) {
	rec := OutcomeRecord{
		RecordID: "rec-1",
		Outcome:  "completed",
	}
	payload, _ := BuildSharePayload([]OutcomeRecord{rec}, PrivacyModeShareAll, "u", "v")
	got := payload.Records[0]
	if got.SuccessScore != 0.5 || got.SuccessScoreBasis != string(BasisSelfReportComplete) {
		t.Errorf("expected 0.5 / self_report_complete, got %v / %q", got.SuccessScore, got.SuccessScoreBasis)
	}
	if !got.SuccessScoreHas {
		t.Error("SuccessScoreHas should be true for scoreable record")
	}
}
