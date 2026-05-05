package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/pkg/config"
)

// newTestCLI creates a minimal CLI for verification tests (no daemon needed).
func newTestCLI(t *testing.T) *CLI {
	t.Helper()
	tmpDir := t.TempDir()
	paths := &config.Paths{
		Root:         tmpDir,
		WorktreesDir: filepath.Join(tmpDir, "wts"),
		ReposDir:     filepath.Join(tmpDir, "repos"),
	}
	return &CLI{paths: paths}
}

// ---------------------------------------------------------------------------
// findDuplicateBlocks
// ---------------------------------------------------------------------------

func TestFindDuplicateBlocks_Disabled(t *testing.T) {
	cli := newTestCLI(t)

	// findDuplicateBlocks is intentionally disabled (returns nil) because the
	// 3-line window produced excessive false positives on test code.
	content := strings.Join([]string{
		"func handler() {",
		"    doStuff()",
		"}",
		"",
		"func handler() {",
		"    doStuff()",
		"}",
		"",
		"func other() {}",
	}, "\n")

	dups := cli.findDuplicateBlocks(content)
	if len(dups) != 0 {
		t.Errorf("findDuplicateBlocks() should be disabled and return nil, got %v", dups)
	}
}

// ---------------------------------------------------------------------------
// linesMatch
// ---------------------------------------------------------------------------

func TestLinesMatch(t *testing.T) {
	cli := newTestCLI(t)

	tests := []struct {
		name string
		a, b []string
		want bool
	}{
		{"exact match", []string{"a", "b"}, []string{"a", "b"}, true},
		{"whitespace trimmed", []string{"  a  ", "\tb\t"}, []string{"a", "b"}, true},
		{"different content", []string{"a", "b"}, []string{"a", "c"}, false},
		{"different length", []string{"a"}, []string{"a", "b"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cli.linesMatch(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("linesMatch() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// calculateOverallScore
// ---------------------------------------------------------------------------

func TestCalculateOverallScore(t *testing.T) {
	cli := newTestCLI(t)

	tests := []struct {
		name   string
		result *VerificationResult
		want   float64
	}{
		{
			name: "all perfect scores",
			result: &VerificationResult{
				TaskAlignment:    VerificationCheck{Score: 100},
				FileIntegrity:    VerificationCheck{Score: 100},
				SyntaxValidation: VerificationCheck{Score: 100},
				TestExecution:    VerificationCheck{Score: 100},
				InputValidation:  VerificationCheck{Score: 100},
			},
			want: 100.0,
		},
		{
			name: "all zero scores",
			result: &VerificationResult{
				TaskAlignment:    VerificationCheck{Score: 0},
				FileIntegrity:    VerificationCheck{Score: 0},
				SyntaxValidation: VerificationCheck{Score: 0},
				TestExecution:    VerificationCheck{Score: 0},
				InputValidation:  VerificationCheck{Score: 0},
			},
			want: 0.0,
		},
		{
			name: "file integrity dominates (weight 0.35)",
			result: &VerificationResult{
				TaskAlignment:    VerificationCheck{Score: 100},
				FileIntegrity:    VerificationCheck{Score: 0}, // 0 * 0.35 = 0
				SyntaxValidation: VerificationCheck{Score: 100},
				TestExecution:    VerificationCheck{Score: 100},
				InputValidation:  VerificationCheck{Score: 100},
			},
			want: 65.0, // 15 + 0 + 20 + 25 + 5
		},
		{
			name: "borderline pass at 70",
			result: &VerificationResult{
				TaskAlignment:    VerificationCheck{Score: 70},
				FileIntegrity:    VerificationCheck{Score: 70},
				SyntaxValidation: VerificationCheck{Score: 70},
				TestExecution:    VerificationCheck{Score: 70},
				InputValidation:  VerificationCheck{Score: 70},
			},
			want: 70.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cli.calculateOverallScore(tt.result)
			if got != tt.want {
				t.Errorf("calculateOverallScore() = %.1f, want %.1f", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// checkLanguageSpecificIntegrity
// ---------------------------------------------------------------------------

func TestCheckPythonIntegrity(t *testing.T) {
	cli := newTestCLI(t)

	tests := []struct {
		name    string
		content string
		wantErr bool
	}{
		{
			name:    "valid python",
			content: "def hello():\n    print('hi')\n",
			wantErr: false,
		},
		{
			name:    "missing colon in def (no colon anywhere)",
			content: "def hello()\n    print('hi')\n",
			wantErr: true,
		},
		{
			name:    "mismatched parens",
			content: "def hello():\n    print('hi'\n",
			wantErr: true,
		},
		{
			name:    "balanced brackets",
			content: "x = {'a': [1, 2], 'b': (3, 4)}\n",
			wantErr: false,
		},
		{
			name:    "unclosed bracket",
			content: "x = {'a': [1, 2]\n",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := cli.checkPythonIntegrity(tt.content)
			if (err != nil) != tt.wantErr {
				t.Errorf("checkPythonIntegrity() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestCheckGoIntegrity(t *testing.T) {
	cli := newTestCLI(t)

	tests := []struct {
		name    string
		content string
		wantErr bool
	}{
		{
			name:    "valid go",
			content: "package main\n\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n",
			wantErr: false,
		},
		{
			name:    "missing package",
			content: "func main() {\n}\n",
			wantErr: true,
		},
		{
			name:    "mismatched braces",
			content: "package main\n\nfunc main() {\n\tfmt.Println(\"hi\")\n",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := cli.checkGoIntegrity(tt.content)
			if (err != nil) != tt.wantErr {
				t.Errorf("checkGoIntegrity() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestCheckJavaScriptIntegrity(t *testing.T) {
	cli := newTestCLI(t)

	tests := []struct {
		name    string
		content string
		wantErr bool
	}{
		{
			name:    "valid js",
			content: "function hello() {\n  console.log('hi');\n}\n",
			wantErr: false,
		},
		{
			name:    "mismatched braces",
			content: "function hello() {\n  console.log('hi');\n",
			wantErr: true,
		},
		{
			name:    "arrow function no braces ok",
			content: "const hello = () => console.log('hi');\n",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := cli.checkJavaScriptIntegrity(tt.content)
			if (err != nil) != tt.wantErr {
				t.Errorf("checkJavaScriptIntegrity() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestCheckMarkdownIntegrity(t *testing.T) {
	cli := newTestCLI(t)

	tests := []struct {
		name    string
		content string
		wantErr bool
	}{
		{
			name:    "balanced code blocks",
			content: "# Title\n```go\nfmt.Println()\n```\n",
			wantErr: false,
		},
		{
			name:    "unclosed code block",
			content: "# Title\n```go\nfmt.Println()\n",
			wantErr: true,
		},
		{
			name:    "no code blocks",
			content: "# Title\nSome text\n",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := cli.checkMarkdownIntegrity(tt.content)
			if (err != nil) != tt.wantErr {
				t.Errorf("checkMarkdownIntegrity() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// detectTestFramework
// ---------------------------------------------------------------------------

func TestDetectTestFramework(t *testing.T) {
	cli := newTestCLI(t)

	tests := []struct {
		name     string
		setup    func(dir string)
		expected string
	}{
		{
			name: "npm via package.json",
			setup: func(dir string) {
				os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"scripts":{"test":"jest"}}`), 0644)
			},
			expected: "npm",
		},
		{
			name: "pytest via pytest.ini",
			setup: func(dir string) {
				os.WriteFile(filepath.Join(dir, "pytest.ini"), []byte("[pytest]"), 0644)
			},
			expected: "pytest",
		},
		{
			name: "go via test file",
			setup: func(dir string) {
				os.WriteFile(filepath.Join(dir, "main_test.go"), []byte("package main"), 0644)
			},
			expected: "go",
		},
		{
			name: "go via nested test file",
			setup: func(dir string) {
				nestedDir := filepath.Join(dir, "internal", "cli")
				os.MkdirAll(nestedDir, 0755)
				os.WriteFile(filepath.Join(nestedDir, "verify_test.go"), []byte("package cli"), 0644)
			},
			expected: "go",
		},
		{
			name: "pytest via nested test file",
			setup: func(dir string) {
				nestedDir := filepath.Join(dir, "tests")
				os.MkdirAll(nestedDir, 0755)
				os.WriteFile(filepath.Join(nestedDir, "test_api.py"), []byte("def test_ok():\n    pass\n"), 0644)
			},
			expected: "pytest",
		},
		{
			name: "make via Makefile",
			setup: func(dir string) {
				os.WriteFile(filepath.Join(dir, "Makefile"), []byte("test:\n\techo ok"), 0644)
			},
			expected: "make",
		},
		{
			name:     "no framework",
			setup:    func(dir string) {},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			tt.setup(dir)

			// detectTestFramework uses os.Stat/Glob relative to cwd
			origDir, _ := os.Getwd()
			os.Chdir(dir)
			defer os.Chdir(origDir)

			got := cli.detectTestFramework()
			if got != tt.expected {
				t.Errorf("detectTestFramework() = %q, want %q", got, tt.expected)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// extractTestFailures
// ---------------------------------------------------------------------------

func TestExtractTestFailures(t *testing.T) {
	cli := newTestCLI(t)

	tests := []struct {
		name      string
		output    string
		framework string
		wantMin   int // minimum number of failures extracted
	}{
		{
			name:      "pytest failures",
			output:    "FAILED test_foo.py::test_bar - AssertionError\nERROR test_baz.py - ImportError\npassed 3",
			framework: "pytest",
			wantMin:   2,
		},
		{
			name:      "go test failures",
			output:    "--- FAIL: TestFoo (0.00s)\n    foo_test.go:10: expected 1 got 2\nFAIL:\tok\n",
			framework: "go",
			wantMin:   1,
		},
		{
			name:      "npm failures",
			output:    "FAIL src/app.test.js\n  ✕ should render (5ms)\n  ✓ should pass",
			framework: "npm",
			wantMin:   2,
		},
		{
			name:      "no failures",
			output:    "all tests passed\nok",
			framework: "pytest",
			wantMin:   0,
		},
		{
			name:      "truncated to 5 max",
			output:    "FAILED a\nFAILED b\nFAILED c\nFAILED d\nFAILED e\nFAILED f\nFAILED g",
			framework: "pytest",
			wantMin:   5, // should cap at 5 + "and N more"
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cli.extractTestFailures(tt.output, tt.framework)
			if len(got) < tt.wantMin {
				t.Errorf("extractTestFailures() returned %d failures, want at least %d (got=%v)", len(got), tt.wantMin, got)
			}
			if tt.name == "truncated to 5 max" {
				if len(got) != 6 {
					t.Fatalf("extractTestFailures() returned %d entries, want 6 (5 failures + summary)", len(got))
				}
				if got[5] != "... and 2 more failures" {
					t.Fatalf("extractTestFailures() summary = %q, want %q", got[5], "... and 2 more failures")
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// removeDuplicateBlocks - file-level test
// ---------------------------------------------------------------------------

func TestRemoveDuplicateBlocks_Disabled(t *testing.T) {
	cli := newTestCLI(t)

	// removeDuplicateBlocks is a no-op now — verify it doesn't modify files
	input := strings.Join([]string{
		"func handler() {",
		"    doStuff()",
		"}",
		"",
		"func handler() {",
		"    doStuff()",
		"}",
		"",
		"func other() {}",
	}, "\n")

	dir := t.TempDir()
	file := filepath.Join(dir, "test.go")
	os.WriteFile(file, []byte(input), 0644)

	err := cli.removeDuplicateBlocks(file)
	if err != nil {
		t.Fatalf("removeDuplicateBlocks() error = %v", err)
	}

	// File should be untouched since removeDuplicateBlocks is disabled
	result, _ := os.ReadFile(file)
	if string(result) != input {
		t.Error("removeDuplicateBlocks() modified the file, but it should be a no-op")
	}
}

// ---------------------------------------------------------------------------
// generateRecommendations
// ---------------------------------------------------------------------------

func TestGenerateRecommendations(t *testing.T) {
	cli := newTestCLI(t)

	tests := []struct {
		name    string
		result  *VerificationResult
		wantAny string // must contain this substring in at least one recommendation
	}{
		{
			name: "passing result",
			result: &VerificationResult{
				OverallScore:     90,
				TaskAlignment:    VerificationCheck{Passed: true},
				FileIntegrity:    VerificationCheck{Passed: true},
				SyntaxValidation: VerificationCheck{Passed: true},
				TestExecution:    VerificationCheck{Passed: true},
				InputValidation:  VerificationCheck{Passed: true},
			},
			wantAny: "Excellent",
		},
		{
			name: "failing file integrity",
			result: &VerificationResult{
				OverallScore:     50,
				TaskAlignment:    VerificationCheck{Passed: true},
				FileIntegrity:    VerificationCheck{Passed: false, Issues: []string{"truncated"}},
				SyntaxValidation: VerificationCheck{Passed: true},
				TestExecution:    VerificationCheck{Passed: true},
				InputValidation:  VerificationCheck{Passed: true},
			},
			wantAny: "Fix file integrity",
		},
		{
			name: "failing tests",
			result: &VerificationResult{
				OverallScore:     40,
				TaskAlignment:    VerificationCheck{Passed: true},
				FileIntegrity:    VerificationCheck{Passed: true},
				SyntaxValidation: VerificationCheck{Passed: true},
				TestExecution:    VerificationCheck{Passed: false},
				InputValidation:  VerificationCheck{Passed: true},
			},
			wantAny: "Fix failing tests",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recs := cli.generateRecommendations(tt.result)
			found := false
			for _, rec := range recs {
				if strings.Contains(rec, tt.wantAny) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("generateRecommendations() missing %q in recommendations: %v", tt.wantAny, recs)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// truncateString
// ---------------------------------------------------------------------------

func TestVerifyTruncateString(t *testing.T) {
	cli := newTestCLI(t)

	tests := []struct {
		name   string
		input  string
		maxLen int
		want   string
	}{
		{"short string", "hello", 10, "hello"},
		{"exact length", "hello", 5, "hello"},
		{"truncated", "hello world", 8, "hello..."},
		{"very short max", "hello", 4, "h..."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cli.truncateString(tt.input, tt.maxLen)
			if got != tt.want {
				t.Errorf("truncateString(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// checkLanguageSpecificIntegrity dispatch
// ---------------------------------------------------------------------------

func TestCheckLanguageSpecificIntegrity(t *testing.T) {
	cli := newTestCLI(t)

	tests := []struct {
		name    string
		file    string
		content string
		wantErr bool
	}{
		{"python ok", "test.py", "def f():\n    pass\n", false},
		{"go ok", "test.go", "package main\n\nfunc main() {}\n", false},
		{"js ok", "test.js", "function f() {}\n", false},
		{"md ok", "test.md", "# Title\n```\ncode\n```\n", false},
		{"unknown ext ok", "test.xyz", "anything", false},
		{"python broken", "test.py", "x = (\n", true},
		{"go broken", "test.go", "func main() {\n", true},        // missing package
		{"js broken", "test.js", "function f() {\n", true},       // unclosed brace
		{"md broken", "test.md", "# Title\n```go\ncode\n", true}, // unclosed code block
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := cli.checkLanguageSpecificIntegrity(tt.file, tt.content)
			if (err != nil) != tt.wantErr {
				t.Errorf("checkLanguageSpecificIntegrity(%s) error = %v, wantErr %v", tt.file, err, tt.wantErr)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// extractFilenameFromIssue
// ---------------------------------------------------------------------------

func TestExtractFilenameFromIssue(t *testing.T) {
	cli := newTestCLI(t)

	tests := []struct {
		name  string
		issue string
		want  string
	}{
		{"python file", "Syntax error in /tmp/test.py: invalid syntax", "/tmp/test.py"},
		{"go file", "File internal/cli/verify.go has duplicate code blocks at lines: [1 5]", "internal/cli/verify.go"},
		{"no file", "Something went wrong", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cli.extractFilenameFromIssue(tt.issue)
			if got != tt.want {
				t.Errorf("extractFilenameFromIssue(%q) = %q, want %q", tt.issue, got, tt.want)
			}
		})
	}
}

func TestAttemptAutoFix_PythonSyntaxIssueCaseInsensitive(t *testing.T) {
	cli := newTestCLI(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "broken.py")
	input := "def broken()\n    return 1\n"
	if err := os.WriteFile(file, []byte(input), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	result := &VerificationResult{
		SyntaxValidation: VerificationCheck{
			Passed: false,
			Issues: []string{fmt.Sprintf("Syntax error in %s: invalid syntax", file)},
		},
		AutoFixResults: make(map[string]string),
	}

	cli.attemptAutoFix(context.Background(), result)

	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !strings.Contains(string(got), "def broken():") {
		t.Fatalf("attemptAutoFix() did not add missing colon, got %q", string(got))
	}
	if result.AutoFixResults[result.SyntaxValidation.Issues[0]] != "Fixed Python syntax" {
		t.Fatalf("attemptAutoFix() did not record auto-fix result: %#v", result.AutoFixResults)
	}
}

// ---------------------------------------------------------------------------
// getLastVerificationForCommit
// ---------------------------------------------------------------------------

func TestGetLastVerificationForCommit(t *testing.T) {
	cli := newTestCLI(t)

	logFile := filepath.Join(cli.paths.Root, "verification.log")

	t.Run("no log file", func(t *testing.T) {
		_, found := cli.getLastVerificationForCommit("abc123")
		if found {
			t.Error("expected found=false when log file doesn't exist")
		}
	})

	t.Run("matching commit passed", func(t *testing.T) {
		entry := map[string]interface{}{
			"commit_sha":     "abc123def456",
			"overall_passed": true,
			"overall_score":  85.0,
		}
		data, _ := json.Marshal(entry)
		os.WriteFile(logFile, data, 0644)

		passed, found := cli.getLastVerificationForCommit("abc123def456")
		if !found {
			t.Fatal("expected found=true for matching commit")
		}
		if !passed {
			t.Error("expected passed=true")
		}
	})

	t.Run("matching commit failed", func(t *testing.T) {
		entry := map[string]interface{}{
			"commit_sha":     "fail789",
			"overall_passed": false,
			"overall_score":  40.0,
		}
		data, _ := json.Marshal(entry)
		os.WriteFile(logFile, data, 0644)

		passed, found := cli.getLastVerificationForCommit("fail789")
		if !found {
			t.Fatal("expected found=true")
		}
		if passed {
			t.Error("expected passed=false for failing commit")
		}
	})

	t.Run("no matching commit", func(t *testing.T) {
		entry := map[string]interface{}{
			"commit_sha":     "other_sha",
			"overall_passed": true,
		}
		data, _ := json.Marshal(entry)
		os.WriteFile(logFile, data, 0644)

		_, found := cli.getLastVerificationForCommit("not_in_log")
		if found {
			t.Error("expected found=false for non-matching commit")
		}
	})

	t.Run("multiple entries uses latest match", func(t *testing.T) {
		lines := []string{}
		// First run: failed
		e1, _ := json.Marshal(map[string]interface{}{
			"commit_sha": "sha_multi", "overall_passed": false,
		})
		lines = append(lines, string(e1))
		// Second run: passed (after --fix)
		e2, _ := json.Marshal(map[string]interface{}{
			"commit_sha": "sha_multi", "overall_passed": true,
		})
		lines = append(lines, string(e2))
		os.WriteFile(logFile, []byte(strings.Join(lines, "\n")), 0644)

		passed, found := cli.getLastVerificationForCommit("sha_multi")
		if !found {
			t.Fatal("expected found=true")
		}
		if !passed {
			t.Error("expected passed=true (latest entry should win)")
		}
	})
}

// ---------------------------------------------------------------------------
// presentVerificationResultsJSON
// ---------------------------------------------------------------------------

func TestPresentVerificationResultsJSON(t *testing.T) {
	cli := newTestCLI(t)

	result := &VerificationResult{
		OverallScore:  85.5,
		OverallPassed: true,
		TaskAlignment: VerificationCheck{Name: "Task Alignment", Score: 90, Passed: true},
		FileIntegrity: VerificationCheck{Name: "File Integrity", Score: 100, Passed: true},
	}

	// presentVerificationResultsJSON prints to stdout — just verify it doesn't error
	err := cli.presentVerificationResultsJSON(result)
	if err != nil {
		t.Errorf("presentVerificationResultsJSON() error = %v", err)
	}
}

// ---------------------------------------------------------------------------
// getBaseBranchRef
// ---------------------------------------------------------------------------

func TestGetBaseBranchRef(t *testing.T) {
	cli := newTestCLI(t)

	t.Run("returns origin/main in this repo", func(t *testing.T) {
		// This test runs inside the open-agent-teams repo which uses origin/main.
		// If running in CI without a remote, it may return empty — that's ok.
		ref := cli.getBaseBranchRef()
		if ref != "" && ref != "origin/main" && ref != "origin/master" {
			t.Errorf("getBaseBranchRef() = %q, want origin/main or origin/master or empty", ref)
		}
	})

	t.Run("returns empty for repo without origin", func(t *testing.T) {
		dir := t.TempDir()
		// Init a bare git repo with no remote
		origDir, _ := os.Getwd()
		os.Chdir(dir)
		defer os.Chdir(origDir)

		os.MkdirAll(filepath.Join(dir, ".git"), 0755)
		// Use exec to init
		cmd := execCommand("git", "init")
		cmd.Dir = dir
		cmd.Run()

		ref := cli.getBaseBranchRef()
		if ref != "" {
			t.Errorf("getBaseBranchRef() = %q for repo without origin, want empty", ref)
		}
	})
}

// execCommand is a test helper wrapping exec.Command
func execCommand(name string, args ...string) *exec.Cmd {
	return exec.Command(name, args...)
}

func TestVerifyWorkerBlockedByPendingVerifier(t *testing.T) {
	tmpDir := t.TempDir()
	// Resolve symlinks (macOS /tmp -> /private/tmp) so cwd matches paths.WorktreesDir.
	tmpDir, _ = filepath.EvalSymlinks(tmpDir)
	paths := &config.Paths{
		Root:         tmpDir,
		WorktreesDir: filepath.Join(tmpDir, "wts"),
		ReposDir:     filepath.Join(tmpDir, "repos"),
		StateFile:    filepath.Join(tmpDir, "state.json"),
	}
	cli := &CLI{paths: paths}

	workerDir := filepath.Join(tmpDir, "wts", "test-repo", "my-worker")
	os.MkdirAll(workerDir, 0755)

	// Create a state file with a worker that has a pending verifier (< 5 min old).
	st := map[string]interface{}{
		"repos": map[string]interface{}{
			"test-repo": map[string]interface{}{
				"github_url":   "https://github.com/test/repo",
				"session_name": "test-session",
				"agents": map[string]interface{}{
					"my-worker": map[string]interface{}{
						"type":                "worker",
						"window_name":         "my-worker",
						"verification_status": "pending",
						"verification_agent":  "verify-my-worker",
						"created_at":          time.Now().Add(-10 * time.Minute).Format(time.RFC3339),
					},
					"verify-my-worker": map[string]interface{}{
						"type":        "verification",
						"window_name": "verify-my-worker",
						"created_at":  time.Now().Add(-30 * time.Second).Format(time.RFC3339),
					},
				},
			},
		},
	}
	data, _ := json.MarshalIndent(st, "", "  ")
	os.WriteFile(paths.StateFile, data, 0644)

	origDir, _ := os.Getwd()
	os.Chdir(workerDir)
	defer os.Chdir(origDir)

	err := cli.verifyWorker(nil)
	if err == nil {
		t.Fatal("verifyWorker() should have returned an error when verifier is < 5 min old")
	}
	if !strings.Contains(err.Error(), "went dormant to wait for verdict") {
		t.Errorf("error should mention 'went dormant to wait for verdict', got: %v", err)
	}
}

func TestVerifyWorkerAllowedWhenVerifierExpired(t *testing.T) {
	tmpDir := t.TempDir()
	tmpDir, _ = filepath.EvalSymlinks(tmpDir)
	paths := &config.Paths{
		Root:         tmpDir,
		WorktreesDir: filepath.Join(tmpDir, "wts"),
		ReposDir:     filepath.Join(tmpDir, "repos"),
		StateFile:    filepath.Join(tmpDir, "state.json"),
	}
	cli := &CLI{paths: paths}

	workerDir := filepath.Join(tmpDir, "wts", "test-repo", "my-worker")
	os.MkdirAll(workerDir, 0755)

	// Create a state file with a verifier that has been running for > 5 minutes.
	st := map[string]interface{}{
		"repos": map[string]interface{}{
			"test-repo": map[string]interface{}{
				"github_url":   "https://github.com/test/repo",
				"session_name": "test-session",
				"agents": map[string]interface{}{
					"my-worker": map[string]interface{}{
						"type":                "worker",
						"window_name":         "my-worker",
						"verification_status": "pending",
						"verification_agent":  "verify-my-worker",
						"created_at":          time.Now().Add(-30 * time.Minute).Format(time.RFC3339),
					},
					"verify-my-worker": map[string]interface{}{
						"type":        "verification",
						"window_name": "verify-my-worker",
						"created_at":  time.Now().Add(-10 * time.Minute).Format(time.RFC3339),
					},
				},
			},
		},
	}
	data, _ := json.MarshalIndent(st, "", "  ")
	os.WriteFile(paths.StateFile, data, 0644)

	origDir, _ := os.Getwd()
	os.Chdir(workerDir)
	defer os.Chdir(origDir)

	// verifyWorker will pass the guard but fail later (no git repo, etc.) — that's fine.
	// We only care that the verification guard does NOT block.
	err := cli.verifyWorker(nil)
	if err != nil && strings.Contains(err.Error(), "self-verify blocked") {
		t.Fatal("verifyWorker() should NOT block when verifier has been running > 5 minutes")
	}
}

func TestVerifyWorkerAllowedWhenVerifierGone(t *testing.T) {
	tmpDir := t.TempDir()
	tmpDir, _ = filepath.EvalSymlinks(tmpDir)
	paths := &config.Paths{
		Root:         tmpDir,
		WorktreesDir: filepath.Join(tmpDir, "wts"),
		ReposDir:     filepath.Join(tmpDir, "repos"),
		StateFile:    filepath.Join(tmpDir, "state.json"),
	}
	cli := &CLI{paths: paths}

	workerDir := filepath.Join(tmpDir, "wts", "test-repo", "my-worker")
	os.MkdirAll(workerDir, 0755)

	// Verifier referenced but not in agent list (crashed and cleaned up).
	st := map[string]interface{}{
		"repos": map[string]interface{}{
			"test-repo": map[string]interface{}{
				"github_url":   "https://github.com/test/repo",
				"session_name": "test-session",
				"agents": map[string]interface{}{
					"my-worker": map[string]interface{}{
						"type":                "worker",
						"window_name":         "my-worker",
						"verification_status": "pending",
						"verification_agent":  "verify-my-worker",
						"created_at":          time.Now().Add(-10 * time.Minute).Format(time.RFC3339),
					},
				},
			},
		},
	}
	data, _ := json.MarshalIndent(st, "", "  ")
	os.WriteFile(paths.StateFile, data, 0644)

	origDir, _ := os.Getwd()
	os.Chdir(workerDir)
	defer os.Chdir(origDir)

	err := cli.verifyWorker(nil)
	if err != nil && strings.Contains(err.Error(), "self-verify blocked") {
		t.Fatal("verifyWorker() should NOT block when verifier is gone from state")
	}
}

// TestGatherVerificationContextUsesPinnedBaseSHA verifies that
// gatherVerificationContext reads the worker's pinned BaseSHA from state
// and uses it as the diff base. The git diff sub-commands themselves
// fail (no real repo), but BaseRef must still reflect what the verifier
// would diff against.
func TestGatherVerificationContextUsesPinnedBaseSHA(t *testing.T) {
	tmpDir := t.TempDir()
	tmpDir, _ = filepath.EvalSymlinks(tmpDir)
	paths := &config.Paths{
		Root:         tmpDir,
		WorktreesDir: filepath.Join(tmpDir, "wts"),
		ReposDir:     filepath.Join(tmpDir, "repos"),
		StateFile:    filepath.Join(tmpDir, "state.json"),
	}
	cli := &CLI{paths: paths}

	pinnedSHA := "abcdef0123456789abcdef0123456789abcdef01"
	st := map[string]interface{}{
		"repos": map[string]interface{}{
			"test-repo": map[string]interface{}{
				"github_url":   "https://github.com/test/repo",
				"session_name": "test-session",
				"agents": map[string]interface{}{
					"my-worker": map[string]interface{}{
						"type":        "worker",
						"window_name": "my-worker",
						"created_at":  time.Now().Format(time.RFC3339),
						"base_sha":    pinnedSHA,
					},
				},
			},
		},
	}
	data, _ := json.MarshalIndent(st, "", "  ")
	if err := os.WriteFile(paths.StateFile, data, 0644); err != nil {
		t.Fatalf("write state: %v", err)
	}

	vctx := cli.gatherVerificationContext("test-repo", "my-worker")
	if vctx.BaseRef != pinnedSHA {
		t.Errorf("BaseRef = %q, want pinned SHA %q", vctx.BaseRef, pinnedSHA)
	}
}

// TestGatherVerificationContextFallbackToOriginMain verifies that when no
// BaseSHA is set on the worker (upgrade path or daemon-side snapshot
// failure), gatherVerificationContext falls back to literal "origin/main".
func TestGatherVerificationContextFallbackToOriginMain(t *testing.T) {
	tmpDir := t.TempDir()
	tmpDir, _ = filepath.EvalSymlinks(tmpDir)
	paths := &config.Paths{
		Root:         tmpDir,
		WorktreesDir: filepath.Join(tmpDir, "wts"),
		ReposDir:     filepath.Join(tmpDir, "repos"),
		StateFile:    filepath.Join(tmpDir, "state.json"),
	}
	cli := &CLI{paths: paths}

	st := map[string]interface{}{
		"repos": map[string]interface{}{
			"test-repo": map[string]interface{}{
				"github_url":   "https://github.com/test/repo",
				"session_name": "test-session",
				"agents": map[string]interface{}{
					"my-worker": map[string]interface{}{
						"type":        "worker",
						"window_name": "my-worker",
						"created_at":  time.Now().Format(time.RFC3339),
					},
				},
			},
		},
	}
	data, _ := json.MarshalIndent(st, "", "  ")
	if err := os.WriteFile(paths.StateFile, data, 0644); err != nil {
		t.Fatalf("write state: %v", err)
	}

	vctx := cli.gatherVerificationContext("test-repo", "my-worker")
	if vctx.BaseRef != "origin/main" {
		t.Errorf("BaseRef = %q, want %q (fallback)", vctx.BaseRef, "origin/main")
	}
}

// TestGetDiffBaseRefUsesPinnedBaseSHA verifies that self-verify
// (getImplementationSummary, getModifiedFiles via getDiffBaseRef) reads
// the worker's pinned BaseSHA from state when the CLI is run inside the
// worker's worktree -- ensuring self-verify and the verifier diff
// against the same base.
func TestGetDiffBaseRefUsesPinnedBaseSHA(t *testing.T) {
	tmpDir := t.TempDir()
	tmpDir, _ = filepath.EvalSymlinks(tmpDir)
	paths := &config.Paths{
		Root:         tmpDir,
		WorktreesDir: filepath.Join(tmpDir, "wts"),
		ReposDir:     filepath.Join(tmpDir, "repos"),
		StateFile:    filepath.Join(tmpDir, "state.json"),
	}
	cli := &CLI{paths: paths}

	pinnedSHA := "deadbeef0123456789deadbeef0123456789dead"
	st := map[string]interface{}{
		"repos": map[string]interface{}{
			"test-repo": map[string]interface{}{
				"github_url":   "https://github.com/test/repo",
				"session_name": "test-session",
				"agents": map[string]interface{}{
					"my-worker": map[string]interface{}{
						"type":        "worker",
						"window_name": "my-worker",
						"created_at":  time.Now().Format(time.RFC3339),
						"base_sha":    pinnedSHA,
					},
				},
			},
		},
	}
	data, _ := json.MarshalIndent(st, "", "  ")
	if err := os.WriteFile(paths.StateFile, data, 0644); err != nil {
		t.Fatalf("write state: %v", err)
	}

	workerDir := filepath.Join(paths.WorktreesDir, "test-repo", "my-worker")
	if err := os.MkdirAll(workerDir, 0755); err != nil {
		t.Fatalf("mkdir worker: %v", err)
	}

	origDir, _ := os.Getwd()
	if err := os.Chdir(workerDir); err != nil {
		t.Fatalf("chdir worker: %v", err)
	}
	defer os.Chdir(origDir)

	if got := cli.getDiffBaseRef(); got != pinnedSHA {
		t.Errorf("getDiffBaseRef() = %q, want pinned SHA %q", got, pinnedSHA)
	}
}

// TestGetDiffBaseRefFallsBackWithoutPinnedSHA verifies that when the
// worker has no BaseSHA on its state, getDiffBaseRef falls through to
// the live getBaseBranchRef() (which returns "" without a real git
// repo). This is the documented happy path for self-verify run
// BEFORE any request-review.
func TestGetDiffBaseRefFallsBackWithoutPinnedSHA(t *testing.T) {
	tmpDir := t.TempDir()
	tmpDir, _ = filepath.EvalSymlinks(tmpDir)
	paths := &config.Paths{
		Root:         tmpDir,
		WorktreesDir: filepath.Join(tmpDir, "wts"),
		ReposDir:     filepath.Join(tmpDir, "repos"),
		StateFile:    filepath.Join(tmpDir, "state.json"),
	}
	cli := &CLI{paths: paths}

	st := map[string]interface{}{
		"repos": map[string]interface{}{
			"test-repo": map[string]interface{}{
				"github_url":   "https://github.com/test/repo",
				"session_name": "test-session",
				"agents": map[string]interface{}{
					"my-worker": map[string]interface{}{
						"type":        "worker",
						"window_name": "my-worker",
						"created_at":  time.Now().Format(time.RFC3339),
					},
				},
			},
		},
	}
	data, _ := json.MarshalIndent(st, "", "  ")
	if err := os.WriteFile(paths.StateFile, data, 0644); err != nil {
		t.Fatalf("write state: %v", err)
	}

	workerDir := filepath.Join(paths.WorktreesDir, "test-repo", "my-worker")
	if err := os.MkdirAll(workerDir, 0755); err != nil {
		t.Fatalf("mkdir worker: %v", err)
	}

	origDir, _ := os.Getwd()
	if err := os.Chdir(workerDir); err != nil {
		t.Fatalf("chdir worker: %v", err)
	}
	defer os.Chdir(origDir)

	// No pinned BaseSHA, no real git repo in cwd -> live fallback returns "".
	got := cli.getDiffBaseRef()
	if got == "deadbeef0123456789deadbeef0123456789dead" {
		t.Errorf("getDiffBaseRef() should not return pinned SHA when state has no base_sha; got %q", got)
	}
	// Don't assert on the exact fallback value: getBaseBranchRef may
	// resolve via the OUTER repo's .git if the test runs from inside
	// open-agent-teams. The contract under test is just "did NOT use a
	// stale or fabricated SHA"; the fallback string is verified by the
	// gatherVerificationContext tests above.
}

// TestGatherVerificationContextNoStateFallback verifies that when state
// can't be loaded at all (no state file), the function still returns a
// usable context with BaseRef defaulting to origin/main rather than
// crashing.
func TestGatherVerificationContextNoStateFallback(t *testing.T) {
	tmpDir := t.TempDir()
	tmpDir, _ = filepath.EvalSymlinks(tmpDir)
	paths := &config.Paths{
		Root:         tmpDir,
		WorktreesDir: filepath.Join(tmpDir, "wts"),
		ReposDir:     filepath.Join(tmpDir, "repos"),
		StateFile:    filepath.Join(tmpDir, "no-such-state.json"),
	}
	cli := &CLI{paths: paths}

	vctx := cli.gatherVerificationContext("test-repo", "my-worker")
	if vctx.BaseRef != "origin/main" {
		t.Errorf("BaseRef = %q, want %q (fallback when no state)", vctx.BaseRef, "origin/main")
	}
}
