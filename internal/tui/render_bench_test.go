package tui

import (
	"fmt"
	"testing"
)

// BenchmarkRenderLines_Fresh measures full render of N lines with no cache.
func BenchmarkRenderLines_Fresh_100(b *testing.B) {
	benchmarkRenderFresh(b, 100)
}

func BenchmarkRenderLines_Fresh_1000(b *testing.B) {
	benchmarkRenderFresh(b, 1000)
}

func BenchmarkRenderLines_Fresh_5000(b *testing.B) {
	benchmarkRenderFresh(b, 5000)
}

func benchmarkRenderFresh(b *testing.B, n int) {
	filter := NewOutputFilter(DefaultFilterConfig())
	lines := buildRealisticLines(n)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := NewLineRenderer(filter, 120)
		r.RenderLines("agent1", lines)
	}
}

// BenchmarkRenderLines_Incremental measures appending 10 new lines to an
// already-cached buffer of N lines. This is the hot path in the TUI.
func BenchmarkRenderLines_Incremental_100(b *testing.B) {
	benchmarkRenderIncremental(b, 100)
}

func BenchmarkRenderLines_Incremental_1000(b *testing.B) {
	benchmarkRenderIncremental(b, 1000)
}

func BenchmarkRenderLines_Incremental_5000(b *testing.B) {
	benchmarkRenderIncremental(b, 5000)
}

func benchmarkRenderIncremental(b *testing.B, n int) {
	filter := NewOutputFilter(DefaultFilterConfig())
	r := NewLineRenderer(filter, 120)
	baselines := buildRealisticLines(n)

	// Prime the cache
	r.RenderLines("agent1", baselines)

	// 10 new lines per iteration
	newLines := []string{
		"The agent is now processing your request.",
		"",
		"(*) execute(\"go test ./...\")",
		"⎿ ok  	github.com/example/pkg	0.5s",
		"  [Command succeeded with exit code 0]",
		"",
		"All tests are passing. Let me now check",
		"All tests are passing. Let me now check the coverage",
		"All tests are passing. Let me now check the coverage report.",
		"",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lines := make([]string, len(baselines)+len(newLines))
		copy(lines, baselines)
		copy(lines[len(baselines):], newLines)
		r.RenderLines("agent1", lines)
		// Reset cache so next iteration re-renders the new lines
		if c, ok := r.cache["agent1"]; ok {
			resetTo := len(baselines)
			if resetTo > len(c.rendered) {
				resetTo = len(c.rendered)
			}
			c.rawCount = resetTo
			c.rendered = c.rendered[:resetTo]
			c.cachedCount = resetTo
		}
	}
}

// BenchmarkRenderLines_CacheInvalidation measures the cost of a full cache
// rebuild (what happens when dedup replaces a line in-place).
func BenchmarkRenderLines_CacheInvalidation_1000(b *testing.B) {
	filter := NewOutputFilter(DefaultFilterConfig())
	r := NewLineRenderer(filter, 120)
	lines := buildRealisticLines(1000)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Prime cache
		r.RenderLines("agent1", lines)
		// Invalidate (simulates dedup in-place replacement)
		r.InvalidateCacheForAgent("agent1")
		// Full re-render
		r.RenderLines("agent1", lines)
	}
}

// BenchmarkClassify measures per-line classification cost.
func BenchmarkClassify(b *testing.B) {
	filter := NewOutputFilter(DefaultFilterConfig())
	lines := []string{
		"This is a normal response from the agent.",
		"(*) execute(\"git status\")",
		"⎿ On branch main",
		"  [Command succeeded with exit code 0]",
		"Thinking...",
		"⠋",
		"┌──────────────────────────┐",
		"> What should I do?",
		"13.4K tokens",
		"",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, line := range lines {
			filter.Classify(line)
		}
	}
}

// BenchmarkFilterLines measures filtering a batch of mixed content.
func BenchmarkFilterLines_100(b *testing.B) {
	filter := NewOutputFilter(DefaultFilterConfig())
	lines := buildRealisticLines(100)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		filter.FilterLines(lines)
	}
}

// --- helpers ---

func buildRealisticLines(n int) []string {
	// Build a realistic mix of line types
	templates := []string{
		"This is a normal text response line from the AI agent explaining something.",
		"",
		"(*) execute(\"git diff --stat\")",
		"⎿  internal/tui/app.go | 15 +++++---",
		"   internal/tui/dedup.go | 42 ++++++++++++++++++--------",
		"  [Command succeeded with exit code 0]",
		"",
		"I can see the changes look correct. **The main modifications** are:",
		"",
		"- Updated the deduplication algorithm for better accuracy",
		"- Added word-boundary checking to prevent false positives",
		"- Improved the render cache invalidation strategy",
		"",
		"(*) read_file(\"config.yaml\")",
		"⎿ database:",
		"    host: localhost",
		"    port: 5432",
		"",
		"> What else should I check?",
		"",
		"Let me also verify the test coverage to make sure we haven't missed anything.",
	}

	lines := make([]string, n)
	for i := range lines {
		lines[i] = templates[i%len(templates)]
		if i%len(templates) == 0 && i > 0 {
			// Vary lines slightly to prevent unrealistic caching
			lines[i] = fmt.Sprintf("Response variation %d: The agent continues processing.", i)
		}
	}
	return lines
}
