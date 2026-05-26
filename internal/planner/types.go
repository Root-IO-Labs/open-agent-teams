package planner

import (
	"time"
)

// ExecutionPlan represents a complete plan for executing a requirement
type ExecutionPlan struct {
	ID          string                 `json:"id"`
	Requirement string                 `json:"requirement"`
	Analysis    *RequirementAnalysis   `json:"analysis"`
	Tasks       []*Task                `json:"tasks"`
	Assignments []*AgentAssignment     `json:"assignments"`
	Waves       []*ExecutionWave       `json:"waves"`
	Status      PlanStatus             `json:"status"`
	CreatedAt   time.Time              `json:"created_at"`
	StartedAt   *time.Time             `json:"started_at,omitempty"`
	CompletedAt *time.Time             `json:"completed_at,omitempty"`
}

// RequirementAnalysis represents the analysis of a user requirement
type RequirementAnalysis struct {
	Requirement           string           `json:"requirement"`
	Complexity           ComplexityLevel  `json:"complexity"`
	AffectedAreas        []string         `json:"affected_areas"`
	RequiredCapabilities []string         `json:"required_capabilities"`
	EstimatedEffort      string           `json:"estimated_effort"`
	Risks                []string         `json:"risks"`
	CodeAnalysis         *CodeAnalysis    `json:"code_analysis,omitempty"`
	AffectedCommunities  []string         `json:"affected_communities,omitempty"`
	Timestamp            time.Time        `json:"timestamp"`
}

// Task represents a unit of work
type Task struct {
	ID           string       `json:"id"`
	Type         string       `json:"type"`
	Description  string       `json:"description"`
	Priority     Priority     `json:"priority"`
	Area         string       `json:"area,omitempty"`
	Template     string       `json:"template,omitempty"`
	Dependencies []string     `json:"dependencies,omitempty"`
	TargetFiles  []string     `json:"target_files,omitempty"`
	EstimatedTime string      `json:"estimated_time,omitempty"`
}

// AgentAssignment represents the assignment of an agent to a task
type AgentAssignment struct {
	TaskID        string           `json:"task_id"`
	Task          *Task            `json:"task"`
	AgentID       string           `json:"agent_id,omitempty"`
	AgentName     string           `json:"agent_name,omitempty"`
	AgentTemplate string           `json:"agent_template"`
	Capabilities  []string         `json:"capabilities"`
	Status        AssignmentStatus `json:"status"`
	StartedAt     *time.Time       `json:"started_at,omitempty"`
	CompletedAt   *time.Time       `json:"completed_at,omitempty"`
	Error         string           `json:"error,omitempty"`
}

// ExecutionWave represents a group of tasks that can be executed in parallel
type ExecutionWave struct {
	Number      int                `json:"number"`
	Assignments []*AgentAssignment `json:"assignments"`
	Status      WaveStatus         `json:"status"`
	StartedAt   *time.Time         `json:"started_at,omitempty"`
	CompletedAt *time.Time         `json:"completed_at,omitempty"`
}

// ComplexityLevel represents the complexity of a task or requirement
type ComplexityLevel string

const (
	ComplexityLow    ComplexityLevel = "low"
	ComplexityMedium ComplexityLevel = "medium"
	ComplexityHigh   ComplexityLevel = "high"
)

// Priority represents task priority
type Priority string

const (
	PriorityLow    Priority = "low"
	PriorityMedium Priority = "medium"
	PriorityHigh   Priority = "high"
	PriorityCritical Priority = "critical"
)

// PlanStatus represents the status of an execution plan
type PlanStatus string

const (
	PlanStatusReady     PlanStatus = "ready"
	PlanStatusExecuting PlanStatus = "executing"
	PlanStatusCompleted PlanStatus = "completed"
	PlanStatusFailed    PlanStatus = "failed"
	PlanStatusCancelled PlanStatus = "cancelled"
)

// AssignmentStatus represents the status of an agent assignment
type AssignmentStatus string

const (
	AssignmentStatusPending   AssignmentStatus = "pending"
	AssignmentStatusRunning   AssignmentStatus = "running"
	AssignmentStatusCompleted AssignmentStatus = "completed"
	AssignmentStatusFailed    AssignmentStatus = "failed"
	AssignmentStatusCancelled AssignmentStatus = "cancelled"
)

// WaveStatus represents the status of an execution wave
type WaveStatus string

const (
	WaveStatusPending   WaveStatus = "pending"
	WaveStatusExecuting WaveStatus = "executing"
	WaveStatusCompleted WaveStatus = "completed"
	WaveStatusFailed    WaveStatus = "failed"
	WaveStatusCancelled WaveStatus = "cancelled"
)