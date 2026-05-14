# OAT Planner Agent Documentation

## Overview

The OAT Planner Agent is a persistent, interactive agent that follows the **Overlord spec-first, test-driven methodology**. It transforms vague user requirements into executable, wave-based work plans with tremendous attention to testing criteria. This document describes the latest implementation on the `feature/planner-agent` branch.

## Architecture

### Agent Type
- **Type**: `planner` (persistent agent)
- **Location**: Shows at top of agent panel in TUI
- **Interaction**: Modal-based UI overlay (PlannerView)
- **Purpose**: Planning-only agent that creates specs and tasks but never implements

### Key Components

1. **PlannerView** (`/internal/tui/views/planner_view.go`)
   - 868+ line interactive TUI component
   - Handles user input and JSON response parsing
   - State machine with proper phase transitions
   - Modal overlay on main TUI

2. **Planner Prompt** (`/internal/prompts/planner.md`)
   - Enhanced with Overlord methodology
   - Multi-phase planning process
   - Test-first, spec-first approach
   - Wave-based task organization

3. **State Management** (`/internal/state/state.go`)
   - `AgentTypePlanner` defined as persistent agent
   - Proper initialization in workspace setup

## Multi-Phase Planning Process

### Phase 1: Requirements Evolution (`clarifying`)
- Transforms fuzzy ideas into clear requirements
- Asks targeted questions (max 3 per turn)
- Gathers:
  - What needs to be built and why
  - Success criteria
  - Technical constraints
  - Testing requirements
  - Out-of-scope items

### Phase 2: Architecture & Design (`architecture`)
- Creates system design once requirements clear
- Produces:
  - Operational specification (how system works)
  - Interface contracts
  - Test strategy (unit, integration, blackbox)
  - Gate mechanism (`./scripts/check.sh`)

### Phase 3: Implementation Planning (`draft_plan`)
- Creates task breakdown with test-first approach
- Organizes into waves:
  - Wave 0: Foundation (tests, CI, contracts)
  - Wave 1: Core (TDD implementation)
  - Wave 2: Features (integration)
  - Wave 3: Polish (documentation, performance)
- Every task includes spec reference

### Phase 4: Ticket Generation (`ready_for_review`)
- Finalizes plan with test tickets
- Ready for user approval
- Generates:
  - Test tickets before implementation tickets
  - Spec-first guidance in every ticket
  - Test access restrictions for workers
  - Complete work graph for dispatch

## JSON Response Format

The planner always responds with structured JSON:

```json
{
  "phase": "clarifying|architecture|draft_plan|ready_for_review",
  "message": "User-facing text displayed in chat",
  "questions": ["Question 1?", "Question 2?", "Question 3?"],
  "requirement": {
    "title": "Short descriptive title",
    "original": "Original user request verbatim",
    "refined": "Precise, unambiguous restatement",
    "operational_spec": "How the system works (architecture phase)"
  },
  "test_strategy": {
    "unit": "Unit testing approach",
    "integration": "Integration testing approach",
    "blackbox": "User-facing testing approach",
    "gate_script": "./scripts/check.sh requirements"
  },
  "tasks": [
    {
      "id": "T1",
      "title": "Brief task title",
      "description": "What worker must produce",
      "type": "test|implementation|documentation",
      "wave": 0,
      "dependencies": [],
      "spec_reference": "Section of operational spec",
      "test_first": true,
      "acceptance_criteria": ["Criterion 1", "Criterion 2"]
    }
  ]
}
```

### Field Descriptions

- **phase**: Current planning phase (progresses linearly)
- **message**: Human-readable text shown to user
- **questions**: Only present in `clarifying` phase
- **requirement**: Evolves through phases, contains operational spec
- **test_strategy**: Defined in `architecture` phase
- **tasks**: Present in `draft_plan` and `ready_for_review`
  - **type**: `test` tasks must precede `implementation`
  - **wave**: 0-3, with Wave 0 always containing tests and CI
  - **test_first**: True for all TDD implementation tasks
  - **spec_reference**: Links to operational spec section

## Wave-Based Execution

### Wave Structure

| Wave | Name | Purpose | Requirements |
|------|------|---------|--------------|
| 0 | Foundation | Test specs, CI/CD, contracts, gate | ALL test specifications |
| 1 | Core | Primary business logic | Unit tests from Wave 0 |
| 2 | Features | Secondary features | Integration tests |
| 3 | Polish | Docs, optimization | Blackbox tests |

### Critical Wave Rules

1. **Wave 0 ALWAYS includes**:
   - `./scripts/check.sh` gate script
   - All test specifications
   - CI/CD setup
   - Interface contracts

2. **Test-First Enforcement**:
   - Test tickets in Wave N
   - Implementation in Wave N+1 (or same wave if independent)
   - No implementation without test spec

3. **The Ratchet Mechanism**:
   - `check.sh` is the single gate
   - Gate evolves but never weakens
   - CI runs exact same script
   - Green = mergeable, Red = blocked

## Overlord Philosophy Integration

### Spec-First Development
- Operational specification is source of truth
- Tests validate implementation
- Workers implement to spec, not to pass tests
- Test access restricted for workers

### Test-Driven Development
- Test specifications created before implementation
- Unit tests guide development (TDD)
- Integration tests validate features
- Blackbox tests confirm user experience

### Test Arbitration Protocol
- Workers can't see test implementation code
- If test seems wrong, worker creates `blocker:test-arbitration` issue
- Supervisor compares test to spec
- Decision: fix test, fix code, or escalate

## User Interaction Flow

1. **User initiates planning**:
   - Clicks planner in agent panel
   - Modal overlay appears
   - Enters requirements

2. **Planner clarifies** (Phase 1):
   - Asks up to 3 questions per turn
   - User provides answers
   - Continues until clear

3. **Planner architects** (Phase 2):
   - Creates operational spec
   - Defines test strategy
   - User reviews design

4. **Planner plans** (Phase 3):
   - Creates wave-based tasks
   - Test tasks before implementation
   - Shows task dependencies

5. **User approves** (Phase 4):
   - Reviews complete plan
   - Types "approve" or uses hotkey
   - Plan dispatched to workspace

## Plan Storage (Planned Feature)

Per meeting requirements, plans will be stored in a separate version-controlled repository:

### Storage Structure
```
plans/
├── 2024-05-13-json-csv-converter/
│   ├── requirements.md
│   ├── operational-spec.md
│   ├── test-strategy.md
│   ├── workgraph.yml
│   └── approval.json
```

### Benefits
- Version control for all plans
- Central PM record
- Traceability
- Plan evolution tracking

## Integration Points

### With Workspace Agent
- Workspace receives approved plan
- Manages shared context across agents
- Spawns workers for tasks
- Tracks wave completion

### With Supervisor Agent
- Supervisor monitors task execution
- Handles test arbitration
- Reports blockers to planner
- Validates spec compliance

### With Workers
- Workers receive atomic tasks
- Implement to spec (not tests)
- No access to test code
- Create blocker issues if stuck

## Configuration

### Enable Planner
The planner is automatically initialized when creating a new OAT workspace:

```bash
oat init <repo-name>
```

### Custom Prompts
Place custom planner prompts in:
```
.oat/agents/planner.md
```

### Plan Storage Repository
Configure in workspace settings:
```json
{
  "planner": {
    "storage_repo": "github.com/org/project-plans"
  }
}
```

## Testing the Planner

### Manual Testing
1. Start OAT: `oat start`
2. Click planner in agent panel
3. Enter requirement: "Build a REST API for user management"
4. Answer clarifying questions
5. Review architecture phase
6. Verify Wave 0 contains test specs
7. Check task dependencies
8. Approve plan

### Automated Testing
```bash
# Send test message to planner
oat message planner "Create a todo app with authentication"

# Check planner response
oat agent status planner
```

## Common Patterns

### Pattern 1: Simple CRUD Application
- Wave 0: Database schema tests, API contracts, CI
- Wave 1: Model implementation, basic CRUD
- Wave 2: Authentication, authorization
- Wave 3: UI, documentation

### Pattern 2: CLI Tool
- Wave 0: Interface tests, argument parsing tests
- Wave 1: Core logic implementation
- Wave 2: File I/O, error handling
- Wave 3: Help docs, examples

### Pattern 3: Library/Package
- Wave 0: Public API tests, type definitions
- Wave 1: Core functionality
- Wave 2: Helper functions, utilities
- Wave 3: Examples, benchmarks

## Troubleshooting

### Planner Not Visible
- Check `agentTypePriority` in `/internal/tui/app.go`
- Verify `isPrimaryAgent` returns true for planner
- Ensure planner initialized in workspace

### JSON Parse Errors
- Check planner prompt for proper JSON format
- Verify all required fields present
- Look for trailing commas or syntax errors

### Phase Transitions
- Planner must progress through phases in order
- Can't skip from `clarifying` to `ready_for_review`
- User can request return to earlier phase

## Best Practices

1. **Always start with Wave 0 tests**
   - Define all test specifications first
   - Include CI/CD setup
   - Create gate script

2. **Reference operational spec**
   - Every implementation task needs spec reference
   - Workers implement to spec, not tests
   - Spec is single source of truth

3. **Enforce test-first**
   - No implementation without test spec
   - Tests define behavior
   - Implementation satisfies tests

4. **Use proper wave organization**
   - Dependencies determine waves
   - Parallel execution within waves
   - Sequential execution across waves

5. **Include acceptance criteria**
   - Measurable, not subjective
   - Verifiable by tests
   - Clear success definition

## Future Enhancements

1. **Plan Storage Implementation**
   - Git repository for plans
   - Automatic versioning
   - Plan comparison tools

2. **Enhanced UI Features**
   - Visual wave diagram
   - Dependency graph
   - Progress tracking

3. **AI Improvements**
   - Learn from previous plans
   - Suggest common patterns
   - Auto-detect project type

4. **Integration Features**
   - Direct GitHub issue creation
   - Jira ticket export
   - Slack notifications

## Summary

The OAT Planner Agent represents a significant advancement in AI-assisted project planning. By combining:
- Overlord's spec-first methodology
- Test-driven development principles
- Wave-based task organization
- Interactive modal UI
- Structured JSON responses

It transforms vague ideas into executable, testable, high-quality software projects. The planner ensures that every project starts with a solid foundation of tests and specifications, leading to more reliable and maintainable code.

## References

- [Overlord-V1 Repository](https://github.com/johnnyrootio/Overlord-V1)
- [Meeting Notes](May 11, 2026 sync)
- [OAT Documentation](https://github.com/Root-IO-Labs/open-agent-teams)
- [Planner Prompt](/internal/prompts/planner.md)
- [PlannerView Implementation](/internal/tui/views/planner_view.go)