package routing

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestRouteForTask_DeterministicGivenInputs is the cardinal-rule guard. The
// router must depend only on its arguments — no hidden global state, no logger
// state, no clock, no filesystem. We prove this by calling RouteForTask many
// times with identical inputs and asserting byte-identical outputs.
//
// If this test ever fails, someone introduced non-determinism into the router
// hot path. Common culprits: a `time.Now()` tiebreaker, a math/rand call in
// candidate ordering, a once.Do that mutates package state on first call.
func TestRouteForTask_DeterministicGivenInputs(t *testing.T) {
	ps := minimalStoreForTests(t)
	pricing := LoadEmbeddedPricing()
	ctx := RouteContext{
		TaskText: "Refactor the worker pool to use context.WithCancel instead of explicit done channels. Touch internal/daemon/worker.go and internal/daemon/pool.go.",
		Role:     RoleWorker,
	}

	first, err := ps.RouteForTask(ctx, pricing)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}

	for i := 0; i < 200; i++ {
		got, err := ps.RouteForTask(ctx, pricing)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if got.ChosenModel != first.ChosenModel {
			t.Fatalf("call %d: chose %q, first chose %q — router is non-deterministic", i, got.ChosenModel, first.ChosenModel)
		}
		if got.Reason != first.Reason {
			t.Fatalf("call %d: reason %q != first %q", i, got.Reason, first.Reason)
		}
		if got.Complexity != first.Complexity {
			t.Fatalf("call %d: complexity %q != first %q", i, got.Complexity, first.Complexity)
		}
		if !slicesEqualStr(got.Candidates, first.Candidates) {
			t.Fatalf("call %d: candidates %v != first %v", i, got.Candidates, first.Candidates)
		}
	}
}

// TestRouteForTask_NoLoggerImport is the structural guard. route_for_task.go's
// import set must be a subset of the allow-list below. Adding to the allow-list
// requires explicit review. The router must not depend on the corpus
// subsystem, even transitively via std-lib pulls that signal new behavior
// (`os`, `path/filepath`, `time` — any of these could indicate hidden state
// being introduced).
func TestRouteForTask_NoLoggerImport(t *testing.T) {
	allowed := map[string]struct{}{
		"fmt":  {},
		"sort": {},
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "route_for_task.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse route_for_task.go: %v", err)
	}

	for _, imp := range f.Imports {
		raw := strings.Trim(imp.Path.Value, `"`)
		if _, ok := allowed[raw]; !ok {
			t.Errorf("route_for_task.go imports %q — not on the router-purity allow-list. "+
				"If this import is genuinely needed by the router (not an accidental coupling), "+
				"add it to the allow-list in this test with a comment explaining why.", raw)
		}
	}
}

// TestRouteForTask_NoSymbolReferences is the deeper structural guard. Same-
// package symbols share a namespace so an import check isn't enough. Verify
// that route_for_task.go's AST contains no references to logger, backfiller,
// or corpus types — even by name.
func TestRouteForTask_NoSymbolReferences(t *testing.T) {
	forbidden := []string{
		"OutcomeLogger",
		"OutcomeRecord",
		"PRBackfiller",
		"PRBackfillEntry",
		"LoggedTaskFeatures",
		"PRStateSnapshot",
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "route_for_task.go", nil, 0)
	if err != nil {
		t.Fatalf("parse route_for_task.go: %v", err)
	}

	seen := map[string]struct{}{}
	ast.Inspect(f, func(n ast.Node) bool {
		ident, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		for _, banned := range forbidden {
			if ident.Name == banned {
				seen[banned] = struct{}{}
			}
		}
		return true
	})

	if len(seen) > 0 {
		var names []string
		for k := range seen {
			names = append(names, k)
		}
		t.Errorf("route_for_task.go references logger/backfill symbols: %v. "+
			"The router must not depend on the corpus subsystem. Move the dependency "+
			"to a wrapper or a new file, or refactor.", names)
	}
}

func slicesEqualStr(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
