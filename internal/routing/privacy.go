package routing

import (
	"os"
	"strings"
)

// PrivacyMode controls how OutcomeRecords are sanitized at write time and
// what (if anything) leaves the local machine via opt-in upload.
//
// Default: PrivacyModeLocal — full text on disk, never uploaded.
// Strict mode redacts free-text fields, keeping only hashes/lengths.
// Share modes are aspirational; the upload endpoint is placeholder-only
// in v0 (see oat routing share --dry-run).
type PrivacyMode string

const (
	// PrivacyModeStrict redacts task_text, summary, failure_reason at write
	// time. Hashes (Prompt.UserMessageHash, completion summary hash if
	// added later) are kept so analyses still work without raw content.
	PrivacyModeStrict PrivacyMode = "strict"

	// PrivacyModeLocal (default) keeps the full record on disk. No data
	// ever leaves the local machine via outcome logging — even share-mode
	// users only upload when they explicitly run `oat routing share`.
	PrivacyModeLocal PrivacyMode = "local"

	// PrivacyModeShareFeatures: same on-disk content as local; opt-in
	// upload sends features-only payload (no task_text, no summary).
	PrivacyModeShareFeatures PrivacyMode = "share-features"

	// PrivacyModeShareAll: same on-disk content as local; opt-in upload
	// sends everything including task_text. Highest-data-richness option,
	// for users contributing to the community corpus.
	PrivacyModeShareAll PrivacyMode = "share-all"
)

// PrivacyMetadata is stamped on every OutcomeRecord so a future reader
// can tell whether the record was redacted at write-time vs full-fat.
// Critical for sanitization: a strict-mode record never has task_text,
// so we can't sanitize-on-upload from a record that never had the data.
type PrivacyMetadata struct {
	// TaskTextPresent: true if OutcomeRecord.TaskText holds the raw text.
	// false in strict mode (text was redacted at write time).
	TaskTextPresent bool `json:"task_text_present,omitempty"`

	// SummaryPresent: same semantics for OutcomeRecord.Summary.
	SummaryPresent bool `json:"summary_present,omitempty"`

	// UploadConsent: the privacy mode the user was in when this record
	// was written. Determines what an opt-in upload may include.
	UploadConsent string `json:"upload_consent,omitempty"`

	// UserID: stable per-install UUID, generated lazily on first share
	// opt-in. Empty for local-only users — never beforehand. This means
	// pre-share records have no UserID; that's intentional, they aren't
	// uploadable anyway.
	UserID string `json:"user_id,omitempty"`
}

// ParsePrivacyMode reads OAT_LOG_PRIVACY and returns the resolved mode.
// Unknown / empty values fall back to PrivacyModeLocal.
func ParsePrivacyMode(env string) PrivacyMode {
	switch PrivacyMode(strings.ToLower(strings.TrimSpace(env))) {
	case PrivacyModeStrict:
		return PrivacyModeStrict
	case PrivacyModeShareFeatures:
		return PrivacyModeShareFeatures
	case PrivacyModeShareAll:
		return PrivacyModeShareAll
	case PrivacyModeLocal:
		return PrivacyModeLocal
	default:
		return PrivacyModeLocal
	}
}

// CurrentPrivacyMode reads OAT_LOG_PRIVACY from the environment.
func CurrentPrivacyMode() PrivacyMode {
	return ParsePrivacyMode(os.Getenv("OAT_LOG_PRIVACY"))
}

// IsLoggingDisabled returns true if OAT_OUTCOME_LOG=off (any case). The
// kill switch — short-circuits the entire logging path. Documented in
// --help so users have a one-flag escape hatch.
func IsLoggingDisabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("OAT_OUTCOME_LOG")))
	return v == "off" || v == "0" || v == "false" || v == "disabled"
}

// applyPrivacy mutates the record in place to honor the given privacy
// mode. Strict mode redacts free-text fields and stamps the privacy
// metadata; other modes only stamp metadata.
//
// Hashes (Prompt.UserMessageHash, etc.) are preserved across all modes
// because they're derived from the redacted-away fields and are
// privacy-safe by construction.
func applyPrivacy(rec *OutcomeRecord, mode PrivacyMode, userID string) {
	if rec.Privacy == nil {
		rec.Privacy = &PrivacyMetadata{}
	}
	rec.Privacy.UploadConsent = string(mode)
	rec.Privacy.UserID = userID

	switch mode {
	case PrivacyModeStrict:
		// Note current presence (will be flipped to false below).
		_ = rec.TaskText
		// Redact free-text fields.
		rec.TaskText = ""
		rec.Summary = ""
		rec.FailureReason = ""
		rec.Privacy.TaskTextPresent = false
		rec.Privacy.SummaryPresent = false
	case PrivacyModeLocal, PrivacyModeShareFeatures, PrivacyModeShareAll:
		rec.Privacy.TaskTextPresent = rec.TaskText != ""
		rec.Privacy.SummaryPresent = rec.Summary != ""
	}
}
