package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// getOriginalTask retrieves the original task from agent state.
// It tries the daemon socket first, then falls back to reading the state file directly.
func (c *CLI) getOriginalTask(repoName, agentName string) (string, error) {
	// Try daemon socket first
	resp, err := c.sendDaemonRequest("list_agents", map[string]interface{}{
		"repo": repoName,
		"rich": true,
	})
	if err == nil && resp != nil && resp.Data != nil {
		if dataMap, ok := resp.Data.(map[string]interface{}); ok {
			if agents, ok := dataMap["agents"].([]interface{}); ok {
				for _, a := range agents {
					if agent, ok := a.(map[string]interface{}); ok {
						if name, _ := agent["name"].(string); name == agentName {
							if task, _ := agent["task"].(string); task != "" {
								return task, nil
							}
						}
					}
				}
			}
		}
	}

	// Fallback: read state file directly
	stateFile := filepath.Join(c.paths.Root, "state.json")
	data, err := os.ReadFile(stateFile)
	if err == nil {
		var stateData map[string]interface{}
		if json.Unmarshal(data, &stateData) == nil {
			if repos, ok := stateData["repos"].(map[string]interface{}); ok {
				if repo, ok := repos[repoName].(map[string]interface{}); ok {
					if agents, ok := repo["agents"].(map[string]interface{}); ok {
						if agent, ok := agents[agentName].(map[string]interface{}); ok {
							if task, _ := agent["task"].(string); task != "" {
								return task, nil
							}
						}
					}
				}
			}
		}
	}

	return "", fmt.Errorf("could not retrieve task for agent %s in repo %s", agentName, repoName)
}

// getBaseBranchRef returns the remote ref for the default branch (e.g., "origin/main" or "origin/master").
// Mirrors the logic in worktree.Manager.GetDefaultBranch():
//  1. Try git symbolic-ref refs/remotes/origin/HEAD
//  2. Fallback: try origin/main, then origin/master
//  3. Returns empty string if none found
func (c *CLI) getBaseBranchRef() string {
	// Try symbolic-ref first (most reliable when set)
	cmd := exec.Command("git", "symbolic-ref", "refs/remotes/origin/HEAD")
	if out, err := cmd.Output(); err == nil {
		refPath := strings.TrimSpace(string(out))
		// refPath is like "refs/remotes/origin/main" → we want "origin/main"
		if parts := strings.SplitN(refPath, "refs/remotes/", 2); len(parts) == 2 {
			return parts[1]
		}
	}

	// Fallback: probe common branch names
	for _, branch := range []string{"origin/main", "origin/master"} {
		cmd := exec.Command("git", "rev-parse", "--verify", branch)
		if err := cmd.Run(); err == nil {
			return branch
		}
	}

	return ""
}

// getImplementationSummary creates a summary of the current implementation.
// It diffs against the base branch so committed work is visible (same as getModifiedFiles).
func (c *CLI) getImplementationSummary() string {
	var statOutput, diffOutput []byte

	// Build diff refs: base branch first, then HEAD as fallback
	refs := []string{"HEAD"}
	if base := c.getBaseBranchRef(); base != "" {
		refs = []string{base + "...HEAD", "HEAD"}
	}

	for _, ref := range refs {
		cmd := exec.Command("git", "diff", "--stat", ref)
		if out, err := cmd.Output(); err == nil && len(out) > 0 {
			statOutput = out
			diffCmd := exec.Command("git", "diff", "--unified=3", ref)
			diffOutput, _ = diffCmd.Output()
			break
		}
	}

	if len(statOutput) == 0 {
		return ""
	}
	if len(diffOutput) == 0 {
		diffOutput = []byte("No diff available")
	}

	return fmt.Sprintf("File changes:\n%s\n\nKey changes:\n%s",
		string(statOutput),
		c.truncateString(string(diffOutput), 1000))
}

// getModifiedFiles returns list of modified files in the current git working directory.
// It diffs against the detected base branch so that already-committed changes are included,
// and also includes any uncommitted changes against HEAD.
func (c *CLI) getModifiedFiles() []string {
	seen := make(map[string]bool)
	var result []string

	// Directories to skip — these contain templates/config, not user source code
	skipPrefixes := []string{".oat/", ".oat\\", ".git/", ".git\\"}

	// Build diff commands: base branch diff first, then HEAD and staged
	baseDiff := []string{"git", "diff", "--name-only", "HEAD"}
	if base := c.getBaseBranchRef(); base != "" {
		baseDiff = []string{"git", "diff", "--name-only", base + "...HEAD"}
	}

	for _, diffCmd := range [][]string{
		baseDiff,
		{"git", "diff", "--name-only", "HEAD"}, // uncommitted changes
		{"git", "diff", "--name-only", "--cached", "HEAD"}, // staged changes
	} {
		cmd := exec.Command(diffCmd[0], diffCmd[1:]...)
		output, err := cmd.Output()
		if err != nil {
			continue
		}
		for _, file := range strings.Split(strings.TrimSpace(string(output)), "\n") {
			file = strings.TrimSpace(file)
			if file == "" || seen[file] {
				continue
			}
			// Skip non-source directories
			skip := false
			for _, prefix := range skipPrefixes {
				if strings.HasPrefix(file, prefix) {
					skip = true
					break
				}
			}
			if skip {
				continue
			}
			seen[file] = true
			if absPath, err := filepath.Abs(file); err == nil {
				result = append(result, absPath)
			} else {
				result = append(result, file)
			}
		}
	}

	return result
}

// changedDirectories returns unique directories containing modified files with the given extension.
// Used to scope test runners (pytest, etc.) to only changed areas.
func (c *CLI) changedDirectories(ext string) []string {
	seen := make(map[string]bool)
	var dirs []string
	cwd, _ := os.Getwd()

	for _, file := range c.getModifiedFiles() {
		if ext != "" && !strings.HasSuffix(strings.ToLower(file), ext) {
			continue
		}
		rel := file
		if cwd != "" {
			if r, err := filepath.Rel(cwd, file); err == nil {
				rel = r
			}
		}
		dir := filepath.Dir(rel)
		if dir == "" {
			dir = "."
		}
		if !seen[dir] {
			seen[dir] = true
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

// changedGoPackages returns unique Go package paths (e.g., "./internal/cli/...")
// for directories that contain modified .go files.
func (c *CLI) changedGoPackages() []string {
	seen := make(map[string]bool)
	var pkgs []string
	cwd, _ := os.Getwd()

	for _, file := range c.getModifiedFiles() {
		if !strings.HasSuffix(strings.ToLower(file), ".go") {
			continue
		}
		rel := file
		if cwd != "" {
			if r, err := filepath.Rel(cwd, file); err == nil {
				rel = r
			}
		}
		dir := filepath.Dir(rel)
		if dir == "." {
			dir = "./."
		} else {
			dir = "./" + dir
		}
		// Use .../... to catch tests in the package and sub-packages
		if !seen[dir] {
			seen[dir] = true
			pkgs = append(pkgs, dir)
		}
	}
	return pkgs
}

// checkTaskAlignmentHeuristic uses lightweight heuristics to check task-vs-implementation alignment.
// These are weak signals only — keyword presence/absence is too noisy for hard pass/fail.
// The independent verification agent (oat worker request-review) provides thorough review.
func (c *CLI) checkTaskAlignmentHeuristic(ctx context.Context, originalTask, implementation string) (float64, []string, error) {
	const (
		baseScore = 80.0
		floor     = 40.0
	)

	score := baseScore
	gaps := []string{}

	taskLower := strings.ToLower(originalTask)
	implLower := strings.ToLower(implementation)

	if strings.Contains(taskLower, "test") && !strings.Contains(implLower, "test") {
		gaps = append(gaps, "Task mentions testing but no test-related changes found (weak signal)")
		score -= 10
	}

	if strings.Contains(taskLower, "fix") && !strings.Contains(implLower, "fix") {
		gaps = append(gaps, "Task mentions fixing but changes don't appear to be fixes (weak signal)")
		score -= 10
	}

	if strings.Contains(taskLower, "add") && strings.Contains(implLower, "delete") {
		gaps = append(gaps, "Task asks to add but implementation mostly deletes (weak signal)")
		score -= 15
	}

	if len(implementation) < 100 && len(originalTask) > 200 {
		gaps = append(gaps, "Complex task but minimal implementation - possibly incomplete")
		score -= 10
	}

	if score < floor {
		score = floor
	}

	return score, gaps, nil
}

// hasEvidenceOfIssueReading checks if agent processed GitHub issues
func (c *CLI) hasEvidenceOfIssueReading() bool {
	// Check recent git log for issue references
	cmd := exec.Command("git", "log", "--oneline", "-5")
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	logStr := strings.ToLower(string(output))
	issuePatterns := []string{"#", "issue", "closes", "fixes", "resolves"}

	for _, pattern := range issuePatterns {
		if strings.Contains(logStr, pattern) {
			return true
		}
	}

	// Check if any modified files contain issue references
	files := c.getModifiedFiles()
	for _, file := range files {
		if content, err := os.ReadFile(file); err == nil {
			contentStr := strings.ToLower(string(content))
			for _, pattern := range issuePatterns {
				if strings.Contains(contentStr, pattern) {
					return true
				}
			}
		}
	}

	return false
}

// findDuplicateBlocks is disabled. The 3-line sliding window produced excessive
// false positives on test fixtures, assertion patterns, decorator blocks, and
// repeated setup helpers — causing workers to waste time refactoring legitimate
// code. A smarter, AST-aware duplicate detector can be added later.
func (c *CLI) findDuplicateBlocks(content string) []int {
	return nil
}

// linesMatch checks if two line slices are identical (kept for future use).
func (c *CLI) linesMatch(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if strings.TrimSpace(a[i]) != strings.TrimSpace(b[i]) {
			return false
		}
	}
	return true
}

// checkLanguageSpecificIntegrity performs language-specific integrity checks
func (c *CLI) checkLanguageSpecificIntegrity(file, content string) error {
	ext := strings.ToLower(filepath.Ext(file))

	switch ext {
	case ".py":
		return c.checkPythonIntegrity(content)
	case ".go":
		return c.checkGoIntegrity(content)
	case ".js", ".ts":
		return c.checkJavaScriptIntegrity(content)
	case ".md":
		return c.checkMarkdownIntegrity(content)
	}

	return nil
}

// checkPythonIntegrity checks Python-specific integrity issues
func (c *CLI) checkPythonIntegrity(content string) error {
	// Check for incomplete function definitions
	if strings.Contains(content, "def ") && !strings.Contains(content, ":") {
		return fmt.Errorf("incomplete Python function definition detected")
	}

	// Check for unmatched brackets using a simple count-based approach.
	// A full parser would need to skip strings/comments, but count-based
	// catches gross truncation (the main LLM failure mode) without false positives.
	for open, close := range map[rune]rune{'(': ')', '[': ']', '{': '}'} {
		openCount := strings.Count(content, string(open))
		closeCount := strings.Count(content, string(close))
		if openCount != closeCount {
			return fmt.Errorf("mismatched brackets: %d '%c' vs %d '%c'", openCount, open, closeCount, close)
		}
	}

	return nil
}

// checkGoIntegrity checks Go-specific integrity issues
func (c *CLI) checkGoIntegrity(content string) error {
	// Check for incomplete package declaration
	if !strings.Contains(content, "package ") {
		return fmt.Errorf("missing package declaration")
	}

	// Check for unclosed braces
	openBraces := strings.Count(content, "{")
	closeBraces := strings.Count(content, "}")
	if openBraces != closeBraces {
		return fmt.Errorf("mismatched braces: %d open, %d close", openBraces, closeBraces)
	}

	return nil
}

// checkJavaScriptIntegrity checks JavaScript/TypeScript integrity
func (c *CLI) checkJavaScriptIntegrity(content string) error {
	// Check for unclosed braces
	openBraces := strings.Count(content, "{")
	closeBraces := strings.Count(content, "}")
	if openBraces != closeBraces {
		return fmt.Errorf("mismatched braces: %d open, %d close", openBraces, closeBraces)
	}

	// Check for incomplete function definitions
	if strings.Contains(content, "function ") && !strings.Contains(content, "{") {
		return fmt.Errorf("incomplete function definition detected")
	}

	return nil
}

// checkMarkdownIntegrity checks Markdown integrity
func (c *CLI) checkMarkdownIntegrity(content string) error {
	// Check for unclosed code blocks
	codeBlockCount := strings.Count(content, "```")
	if codeBlockCount%2 != 0 {
		return fmt.Errorf("unclosed markdown code block detected")
	}

	return nil
}

func hasRecursiveFile(root string, match func(path string, entry fs.DirEntry) bool) bool {
	foundErr := errors.New("found match")
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", ".oat", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if match(path, entry) {
			return foundErr
		}
		return nil
	})

	return errors.Is(err, foundErr)
}

// detectTestFramework detects the project's test framework
func (c *CLI) detectTestFramework() string {
	// Check for package.json with test script
	if _, err := os.Stat("package.json"); err == nil {
		if content, err := os.ReadFile("package.json"); err == nil {
			var pkg map[string]interface{}
			if json.Unmarshal(content, &pkg) == nil {
				if scripts, ok := pkg["scripts"].(map[string]interface{}); ok {
					if _, hasTest := scripts["test"]; hasTest {
						return "npm"
					}
				}
			}
		}
	}

	// Check for pytest
	for _, file := range []string{"pytest.ini", "setup.cfg", "pyproject.toml"} {
		if _, err := os.Stat(file); err == nil {
			return "pytest"
		}
	}

	// Check for Python test files
	if files, _ := filepath.Glob("test_*.py"); len(files) > 0 {
		return "pytest"
	}
	if hasRecursiveFile(".", func(path string, entry fs.DirEntry) bool {
		name := entry.Name()
		return strings.HasPrefix(name, "test_") && strings.HasSuffix(name, ".py")
	}) {
		return "pytest"
	}

	// Check for Go tests
	if files, _ := filepath.Glob("*_test.go"); len(files) > 0 {
		return "go"
	}
	if hasRecursiveFile(".", func(path string, entry fs.DirEntry) bool {
		return strings.HasSuffix(entry.Name(), "_test.go")
	}) {
		return "go"
	}

	// Check for Makefile with test target
	if _, err := os.Stat("Makefile"); err == nil {
		if content, err := os.ReadFile("Makefile"); err == nil {
			if strings.Contains(string(content), "test:") {
				return "make"
			}
		}
	}

	return ""
}

// extractTestFailures extracts specific test failure information
func (c *CLI) extractTestFailures(output, framework string) []string {
	failures := []string{}

	switch framework {
	case "npm":
		// Extract npm test failures
		lines := strings.Split(output, "\n")
		for _, line := range lines {
			if strings.Contains(line, "FAIL") || strings.Contains(line, "✕") {
				failures = append(failures, strings.TrimSpace(line))
			}
		}

	case "pytest":
		// Extract pytest failures
		lines := strings.Split(output, "\n")
		for _, line := range lines {
			if strings.Contains(line, "FAILED") || strings.Contains(line, "ERROR") {
				failures = append(failures, strings.TrimSpace(line))
			}
		}

	case "go":
		// Extract go test failures
		lines := strings.Split(output, "\n")
		for _, line := range lines {
			if strings.Contains(line, "FAIL:") || strings.Contains(line, "--- FAIL:") {
				failures = append(failures, strings.TrimSpace(line))
			}
		}
	}

	// Limit to first 5 failures to avoid overwhelming output
	if len(failures) > 5 {
		remaining := len(failures) - 5
		failures = failures[:5]
		failures = append(failures, fmt.Sprintf("... and %d more failures", remaining))
	}

	return failures
}

// calculateOverallScore computes weighted score across all checks
func (c *CLI) calculateOverallScore(result *VerificationResult) float64 {
	// Optimized weights for benchmark performance gaps (must sum to 1.0)
	// Focus on checks that prevent LLM errors and catch real issues
	weights := map[string]float64{
		"file_integrity":    0.35, // Most critical - prevents 40% of LLM errors
		"test_execution":    0.25, // Critical - catches breaking changes
		"syntax_validation": 0.20, // Important for basic correctness
		"task_alignment":    0.15, // Important but has technical debt
		"input_validation":  0.05, // Nice to have
	}

	totalScore := 0.0
	totalScore += result.TaskAlignment.Score * weights["task_alignment"]
	totalScore += result.FileIntegrity.Score * weights["file_integrity"]
	totalScore += result.SyntaxValidation.Score * weights["syntax_validation"]
	totalScore += result.TestExecution.Score * weights["test_execution"]
	totalScore += result.InputValidation.Score * weights["input_validation"]

	return totalScore
}

// generateRecommendations creates actionable recommendations based on results
func (c *CLI) generateRecommendations(result *VerificationResult) []string {
	recommendations := []string{}

	if !result.TaskAlignment.Passed {
		recommendations = append(recommendations, "🎯 Re-read your original task and ensure your implementation addresses all requirements")
		if len(result.TaskAlignment.Issues) > 0 {
			recommendations = append(recommendations, fmt.Sprintf("   Specific gaps: %s", strings.Join(result.TaskAlignment.Issues, ", ")))
		}
	}

	if !result.FileIntegrity.Passed {
		recommendations = append(recommendations, "🔧 Fix file integrity issues before creating PR")
		for _, issue := range result.FileIntegrity.Issues {
			if strings.Contains(issue, "truncated") {
				recommendations = append(recommendations, "   Rewrite truncated files completely")
			}
			if strings.Contains(issue, "duplicate") {
				recommendations = append(recommendations, "   Remove duplicate code blocks")
			}
		}
	}

	if !result.SyntaxValidation.Passed {
		recommendations = append(recommendations, "⚠️  Fix syntax errors before creating PR")
		if len(result.SyntaxValidation.Issues) > 0 {
			recommendations = append(recommendations, "   Run language-specific linting tools")
		}
	}

	if !result.TestExecution.Passed {
		recommendations = append(recommendations, "🧪 Fix failing tests before creating PR")
		recommendations = append(recommendations, "   Consider running tests individually to isolate failures")
	}

	if result.OverallScore < 70 {
		recommendations = append(recommendations, "⚡ Overall quality below 70% - consider significant revisions before PR")
	} else if result.OverallScore < 85 {
		recommendations = append(recommendations, "✨ Good quality! Address remaining issues for optimal PR success")
	} else {
		recommendations = append(recommendations, "🚀 Excellent quality! Ready to create PR")
	}

	return recommendations
}

// attemptAutoFix tries to automatically fix detected issues
func (c *CLI) attemptAutoFix(ctx context.Context, result *VerificationResult) {
	fmt.Println("\n🔧 Attempting automatic fixes...")

	// Auto-fix syntax errors
	if !result.SyntaxValidation.Passed {
		for _, issue := range result.SyntaxValidation.Issues {
			issueLower := strings.ToLower(issue)
			if strings.Contains(issueLower, ".py") && strings.Contains(issueLower, "syntax") {
				// Try basic Python syntax fixes
				filename := c.extractFilenameFromIssue(issue)
				if filename != "" {
					if err := c.autoFixPythonSyntax(filename); err == nil {
						result.AutoFixResults[issue] = "Fixed Python syntax"
						fmt.Printf("   ✅ Fixed Python syntax in %s\n", filename)
					}
				}
			}
		}
	}

	// Duplicate block auto-fix removed — the 3-line window caused destructive
	// false positives (corrupted test files, removed decorators/fixtures).
	// A smarter fix mechanism can be added with an AST-aware detector later.
}

// extractFilenameFromIssue extracts filename from error message
func (c *CLI) extractFilenameFromIssue(issue string) string {
	// Simple regex to extract filename
	re := regexp.MustCompile(`[\w\-_./]+\.\w+`)
	match := re.FindString(issue)
	return match
}

// autoFixPythonSyntax attempts basic Python syntax fixes
func (c *CLI) autoFixPythonSyntax(filename string) error {
	content, err := os.ReadFile(filename)
	if err != nil {
		return err
	}

	contentStr := string(content)

	// Fix common issues: missing colons in function definitions
	re := regexp.MustCompile(`(?m)^(\s*def\s+\w+\([^)]*\))\s*$`)
	contentStr = re.ReplaceAllString(contentStr, "$1:")

	return os.WriteFile(filename, []byte(contentStr), 0644)
}

// removeDuplicateBlocks is disabled — the 3-line window auto-fix corrupted
// files by removing decorators, fixture definitions, and function bodies that
// happened to share 3 similar lines. Multiple workers had to restore from git.
func (c *CLI) removeDuplicateBlocks(filename string) error {
	return nil
}

// presentVerificationResults displays the verification results to the user
func (c *CLI) presentVerificationResults(result *VerificationResult, verbose bool) error {
	fmt.Println("\n" + strings.Repeat("=", 60))
	fmt.Printf("🎯 VERIFICATION RESULTS - Overall Score: %.1f/100\n", result.OverallScore)
	fmt.Println(strings.Repeat("=", 60))

	// Show overall status
	if result.OverallPassed {
		fmt.Printf("✅ PASSED - Quality threshold met (≥70%%)\n")
	} else {
		fmt.Printf("❌ FAILED - Quality below threshold (<70%%)\n")
	}

	fmt.Println()

	// Show individual check results
	checks := []VerificationCheck{
		result.TaskAlignment,
		result.FileIntegrity,
		result.SyntaxValidation,
		result.TestExecution,
		result.InputValidation,
	}

	for _, check := range checks {
		status := "❌"
		if check.Passed {
			status = "✅"
		}

		fmt.Printf("%s %s: %.1f/100", status, check.Name, check.Score)
		if check.Duration != "" && verbose {
			fmt.Printf(" (%s)", check.Duration)
		}
		fmt.Println()

		if len(check.Issues) > 0 && (verbose || !check.Passed) {
			for _, issue := range check.Issues {
				fmt.Printf("   • %s\n", issue)
			}
		}

		if check.Details != "" && verbose {
			fmt.Printf("   Details: %s\n", c.truncateString(check.Details, 200))
		}
	}

	// Show auto-fix results
	if result.AutoFixAttempted && len(result.AutoFixResults) > 0 {
		fmt.Println("\n🔧 Auto-fix Results:")
		for issue, fix := range result.AutoFixResults {
			fmt.Printf("   ✅ %s: %s\n", c.truncateString(issue, 50), fix)
		}
	}

	// Show recommendations
	if len(result.Recommendations) > 0 {
		fmt.Println("\n💡 Recommendations:")
		for _, rec := range result.Recommendations {
			fmt.Printf("   %s\n", rec)
		}
	}

	// Final advice
	fmt.Println()
	if result.OverallPassed {
		fmt.Println("🚀 Ready to proceed with 'oat pr create'!")
	} else {
		fmt.Println("⚠️  Consider fixing issues above before creating PR.")
		fmt.Println("   You can run 'oat worker verify --fix' to attempt automatic fixes.")
	}

	return nil
}

// presentVerificationResultsJSON outputs the full result as JSON to stdout.
// This is consumed by blackbox test pipelines and automation.
func (c *CLI) presentVerificationResultsJSON(result *VerificationResult) error {
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal verification result: %w", err)
	}
	fmt.Println(string(data))
	return nil
}

// getCurrentCommitSHA returns the current HEAD commit SHA (short form).
func (c *CLI) getCurrentCommitSHA() string {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// logVerificationResult logs verification data for analytics (non-blocking).
// The log includes the commit SHA so that `oat pr create` can check if verification
// passed for the current commit.
func (c *CLI) logVerificationResult(result *VerificationResult, repoName, agentName string, duration time.Duration) {
	logData := map[string]interface{}{
		"timestamp":        time.Now().Format(time.RFC3339),
		"commit_sha":       c.getCurrentCommitSHA(),
		"repo_name":        repoName,
		"agent_name":       agentName,
		"overall_score":    result.OverallScore,
		"overall_passed":   result.OverallPassed,
		"duration_seconds": duration.Seconds(),
		"checks": map[string]interface{}{
			"task_alignment":    map[string]interface{}{"score": result.TaskAlignment.Score, "passed": result.TaskAlignment.Passed},
			"input_validation":  map[string]interface{}{"score": result.InputValidation.Score, "passed": result.InputValidation.Passed},
			"file_integrity":    map[string]interface{}{"score": result.FileIntegrity.Score, "passed": result.FileIntegrity.Passed},
			"syntax_validation": map[string]interface{}{"score": result.SyntaxValidation.Score, "passed": result.SyntaxValidation.Passed},
			"test_execution":    map[string]interface{}{"score": result.TestExecution.Score, "passed": result.TestExecution.Passed},
		},
		"auto_fix_attempted": result.AutoFixAttempted,
		"auto_fix_count":     len(result.AutoFixResults),
	}

	logFile := filepath.Join(c.paths.Root, "verification.log")
	if logContent, err := json.Marshal(logData); err == nil {
		if f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
			defer f.Close()
			f.WriteString(string(logContent) + "\n")
		}
	}
}

// getLastVerificationForCommit reads the verification log and returns the most recent
// entry whose commit_sha matches the given SHA. Returns (passed, found).
func (c *CLI) getLastVerificationForCommit(commitSHA string) (bool, bool) {
	logFile := filepath.Join(c.paths.Root, "verification.log")
	data, err := os.ReadFile(logFile)
	if err != nil {
		return false, false
	}

	// The log is newline-delimited JSON, most recent entry last.
	// Scan backwards for the matching SHA.
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var entry map[string]interface{}
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		sha, _ := entry["commit_sha"].(string)
		if sha == commitSHA {
			passed, _ := entry["overall_passed"].(bool)
			return passed, true
		}
	}
	return false, false
}

// truncateString truncates a string to maxLen characters with ellipsis
func (c *CLI) truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
