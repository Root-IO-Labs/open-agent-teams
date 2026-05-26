package factory

import (
	"time"

	"github.com/Root-IO-Labs/open-agent-teams-8/internal/state"
)

type AgentTemplate struct {
	APIVersion string           `yaml:"apiVersion" json:"apiVersion"`
	Kind       string           `yaml:"kind" json:"kind"`
	Metadata   TemplateMetadata `yaml:"metadata" json:"metadata"`
	Spec       TemplateSpec     `yaml:"spec" json:"spec"`
}

type TemplateMetadata struct {
	Name        string   `yaml:"name" json:"name"`
	Version     string   `yaml:"version" json:"version"`
	Author      string   `yaml:"author" json:"author"`
	Description string   `yaml:"description" json:"description"`
	Tags        []string `yaml:"tags" json:"tags"`
}

type TemplateSpec struct {
	Base         BaseConfig         `yaml:"base" json:"base"`
	Capabilities CapabilityRequests `yaml:"capabilities" json:"capabilities"`
	Resources    ResourceLimits     `yaml:"resources" json:"resources"`
	Prompt       PromptConfig       `yaml:"prompt" json:"prompt"`
	Behavior     BehaviorConfig     `yaml:"behavior" json:"behavior"`
	Success      SuccessCriteria    `yaml:"success" json:"success"`
}

type BaseConfig struct {
	Type        string  `yaml:"type" json:"type"`
	Model       string  `yaml:"model" json:"model"`
	Temperature float32 `yaml:"temperature" json:"temperature"`
}

type CapabilityRequests struct {
	Tools  []ToolRequirement `yaml:"tools" json:"tools"`
	APIs   []string          `yaml:"apis" json:"apis"`
	Models ModelRequirements `yaml:"models" json:"models"`
}

type ToolRequirement struct {
	Name    string `yaml:"name" json:"name"`
	Version string `yaml:"version" json:"version"`
}

type ModelRequirements struct {
	Primary   string `yaml:"primary" json:"primary"`
	Secondary string `yaml:"secondary" json:"secondary"`
}

type ResourceLimits struct {
	Memory    string            `yaml:"memory" json:"memory"`
	CPU       int               `yaml:"cpu" json:"cpu"`
	Timeout   time.Duration     `yaml:"timeout" json:"timeout"`
	APILimits map[string]int    `yaml:"api_limits" json:"api_limits"`
}

type PromptConfig struct {
	System       string `yaml:"system" json:"system"`
	TaskTemplate string `yaml:"task_template" json:"task_template"`
}

type BehaviorConfig struct {
	AutoComplete        bool `yaml:"auto_complete" json:"auto_complete"`
	RequireVerification bool `yaml:"require_verification" json:"require_verification"`
	PRCreation          string `yaml:"pr_creation" json:"pr_creation"`
}

type SuccessCriteria struct {
	Conditions []SuccessCondition `yaml:"conditions" json:"conditions"`
}

type SuccessCondition struct {
	Type    string `yaml:"type" json:"type"`
	Path    string `yaml:"path,omitempty" json:"path,omitempty"`
	Command string `yaml:"command,omitempty" json:"command,omitempty"`
}

type InjectedCapabilities struct {
	Tools      map[string]Tool          `json:"tools"`
	APIs       map[string]APIClient     `json:"apis"`
	Models     map[string]ModelProvider `json:"models"`
	Extensions []Extension              `json:"extensions"`
}

type AllocatedResources struct {
	AgentID   string
	Memory    int64
	CPU       float64
	APITokens map[string]int
	StartTime time.Time
}

type ResourceReport struct {
	Timestamp time.Time
	System    SystemResources
	Agents    []AgentResourceUsage
}

type SystemResources struct {
	TotalMemory     int64
	TotalCPU        float64
	AvailableMemory int64
	AvailableCPU    float64
}

type AgentResourceUsage struct {
	AgentID  string
	Memory   int64
	CPU      float64
	Duration time.Duration
	Tokens   map[string]int
}

type Tool interface {
	Name() string
	Version() string
	Execute(args []string) error
}

type APIClient interface {
	Name() string
	Call(method, endpoint string, data interface{}) (interface{}, error)
}

type ModelProvider interface {
	Name() string
	Complete(prompt string) (string, error)
}

type Extension interface {
	Name() string
	Apply(agent *Agent) error
}