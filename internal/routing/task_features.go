package routing

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// LoggedTaskFeatures is the structural feature vector extracted at log time. All
// fields are cheap to compute (regex + filesystem walk; no parsers, no
// embeddings, no network). Phase 1 ships these into every OutcomeRecord so
// Phase 3 lookup-routing and Phase 4 training have richer signal than the raw
// task text alone.
//
// Design notes:
//
//   - Features are deliberately conservative in scope. AST node counts,
//     embedding-based similarity, and git-history features are deferred to
//     Phase 4 only if these structural features hit a clear ceiling on
//     offline replay.
//   - All counts saturate cleanly at zero. Empty task text and missing
//     worktrees produce a non-nil struct with zero values rather than nil
//     so downstream consumers don't have to nil-check every field.
//   - Field names are stable. Renaming a field is a schema-breaking change
//     because feature buckets are computed from the JSON shape.
type LoggedTaskFeatures struct {
	// Length signals
	CharCount      int `json:"char_count"`
	LineCount      int `json:"line_count"`
	ParagraphCount int `json:"paragraph_count"`

	// Content-type signals — strong predictors for "this is a debugging task"
	// vs "this is a feature-implementation task".
	HasStackTrace  bool `json:"has_stack_trace,omitempty"`
	HasCIFailure   bool `json:"has_ci_failure,omitempty"`
	CodeBlockCount int  `json:"code_block_count,omitempty"`
	CodeBlockChars int  `json:"code_block_chars,omitempty"`

	// Reference signals — files/tests mentioned by path.
	FilePathMentions int `json:"file_path_mentions,omitempty"`
	TestFileMentions int `json:"test_file_mentions,omitempty"`
	TODOMentions     int `json:"todo_mentions,omitempty"`

	// Intent signal — first imperative verb (refactor, fix, add, implement,
	// debug, port, migrate). Empty string if none detected. Useful as a
	// categorical feature in later training.
	ImperativeVerb string `json:"imperative_verb,omitempty"`

	// Worktree-derived signals. Empty/zero if WorktreePath is empty or
	// inaccessible (review/verification agents, deleted worktrees).
	LangDistribution  map[string]float64 `json:"lang_distribution,omitempty"` // file-counted, fractions sum to ≤1.0
	TestToSourceRatio float64            `json:"test_to_source_ratio,omitempty"`
	RepoSizeBucket    string             `json:"repo_size_bucket,omitempty"` // small | medium | large | xlarge | "" if unknown
	HasTestInfra      bool               `json:"has_test_infra,omitempty"`
}

// Regex patterns are compiled once at package init.
var (
	// Code blocks: triple-backtick fenced. Captures the body so we can sum chars.
	reCodeBlock = regexp.MustCompile("(?s)```[a-zA-Z0-9_-]*\\n?(.*?)```")

	// Stack-trace heuristics — covers Go, Python, JavaScript/Node, Java/Kotlin.
	// We don't try to parse; just detect the presence of frame-like lines.
	reStackTraceGo     = regexp.MustCompile(`(?m)^\s*[\w/.-]+\.go:\d+\s*\+0x[0-9a-f]+`)
	reStackTracePython = regexp.MustCompile(`(?m)^\s*File "[^"]+", line \d+`)
	reStackTraceJS     = regexp.MustCompile(`(?m)^\s*at \S+ \([^)]+:\d+:\d+\)`)
	reStackTraceJava   = regexp.MustCompile(`(?m)^\s*at [\w.$]+\([\w.$]+\.java:\d+\)`)
	reTracebackHeader  = regexp.MustCompile(`(?i)\b(traceback \(most recent call last\)|panic:|exception in thread|caused by:)`)

	// CI failure markers — signal that the task is "fix CI" rather than
	// "implement feature".
	reCIFailure = regexp.MustCompile(`(?i)\b(failed in \d+ms|failed in \d+s|exit (code|status) [1-9]|FAIL\s+[\w/.-]+|##\[error\]|build failed|tests failed|::error::)`)

	// File path mentions: relative paths or absolute-ish paths with extensions.
	// Anchored on a leading delimiter (start, whitespace, quote) so we don't
	// match version numbers like "1.2". The trailing boundary is `\b` which
	// matches between an alnum (end of extension) and any non-alnum (period,
	// space, quote, comma, end of input). RE2 supports \b.
	reFilePath = regexp.MustCompile(`(?:^|[\s"'` + "`" + `(])([a-zA-Z0-9_./-]+\.(?:go|py|ts|tsx|js|jsx|rs|java|kt|rb|cpp|cc|c|h|hpp|cs|php|swift|mm|sh|yaml|yml|toml|json|md|txt|sql))\b`)

	// Test-file path subset of FilePath: looks for _test, test_, tests/, /test/.
	reTestPath = regexp.MustCompile(`(?:_test\.|test_|/tests?/|/specs?/|\.spec\.|\.test\.)`)

	// TODO / FIXME mentions.
	reTODO = regexp.MustCompile(`\b(TODO|FIXME|XXX|HACK)\b`)

	// First-line imperative verb. Order matters — earlier wins on ties.
	imperativeVerbs = []string{"refactor", "fix", "add", "implement", "debug", "port", "migrate", "remove", "rename", "update", "build", "create"}
	reImperative    = regexp.MustCompile(`(?i)^[^a-zA-Z]*(` + strings.Join(imperativeVerbs, "|") + `)\b`)
)

// ExtractLoggedTaskFeatures returns a feature vector for the given task text and
// optional worktree path. Never returns nil. Worktree fields are zero-valued
// if worktreePath is empty or stat fails — the function does not error.
//
// This is intended to be called at log time on the daemon's hot path. Bound
// the worst-case work: text-side scans are O(len(text)), worktree walk is
// capped at filesWalkCap files.
func ExtractLoggedTaskFeatures(taskText, worktreePath string) *LoggedTaskFeatures {
	tf := &LoggedTaskFeatures{}
	tf.fillFromText(taskText)
	if worktreePath != "" {
		tf.fillFromWorktree(worktreePath)
	}
	return tf
}

func (tf *LoggedTaskFeatures) fillFromText(s string) {
	if s == "" {
		return
	}
	tf.CharCount = len(s)
	tf.LineCount = strings.Count(s, "\n") + 1
	// Paragraph count: blocks separated by blank lines.
	paras := strings.Split(strings.TrimSpace(s), "\n\n")
	count := 0
	for _, p := range paras {
		if strings.TrimSpace(p) != "" {
			count++
		}
	}
	tf.ParagraphCount = count

	// Code blocks
	if matches := reCodeBlock.FindAllStringSubmatch(s, -1); len(matches) > 0 {
		tf.CodeBlockCount = len(matches)
		for _, m := range matches {
			if len(m) > 1 {
				tf.CodeBlockChars += len(m[1])
			}
		}
	}

	// Stack trace / CI failure
	if reStackTraceGo.MatchString(s) ||
		reStackTracePython.MatchString(s) ||
		reStackTraceJS.MatchString(s) ||
		reStackTraceJava.MatchString(s) ||
		reTracebackHeader.MatchString(s) {
		tf.HasStackTrace = true
	}
	if reCIFailure.MatchString(s) {
		tf.HasCIFailure = true
	}

	// File path mentions (and test-file subset)
	for _, m := range reFilePath.FindAllStringSubmatch(s, -1) {
		if len(m) < 2 {
			continue
		}
		tf.FilePathMentions++
		if reTestPath.MatchString(m[1]) {
			tf.TestFileMentions++
		}
	}

	// TODO mentions
	tf.TODOMentions = len(reTODO.FindAllString(s, -1))

	// Imperative verb on the first non-empty line.
	if firstLine := firstNonEmptyLine(s); firstLine != "" {
		if m := reImperative.FindStringSubmatch(firstLine); len(m) > 1 {
			tf.ImperativeVerb = strings.ToLower(m[1])
		}
	}
}

// filesWalkCap bounds worktree introspection. Real codebases can have 100k+
// files; we don't need exact counts to compute a language distribution, so we
// stop early. The cap is high enough to cover small-to-medium repos exactly
// and produce a representative sample for larger ones.
const filesWalkCap = 5000

// langExtensions maps file extensions to a coarse language label. Unknown
// extensions don't contribute to the distribution. Lowercase keys.
var langExtensions = map[string]string{
	".go":    "go",
	".py":    "python",
	".ts":    "typescript",
	".tsx":   "typescript",
	".js":    "javascript",
	".jsx":   "javascript",
	".rs":    "rust",
	".java":  "java",
	".kt":    "kotlin",
	".rb":    "ruby",
	".cpp":   "cpp",
	".cc":    "cpp",
	".cxx":   "cpp",
	".c":     "c",
	".h":     "c",
	".hpp":   "cpp",
	".cs":    "csharp",
	".php":   "php",
	".swift": "swift",
	".m":     "objc",
	".mm":    "objc",
	".sh":    "shell",
	".bash":  "shell",
}

// testInfraFiles names that indicate a real test setup is present (vs. just a
// few sample test files). Match against basename.
var testInfraFiles = map[string]struct{}{
	"pytest.ini":        {},
	"pyproject.toml":    {},
	"go.sum":            {},
	"jest.config.js":    {},
	"jest.config.ts":    {},
	"jest.config.cjs":   {},
	"jest.config.mjs":   {},
	"vitest.config.js":  {},
	"vitest.config.ts":  {},
	"karma.conf.js":     {},
	"playwright.config": {},
	"cypress.config.js": {},
	"cypress.config.ts": {},
	"tox.ini":           {},
	"phpunit.xml":       {},
	"rspec":             {},
}

func (tf *LoggedTaskFeatures) fillFromWorktree(worktreePath string) {
	info, err := os.Stat(worktreePath)
	if err != nil || !info.IsDir() {
		return
	}

	langCounts := make(map[string]int)
	totalSourceFiles := 0
	totalTestFiles := 0
	totalSourceBytes := int64(0)
	hasInfra := false
	walked := 0

	walkFn := func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // skip unreadable entries; feature extraction is best-effort
		}
		if d.IsDir() {
			// Skip vendor/dependency directories — they swamp the language signal.
			name := d.Name()
			if path != worktreePath && (name == "node_modules" || name == "vendor" || name == ".git" || name == "dist" || name == "build" || name == ".venv" || name == "__pycache__" || name == "target") {
				return filepath.SkipDir
			}
			return nil
		}
		walked++
		if walked > filesWalkCap {
			return filepath.SkipAll
		}

		base := strings.ToLower(d.Name())
		if _, ok := testInfraFiles[base]; ok {
			hasInfra = true
		}

		ext := strings.ToLower(filepath.Ext(base))
		lang, isSource := langExtensions[ext]
		if !isSource {
			return nil
		}
		langCounts[lang]++
		totalSourceFiles++

		if reTestPath.MatchString(path) {
			totalTestFiles++
		}

		if fi, err := d.Info(); err == nil {
			totalSourceBytes += fi.Size()
		}
		return nil
	}
	_ = filepath.WalkDir(worktreePath, walkFn)

	// Estimate LOC once after summing bytes — per-file integer division
	// truncates files <40 bytes to zero, which underflows the bucket on
	// small repos.
	totalLOCEstimate := totalSourceBytes / 40

	if totalSourceFiles > 0 {
		tf.LangDistribution = make(map[string]float64, len(langCounts))
		for lang, n := range langCounts {
			tf.LangDistribution[lang] = float64(n) / float64(totalSourceFiles)
		}
		nonTest := totalSourceFiles - totalTestFiles
		if nonTest > 0 {
			tf.TestToSourceRatio = float64(totalTestFiles) / float64(nonTest)
		}
		tf.RepoSizeBucket = bucketRepoSize(totalLOCEstimate)
	}
	tf.HasTestInfra = hasInfra
}

// bucketRepoSize maps an estimated total LOC to a coarse bucket. Buckets are
// log-spaced because repo sizes are heavy-tailed. A repo with any source
// files at all is at least "small" — only a totally empty/non-source tree
// returns "".
func bucketRepoSize(loc int64) string {
	switch {
	case loc < 0:
		return ""
	case loc < 10_000:
		return "small"
	case loc < 100_000:
		return "medium"
	case loc < 1_000_000:
		return "large"
	default:
		return "xlarge"
	}
}

func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if t != "" {
			return t
		}
	}
	return ""
}
