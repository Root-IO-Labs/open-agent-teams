package routing

import (
	"errors"
	"time"
)

// SharePayload is the wire shape that `oat routing share` produces. v0
// ships --dry-run only; the actual upload endpoint is placeholder.
//
// The schema is deliberately separate from OutcomeRecord so the upload
// contract can evolve independently of the on-disk corpus contract. A
// uploader-side update can reshape the payload without forcing every
// existing local corpus to migrate.
type SharePayload struct {
	SchemaVersion int                  `json:"schema_version"`
	GeneratedAt   string               `json:"generated_at"`
	UserID        string               `json:"user_id,omitempty"`
	OATVersion    string               `json:"oat_version,omitempty"`
	Consent       string               `json:"consent"` // share-features | share-all
	Records       []SharePayloadRecord `json:"records"`
}

const sharePayloadSchemaVersion = 1

// PlaceholderShareEndpoint is the URL `oat routing share` will POST to once
// the receiving service exists. v0 doesn't actually call it — share is
// --dry-run-only — but the constant lives here so the wiring point is
// obvious when the endpoint goes live.
//
// .invalid TLD per RFC 2606 — guarantees the placeholder won't ever resolve
// and a misconfigured upload won't go to a typo-squatter.
const PlaceholderShareEndpoint = "https://routing.oat-cli.invalid/v1/share"

// SharePayloadRecord is the per-record shape sent to the community endpoint.
// Includes everything that's privacy-safe under the user's consent level.
//
// share-features: hashes + features + decision context + outcome + score.
//
//	No task_text, no summary, no failure_reason.
//
// share-all:      same as share-features PLUS task_text + summary +
//
//	failure_reason. The fullest data option, only sent if user explicitly
//	opted in via OAT_LOG_PRIVACY=share-all.
type SharePayloadRecord struct {
	RecordID             string              `json:"record_id"`
	OATVersion           string              `json:"oat_version,omitempty"`
	PricingSnapshotID    string              `json:"pricing_snapshot_id,omitempty"`
	TS                   string              `json:"ts"`
	Repo                 string              `json:"repo,omitempty"`
	AgentType            string              `json:"agent_type,omitempty"`
	Model                string              `json:"model"`
	Provider             string              `json:"provider,omitempty"`
	ModelCanonical       string              `json:"model_canonical,omitempty"`
	RoutingSource        string              `json:"routing_source,omitempty"`
	DecisionReason       string              `json:"decision_reason,omitempty"`
	CandidatesConsidered []string            `json:"candidates_considered,omitempty"`
	WallMs               int64               `json:"wall_ms,omitempty"`
	TokensIn             int64               `json:"tokens_in,omitempty"`
	TokensOut            int64               `json:"tokens_out,omitempty"`
	CacheRead            int64               `json:"cache_read,omitempty"`
	CacheWrite           int64               `json:"cache_write,omitempty"`
	Outcome              string              `json:"outcome,omitempty"`
	RemovalReason        string              `json:"removal_reason,omitempty"`
	VerifyPassed         *bool               `json:"verify_passed,omitempty"`
	PRStateHistory       []PRStateSnapshot   `json:"pr_state_history,omitempty"`
	TaskFeatures         *LoggedTaskFeatures `json:"task_features,omitempty"`
	Prompt               *PromptMetadata     `json:"prompt,omitempty"`
	SuccessScore         float64             `json:"success_score"`
	SuccessScoreBasis    string              `json:"success_score_basis,omitempty"`
	SuccessScoreHas      bool                `json:"success_score_present"`

	// share-all only
	TaskText      string `json:"task_text,omitempty"`
	Summary       string `json:"summary,omitempty"`
	FailureReason string `json:"failure_reason,omitempty"`
	IssueNum      string `json:"issue_number,omitempty"`
}

// ErrShareNotPermitted is returned when the user's privacy mode disallows
// sharing. Exported so CLI callers can recognize and emit a friendly
// remediation hint instead of a generic error.
var ErrShareNotPermitted = errors.New("share-mode opt-in required")

// BuildSharePayload constructs the upload payload from the given records
// according to the user's privacy mode. Returns ErrShareNotPermitted if
// the mode forbids sharing.
//
// share-features: drops task_text/summary/failure_reason/issue_number per
//
//	record. Hashes (Prompt.UserMessageHash, etc.) are kept.
//
// share-all:      includes those fields verbatim.
//
// Repo is sanitized in share-features (could embed a private path); kept
// in share-all where the user has explicitly consented.
func BuildSharePayload(records []OutcomeRecord, mode PrivacyMode, userID, oatVersion string) (SharePayload, error) {
	if mode != PrivacyModeShareFeatures && mode != PrivacyModeShareAll {
		return SharePayload{}, ErrShareNotPermitted
	}

	out := SharePayload{
		SchemaVersion: sharePayloadSchemaVersion,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		UserID:        userID,
		OATVersion:    oatVersion,
		Consent:       string(mode),
		Records:       make([]SharePayloadRecord, 0, len(records)),
	}

	for _, rec := range records {
		score, basis, has := DeriveSuccessScore(rec)
		sr := SharePayloadRecord{
			RecordID:             rec.RecordID,
			OATVersion:           rec.OATVersion,
			PricingSnapshotID:    rec.PricingSnapshotID,
			TS:                   rec.TS,
			AgentType:            rec.AgentType,
			Model:                rec.Model,
			Provider:             rec.Provider,
			ModelCanonical:       rec.ModelCanonical,
			RoutingSource:        rec.RoutingSource,
			DecisionReason:       rec.DecisionReason,
			CandidatesConsidered: rec.CandidatesConsidered,
			WallMs:               rec.WallMs,
			TokensIn:             rec.TokensIn,
			TokensOut:            rec.TokensOut,
			CacheRead:            rec.CacheRead,
			CacheWrite:           rec.CacheWrite,
			Outcome:              rec.Outcome,
			RemovalReason:        rec.RemovalReason,
			VerifyPassed:         rec.VerifyPassed,
			PRStateHistory:       rec.PRStateHistory,
			TaskFeatures:         rec.TaskFeatures,
			Prompt:               rec.Prompt,
			SuccessScore:         score,
			SuccessScoreBasis:    string(basis),
			SuccessScoreHas:      has,
		}

		if mode == PrivacyModeShareAll {
			sr.TaskText = rec.TaskText
			sr.Summary = rec.Summary
			sr.FailureReason = rec.FailureReason
			sr.IssueNum = rec.IssueNum
			sr.Repo = rec.Repo
		}
		// share-features deliberately omits Repo too — repo names can leak
		// project identity. Operators on share-all explicitly consent.

		out.Records = append(out.Records, sr)
	}
	return out, nil
}
