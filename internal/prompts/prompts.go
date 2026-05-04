package prompts

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Root-IO-Labs/open-agent-teams/internal/prompts/commands"
	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
)

// AgentType is an alias for state.AgentType.
// Deprecated: Use state.AgentType directly instead.
type AgentType = state.AgentType

// Deprecated: Use state.AgentTypeSupervisor directly.
const TypeSupervisor = state.AgentTypeSupervisor

// Deprecated: Use state.AgentTypeWorker directly.
const TypeWorker = state.AgentTypeWorker

// Deprecated: Use state.AgentTypeMergeQueue directly.
const TypeMergeQueue = state.AgentTypeMergeQueue

// Deprecated: Use state.AgentTypeWorkspace directly.
const TypeWorkspace = state.AgentTypeWorkspace

// Deprecated: Use state.AgentTypeReview directly.
const TypeReview = state.AgentTypeReview

// Deprecated: Use state.AgentTypePRShepherd directly.
const TypePRShepherd = state.AgentTypePRShepherd

// Embedded default prompts
// Only supervisor and workspace are "hardcoded" - other agent types (worker, merge-queue, review)
// should come from configurable agent definitions in agent-templates.
//
//go:embed supervisor.md
var defaultSupervisorPrompt string

//go:embed workspace.md
var defaultWorkspacePrompt string

// GetDefaultPrompt returns the default prompt for the given agent type.
// Only supervisor and workspace have embedded default prompts.
// Worker, merge-queue, and review prompts should come from agent definitions.
func GetDefaultPrompt(agentType state.AgentType) string {
	switch agentType {
	case state.AgentTypeSupervisor:
		return defaultSupervisorPrompt
	case state.AgentTypeWorkspace:
		return defaultWorkspacePrompt
	case state.AgentTypeWorker, state.AgentTypeMergeQueue, state.AgentTypeReview, state.AgentTypeVerification:
		// These agent types should use configurable agent definitions
		// from ~/.oat/repos/<repo>/agents/ or <repo>/.oat/agents/
		return ""
	default:
		return ""
	}
}

// LoadCustomPrompt loads a custom prompt from the repository's .oat directory.
// Returns empty string if the file doesn't exist.
//
// Deprecated: This function is deprecated. Use the configurable agent system instead:
// - Agent definitions: <repo>/.oat/agents/<agent-name>.md
// - Local overrides: ~/.oat/repos/<repo>/agents/<agent-name>.md
// See internal/agents package for the new system.
func LoadCustomPrompt(repoPath string, agentType state.AgentType) (string, error) {
	var filename string
	switch agentType {
	case state.AgentTypeSupervisor:
		filename = "SUPERVISOR.md"
	case state.AgentTypeWorker:
		filename = "WORKER.md"
	case state.AgentTypeMergeQueue:
		filename = "MERGE-QUEUE.md"
	case state.AgentTypePRShepherd:
		filename = "PR-SHEPHERD.md"
	case state.AgentTypeWorkspace:
		filename = "WORKSPACE.md"
	case state.AgentTypeReview:
		filename = "REVIEW.md"
	case state.AgentTypeVerification:
		filename = "VERIFICATION.md"
	case state.AgentTypeGenericPersistent:
		// Generic persistent agents get their prompt from the spawn request,
		// not from embedded templates. No custom prompt file to load.
		return "", nil
	default:
		return "", fmt.Errorf("unknown agent type: %s", agentType)
	}

	promptPath := filepath.Join(repoPath, ".oat", filename)

	// Check if file exists
	if _, err := os.Stat(promptPath); os.IsNotExist(err) {
		return "", nil // File doesn't exist, return empty string (not an error)
	}

	// Read the file
	content, err := os.ReadFile(promptPath)
	if err != nil {
		return "", fmt.Errorf("failed to read custom prompt: %w", err)
	}

	return string(content), nil
}

// GetPrompt returns the complete prompt for an agent, combining default, custom prompts, CLI docs, and slash commands
func GetPrompt(repoPath string, agentType state.AgentType, cliDocs string) (string, error) {
	defaultPrompt := GetDefaultPrompt(agentType)

	customPrompt, err := LoadCustomPrompt(repoPath, agentType)
	if err != nil {
		return "", err
	}

	// Build the complete prompt
	var result string
	result = defaultPrompt

	// Add CLI documentation
	if cliDocs != "" {
		result += fmt.Sprintf("\n\n---\n\n%s", cliDocs)
	}

	// Add slash commands section
	slashCommands := GetSlashCommandsPrompt(string(agentType))
	if slashCommands != "" {
		result += fmt.Sprintf("\n\n---\n\n%s", slashCommands)
	}

	// Add custom prompt if it exists
	if customPrompt != "" {
		result += fmt.Sprintf("\n\n---\n\nRepository-specific instructions:\n\n%s", customPrompt)
	}

	return result, nil
}

// GenerateTrackingModePrompt generates prompt text explaining which PRs to track
// based on the tracking mode. The trackMode parameter should be "all", "author", or "assigned".
func GenerateTrackingModePrompt(trackMode string) string {
	switch trackMode {
	case "author":
		return `## PR Tracking Mode: Author Only

**IMPORTANT**: This repository is configured to track only PRs where you (or the oat system) are the author.

When listing and monitoring PRs, use:
` + "```bash" + `
gh pr list --author @me --label oat
` + "```" + `

Do NOT process or attempt to merge PRs authored by others. Focus only on PRs created by oat workers.`

	case "assigned":
		return `## PR Tracking Mode: Assigned Only

**IMPORTANT**: This repository is configured to track only PRs where you (or the oat system) are assigned.

When listing and monitoring PRs, use:
` + "```bash" + `
gh pr list --assignee @me --label oat
` + "```" + `

Do NOT process or attempt to merge PRs unless they are assigned to you. Focus only on PRs explicitly assigned to oat.`

	default: // "all"
		return `## PR Tracking Mode: All PRs

This repository is configured to track all PRs with the oat label.

When listing and monitoring PRs, use:
` + "```bash" + `
gh pr list --label oat
` + "```" + `

Monitor and process all oat-labeled PRs regardless of author or assignee.`
	}
}

// GenerateForkWorkflowPrompt generates prompt text explaining fork-based workflow.
// This is injected into all agent prompts when working in a fork.
func GenerateForkWorkflowPrompt(upstreamOwner, upstreamRepo, forkOwner string) string {
	return fmt.Sprintf(`## Fork Workflow (Auto-detected)

You are working in a fork of **%s/%s**.

**Key differences from upstream workflow:**

### Git Remotes
- **origin**: Your fork (%s/%s) - push branches here
- **upstream**: Original repo (%s/%s) - PRs target this repo

### Creating PRs
PRs should target the upstream repository, not your fork:
`+"```bash"+`
# Create a PR targeting upstream
gh pr create --repo %s/%s --head %s:<branch-name>

# View your PRs on upstream
gh pr list --repo %s/%s --author @me
`+"```"+`

### Keeping Synced
Keep your fork updated with upstream:
`+"```bash"+`
# Fetch upstream changes
git fetch upstream main

# Rebase your work
git rebase upstream/main

# Update your fork's main
git checkout main && git merge --ff-only upstream/main && git push origin main
`+"```"+`

### Important Notes
- **You cannot merge PRs** - upstream maintainers do that
- Create branches on your fork (origin), target upstream for PRs
- Keep rebasing onto upstream/main to avoid conflicts
- The pr-shepherd agent handles getting PRs ready for review
`, upstreamOwner, upstreamRepo,
		forkOwner, upstreamRepo,
		upstreamOwner, upstreamRepo,
		upstreamOwner, upstreamRepo, forkOwner,
		upstreamOwner, upstreamRepo)
}

// GetSlashCommandsPrompt returns a formatted prompt section containing slash commands
// relevant to agentType (the string form of state.AgentType, e.g. "worker").
// Pass empty agentType to include every command (used in tests).
func GetSlashCommandsPrompt(agentType string) string {
	var builder strings.Builder

	builder.WriteString("## Slash Commands\n\n")
	builder.WriteString("The following slash commands are available for use:\n\n")

	for _, cmd := range commands.AvailableCommands {
		if !commands.CommandAppliesToAgentType(cmd, agentType) {
			continue
		}
		content, err := commands.GetCommand(cmd.Name)
		if err != nil {
			continue
		}
		builder.WriteString(content)
		builder.WriteString("\n---\n\n")
	}

	return builder.String()
}
