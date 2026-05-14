package planner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
	"io/ioutil"
	"sync"
)

// PlanStorage handles persistent storage of plans with versioning
type PlanStorage struct {
	basePath string
	mu       sync.RWMutex
}

// PlanDocument represents a complete planning session
type PlanDocument struct {
	ID              string                 `json:"id"`
	Version         int                    `json:"version"`
	CreatedAt       time.Time              `json:"created_at"`
	UpdatedAt       time.Time              `json:"updated_at"`
	Status          string                 `json:"status"` // draft, approved, executing, completed
	Requirement     RequirementDoc         `json:"requirement"`
	TestStrategy    TestStrategyDoc        `json:"test_strategy"`
	Tasks           []TaskDoc              `json:"tasks"`
	Metadata        map[string]interface{} `json:"metadata"`
	History         []PlanRevision         `json:"history"`
}

// RequirementDoc stores requirement details
type RequirementDoc struct {
	Title           string    `json:"title"`
	Original        string    `json:"original"`
	Refined         string    `json:"refined"`
	OperationalSpec string    `json:"operational_spec"`
	LastUpdated     time.Time `json:"last_updated"`
}

// TestStrategyDoc stores test strategy
type TestStrategyDoc struct {
	Unit        string `json:"unit"`
	Integration string `json:"integration"`
	Blackbox    string `json:"blackbox"`
	GateScript  string `json:"gate_script"`
}

// TaskDoc represents a task in the plan
type TaskDoc struct {
	ID                 string   `json:"id"`
	Title              string   `json:"title"`
	Description        string   `json:"description"`
	Type               string   `json:"type"`
	Wave               int      `json:"wave"`
	Dependencies       []string `json:"dependencies"`
	SpecReference      string   `json:"spec_reference"`
	TestFirst          bool     `json:"test_first"`
	AcceptanceCriteria []string `json:"acceptance_criteria"`
	Status             string   `json:"status"` // pending, assigned, in_progress, completed, failed
	AssignedTo         string   `json:"assigned_to,omitempty"`
	CompletedAt        *time.Time `json:"completed_at,omitempty"`
}

// PlanRevision tracks changes to the plan
type PlanRevision struct {
	Version     int       `json:"version"`
	Timestamp   time.Time `json:"timestamp"`
	ChangedBy   string    `json:"changed_by"` // user or agent
	Description string    `json:"description"`
	Changes     []Change  `json:"changes"`
}

// Change represents a specific modification
type Change struct {
	Type   string      `json:"type"` // add_task, remove_task, update_task, update_requirement
	Path   string      `json:"path"` // JSON path to changed element
	Before interface{} `json:"before,omitempty"`
	After  interface{} `json:"after,omitempty"`
}

// NewPlanStorage creates a new plan storage instance
func NewPlanStorage(basePath string) (*PlanStorage, error) {
	// Default to ~/.oat/plans if not specified
	if basePath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("failed to get home directory: %w", err)
		}
		basePath = filepath.Join(home, ".oat", "plans")
	}

	// Create directory if it doesn't exist
	if err := os.MkdirAll(basePath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create plans directory: %w", err)
	}

	return &PlanStorage{
		basePath: basePath,
	}, nil
}

// SavePlan saves a plan to disk with versioning
func (ps *PlanStorage) SavePlan(plan *PlanDocument) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	// Generate ID if new plan
	if plan.ID == "" {
		plan.ID = fmt.Sprintf("plan-%d", time.Now().Unix())
		plan.CreatedAt = time.Now()
		plan.Version = 1
	} else {
		// Increment version for existing plan
		existing, _ := ps.loadPlanNoLock(plan.ID)
		if existing != nil {
			plan.Version = existing.Version + 1
			plan.CreatedAt = existing.CreatedAt
			
			// Add to history
			revision := PlanRevision{
				Version:     plan.Version,
				Timestamp:   time.Now(),
				ChangedBy:   "planner",
				Description: "Plan updated",
				Changes:     ps.detectChanges(existing, plan),
			}
			plan.History = append(existing.History, revision)
		}
	}

	plan.UpdatedAt = time.Now()

	// Create plan directory
	planDir := filepath.Join(ps.basePath, plan.ID)
	if err := os.MkdirAll(planDir, 0755); err != nil {
		return fmt.Errorf("failed to create plan directory: %w", err)
	}

	// Save main plan JSON
	planPath := filepath.Join(planDir, "plan.json")
	data, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal plan: %w", err)
	}

	if err := ioutil.WriteFile(planPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write plan file: %w", err)
	}

	// Save versioned backup
	versionPath := filepath.Join(planDir, fmt.Sprintf("v%d.json", plan.Version))
	if err := ioutil.WriteFile(versionPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write version file: %w", err)
	}

	// Generate and save markdown documentation
	if err := ps.saveMarkdown(plan); err != nil {
		return fmt.Errorf("failed to save markdown: %w", err)
	}

	// Generate work graph for dispatch
	if err := ps.saveWorkGraph(plan); err != nil {
		return fmt.Errorf("failed to save work graph: %w", err)
	}

	return nil
}

// LoadPlan loads a plan from disk
func (ps *PlanStorage) LoadPlan(planID string) (*PlanDocument, error) {
	ps.mu.RLock()
	defer ps.mu.RUnlock()

	return ps.loadPlanNoLock(planID)
}

func (ps *PlanStorage) loadPlanNoLock(planID string) (*PlanDocument, error) {
	planPath := filepath.Join(ps.basePath, planID, "plan.json")
	
	data, err := ioutil.ReadFile(planPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("plan not found: %s", planID)
		}
		return nil, fmt.Errorf("failed to read plan: %w", err)
	}

	var plan PlanDocument
	if err := json.Unmarshal(data, &plan); err != nil {
		return nil, fmt.Errorf("failed to unmarshal plan: %w", err)
	}

	return &plan, nil
}

// UpdatePlan updates specific fields in a plan
func (ps *PlanStorage) UpdatePlan(planID string, updates map[string]interface{}) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	plan, err := ps.loadPlanNoLock(planID)
	if err != nil {
		return err
	}

	// Apply updates based on field names
	for field, value := range updates {
		switch field {
		case "requirement":
			if req, ok := value.(RequirementDoc); ok {
				plan.Requirement = req
			}
		case "test_strategy":
			if ts, ok := value.(TestStrategyDoc); ok {
				plan.TestStrategy = ts
			}
		case "tasks":
			if tasks, ok := value.([]TaskDoc); ok {
				plan.Tasks = tasks
			}
		case "status":
			if status, ok := value.(string); ok {
				plan.Status = status
			}
		}
	}

	// Save updated plan
	return ps.SavePlan(plan)
}

// ListPlans returns all available plans
func (ps *PlanStorage) ListPlans() ([]PlanDocument, error) {
	ps.mu.RLock()
	defer ps.mu.RUnlock()

	entries, err := ioutil.ReadDir(ps.basePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read plans directory: %w", err)
	}

	var plans []PlanDocument
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		plan, err := ps.loadPlanNoLock(entry.Name())
		if err != nil {
			continue // Skip invalid plans
		}
		plans = append(plans, *plan)
	}

	return plans, nil
}

// saveMarkdown generates a human-readable markdown document
func (ps *PlanStorage) saveMarkdown(plan *PlanDocument) error {
	mdPath := filepath.Join(ps.basePath, plan.ID, "plan.md")
	
	md := fmt.Sprintf(`# %s

## Overview
- **ID**: %s
- **Version**: %d
- **Status**: %s
- **Created**: %s
- **Updated**: %s

## Requirement

### Original Request
%s

### Refined Specification
%s

### Operational Specification
%s

## Test Strategy

- **Unit Testing**: %s
- **Integration Testing**: %s
- **Blackbox Testing**: %s
- **Gate Script**: %s

## Task Waves

`, plan.Requirement.Title, plan.ID, plan.Version, plan.Status,
		plan.CreatedAt.Format(time.RFC3339),
		plan.UpdatedAt.Format(time.RFC3339),
		plan.Requirement.Original,
		plan.Requirement.Refined,
		plan.Requirement.OperationalSpec,
		plan.TestStrategy.Unit,
		plan.TestStrategy.Integration,
		plan.TestStrategy.Blackbox,
		plan.TestStrategy.GateScript)

	// Group tasks by wave
	waveMap := make(map[int][]TaskDoc)
	for _, task := range plan.Tasks {
		waveMap[task.Wave] = append(waveMap[task.Wave], task)
	}

	// Write tasks by wave
	for wave := 0; wave <= len(waveMap); wave++ {
		tasks, exists := waveMap[wave]
		if !exists {
			continue
		}

		md += fmt.Sprintf("### Wave %d\n\n", wave)
		for _, task := range tasks {
			md += fmt.Sprintf("#### %s (%s)\n", task.Title, task.ID)
			md += fmt.Sprintf("- **Type**: %s\n", task.Type)
			md += fmt.Sprintf("- **Description**: %s\n", task.Description)
			if task.SpecReference != "" {
				md += fmt.Sprintf("- **Spec Reference**: %s\n", task.SpecReference)
			}
			if len(task.Dependencies) > 0 {
				md += fmt.Sprintf("- **Dependencies**: %v\n", task.Dependencies)
			}
			md += fmt.Sprintf("- **Status**: %s\n", task.Status)
			if task.AssignedTo != "" {
				md += fmt.Sprintf("- **Assigned To**: %s\n", task.AssignedTo)
			}
			md += "\n**Acceptance Criteria:**\n"
			for _, criteria := range task.AcceptanceCriteria {
				md += fmt.Sprintf("- %s\n", criteria)
			}
			md += "\n"
		}
	}

	// Add revision history
	if len(plan.History) > 0 {
		md += "## Revision History\n\n"
		for _, rev := range plan.History {
			md += fmt.Sprintf("### Version %d (%s)\n", rev.Version, rev.Timestamp.Format(time.RFC3339))
			md += fmt.Sprintf("- Changed by: %s\n", rev.ChangedBy)
			md += fmt.Sprintf("- Description: %s\n", rev.Description)
			if len(rev.Changes) > 0 {
				md += "- Changes:\n"
				for _, change := range rev.Changes {
					md += fmt.Sprintf("  - %s at %s\n", change.Type, change.Path)
				}
			}
			md += "\n"
		}
	}

	return ioutil.WriteFile(mdPath, []byte(md), 0644)
}

// saveWorkGraph generates a YAML work graph for task dispatch
func (ps *PlanStorage) saveWorkGraph(plan *PlanDocument) error {
	graphPath := filepath.Join(ps.basePath, plan.ID, "workgraph.yml")
	
	// Simple YAML generation (you might want to use a proper YAML library)
	yaml := fmt.Sprintf(`# Work Graph for %s
# Generated: %s

plan_id: %s
version: %d
status: %s

waves:
`, plan.Requirement.Title, time.Now().Format(time.RFC3339), plan.ID, plan.Version, plan.Status)

	// Group tasks by wave
	waveMap := make(map[int][]TaskDoc)
	for _, task := range plan.Tasks {
		waveMap[task.Wave] = append(waveMap[task.Wave], task)
	}

	for wave := 0; wave <= len(waveMap); wave++ {
		tasks, exists := waveMap[wave]
		if !exists {
			continue
		}

		yaml += fmt.Sprintf("  - wave: %d\n    tasks:\n", wave)
		for _, task := range tasks {
			yaml += fmt.Sprintf("      - id: %s\n", task.ID)
			yaml += fmt.Sprintf("        title: %s\n", task.Title)
			yaml += fmt.Sprintf("        type: %s\n", task.Type)
			yaml += fmt.Sprintf("        status: %s\n", task.Status)
			if len(task.Dependencies) > 0 {
				yaml += "        dependencies:\n"
				for _, dep := range task.Dependencies {
					yaml += fmt.Sprintf("          - %s\n", dep)
				}
			}
		}
	}

	return ioutil.WriteFile(graphPath, []byte(yaml), 0644)
}

// detectChanges compares two plans and returns the differences
func (ps *PlanStorage) detectChanges(before, after *PlanDocument) []Change {
	var changes []Change

	// Check requirement changes
	if before.Requirement != after.Requirement {
		changes = append(changes, Change{
			Type:   "update_requirement",
			Path:   "/requirement",
			Before: before.Requirement,
			After:  after.Requirement,
		})
	}

	// Check task changes (simplified - you'd want more sophisticated diffing)
	if len(before.Tasks) != len(after.Tasks) {
		changes = append(changes, Change{
			Type:   "update_tasks",
			Path:   "/tasks",
			Before: len(before.Tasks),
			After:  len(after.Tasks),
		})
	}

	return changes
}

// GetActivePlans returns plans that are currently being executed
func (ps *PlanStorage) GetActivePlans() ([]PlanDocument, error) {
	allPlans, err := ps.ListPlans()
	if err != nil {
		return nil, err
	}

	var activePlans []PlanDocument
	for _, plan := range allPlans {
		if plan.Status == "executing" || plan.Status == "approved" {
			activePlans = append(activePlans, plan)
		}
	}

	return activePlans, nil
}