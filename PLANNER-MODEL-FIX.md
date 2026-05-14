# Planner Model Configuration Fix

## Problem
The planner agent fails with "The api_key client option must be set" because it's trying to use a model (like openrouter:deepseek) when the API key isn't set.

## Root Cause
The planner agent is being spawned with a hardcoded model that may not have its API key configured in the environment.

## Solution

### 1. Immediate Fix - Use Available Model
When spawning the planner agent, OAT should:
1. Check which API keys are available (ANTHROPIC_API_KEY, OPENAI_API_KEY, etc.)
2. Use the first available model
3. Let users override with `--model` flag

### 2. Implementation Steps

#### Step 1: Update Planner Agent Spawn
The planner agent should be spawned with an available model:

```bash
# Check available models
if [ -n "$ANTHROPIC_API_KEY" ]; then
    MODEL="anthropic:claude-sonnet-4-6"
elif [ -n "$OPENAI_API_KEY" ]; then
    MODEL="openai:gpt-4-turbo"
elif [ -n "$OPENROUTER_API_KEY" ]; then
    MODEL="openrouter:deepseek/deepseek-v3.2"
else
    echo "Error: No API keys found. Set ANTHROPIC_API_KEY, OPENAI_API_KEY, or OPENROUTER_API_KEY"
    exit 1
fi

# Spawn planner with available model
oat agent spawn planner --model $MODEL
```

#### Step 2: Add Model Selection to TUI
When starting the planner TUI, allow model selection:

```bash
# With explicit model
oat tui planner --model anthropic:claude-sonnet-4-6

# Auto-detect (default)
oat tui planner
```

#### Step 3: Update Daemon Agent Spawning
The daemon should use the routing system to select an appropriate model:

```go
// In daemon agent spawn logic
func spawnPlannerAgent(repo string, modelOverride string) error {
    model := modelOverride
    if model == "" {
        // Use OAT's routing system
        decision, err := routing.RouteForTask(routing.RouteContext{
            Role: routing.RolePlanner,
            TaskText: "Planning and task decomposition",
        })
        if err != nil {
            return fmt.Errorf("no available models: %w", err)
        }
        model = decision.ChosenModel
    }
    
    // Spawn with selected model
    return spawnAgent("planner", model, plannerPrompt)
}
```

### 3. User Experience

#### For Users with Multiple API Keys
```bash
# List available models
$ oat model list
Available models:
- anthropic:claude-sonnet-4-6 (ANTHROPIC_API_KEY set)
- openai:gpt-4-turbo (OPENAI_API_KEY set)

# Start planner with specific model
$ oat tui planner --model anthropic:claude-sonnet-4-6

# Or let OAT choose
$ oat tui planner  # Uses first available
```

#### For Users with Single API Key
```bash
# Automatically uses the available model
$ oat tui planner
Using model: anthropic:claude-sonnet-4-6
```

### 4. Environment Variables Priority
When auto-selecting, prefer in this order:
1. ANTHROPIC_API_KEY → anthropic:claude-sonnet-4-6
2. OPENAI_API_KEY → openai:gpt-4-turbo  
3. OPENROUTER_API_KEY → openrouter:deepseek/deepseek-v3.2
4. DEEPSEEK_API_KEY → deepseek:deepseek-v3.2

### 5. Configuration File Support
Users can set default model in `~/.oat/config.toml`:

```toml
[planner]
default_model = "anthropic:claude-sonnet-4-6"

# Or use auto-detection
default_model = "auto"
```

## Testing

1. **Test with no API keys**: Should show helpful error
2. **Test with single API key**: Should auto-select that model
3. **Test with multiple API keys**: Should use priority order or user selection
4. **Test with explicit --model**: Should use specified model if available

## Benefits

1. **No separate setup**: Planner uses same models as rest of OAT
2. **Flexibility**: Users can choose which model to use
3. **Fallback**: Works with any available API key
4. **Consistent**: Uses OAT's existing routing system