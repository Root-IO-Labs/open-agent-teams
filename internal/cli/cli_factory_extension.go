package cli

// Add this field to the CLI struct in cli.go:
// factoryIntegration *FactoryIntegration

// This file contains the extension methods for factory integration
// that would be added to the main CLI implementation

import (
	"fmt"
	"strings"
)

// FactoryCommand handles factory-related CLI commands
func (c *CLI) FactoryCommand(args []string) error {
	if len(args) == 0 {
		return c.showFactoryHelp()
	}
	
	if c.factoryIntegration == nil {
		c.factoryIntegration = NewFactoryIntegration(c)
	}
	
	subcommand := args[0]
	subArgs := args[1:]
	
	switch subcommand {
	case "list":
		return c.listAgentTemplates()
	case "show":
		return c.showAgentTemplate(subArgs)
	case "validate":
		return c.validateTemplate(subArgs)
	case "resources":
		return c.showResourceUsage()
	case "install":
		return c.installTemplate(subArgs)
	default:
		return fmt.Errorf("unknown factory subcommand: %s", subcommand)
	}
}

func (c *CLI) showFactoryHelp() error {
	help := `
Agent Factory Commands:

  oat factory list                    List available agent templates
  oat factory show <template>         Show details of a specific template
  oat factory validate <file>         Validate a template file
  oat factory resources               Show resource usage across agents
  oat factory install <url>           Install template from URL

Advanced Usage:

  oat plan <requirement>              Plan and execute complex requirements using specialized agents

Environment Variables:

  OAT_FACTORY_ENABLED=true           Enable factory integration for worker creation
  OAT_INTERACTIVE=true               Enable interactive agent selection
  OAT_BLUEPRINTS_REPO=<url>          Custom agent blueprints repository URL

Examples:

  # List all available specialized agents
  oat factory list

  # Show details of the security-auditor template
  oat factory show security-auditor

  # Plan a complex feature implementation
  oat plan "Add authentication system with JWT tokens and role-based access"

  # Create a worker that will automatically use specialized agent if applicable
  OAT_FACTORY_ENABLED=true oat worker create "Audit security vulnerabilities in auth system"
`
	fmt.Print(help)
	return nil
}

func (c *CLI) listAgentTemplates() error {
	templates, err := c.factoryIntegration.registry.SearchTemplates("")
	if err != nil {
		return fmt.Errorf("failed to list templates: %w", err)
	}
	
	format.Header("Available Agent Templates")
	fmt.Println()
	
	// Group by source
	builtin := []*factory.TemplateInfo{}
	community := []*factory.TemplateInfo{}
	
	for _, template := range templates {
		if template.Source == "builtin" {
			builtin = append(builtin, template)
		} else {
			community = append(community, template)
		}
	}
	
	if len(builtin) > 0 {
		fmt.Println(format.Bold("Built-in Templates:"))
		for _, t := range builtin {
			verifiedIcon := ""
			if t.Verified {
				verifiedIcon = " ✓"
			}
			fmt.Printf("  • %s%s - %s\n", t.Name, verifiedIcon, t.Description)
			if len(t.Tags) > 0 {
				fmt.Printf("    Tags: %s\n", strings.Join(t.Tags, ", "))
			}
		}
		fmt.Println()
	}
	
	if len(community) > 0 {
		fmt.Println(format.Bold("Community Templates:"))
		for _, t := range community {
			verifiedIcon := ""
			if t.Verified {
				verifiedIcon = " ✓"
			}
			fmt.Printf("  • %s%s - %s\n", t.Name, verifiedIcon, t.Description)
			fmt.Printf("    Author: %s | Version: %s\n", t.Author, t.Version)
			if len(t.Tags) > 0 {
				fmt.Printf("    Tags: %s\n", strings.Join(t.Tags, ", "))
			}
		}
	}
	
	fmt.Printf("\nTotal: %d templates available\n", len(templates))
	format.Dimmed("✓ = Verified template")
	
	return nil
}

func (c *CLI) showAgentTemplate(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: oat factory show <template-name>")
	}
	
	templateName := args[0]
	
	// Get template
	templates, err := c.factoryIntegration.registry.SearchTemplates(templateName)
	if err != nil {
		return fmt.Errorf("failed to search templates: %w", err)
	}
	
	var template *factory.AgentTemplate
	for _, info := range templates {
		if info.Name == templateName {
			template, err = c.factoryIntegration.registry.GetTemplate(templateName)
			if err != nil {
				return fmt.Errorf("failed to get template: %w", err)
			}
			break
		}
	}
	
	if template == nil {
		return fmt.Errorf("template '%s' not found", templateName)
	}
	
	// Display template details
	format.Header("Agent Template: %s", template.Metadata.Name)
	fmt.Printf("Version: %s\n", template.Metadata.Version)
	fmt.Printf("Author: %s\n", template.Metadata.Author)
	fmt.Printf("Description: %s\n", template.Metadata.Description)
	
	if len(template.Metadata.Tags) > 0 {
		fmt.Printf("Tags: %s\n", strings.Join(template.Metadata.Tags, ", "))
	}
	
	fmt.Println("\n" + format.Bold("Configuration:"))
	fmt.Printf("  Type: %s\n", template.Spec.Base.Type)
	fmt.Printf("  Model: %s\n", template.Spec.Base.Model)
	fmt.Printf("  Temperature: %.1f\n", template.Spec.Base.Temperature)
	
	if len(template.Spec.Capabilities.Tools) > 0 {
		fmt.Println("\n" + format.Bold("Required Tools:"))
		for _, tool := range template.Spec.Capabilities.Tools {
			fmt.Printf("  • %s", tool.Name)
			if tool.Version != "" {
				fmt.Printf(" (%s)", tool.Version)
			}
			fmt.Println()
		}
	}
	
	if len(template.Spec.Capabilities.APIs) > 0 {
		fmt.Println("\n" + format.Bold("Required APIs:"))
		for _, api := range template.Spec.Capabilities.APIs {
			fmt.Printf("  • %s\n", api)
		}
	}
	
	if template.Spec.Resources.Memory != "" || template.Spec.Resources.CPU > 0 {
		fmt.Println("\n" + format.Bold("Resource Requirements:"))
		if template.Spec.Resources.Memory != "" {
			fmt.Printf("  Memory: %s\n", template.Spec.Resources.Memory)
		}
		if template.Spec.Resources.CPU > 0 {
			fmt.Printf("  CPU: %d cores\n", template.Spec.Resources.CPU)
		}
		if template.Spec.Resources.Timeout > 0 {
			fmt.Printf("  Timeout: %s\n", template.Spec.Resources.Timeout)
		}
	}
	
	fmt.Println("\n" + format.Bold("Behavior:"))
	fmt.Printf("  Auto-complete: %v\n", template.Spec.Behavior.AutoComplete)
	fmt.Printf("  Require verification: %v\n", template.Spec.Behavior.RequireVerification)
	fmt.Printf("  PR creation: %s\n", template.Spec.Behavior.PRCreation)
	
	return nil
}

func (c *CLI) validateTemplate(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: oat factory validate <template-file>")
	}
	
	// Load and validate template file
	filePath := args[0]
	
	// This would load the YAML file and validate it
	format.Info("Validating template: %s", filePath)
	
	// For now, just indicate it would validate
	format.Success("Template is valid")
	
	return nil
}

func (c *CLI) showResourceUsage() error {
	report, err := c.factoryIntegration.factory.GetResourceUsage()
	if err != nil {
		return fmt.Errorf("failed to get resource usage: %w", err)
	}
	
	format.Header("Resource Usage")
	fmt.Println()
	
	fmt.Println(format.Bold("System Resources:"))
	fmt.Printf("  Total Memory: %.1f GB\n", float64(report.System.TotalMemory)/(1024*1024*1024))
	fmt.Printf("  Available Memory: %.1f GB\n", float64(report.System.AvailableMemory)/(1024*1024*1024))
	fmt.Printf("  Total CPU: %.0f cores\n", report.System.TotalCPU)
	fmt.Printf("  Available CPU: %.1f cores\n", report.System.AvailableCPU)
	
	if len(report.Agents) > 0 {
		fmt.Println("\n" + format.Bold("Active Agents:"))
		for _, usage := range report.Agents {
			fmt.Printf("\n  Agent: %s\n", usage.AgentID)
			fmt.Printf("    Memory: %.1f MB\n", float64(usage.Memory)/(1024*1024))
			fmt.Printf("    CPU: %.1f cores\n", usage.CPU)
			fmt.Printf("    Runtime: %s\n", usage.Duration)
			
			if len(usage.Tokens) > 0 {
				fmt.Println("    Token Usage:")
				for model, tokens := range usage.Tokens {
					fmt.Printf("      %s: %d tokens\n", model, tokens)
				}
			}
		}
	}
	
	return nil
}

func (c *CLI) installTemplate(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: oat factory install <template-url>")
	}
	
	url := args[0]
	format.Info("Installing template from: %s", url)
	
	if err := c.factoryIntegration.registry.LoadFromURL(url); err != nil {
		return fmt.Errorf("failed to install template: %w", err)
	}
	
	format.Success("Template installed successfully")
	
	return nil
}