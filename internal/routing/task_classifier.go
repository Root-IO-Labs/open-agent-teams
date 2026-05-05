package routing

import (
	"regexp"
	"strings"
)

// TaskComplexity is the output of a task classifier.
type TaskComplexity string

const (
	ComplexityTrivial  TaskComplexity = "trivial"
	ComplexitySimple   TaskComplexity = "simple"
	ComplexityStandard TaskComplexity = "standard"
	ComplexityComplex  TaskComplexity = "complex"
	ComplexityUnknown  TaskComplexity = "unknown"
)

// TaskFeatures is what the classifier extracts from a task description. Kept
// deliberately small — we're V1. If/when we need a bigger feature set, this
// struct grows; downstream consumers take what they need.
type TaskFeatures struct {
	Text              string
	LengthChars       int
	FileCountEstimate int            // rough count of "/" tokens in text
	IsTrivial         bool           // "fix typo" / "rename" / single-line edit signal
	IsRefactor        bool           // "refactor" / "rewrite" / "restructure"
	IsBugFix          bool           // "fix bug" / "fix error" / "broken"
	IsDocOrConfig     bool           // mentions .md / docs / .yaml / .toml / readme
	IsAnalysis        bool           // "summarize" / "explore" / "list every" — read-only
	Complexity        TaskComplexity // derived bucketing
}

// Pre-compiled patterns. Package-level so ExtractFeatures doesn't recompile
// on every spawn.
var (
	rxRefactor = regexp.MustCompile(`(?i)\b(refactor|rewrite|restructure)\b`)
	rxBugFix   = regexp.MustCompile(`(?i)\bfix\b.*\b(bug|error|broken|issue)\b`)
	rxTrivial  = regexp.MustCompile(`(?i)\b(typo|rename|change the line|one\s*-?\s*line)\b`)
	rxDoc      = regexp.MustCompile(`(?i)(\bdocs?\b|\.md\b|readme|contributing)`)
	rxConfig   = regexp.MustCompile(`(?i)(\.ya?ml\b|\.toml\b|\.json\b|\bconfig\b)`)
	rxAnalysis = regexp.MustCompile(`(?i)\b(summari[sz]e|analy[sz]e|explore|list every|produce a list)\b`)
)

// ExtractFeatures runs the heuristic classifier on a task description.
// Deterministic — same input always produces the same output. Pre-compiled
// regexes keep it cheap (< 50 μs for typical task text).
//
// Rationale (see docs/routing/REWRITE_PLAN.md Phase 2): we deliberately
// start with heuristics, not an LLM call. Every `oat worker create` would
// pay an LLM RTT if we routed through a classifier model — that's both
// recursion (need a router to pick a classifier model) and latency. V1 is
// regex-cheap; V2 may add an LLM fallback for ambiguous cases if Phase 4
// data shows the heuristic misses ≥10% of tasks.
func ExtractFeatures(taskText string) TaskFeatures {
	text := strings.TrimSpace(taskText)
	lower := strings.ToLower(text)

	f := TaskFeatures{
		Text:              text,
		LengthChars:       len(text),
		FileCountEstimate: strings.Count(text, "/"),
	}

	f.IsTrivial = rxTrivial.MatchString(lower) && f.LengthChars < 160
	f.IsRefactor = rxRefactor.MatchString(lower)
	f.IsBugFix = rxBugFix.MatchString(lower)
	f.IsDocOrConfig = rxDoc.MatchString(lower) || rxConfig.MatchString(lower)
	f.IsAnalysis = rxAnalysis.MatchString(lower)

	// Bucketing. Mutually exclusive: first matching rule wins.
	//
	// IsDocOrConfig / IsAnalysis are kept as features (downstream consumers
	// may want them) but do NOT auto-trigger "simple" — the earlier rule
	// misclassified tasks like "Implement X as described in spec.md" because
	// they mention a .md file in passing. The safer default is length-based.
	switch {
	case f.IsTrivial:
		f.Complexity = ComplexityTrivial
	case f.IsRefactor, f.FileCountEstimate >= 4 && f.LengthChars > 200:
		f.Complexity = ComplexityComplex
	case f.LengthChars < 150:
		f.Complexity = ComplexitySimple
	default:
		f.Complexity = ComplexityStandard
	}

	return f
}
