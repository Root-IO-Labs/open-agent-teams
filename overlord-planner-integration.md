# Overlord Philosophy Integration with OAT Planner

## Executive Summary

The Overlord-V1 repository contains our battle-tested methodology for spec-first, test-driven development with agent orchestration. This document maps Overlord concepts to the new OAT planner architecture and provides a complete integration plan.

## Core Overlord Principles

### 1. The Ratchet Mechanism (`check.sh` Gate)
- **One gate to rule them all**: `./scripts/check.sh` is the single source of truth
- **CI runs exact same script**: No duplication, no drift
- **Deterministic checks**: Same results locally and in CI
- **Never weaken the gate**: Fix code, not tests
- **Progressive strictness**: Gate evolves but never regresses

### 2. Spec-First Development
- **Operational specification is truth**: Not tests, not code
- **Tests validate implementation**: They don't define requirements
- **Workers implement to spec**: Not to pass tests
- **Test access restrictions**: Workers can't see test internals
- **Test arbitration protocol**: Clear escalation when tests seem wrong

### 3. Wave-Based Execution
```yaml
waves:
  wave_0_foundation:
    - Interface contracts
    - Testing infrastructure  
    - CI/CD setup
  wave_1_core:
    - Core functionality
    - Unit tests (TDD)
  wave_2_features:
    - User features
    - Integration tests
  wave_3_polish:
    - Documentation
    - Performance
```

### 4. Phase-Driven Workflow
1. **Phase 0**: Bootstrap (make repo agent-ready)
2. **Phase 1**: Brainstorm & Converge (ideas → specs)
3. **Phase 2**: Work Graph (organize into waves)
4. **Phase 3**: Emit Issues (create GitHub tickets)
5. **Phase 4**: Dispatch (spawn workers)
6. **Phase 5**: Review & Fix Loop
7. **Phase 6**: Deadlock Breakers
8. **Phase 7**: Stop Conditions
9. **Phase 8**: Controller Loop (repeat)

## Mapping to New Planner Architecture

### Current State (JSON-based planner)
```json
{
  "phase": "clarifying|draft_plan|ready_for_review",
  "message": "User-facing text",
  "questions": [],
  "requirement": {},
  "tasks": []
}
```

### Enhanced State (with Overlord concepts)
```json
{
  "phase": "clarifying|draft_plan|ready_for_review",
  "overlord_phase": "brainstorm|specify|plan|wave_organize|dispatch",
  "message": "User-facing text",
  "questions": [],
  "requirement": {
    "title": "Short title",
    "original": "User request",
    "refined": "Precise restatement",
    "operational_spec": "How system works",
    "acceptance_criteria": []
  },
  "specifications": {
    "interface": "Contract definitions",
    "blackbox": "External behavior",
    "test_strategy": "Testing approach"
  },
  "waves": [
    {
      "id": "wave_0",
      "name": "foundation",
      "tasks": []
    }
  ],
  "tasks": [
    {
      "id": "T1",
      "wave": 0,
      "type": "test|implementation",
      "tdd_required": true,
      "spec_reference": "section 3.2",
      "test_access": "restricted"
    }
  ],
  "gate": {
    "script": "./scripts/check.sh",
    "requirements": ["lint", "typecheck", "test"]
  }
}
```

## Integration Plan

### Step 1: Enhance Planner Response Structure
Update `internal/tui/views/planner_view.go`:
```go
type PlannerResponse struct {
    Phase           string              `json:"phase"`
    OverlordPhase   string              `json:"overlord_phase"`
    Message         string              `json:"message"`
    Questions       []string            `json:"questions"`
    Requirement     *PlannerRequirement `json:"requirement"`
    Specifications  *Specifications     `json:"specifications"`
    Waves           []Wave              `json:"waves"`
    Tasks           []PlannerTask       `json:"tasks"`
    Gate            *GateConfig         `json:"gate"`
}

type Specifications struct {
    Interface   string `json:"interface"`
    Blackbox    string `json:"blackbox"`
    TestStrategy string `json:"test_strategy"`
}

type Wave struct {
    ID    string        `json:"id"`
    Name  string        `json:"name"`
    Tasks []PlannerTask `json:"tasks"`
}

type GateConfig struct {
    Script       string   `json:"script"`
    Requirements []string `json:"requirements"`
}
```

### Step 2: Create Overlord-Enhanced Planner Prompt
Combine new JSON structure with Overlord philosophy:

```markdown
# Planner Agent with Overlord Philosophy

You are a planning agent following the Overlord methodology for spec-first, test-driven development.

## Core Principles

1. **Spec-First**: Create operational specifications before any implementation
2. **Test-Driven**: Define tests before code, but implement to spec
3. **Wave-Based**: Organize work into dependency waves
4. **Gate-Controlled**: Everything must pass `./scripts/check.sh`

## Response Format

Always respond with JSON including Overlord concepts:

{
  "overlord_phase": "current phase in Overlord workflow",
  "specifications": {
    "interface": "API/module contracts",
    "blackbox": "External behavior spec",
    "test_strategy": "Testing approach"
  },
  "waves": [...],
  "gate": {
    "script": "./scripts/check.sh",
    "requirements": [...]
  }
}

## Workflow Phases

### Phase 0: Bootstrap Planning
- Tech stack selection
- Testing infrastructure design
- CI/CD architecture
- Gate script requirements

### Phase 1: Brainstorm & Converge
- Transform fuzzy ideas into specs
- Create operational specification
- Define test strategy
- Establish acceptance criteria

### Phase 2: Wave Organization
- Wave 0: Foundation (contracts, CI, tests)
- Wave 1: Core (TDD implementation)
- Wave 2: Features (integration)
- Wave 3: Polish (docs, performance)

### Phase 3: Task Decomposition
- Each task atomic (one worker)
- Test tickets precede implementation
- Clear dependencies
- Spec references

## Spec-First Enforcement

Every implementation task must:
- Reference operational spec section
- Note test access restrictions
- Include TDD requirement
- Define acceptance criteria
```

### Step 3: Create Agent Override Templates
Place in `.oat/agents/` for each repo:

**planner-overlord.md**:
```markdown
MODE: ADDITIVE

# Overlord Planning Enhancement

## Additional Planning Requirements

When creating plans, enforce:

1. **Spec-First Development**
   - Create operational specs before tasks
   - Reference spec sections in every task
   - Note test access restrictions

2. **Wave-Based Organization**
   - Wave 0: Always include check.sh gate
   - Test tickets in same/earlier wave than implementation
   - Dependencies explicitly mapped

3. **Test Strategy**
   - Interface contract tests first
   - Unit tests with TDD
   - Integration tests after units
   - Black box functional tests

4. **Gate Requirements**
   - Every wave must pass check.sh
   - Gate evolves but never weakens
   - CI runs exact gate script
```

### Step 4: Create Workflow Automation Scripts

**scripts/overlord-init.sh**:
```bash
#!/usr/bin/env bash
set -euo pipefail

# Initialize repo with Overlord methodology
echo "==> Initializing Overlord workflow"

# Create check.sh gate
cat > scripts/check.sh << 'EOF'
#!/usr/bin/env bash
set -euo pipefail
echo "==> Running repo gate"
# Add checks based on stack
EOF
chmod +x scripts/check.sh

# Create CLAUDE.md with spec-first rules
cat > CLAUDE.md << 'EOF'
# Repo Rules for AI Agents

## Spec-First Development
- Operational specification is truth
- Tests validate implementation
- No test gaming
- Test arbitration process

## Non-negotiables
- Run ./scripts/check.sh before PR
- Never weaken CI
- Small, focused PRs
EOF

# Create agent overrides
mkdir -p .oat/agents
cp /path/to/templates/*.md .oat/agents/

echo "✅ Overlord workflow initialized"
```

### Step 5: Integrate with PlannerView UI

Update PlannerView to display Overlord concepts:
```go
func (p *PlannerView) renderSpecifications() string {
    if p.lastResponse.Specifications == nil {
        return ""
    }
    
    return fmt.Sprintf(`
Specifications:
├── Interface: %s
├── Blackbox: %s
└── Test Strategy: %s
`, 
        p.lastResponse.Specifications.Interface,
        p.lastResponse.Specifications.Blackbox,
        p.lastResponse.Specifications.TestStrategy,
    )
}

func (p *PlannerView) renderWaves() string {
    var output strings.Builder
    for _, wave := range p.lastResponse.Waves {
        output.WriteString(fmt.Sprintf("\n%s (%d tasks):\n", 
            wave.Name, len(wave.Tasks)))
        for _, task := range wave.Tasks {
            output.WriteString(fmt.Sprintf("  - %s [%s]\n", 
                task.Title, task.Type))
        }
    }
    return output.String()
}
```

## Implementation Checklist

### Phase 1: Core Integration
- [ ] Update PlannerResponse struct with Overlord fields
- [ ] Enhance planner prompt with Overlord methodology
- [ ] Create agent override templates
- [ ] Add wave visualization to PlannerView

### Phase 2: Workflow Automation
- [ ] Create overlord-init.sh script
- [ ] Add check.sh gate template generator
- [ ] Create CLAUDE.md template
- [ ] Add test arbitration workflow

### Phase 3: Documentation
- [ ] Document Overlord concepts in OAT
- [ ] Create examples showing workflow
- [ ] Add troubleshooting guide
- [ ] Create migration guide from old planner

### Phase 4: Testing
- [ ] Test wave-based task organization
- [ ] Verify spec-first enforcement
- [ ] Test gate mechanism
- [ ] Validate test arbitration

## Key Overlord Documents to Include

1. **Core Workflow**: OVERLORD-GREENFIELD-WORKFLOW.md
2. **Testing Strategy**: TESTING-STRATEGY.md  
3. **Spec-First Enforcement**: SPEC-FIRST-ENFORCEMENT.md
4. **Phase 0 Planning**: PHASE-0-PLANNING.md
5. **Agent Prompts**: AGENT-PROMPTS/*.md

## Success Metrics

- Plans include operational specifications
- Tasks organized into logical waves
- Test tickets precede implementation
- check.sh gate prevents regressions
- Workers implement to spec, not tests
- Test arbitration handles conflicts
- System delivers value, not just green tests

## Conclusion

The Overlord methodology provides battle-tested patterns for agent-driven development. By integrating these concepts into the new OAT planner:

1. **Better planning**: Specs before code
2. **Better execution**: Wave-based parallelism
3. **Better quality**: Gate-controlled progress
4. **Better results**: Value delivery, not test gaming

The key is maintaining Overlord's rigorous philosophy while leveraging OAT's improved architecture.