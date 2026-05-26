package factory

import (
	"fmt"
	"regexp"
	"strings"
)

type TemplateValidator struct {
	rules []ValidationRule
}

type ValidationRule interface {
	Validate(template *AgentTemplate) error
}

func NewTemplateValidator() *TemplateValidator {
	return &TemplateValidator{
		rules: []ValidationRule{
			&APIVersionRule{},
			&MetadataRule{},
			&SpecRule{},
			&CapabilityRule{},
			&ResourceRule{},
			&PromptRule{},
		},
	}
}

func (v *TemplateValidator) Validate(template *AgentTemplate) error {
	if template == nil {
		return fmt.Errorf("template is nil")
	}

	for _, rule := range v.rules {
		if err := rule.Validate(template); err != nil {
			return err
		}
	}

	return nil
}

type APIVersionRule struct{}

func (r *APIVersionRule) Validate(template *AgentTemplate) error {
	if template.APIVersion == "" {
		return fmt.Errorf("apiVersion is required")
	}

	if !strings.HasPrefix(template.APIVersion, "agents.oat.dev/") {
		return fmt.Errorf("apiVersion must start with 'agents.oat.dev/'")
	}

	if template.APIVersion != "agents.oat.dev/v1" {
		return fmt.Errorf("only apiVersion 'agents.oat.dev/v1' is currently supported")
	}

	return nil
}

type MetadataRule struct{}

func (r *MetadataRule) Validate(template *AgentTemplate) error {
	if template.Metadata.Name == "" {
		return fmt.Errorf("metadata.name is required")
	}

	if !isValidName(template.Metadata.Name) {
		return fmt.Errorf("metadata.name must be lowercase alphanumeric with hyphens")
	}

	if template.Metadata.Version == "" {
		return fmt.Errorf("metadata.version is required")
	}

	if !isValidVersion(template.Metadata.Version) {
		return fmt.Errorf("metadata.version must follow semantic versioning (e.g., 1.0.0)")
	}

	if template.Metadata.Author == "" {
		return fmt.Errorf("metadata.author is required")
	}

	if template.Metadata.Description == "" {
		return fmt.Errorf("metadata.description is required")
	}

	return nil
}

type SpecRule struct{}

func (r *SpecRule) Validate(template *AgentTemplate) error {
	if template.Spec.Base.Type == "" {
		return fmt.Errorf("spec.base.type is required")
	}

	validTypes := []string{"worker", "review", "persistent"}
	if !contains(validTypes, template.Spec.Base.Type) {
		return fmt.Errorf("spec.base.type must be one of: %v", validTypes)
	}

	if template.Spec.Base.Temperature < 0 || template.Spec.Base.Temperature > 1 {
		return fmt.Errorf("spec.base.temperature must be between 0 and 1")
	}

	if template.Spec.Behavior.PRCreation != "" {
		validPRCreation := []string{"required", "optional", "none"}
		if !contains(validPRCreation, template.Spec.Behavior.PRCreation) {
			return fmt.Errorf("spec.behavior.pr_creation must be one of: %v", validPRCreation)
		}
	}

	return nil
}

type CapabilityRule struct{}

func (r *CapabilityRule) Validate(template *AgentTemplate) error {
	for _, tool := range template.Spec.Capabilities.Tools {
		if tool.Name == "" {
			return fmt.Errorf("tool name is required")
		}

		if tool.Version != "" && !isValidVersionConstraint(tool.Version) {
			return fmt.Errorf("invalid version constraint for tool %s: %s", tool.Name, tool.Version)
		}
	}

	for _, api := range template.Spec.Capabilities.APIs {
		if api == "" {
			return fmt.Errorf("API name cannot be empty")
		}
	}

	return nil
}

type ResourceRule struct{}

func (r *ResourceRule) Validate(template *AgentTemplate) error {
	if template.Spec.Resources.Memory != "" {
		if !isValidMemory(template.Spec.Resources.Memory) {
			return fmt.Errorf("invalid memory format: %s (use format like 2Gi, 512Mi)", template.Spec.Resources.Memory)
		}
	}

	if template.Spec.Resources.CPU < 0 {
		return fmt.Errorf("CPU must be positive")
	}

	if template.Spec.Resources.Timeout < 0 {
		return fmt.Errorf("timeout must be positive")
	}

	for key, value := range template.Spec.Resources.APILimits {
		if key == "" {
			return fmt.Errorf("API limit key cannot be empty")
		}
		if value < 0 {
			return fmt.Errorf("API limit for %s must be positive", key)
		}
	}

	return nil
}

type PromptRule struct{}

func (r *PromptRule) Validate(template *AgentTemplate) error {
	if template.Spec.Prompt.System == "" && template.Spec.Prompt.TaskTemplate == "" {
		return fmt.Errorf("at least one of spec.prompt.system or spec.prompt.task_template is required")
	}

	if template.Spec.Prompt.TaskTemplate != "" {
		if !strings.Contains(template.Spec.Prompt.TaskTemplate, "{task") {
			return fmt.Errorf("spec.prompt.task_template should contain {task} placeholder")
		}
	}

	return nil
}

func isValidName(name string) bool {
	match, _ := regexp.MatchString("^[a-z0-9-]+$", name)
	return match
}

func isValidVersion(version string) bool {
	match, _ := regexp.MatchString(`^\d+\.\d+\.\d+(-[a-zA-Z0-9]+)?$`, version)
	return match
}

func isValidVersionConstraint(constraint string) bool {
	match, _ := regexp.MatchString(`^(>=?|<=?|~>|=)?\s*\d+\.\d+\.\d+(-[a-zA-Z0-9]+)?$`, constraint)
	return match
}

func isValidMemory(memory string) bool {
	match, _ := regexp.MatchString(`^\d+(\.\d+)?[KMG]i?$`, memory)
	return match
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}