package routing

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExtractLoggedTaskFeatures_EmptyInput(t *testing.T) {
	tf := ExtractLoggedTaskFeatures("", "")
	if tf == nil {
		t.Fatal("must not return nil")
	}
	if tf.CharCount != 0 || tf.LineCount != 0 || tf.ParagraphCount != 0 {
		t.Errorf("zero values expected for empty input, got %+v", tf)
	}
	if tf.HasStackTrace || tf.HasCIFailure {
		t.Errorf("no false positives on empty input")
	}
}

func TestExtractLoggedTaskFeatures_LengthSignals(t *testing.T) {
	text := "first paragraph line 1\nfirst paragraph line 2\n\nsecond paragraph\n\nthird"
	tf := ExtractLoggedTaskFeatures(text, "")
	if tf.CharCount != len(text) {
		t.Errorf("CharCount: got %d want %d", tf.CharCount, len(text))
	}
	// 5 newlines + 1 = 6 newline-delimited chunks (including the empty
	// blank-line chunks between paragraphs).
	if tf.LineCount != 6 {
		t.Errorf("LineCount: got %d want 6", tf.LineCount)
	}
	if tf.ParagraphCount != 3 {
		t.Errorf("ParagraphCount: got %d want 3", tf.ParagraphCount)
	}
}

func TestExtractLoggedTaskFeatures_StackTraces(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"go panic", "panic: nil pointer\n\ngoroutine 1:\nmain.main\n\t/usr/src/foo.go:42 +0xab"},
		{"python traceback", "Traceback (most recent call last):\n  File \"app.py\", line 12, in <module>\n    raise ValueError(\"x\")"},
		{"node js", "TypeError: Cannot read property 'foo' of undefined\n    at Module._compile (/app/server.js:42:15)"},
		{"java", "java.lang.NullPointerException\n\tat com.foo.Bar.run(Bar.java:42)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tf := ExtractLoggedTaskFeatures(tc.text, "")
			if !tf.HasStackTrace {
				t.Errorf("expected HasStackTrace=true for %s, text=%q", tc.name, tc.text)
			}
		})
	}

	// Negative case: prose mentioning "stack trace" without an actual frame.
	tf := ExtractLoggedTaskFeatures("Add a stack trace to the error message.", "")
	if tf.HasStackTrace {
		t.Error("false positive on prose mentioning stack trace")
	}
}

func TestExtractLoggedTaskFeatures_CIFailureMarkers(t *testing.T) {
	yes := []string{
		"##[error]Process completed with exit code 1.",
		"FAIL\tpkg/foo\t0.123s",
		"build failed: see logs",
		"::error::tests failed",
		"Tests failed in 12s",
	}
	for _, txt := range yes {
		tf := ExtractLoggedTaskFeatures(txt, "")
		if !tf.HasCIFailure {
			t.Errorf("expected HasCIFailure=true for %q", txt)
		}
	}
	tf := ExtractLoggedTaskFeatures("Add CI checks to the repo.", "")
	if tf.HasCIFailure {
		t.Error("false positive on prose mentioning CI")
	}
}

func TestExtractLoggedTaskFeatures_CodeBlocks(t *testing.T) {
	text := "Implement this:\n```go\nfunc Foo() {}\n```\nand also:\n```python\nprint('x')\n```"
	tf := ExtractLoggedTaskFeatures(text, "")
	if tf.CodeBlockCount != 2 {
		t.Errorf("CodeBlockCount: got %d want 2", tf.CodeBlockCount)
	}
	if tf.CodeBlockChars == 0 {
		t.Error("CodeBlockChars should be > 0")
	}
}

func TestExtractLoggedTaskFeatures_FilePathMentions(t *testing.T) {
	text := "Update internal/foo/bar.go and tests in internal/foo/bar_test.go. Also see docs/README.md."
	tf := ExtractLoggedTaskFeatures(text, "")
	if tf.FilePathMentions != 3 {
		t.Errorf("FilePathMentions: got %d want 3", tf.FilePathMentions)
	}
	if tf.TestFileMentions != 1 {
		t.Errorf("TestFileMentions: got %d want 1", tf.TestFileMentions)
	}
}

func TestExtractLoggedTaskFeatures_ImperativeVerb(t *testing.T) {
	cases := map[string]string{
		"Fix the typo in main.go":     "fix",
		"Refactor the auth module":    "refactor",
		"Add a new endpoint /healthz": "add",
		"Implement the spec":          "implement",
		"Just exploring":              "", // no leading imperative
		"  - fix something":           "fix",
	}
	for text, want := range cases {
		tf := ExtractLoggedTaskFeatures(text, "")
		if tf.ImperativeVerb != want {
			t.Errorf("text=%q ImperativeVerb=%q want %q", text, tf.ImperativeVerb, want)
		}
	}
}

func TestExtractLoggedTaskFeatures_TODOMentions(t *testing.T) {
	text := "TODO(rj): fix this. FIXME later. NOT_A_TODO."
	tf := ExtractLoggedTaskFeatures(text, "")
	if tf.TODOMentions != 2 {
		t.Errorf("TODOMentions: got %d want 2", tf.TODOMentions)
	}
}

func TestExtractLoggedTaskFeatures_WorktreeFromDisk(t *testing.T) {
	dir := t.TempDir()

	mustWrite := func(rel string, body string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	mustWrite("internal/foo/foo.go", "package foo\nfunc F() {}\n")
	mustWrite("internal/foo/foo_test.go", "package foo\n")
	mustWrite("internal/bar/bar.go", "package bar\n")
	mustWrite("scripts/build.sh", "#!/usr/bin/env bash\n")
	mustWrite("docs/README.md", "# docs\n")
	// Test infra signal
	mustWrite("go.sum", "")
	// Should be skipped — vendor dir
	mustWrite("vendor/some/lib.go", "// not counted")
	mustWrite("node_modules/foo/index.js", "// not counted")

	tf := ExtractLoggedTaskFeatures("", dir)
	if !tf.HasTestInfra {
		t.Error("HasTestInfra should be true (go.sum present)")
	}
	if got := tf.LangDistribution["go"]; got <= 0 {
		t.Errorf("LangDistribution[go] should be > 0, got %v", got)
	}
	// vendor/node_modules excluded — only 3 .go files counted (foo.go, foo_test.go, bar.go).
	// .sh and .md don't bump go count.
	totalSourceFromGo := tf.LangDistribution["go"]
	if totalSourceFromGo <= 0.5 {
		// 3 of 5 source files are go (.go ×3, .sh ×1, .md ×1) = 0.6
		t.Errorf("expected go to dominate distribution, got %v", totalSourceFromGo)
	}
	if tf.RepoSizeBucket != "small" {
		t.Errorf("RepoSizeBucket: got %q want small", tf.RepoSizeBucket)
	}
	// 1 test file out of 2 non-test go files = 0.5
	if tf.TestToSourceRatio == 0 {
		t.Error("TestToSourceRatio should be > 0")
	}
}

func TestExtractLoggedTaskFeatures_MissingWorktreeIsSafe(t *testing.T) {
	// Non-existent path must not panic and must leave worktree fields empty.
	tf := ExtractLoggedTaskFeatures("hello", "/no/such/path/exists/at/all")
	if tf == nil {
		t.Fatal("must not be nil")
	}
	if tf.HasTestInfra {
		t.Error("missing worktree should not set HasTestInfra")
	}
	if tf.LangDistribution != nil {
		t.Error("missing worktree should not populate LangDistribution")
	}
}
