// Dry-run the Go classifier + RouteForTask against the manifest tasks.
// Prints: task_id | manifest_cx | classifier_cx | router_v1_pick
//
// Usage (from repo root):
//
//	go run ./benchmarks/routing-test-repo/harness/classify_manifest.go
package main

import (
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"

	"github.com/Root-IO-Labs/open-agent-teams/internal/routing"
)

type task struct {
	ID         string
	Complexity string
	Text       string
}

// Minimal YAML extractor for our manifest (avoids adding a yaml dep).
func extractTasks(path string) []task {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("read %s: %v", path, err)
	}
	content := string(data)

	// Find task blocks by splitting on "  - id:" (two spaces, hyphen, id key).
	blocks := strings.Split(content, "\n  - id:")
	var tasks []task
	for i, b := range blocks {
		if i == 0 {
			continue
		}
		t := task{}
		// ID is the first non-whitespace token
		idEnd := strings.IndexByte(b, '\n')
		if idEnd > 0 {
			t.ID = strings.TrimSpace(b[:idEnd])
		}
		// Complexity
		if m := regexp.MustCompile(`complexity:\s*(\S+)`).FindStringSubmatch(b); len(m) > 1 {
			t.Complexity = m[1]
		}
		// Text — the task_text: | block
		if idx := strings.Index(b, "task_text: |"); idx != -1 {
			after := b[idx+len("task_text: |"):]
			// Gather subsequent indented lines (6+ spaces matches our YAML format)
			lines := strings.Split(after, "\n")
			var textLines []string
			for _, line := range lines[1:] {
				if !strings.HasPrefix(line, "      ") {
					break
				}
				textLines = append(textLines, strings.TrimPrefix(line, "      "))
			}
			t.Text = strings.Join(textLines, " ")
		}
		tasks = append(tasks, t)
	}
	return tasks
}

func main() {
	tasks := extractTasks("benchmarks/routing-test-repo/task-manifest.yaml")
	if len(tasks) == 0 {
		log.Fatal("no tasks extracted — check the manifest path")
	}

	// Load the shipping profiles to use as the routing team.
	ps, err := routing.NewProfileStore("model-routing/profiles")
	if err != nil {
		log.Fatalf("load profiles: %v", err)
	}
	ps.SetContextRegistry(routing.LoadEmbeddedContextRegistry())
	if err := ps.Reload(); err != nil {
		log.Fatalf("reload: %v", err)
	}
	pricing := routing.LoadEmbeddedPricing()

	fmt.Printf("%-20s  %-10s  %-10s  %-45s  %s\n", "task_id", "manifest", "clf_pred", "v1_chosen_model", "reason")
	fmt.Println(strings.Repeat("─", 130))

	for _, t := range tasks {
		features := routing.ExtractFeatures(t.Text)
		dec, derr := ps.RouteForTask(routing.RouteContext{
			TaskText: t.Text,
			Role:     routing.RoleWorker,
		}, pricing)
		pick := "—"
		reason := ""
		if derr != nil {
			reason = "error: " + derr.Error()
		} else {
			pick = dec.ChosenModel
			reason = dec.Reason
		}
		match := "✗ "
		if string(features.Complexity) == t.Complexity {
			match = "✓ "
		}
		fmt.Printf("%s%-18s  %-10s  %-10s  %-45s  %s\n",
			match, t.ID, t.Complexity, features.Complexity, pick, reason)
	}
}
