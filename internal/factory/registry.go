package factory

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type TemplateRegistry interface {
	LoadBuiltinTemplates() error
	LoadCustomTemplates(path string) error
	LoadFromURL(url string) error
	FetchFromRegistry(registryURL string) error
	SearchTemplates(query string) ([]*TemplateInfo, error)
	DownloadTemplate(name string, source string) error
	VerifyTemplate(template *AgentTemplate) (bool, error)
	GetTemplateSignature(name string) (*Signature, error)
}

type TemplateInfo struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Author      string   `json:"author"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	Source      string   `json:"source"`
	Verified    bool     `json:"verified"`
}

type Signature struct {
	Author    string `json:"author"`
	Timestamp string `json:"timestamp"`
	Hash      string `json:"hash"`
	Verified  bool   `json:"verified"`
}

type RegistryManifest struct {
	Version   string          `yaml:"version"`
	Templates []TemplateEntry `yaml:"templates"`
}

type TemplateEntry struct {
	Name     string `yaml:"name"`
	Path     string `yaml:"path"`
	Author   string `yaml:"author"`
	Verified bool   `yaml:"verified"`
}

type templateRegistry struct {
	templates map[string]*AgentTemplate
	info      map[string]*TemplateInfo
}

func NewTemplateRegistry() TemplateRegistry {
	return &templateRegistry{
		templates: make(map[string]*AgentTemplate),
		info:      make(map[string]*TemplateInfo),
	}
}

func (r *templateRegistry) LoadBuiltinTemplates() error {
	builtinTemplates := map[string]*AgentTemplate{
		"worker": {
			APIVersion: "agents.oat.dev/v1",
			Kind:       "AgentTemplate",
			Metadata: TemplateMetadata{
				Name:        "worker",
				Version:     "1.0.0",
				Author:      "oat-core",
				Description: "Standard worker agent for task execution",
				Tags:        []string{"core", "worker", "task"},
			},
			Spec: TemplateSpec{
				Base: BaseConfig{
					Type:        "worker",
					Model:       "default",
					Temperature: 0.7,
				},
				Behavior: BehaviorConfig{
					AutoComplete: true,
					PRCreation:   "required",
				},
			},
		},
		"reviewer": {
			APIVersion: "agents.oat.dev/v1",
			Kind:       "AgentTemplate",
			Metadata: TemplateMetadata{
				Name:        "reviewer",
				Version:     "1.0.0",
				Author:      "oat-core",
				Description: "Code review agent",
				Tags:        []string{"core", "review", "quality"},
			},
			Spec: TemplateSpec{
				Base: BaseConfig{
					Type:        "review",
					Model:       "default",
					Temperature: 0.3,
				},
				Behavior: BehaviorConfig{
					AutoComplete:        true,
					RequireVerification: false,
					PRCreation:          "none",
				},
			},
		},
	}

	for name, template := range builtinTemplates {
		r.templates[name] = template
		r.info[name] = &TemplateInfo{
			Name:        name,
			Version:     template.Metadata.Version,
			Author:      template.Metadata.Author,
			Description: template.Metadata.Description,
			Tags:        template.Metadata.Tags,
			Source:      "builtin",
			Verified:    true,
		}
	}

	return nil
}

func (r *templateRegistry) LoadCustomTemplates(path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}

	return filepath.Walk(path, func(filePath string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if !strings.HasSuffix(filePath, ".yaml") && !strings.HasSuffix(filePath, ".yml") {
			return nil
		}

		data, err := os.ReadFile(filePath)
		if err != nil {
			return fmt.Errorf("failed to read template %s: %w", filePath, err)
		}

		var template AgentTemplate
		if err := yaml.Unmarshal(data, &template); err != nil {
			return fmt.Errorf("failed to parse template %s: %w", filePath, err)
		}

		r.templates[template.Metadata.Name] = &template
		r.info[template.Metadata.Name] = &TemplateInfo{
			Name:        template.Metadata.Name,
			Version:     template.Metadata.Version,
			Author:      template.Metadata.Author,
			Description: template.Metadata.Description,
			Tags:        template.Metadata.Tags,
			Source:      filePath,
			Verified:    false,
		}

		return nil
	})
}

func (r *templateRegistry) LoadFromURL(url string) error {
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("failed to fetch template from %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to fetch template: HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read template: %w", err)
	}

	var template AgentTemplate
	if err := yaml.Unmarshal(data, &template); err != nil {
		return fmt.Errorf("failed to parse template: %w", err)
	}

	r.templates[template.Metadata.Name] = &template
	r.info[template.Metadata.Name] = &TemplateInfo{
		Name:        template.Metadata.Name,
		Version:     template.Metadata.Version,
		Author:      template.Metadata.Author,
		Description: template.Metadata.Description,
		Tags:        template.Metadata.Tags,
		Source:      url,
		Verified:    false,
	}

	return nil
}

func (r *templateRegistry) FetchFromRegistry(registryURL string) error {
	manifestURL := registryURL + "/registry.yaml"
	resp, err := http.Get(manifestURL)
	if err != nil {
		return fmt.Errorf("failed to fetch registry manifest: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to fetch registry: HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read manifest: %w", err)
	}

	var manifest RegistryManifest
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("failed to parse manifest: %w", err)
	}

	for _, entry := range manifest.Templates {
		templateURL := fmt.Sprintf("%s/%s", registryURL, entry.Path)
		if err := r.LoadFromURL(templateURL); err != nil {
			return fmt.Errorf("failed to load template %s: %w", entry.Name, err)
		}

		if info, ok := r.info[entry.Name]; ok {
			info.Verified = entry.Verified
		}
	}

	return nil
}

func (r *templateRegistry) SearchTemplates(query string) ([]*TemplateInfo, error) {
	query = strings.ToLower(query)
	var results []*TemplateInfo

	for _, info := range r.info {
		if strings.Contains(strings.ToLower(info.Name), query) ||
			strings.Contains(strings.ToLower(info.Description), query) {
			results = append(results, info)
			continue
		}

		for _, tag := range info.Tags {
			if strings.Contains(strings.ToLower(tag), query) {
				results = append(results, info)
				break
			}
		}
	}

	return results, nil
}

func (r *templateRegistry) DownloadTemplate(name string, source string) error {
	if source == "" {
		source = "https://raw.githubusercontent.com/oat-agent/agent-blueprints/main/templates"
	}

	templateURL := fmt.Sprintf("%s/%s.yaml", source, name)
	return r.LoadFromURL(templateURL)
}

func (r *templateRegistry) VerifyTemplate(template *AgentTemplate) (bool, error) {
	if template.APIVersion != "agents.oat.dev/v1" {
		return false, fmt.Errorf("unsupported API version: %s", template.APIVersion)
	}

	if template.Kind != "AgentTemplate" {
		return false, fmt.Errorf("invalid kind: %s", template.Kind)
	}

	if template.Metadata.Name == "" {
		return false, fmt.Errorf("template name is required")
	}

	if template.Spec.Base.Type == "" {
		return false, fmt.Errorf("agent type is required")
	}

	return true, nil
}

func (r *templateRegistry) GetTemplateSignature(name string) (*Signature, error) {
	info, ok := r.info[name]
	if !ok {
		return nil, fmt.Errorf("template %s not found", name)
	}

	return &Signature{
		Author:   info.Author,
		Verified: info.Verified,
	}, nil
}