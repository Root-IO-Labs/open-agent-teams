package planner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUpdatePlanDoesNotDeadlockAndPersistsExecutionMetadata(t *testing.T) {
	storage, err := NewPlanStorage(t.TempDir())
	if err != nil {
		t.Fatalf("NewPlanStorage: %v", err)
	}

	plan := &PlanDocument{
		ID:        "plan-test",
		Status:    "approved",
		CreatedAt: time.Now(),
		Requirement: RequirementDoc{
			Title:       "Test plan",
			Original:    "original",
			Refined:     "refined",
			LastUpdated: time.Now(),
		},
		TestStrategy: TestStrategyDoc{
			Unit:       "go test ./internal/planner",
			GateScript: "make pre-commit",
		},
		Tasks: []TaskDoc{
			{ID: "T1", Title: "Task 1", Wave: 1, Status: "pending"},
		},
	}
	if err := storage.SavePlan(plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- storage.UpdatePlan("plan-test", map[string]interface{}{
			"status": "executing",
			"tasks": []TaskDoc{
				{ID: "T1", Title: "Task 1", Wave: 1, Status: "in_progress", AssignedTo: "worker-alpha", PRNumber: 42},
			},
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("UpdatePlan: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("UpdatePlan deadlocked")
	}

	loaded, err := storage.LoadPlan("plan-test")
	if err != nil {
		t.Fatalf("LoadPlan: %v", err)
	}
	if loaded.Status != "executing" {
		t.Fatalf("Status = %q, want executing", loaded.Status)
	}
	if got := loaded.Tasks[0].AssignedTo; got != "worker-alpha" {
		t.Fatalf("AssignedTo = %q, want worker-alpha", got)
	}
	if got := loaded.Tasks[0].PRNumber; got != 42 {
		t.Fatalf("PRNumber = %d, want 42", got)
	}
}

func TestSavePlanWritesNonContiguousWavesInSortedOrder(t *testing.T) {
	dir := t.TempDir()
	storage, err := NewPlanStorage(dir)
	if err != nil {
		t.Fatalf("NewPlanStorage: %v", err)
	}

	plan := &PlanDocument{
		ID:        "plan-waves",
		Status:    "approved",
		CreatedAt: time.Now(),
		Requirement: RequirementDoc{
			Title:       "Wave plan",
			LastUpdated: time.Now(),
		},
		Tasks: []TaskDoc{
			{ID: "T3", Title: "Later", Wave: 3, Status: "pending"},
			{ID: "T1", Title: "First", Wave: 1, Status: "pending"},
		},
	}
	if err := storage.SavePlan(plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}

	workgraphPath := filepath.Join(dir, "plan-waves", "workgraph.yml")
	data, err := os.ReadFile(workgraphPath)
	if err != nil {
		t.Fatalf("read workgraph: %v", err)
	}
	workgraph := string(data)
	wave1 := strings.Index(workgraph, "wave: 1")
	wave3 := strings.Index(workgraph, "wave: 3")
	if wave1 < 0 || wave3 < 0 {
		t.Fatalf("workgraph missing non-contiguous waves:\n%s", workgraph)
	}
	if wave1 > wave3 {
		t.Fatalf("waves not sorted:\n%s", workgraph)
	}
}
