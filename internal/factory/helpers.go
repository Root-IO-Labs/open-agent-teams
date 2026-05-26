package factory

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"text/template"

	"github.com/Root-IO-Labs/open-agent-teams-8/internal/state"
)

func generateAgentID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func mapTemplateType(templateType string) state.AgentType {
	switch templateType {
	case "worker":
		return state.AgentTypeWorker
	case "review":
		return state.AgentTypeReview
	case "persistent":
		return state.AgentTypeSupervisor
	default:
		return state.AgentTypeWorker
	}
}

func mergeResources(template ResourceLimits, override ResourceLimits) ResourceLimits {
	result := template
	
	if override.Memory != "" {
		result.Memory = override.Memory
	}
	if override.CPU != 0 {
		result.CPU = override.CPU
	}
	if override.Timeout != 0 {
		result.Timeout = override.Timeout
	}
	if override.APILimits != nil {
		if result.APILimits == nil {
			result.APILimits = make(map[string]int)
		}
		for k, v := range override.APILimits {
			result.APILimits[k] = v
		}
	}
	
	return result
}

func mergeCapabilities(template CapabilityRequests, override CapabilityRequests) CapabilityRequests {
	result := template
	
	if len(override.Tools) > 0 {
		toolMap := make(map[string]ToolRequirement)
		for _, tool := range template.Tools {
			toolMap[tool.Name] = tool
		}
		for _, tool := range override.Tools {
			toolMap[tool.Name] = tool
		}
		result.Tools = make([]ToolRequirement, 0, len(toolMap))
		for _, tool := range toolMap {
			result.Tools = append(result.Tools, tool)
		}
	}
	
	if len(override.APIs) > 0 {
		apiMap := make(map[string]bool)
		for _, api := range template.APIs {
			apiMap[api] = true
		}
		for _, api := range override.APIs {
			apiMap[api] = true
		}
		result.APIs = make([]string, 0, len(apiMap))
		for api := range apiMap {
			result.APIs = append(result.APIs, api)
		}
	}
	
	if override.Models.Primary != "" {
		result.Models.Primary = override.Models.Primary
	}
	if override.Models.Secondary != "" {
		result.Models.Secondary = override.Models.Secondary
	}
	
	return result
}

func (f *agentFactory) generatePrompt(tmpl *AgentTemplate, req *CreateAgentRequest) (string, error) {
	params := make(map[string]interface{})
	
	for k, v := range req.Parameters {
		params[k] = v
	}
	
	params["task"] = req.Task
	params["repository"] = req.Repository
	params["agent_name"] = req.Name
	params["template"] = req.Template
	
	if req.Issue != nil {
		params["issue"] = *req.Issue
		params["has_issue"] = true
	} else {
		params["has_issue"] = false
	}
	
	var buf bytes.Buffer
	
	if tmpl.Spec.Prompt.System != "" {
		t, err := template.New("system").Parse(tmpl.Spec.Prompt.System)
		if err != nil {
			return "", fmt.Errorf("failed to parse system prompt: %w", err)
		}
		
		if err := t.Execute(&buf, params); err != nil {
			return "", fmt.Errorf("failed to execute system prompt: %w", err)
		}
	}
	
	if tmpl.Spec.Prompt.TaskTemplate != "" {
		t, err := template.New("task").Parse(tmpl.Spec.Prompt.TaskTemplate)
		if err != nil {
			return "", fmt.Errorf("failed to parse task template: %w", err)
		}
		
		buf.WriteString("\n\n## Your Task\n\n")
		if err := t.Execute(&buf, params); err != nil {
			return "", fmt.Errorf("failed to execute task template: %w", err)
		}
	}
	
	buf.WriteString("\n\n## Agent Configuration\n\n")
	buf.WriteString(fmt.Sprintf("- Template: %s\n", req.Template))
	buf.WriteString(fmt.Sprintf("- Model: %s\n", tmpl.Spec.Base.Model))
	buf.WriteString(fmt.Sprintf("- Auto-complete: %v\n", tmpl.Spec.Behavior.AutoComplete))
	buf.WriteString(fmt.Sprintf("- PR Creation: %s\n", tmpl.Spec.Behavior.PRCreation))
	
	if len(tmpl.Spec.Capabilities.Tools) > 0 {
		buf.WriteString("\n## Available Tools\n\n")
		for _, tool := range tmpl.Spec.Capabilities.Tools {
			buf.WriteString(fmt.Sprintf("- %s", tool.Name))
			if tool.Version != "" {
				buf.WriteString(fmt.Sprintf(" (%s)", tool.Version))
			}
			buf.WriteString("\n")
		}
	}
	
	return buf.String(), nil
}

func (f *agentFactory) registerWithDaemon(agent *Agent, prompt string) error {
	return nil
}

func (f *agentFactory) cleanup(agent *Agent) {
	if agent.Resources != nil {
		f.resources.Release(agent.ID)
	}
	
	delete(f.agents, agent.ID)
}