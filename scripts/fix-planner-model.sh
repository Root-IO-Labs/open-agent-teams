#!/bin/bash
# Quick fix to ensure planner agent uses an available model

# Function to detect available model
detect_model() {
    if [ -n "$ANTHROPIC_API_KEY" ]; then
        echo "anthropic:claude-sonnet-4-6"
    elif [ -n "$OPENAI_API_KEY" ]; then
        echo "openai:gpt-4-turbo"
    elif [ -n "$OPENROUTER_API_KEY" ]; then
        echo "openrouter:deepseek/deepseek-v3.2"
    elif [ -n "$DEEPSEEK_API_KEY" ]; then
        echo "deepseek:deepseek-v3.2"
    else
        echo ""
    fi
}

# Get the model
MODEL=$(detect_model)

if [ -z "$MODEL" ]; then
    echo "Error: No API keys found in environment"
    echo "Please set one of: ANTHROPIC_API_KEY, OPENAI_API_KEY, OPENROUTER_API_KEY, DEEPSEEK_API_KEY"
    exit 1
fi

echo "Using model: $MODEL"

# Check if planner agent exists
if oat agent list | grep -q "planner"; then
    echo "Planner agent already exists"
    
    # Kill existing planner if it's using wrong model
    echo "Restarting planner with correct model..."
    oat agent kill planner 2>/dev/null || true
    sleep 1
fi

# Spawn planner with detected model
echo "Spawning planner agent with $MODEL..."
oat agent spawn planner \
    --model "$MODEL" \
    --prompt "$(cat internal/prompts/planner.md)" \
    --persistent

echo "Planner agent ready with model: $MODEL"