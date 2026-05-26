package factory

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams-8/internal/daemon"
	"github.com/Root-IO-Labs/open-agent-teams-8/internal/state"
)

type AgentFactory interface {
	RegisterTemplate(template *AgentTemplate) error
	ListTemplates() ([]*AgentTemplate, error)
	GetTemplate(name string) (*AgentTemplate, error)
	ValidateTemplate(template *AgentTemplate) error
	
	CreateAgent(ctx context.Context, req *CreateAgentRequest) (*Agent, error)
	CreateFromTemplate(ctx context.Context, templateName string, params map[string]interface{}) (*Agent, error)
	
	InjectCapabilities(agent *Agent, caps CapabilityRequests) error
	ValidateCapabilities(caps CapabilityRequests) error
	
	AllocateResources(agent *Agent, limits ResourceLimits) error
	ReleaseResources(agentID string) error
	GetResourceUsage() (*ResourceReport, error)
	
	StartAgent(agent *Agent) error
	StopAgent(agentID string) error
	RestartAgent(agentID string) error
}

type CreateAgentRequest struct {
	Name         string                 `json:"name"`
	Template     string                 `json:"template"`
	Task         string                 `json:"task"`
	Parameters   map[string]interface{} `json:"parameters"`
	Capabilities CapabilityRequests     `json:"capabilities"`
	Resources    ResourceLimits         `json:"resources"`
	Repository   string                 `json:"repository"`
	Issue        *int                   `json:"issue,omitempty"`
}

type Agent struct {
	ID           string                 `json:"id"`
	Name         string                 `json:"name"`
	Type         state.AgentType        `json:"type"`
	Template     string                 `json:"template"`
	Task         string                 `json:"task"`
	Status       AgentStatus            `json:"status"`
	Capabilities *InjectedCapabilities  `json:"capabilities"`
	Resources    *AllocatedResources    `json:"resources"`
	Process      *ProcessInfo           `json:"process"`
	CreatedAt    time.Time              `json:"created_at"`
	StartedAt    *time.Time             `json:"started_at"`
	CompletedAt  *time.Time             `json:"completed_at"`
}

type AgentStatus string

const (
	AgentStatusPending   AgentStatus = "pending"
	AgentStatusBuilding  AgentStatus = "building"
	AgentStatusStarting  AgentStatus = "starting"
	AgentStatusRunning   AgentStatus = "running"
	AgentStatusDormant   AgentStatus = "dormant"
	AgentStatusCompleted AgentStatus = "completed"
	AgentStatusFailed    AgentStatus = "failed"
)

type ProcessInfo struct {
	PID        int    `json:"pid"`
	SessionID  string `json:"session_id"`
	WorktreePath string `json:"worktree_path"`
}

type agentFactory struct {
	mu            sync.RWMutex
	templates     map[string]*AgentTemplate
	agents        map[string]*Agent
	daemon        *daemon.Daemon
	state         *state.State
	resources     *ResourceManager
	capabilities  *CapabilityInjector
	validator     *TemplateValidator
}

func NewFactory(d *daemon.Daemon, s *state.State) AgentFactory {
	return &agentFactory{
		templates:    make(map[string]*AgentTemplate),
		agents:       make(map[string]*Agent),
		daemon:       d,
		state:        s,
		resources:    NewResourceManager(),
		capabilities: NewCapabilityInjector(),
		validator:    NewTemplateValidator(),
	}
}

func (f *agentFactory) CreateAgent(ctx context.Context, req *CreateAgentRequest) (*Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	
	template, err := f.GetTemplate(req.Template)
	if err != nil {
		return nil, fmt.Errorf("template not found: %w", err)
	}
	
	if err := f.validator.Validate(template); err != nil {
		return nil, fmt.Errorf("invalid template: %w", err)
	}
	
	resources := mergeResources(template.Spec.Resources, req.Resources)
	if err := f.resources.CanAllocate(resources); err != nil {
		return nil, fmt.Errorf("insufficient resources: %w", err)
	}
	
	agent := &Agent{
		ID:        generateAgentID(),
		Name:      req.Name,
		Type:      mapTemplateType(template.Spec.Base.Type),
		Template:  req.Template,
		Task:      req.Task,
		Status:    AgentStatusBuilding,
		CreatedAt: time.Now(),
	}
	
	caps := mergeCapabilities(template.Spec.Capabilities, req.Capabilities)
	if err := f.capabilities.Inject(agent, caps); err != nil {
		return nil, fmt.Errorf("capability injection failed: %w", err)
	}
	
	if err := f.resources.Allocate(agent, resources); err != nil {
		return nil, fmt.Errorf("resource allocation failed: %w", err)
	}
	
	prompt, err := f.generatePrompt(template, req)
	if err != nil {
		f.resources.Release(agent.ID)
		return nil, fmt.Errorf("prompt generation failed: %w", err)
	}
	
	if err := f.registerWithDaemon(agent, prompt); err != nil {
		f.resources.Release(agent.ID)
		return nil, fmt.Errorf("daemon registration failed: %w", err)
	}
	
	if err := f.StartAgent(agent); err != nil {
		f.cleanup(agent)
		return nil, fmt.Errorf("agent start failed: %w", err)
	}
	
	f.agents[agent.ID] = agent
	return agent, nil
}

func (f *agentFactory) CreateFromTemplate(ctx context.Context, templateName string, params map[string]interface{}) (*Agent, error) {
	req := &CreateAgentRequest{
		Template:   templateName,
		Parameters: params,
	}
	
	if name, ok := params["name"].(string); ok {
		req.Name = name
	}
	
	if task, ok := params["task"].(string); ok {
		req.Task = task
	}
	
	if repo, ok := params["repository"].(string); ok {
		req.Repository = repo
	}
	
	return f.CreateAgent(ctx, req)
}

func (f *agentFactory) GetTemplate(name string) (*AgentTemplate, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	
	template, ok := f.templates[name]
	if !ok {
		return nil, fmt.Errorf("template %s not found", name)
	}
	
	return template, nil
}

func (f *agentFactory) ListTemplates() ([]*AgentTemplate, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	
	templates := make([]*AgentTemplate, 0, len(f.templates))
	for _, template := range f.templates {
		templates = append(templates, template)
	}
	
	return templates, nil
}

func (f *agentFactory) RegisterTemplate(template *AgentTemplate) error {
	if err := f.validator.Validate(template); err != nil {
		return fmt.Errorf("invalid template: %w", err)
	}
	
	f.mu.Lock()
	defer f.mu.Unlock()
	
	f.templates[template.Metadata.Name] = template
	return nil
}

func (f *agentFactory) ValidateTemplate(template *AgentTemplate) error {
	return f.validator.Validate(template)
}

func (f *agentFactory) InjectCapabilities(agent *Agent, caps CapabilityRequests) error {
	return f.capabilities.Inject(agent, caps)
}

func (f *agentFactory) ValidateCapabilities(caps CapabilityRequests) error {
	return f.capabilities.Validate(caps)
}

func (f *agentFactory) AllocateResources(agent *Agent, limits ResourceLimits) error {
	return f.resources.Allocate(agent, limits)
}

func (f *agentFactory) ReleaseResources(agentID string) error {
	return f.resources.Release(agentID)
}

func (f *agentFactory) GetResourceUsage() (*ResourceReport, error) {
	return f.resources.GetUsageReport(), nil
}

func (f *agentFactory) StartAgent(agent *Agent) error {
	agent.Status = AgentStatusStarting
	now := time.Now()
	agent.StartedAt = &now
	
	agent.Status = AgentStatusRunning
	return nil
}

func (f *agentFactory) StopAgent(agentID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	
	agent, ok := f.agents[agentID]
	if !ok {
		return fmt.Errorf("agent %s not found", agentID)
	}
	
	agent.Status = AgentStatusCompleted
	now := time.Now()
	agent.CompletedAt = &now
	
	f.resources.Release(agentID)
	
	return nil
}

func (f *agentFactory) RestartAgent(agentID string) error {
	if err := f.StopAgent(agentID); err != nil {
		return err
	}
	
	f.mu.RLock()
	agent := f.agents[agentID]
	f.mu.RUnlock()
	
	return f.StartAgent(agent)
}