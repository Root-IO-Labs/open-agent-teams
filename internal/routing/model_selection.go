package routing

import (
	"fmt"
	"os"
	"strings"
)

// ModelConfig represents a model with its requirements
type ModelConfig struct {
	ID          string
	Provider    string
	EnvKey      string
	Priority    int // Lower is better
	Description string
}

// GetAvailableModels returns models that have their API keys configured
func GetAvailableModels() []ModelConfig {
	// Define available models in priority order
	models := []ModelConfig{
		{
			ID:          "anthropic:claude-sonnet-4-6",
			Provider:    "anthropic",
			EnvKey:      "ANTHROPIC_API_KEY",
			Priority:    1,
			Description: "Claude 3.5 Sonnet - Best for complex reasoning",
		},
		{
			ID:          "anthropic:claude-haiku-3",
			Provider:    "anthropic",
			EnvKey:      "ANTHROPIC_API_KEY",
			Priority:    2,
			Description: "Claude 3 Haiku - Fast and efficient",
		},
		{
			ID:          "openrouter:deepseek/deepseek-v3.2",
			Provider:    "openrouter",
			EnvKey:      "OPENROUTER_API_KEY",
			Priority:    3,
			Description: "DeepSeek V3.2 - Good general purpose",
		},
		{
			ID:          "openrouter:qwen/qwen3.5-397b-a17b",
			Provider:    "openrouter",
			EnvKey:      "OPENROUTER_API_KEY",
			Priority:    4,
			Description: "Qwen 3.5 - Large context window",
		},
		{
			ID:          "openrouter:moonshotai/kimi-k2.5",
			Provider:    "openrouter",
			EnvKey:      "OPENROUTER_API_KEY",
			Priority:    5,
			Description: "Kimi K2.5 - Reliable for long context",
		},
		{
			ID:          "deepseek:deepseek-v3.2",
			Provider:    "deepseek",
			EnvKey:      "DEEPSEEK_API_KEY",
			Priority:    6,
			Description: "DeepSeek V3.2 - Direct API",
		},
		{
			ID:          "openai:gpt-4-turbo",
			Provider:    "openai",
			EnvKey:      "OPENAI_API_KEY",
			Priority:    7,
			Description: "GPT-4 Turbo - OpenAI's flagship",
		},
		{
			ID:          "openai:gpt-4o",
			Provider:    "openai",
			EnvKey:      "OPENAI_API_KEY",
			Priority:    8,
			Description: "GPT-4o - Optimized GPT-4",
		},
	}

	// Filter to only models with configured API keys
	available := []ModelConfig{}
	for _, model := range models {
		if os.Getenv(model.EnvKey) != "" {
			available = append(available, model)
		}
	}

	return available
}

// GetDefaultModel returns the best available model for the planner
func GetDefaultModel() (string, error) {
	available := GetAvailableModels()
	if len(available) == 0 {
		return "", fmt.Errorf("no models available: please set ANTHROPIC_API_KEY, OPENAI_API_KEY, or OPENROUTER_API_KEY")
	}
	return available[0].ID, nil
}

// GetModelForTask returns an appropriate model for a given task type
func GetModelForTask(taskType string) (string, error) {
	available := GetAvailableModels()
	if len(available) == 0 {
		return "", fmt.Errorf("no models available")
	}

	// For planner, prefer models with good reasoning
	if taskType == "planner" {
		// Prefer Anthropic models for planning
		for _, model := range available {
			if model.Provider == "anthropic" {
				return model.ID, nil
			}
		}
	}

	// Default to first available
	return available[0].ID, nil
}

// IsModelAvailable checks if a specific model can be used
func IsModelAvailable(modelID string) bool {
	available := GetAvailableModels()
	for _, model := range available {
		if model.ID == modelID {
			return true
		}
	}
	return false
}

// GetModelEnvKey returns the environment variable name for a model
func GetModelEnvKey(modelID string) string {
	// Parse provider from model ID
	parts := strings.Split(modelID, ":")
	if len(parts) < 2 {
		return ""
	}
	
	provider := parts[0]
	switch provider {
	case "anthropic":
		return "ANTHROPIC_API_KEY"
	case "openai":
		return "OPENAI_API_KEY"
	case "openrouter":
		return "OPENROUTER_API_KEY"
	case "deepseek":
		return "DEEPSEEK_API_KEY"
	default:
		return ""
	}
}