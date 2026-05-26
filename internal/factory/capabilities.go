package factory

import (
	"fmt"
	"os/exec"
	"strings"
)

type CapabilityInjector struct {
	tools  map[string]Tool
	apis   map[string]APIClient
	models map[string]ModelProvider
}

func NewCapabilityInjector() *CapabilityInjector {
	return &CapabilityInjector{
		tools:  make(map[string]Tool),
		apis:   make(map[string]APIClient),
		models: make(map[string]ModelProvider),
	}
}

func (ci *CapabilityInjector) Inject(agent *Agent, caps CapabilityRequests) error {
	injected := &InjectedCapabilities{
		Tools:  make(map[string]Tool),
		APIs:   make(map[string]APIClient),
		Models: make(map[string]ModelProvider),
	}

	for _, toolReq := range caps.Tools {
		tool, err := ci.getOrInstallTool(toolReq)
		if err != nil {
			return fmt.Errorf("tool %s: %w", toolReq.Name, err)
		}
		injected.Tools[toolReq.Name] = tool
	}

	for _, apiName := range caps.APIs {
		client, err := ci.getAPIClient(apiName)
		if err != nil {
			return fmt.Errorf("API %s: %w", apiName, err)
		}
		injected.APIs[apiName] = client
	}

	if caps.Models.Primary != "" {
		provider, err := ci.getModelProvider(caps.Models.Primary)
		if err != nil {
			return fmt.Errorf("primary model: %w", err)
		}
		injected.Models["primary"] = provider
	}

	if caps.Models.Secondary != "" {
		provider, err := ci.getModelProvider(caps.Models.Secondary)
		if err != nil {
			return fmt.Errorf("secondary model: %w", err)
		}
		injected.Models["secondary"] = provider
	}

	agent.Capabilities = injected
	return nil
}

func (ci *CapabilityInjector) Validate(caps CapabilityRequests) error {
	for _, toolReq := range caps.Tools {
		if !ci.isToolAvailable(toolReq.Name) {
			if !ci.canInstallTool(toolReq.Name) {
				return fmt.Errorf("tool %s is not available and cannot be installed", toolReq.Name)
			}
		}
	}

	for _, apiName := range caps.APIs {
		if !ci.isAPIConfigured(apiName) {
			return fmt.Errorf("API %s is not configured", apiName)
		}
	}

	return nil
}

func (ci *CapabilityInjector) getOrInstallTool(req ToolRequirement) (Tool, error) {
	if tool, ok := ci.tools[req.Name]; ok {
		return tool, nil
	}

	tool, err := ci.installTool(req)
	if err != nil {
		return nil, err
	}

	ci.tools[req.Name] = tool
	return tool, nil
}

func (ci *CapabilityInjector) installTool(req ToolRequirement) (Tool, error) {
	tool := &CommandLineTool{
		name:    req.Name,
		version: req.Version,
	}

	switch req.Name {
	case "semgrep":
		if err := ci.ensureToolInstalled("semgrep", "pip install semgrep"); err != nil {
			return nil, err
		}
	case "gitleaks":
		if err := ci.ensureToolInstalled("gitleaks", "brew install gitleaks"); err != nil {
			return nil, err
		}
	case "trivy":
		if err := ci.ensureToolInstalled("trivy", "brew install trivy"); err != nil {
			return nil, err
		}
	case "eslint":
		if err := ci.ensureToolInstalled("eslint", "npm install -g eslint"); err != nil {
			return nil, err
		}
	case "pytest":
		if err := ci.ensureToolInstalled("pytest", "pip install pytest"); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown tool: %s", req.Name)
	}

	return tool, nil
}

func (ci *CapabilityInjector) ensureToolInstalled(name string, installCmd string) error {
	if _, err := exec.LookPath(name); err == nil {
		return nil
	}

	fmt.Printf("Installing %s: %s\n", name, installCmd)
	parts := strings.Fields(installCmd)
	cmd := exec.Command(parts[0], parts[1:]...)
	return cmd.Run()
}

func (ci *CapabilityInjector) getAPIClient(apiName string) (APIClient, error) {
	if client, ok := ci.apis[apiName]; ok {
		return client, nil
	}

	switch apiName {
	case "github":
		client := &GitHubAPIClient{}
		ci.apis[apiName] = client
		return client, nil
	case "snyk":
		client := &SnykAPIClient{}
		ci.apis[apiName] = client
		return client, nil
	default:
		return nil, fmt.Errorf("unknown API: %s", apiName)
	}
}

func (ci *CapabilityInjector) getModelProvider(modelName string) (ModelProvider, error) {
	if provider, ok := ci.models[modelName]; ok {
		return provider, nil
	}

	provider := &DefaultModelProvider{name: modelName}
	ci.models[modelName] = provider
	return provider, nil
}

func (ci *CapabilityInjector) isToolAvailable(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func (ci *CapabilityInjector) canInstallTool(name string) bool {
	knownTools := []string{"semgrep", "gitleaks", "trivy", "eslint", "pytest", "ruff", "black"}
	for _, known := range knownTools {
		if known == name {
			return true
		}
	}
	return false
}

func (ci *CapabilityInjector) isAPIConfigured(apiName string) bool {
	switch apiName {
	case "github", "snyk", "datadog", "sentry":
		return true
	default:
		return false
	}
}

type CommandLineTool struct {
	name    string
	version string
}

func (t *CommandLineTool) Name() string {
	return t.name
}

func (t *CommandLineTool) Version() string {
	return t.version
}

func (t *CommandLineTool) Execute(args []string) error {
	cmd := exec.Command(t.name, args...)
	return cmd.Run()
}

type GitHubAPIClient struct{}

func (c *GitHubAPIClient) Name() string {
	return "github"
}

func (c *GitHubAPIClient) Call(method, endpoint string, data interface{}) (interface{}, error) {
	return nil, nil
}

type SnykAPIClient struct{}

func (c *SnykAPIClient) Name() string {
	return "snyk"
}

func (c *SnykAPIClient) Call(method, endpoint string, data interface{}) (interface{}, error) {
	return nil, nil
}

type DefaultModelProvider struct {
	name string
}

func (p *DefaultModelProvider) Name() string {
	return p.name
}

func (p *DefaultModelProvider) Complete(prompt string) (string, error) {
	return "", nil
}