# Testing Strategy: Robotic Barista CLI

This document defines the comprehensive testing philosophy and methodology for the Robotic Barista CLI project, following the OAT multi-agent workflow testing strategy.

## Core Principles

### 1. Test-Driven Development (TDD) with Spec-First Implementation

**Every piece of work must follow TDD, but implementation must be spec-first:**

- Tests are written **before** code
- Tests define the expected behavior
- **Code is written to implement the operational specification** (primary goal)
- Tests validate that the implementation matches the specification
- **Tests are validation tools, not implementation guides**

**Critical Rule**: Workers must **NOT** have access to test implementation code. They implement based on:
- Operational specification
- User manual
- Interface contracts (specifications, not test code)
- Test results (pass/fail, error messages)
- Test specifications (what behavior is tested)

**Why**: This prevents "gaming the test" and ensures the system embodies intended functionality, not just test-passing code.

### 2. Tests Define the Target

The testing philosophy requires a clear target. **Tests are that target.**

- Agents work to make tests pass
- Tests define what "correct" means
- As tests become more comprehensive, the system becomes more correct
- The system self-organizes to meet the test requirements

### 3. Cumulative Test Suite

**Once a test is added, it stays forever.**

- Tests accumulate across waves
- No test is ever removed (unless the feature is removed)
- Each wave adds new tests without removing old ones
- Full regression ensures nothing breaks

**Why**: This ensures progress is permanent. We never regress. Progress only moves forward.

### 4. Wave-Aware Testing

**Tests are organized by waves, but execution is cumulative:**

- **Wave 0 (Foundation)**: Interface contract tests
- **Wave 1 (Core)**: Interface + unit tests
- **Wave 2 (Features)**: Interface + unit + integration tests
- **System-level**: Interface + unit + integration + black box functional tests

**Execution**: All tests that exist must pass, regardless of which wave is being worked on.

### 5. Progressive Strictness

**Test thresholds become stricter over time, but tests themselves remain:**

- Coverage requirements increase (Wave 0: 60%, Wave 1: 70%, Wave 2: 80%)
- Performance thresholds tighten
- Linting rules become stricter
- Tests are never weakened to "get green"

**Why**: The system improves over time, but the bar never lowers.

## Testing Layers

### Layer 1: Interface Contract Tests (Wave 0)

**Purpose**: Define and verify interface contracts between modules.

**When**: Created in **Wave 0** as separate test tickets that precede implementation.

**What they verify**:
- CLI command interfaces (input/output formats, argument parsing)
- Error message formats and exit codes
- Data structure contracts (JSON schema for persistence)
- Service interfaces (recipe service, inventory service, order service)

**Implementation**:
- Contract specifications in `contracts/cli-commands.yaml` (CLI interface contracts)
- Contract specifications in `contracts/data-schema.json` (JSON data structure)
- Contract specifications in `contracts/service-interfaces.yaml` (internal service contracts)
- Tests in `tests/interfaces/` verify contracts are adhered to
- Contracts are version-controlled and evolve with the system

**Example Tests**:
- `tests/interfaces/cli-commands-contract.test.py`: Tests that CLI commands match the contract
- `tests/interfaces/data-schema-contract.test.py`: Tests that JSON data matches the schema
- `tests/interfaces/service-interfaces-contract.test.py`: Tests that services match their interfaces

**Access Restrictions**: Test implementation code is NOT accessible to implementation workers. Workers only see contract specifications and test results.

### Layer 2: Unit Tests (Wave 1)

**Purpose**: Verify individual components work correctly in isolation.

**When**: Created in **Wave 1** as part of implementation tickets (TDD).

**What they verify**:
- Domain entities: Size scaling logic (S=1.0x, M=1.5x, L=2.0x), order state machine transitions
- Domain rules: Inventory validation, order lifecycle rules, inventory consumption
- Services: RecipeService, InventoryService, OrderService logic
- Repositories: JSON persistence layer (read/write, atomic operations)

**Implementation**:
- Written as part of implementation (tests first, then code)
- Focus on single components/modules
- Fast execution (< 1 second per test)
- High coverage of component logic
- Located in `tests/unit/domain/`, `tests/unit/services/`, `tests/unit/storage/`

**Spec-First Approach**: Workers write unit tests first (TDD), but implement to match the operational specification, not to pass tests. Tests validate that implementation matches the spec.

**Example Tests**:
- `tests/unit/domain/test_size_scaling.py`: Test size scaling multipliers
- `tests/unit/domain/test_order_state_machine.py`: Test order state transitions
- `tests/unit/services/test_recipe_service.py`: Test recipe service operations
- `tests/unit/storage/test_json_repository.py`: Test JSON persistence

### Layer 3: Integration/Subassembly Tests (Wave 2+)

**Purpose**: Verify components work together correctly.

**When**: Created in **Wave 2+** as integration tasks.

**What they verify**:
- Recipe creation → inventory management → order processing workflow
- Service interactions: OrderService using RecipeService and InventoryService
- Persistence integration: Services using repositories, data persistence across operations
- Cross-component data flow: Order placement → validation → brewing → inventory consumption

**Implementation**:
- Test multiple components together
- Verify integration contracts
- Test real data flows
- Use actual JSON file storage (not mocks)
- Located in `tests/integration/`

**Example Tests**:
- `tests/integration/test_order_workflow.py`: Full order lifecycle (place → validate → brew)
- `tests/integration/test_persistence.py`: Data persists across operations and app restarts
- `tests/integration/test_inventory_recipe_order.py`: Recipe creation, inventory management, order processing together

### Layer 4: Black Box Functional System Tests (Ongoing, Critical)

**Purpose**: Verify the system works correctly from an external perspective.

**When**: Created based on operational specification, ongoing and cumulative.

**What they verify**:
- System produces correct input-output relationships
- System meets operational specification
- User workflows function correctly
- System observables match expectations

**Implementation**:
- Based on operational specification/user manual
- Test system as a black box (execute actual CLI commands via subprocess)
- Use realistic inputs and verify outputs
- Located in `tests/system/black-box/cli-commands.test.py`
- Test all CLI commands as specified in the user manual
- Verify complete user workflows end-to-end
- Test error scenarios produce correct messages and exit codes
- Test edge cases and error conditions

**Critical Relationship**: Black box tests are **derived from the operational specification**. They validate that the system works as specified, not that it matches implementation details.

**Example Black Box Tests**:
- Complete workflow: `inventory add` → `recipes add` → `order place` → `order validate` → `order brew` → verify inventory consumed
- Error scenarios: Invalid recipe name, insufficient inventory, unit mismatch, invalid state transitions
- Edge cases: Empty inventory, duplicate recipe names, inventory change after validation
- Output format: Verify brew plan format matches spec example exactly
- Exit codes: Verify success (0) and error (non-zero) codes

**Test Battery**: For this deterministic CLI system, collect real-world scenarios:
- Common order workflows (latte, cappuccino, espresso)
- Edge cases (minimum inventory, maximum sizes, invalid inputs)
- Error conditions (missing ingredients, invalid states, unit mismatches)

## Directory Structure

```
tests/
  interfaces/                    # Layer 1: Interface contract tests (Wave 0)
    cli-commands-contract.test.py
    data-schema-contract.test.py
    service-interfaces-contract.test.py
  
  unit/                          # Layer 2: Unit tests (Wave 1)
    domain/
      test_size_scaling.py
      test_order_state_machine.py
      test_inventory_validation.py
    services/
      test_recipe_service.py
      test_inventory_service.py
      test_order_service.py
    storage/
      test_json_repository.py
  
  integration/                   # Layer 3: Integration tests (Wave 2+)
    test_order_workflow.py
    test_persistence.py
    test_inventory_recipe_order.py
  
  system/                        # Layer 4: Black box system tests (Ongoing)
    black-box/
      cli-commands.test.py        # All CLI commands as black box
      user-workflows.test.py      # Complete user scenarios
    test-battery/                 # Real-world test cases
      fixtures/
        real-world-cases/
          common-orders.yaml
          edge-cases.yaml
        error-scenarios.yaml
```

## Test Access and Separation of Concerns

### What Workers Can Access

**Workers implementing features have access to**:
- ✅ **Operational specification** (what the system should do)
- ✅ **User manual** (how it should work from user perspective)
- ✅ **Interface contracts** (expected inputs/outputs, error conditions - as specifications, not test code)
- ✅ **Test results** (pass/fail status, error messages from test runs)
- ✅ **Test specifications** (what behavior is being tested, acceptance criteria)
- ✅ **Test tickets** (the requirements for what tests should validate)

### What Workers Cannot Access

**Workers implementing features must NOT have access to**:
- ❌ **Test implementation code** (the actual test files: `*.test.py`, etc.)
- ❌ **Test internals** (how tests are structured, test helper functions)
- ❌ **Test fixtures/data** (unless needed for understanding requirements)
- ❌ **Test implementation details** (mocking strategies, test setup code)

**Why**: Prevents test gaming. Workers implement to the operational specification. Tests validate compliance.

## Test Failure Arbitration Protocol

### Level 1: Worker (Normal Case)

**Scenario**: Test fails, code doesn't match spec

**Assumption**: Test is correct, code is wrong

**Action**:
1. Worker reviews operational specification
2. Worker fixes implementation to match spec
3. Worker re-runs tests
4. If tests pass → Done ✅

**No escalation needed** - this is the normal TDD cycle.

### Level 2: Supervisor (Suspected Test Issue)

**Scenario**: Worker believes test doesn't match operational specification

**Trigger**: Worker creates blocker issue with label `blocker:test-arbitration`

**Issue format**:
```markdown
## Test Arbitration Request

**Test**: [Test name/ID]
**Failing assertion**: [What the test expects]
**Operational spec reference**: [Section of spec that defines behavior]
**Discrepancy**: [Why test doesn't match spec]

**Proposed resolution**: [Fix test / Update spec / Fix code]
```

**Supervisor review process**:
1. Compare test to operational specification
2. Check if spec changed but test didn't update
3. Review test quality (is it testing the right thing?)
4. Check if spec is ambiguous

**Supervisor decision matrix**:
- ✅ **Test matches spec, code doesn't** → Fix code (create ticket)
- ✅ **Test doesn't match spec** → Fix test (create ticket: "Fix test: [description]")
- ✅ **Spec is ambiguous** → Escalate to Overlord
- ✅ **Test is poorly written** → Create ticket: "Improve test: [description]"

### Level 3: Overlord (Complex Cases)

**Scenario**: Supervisor uncertain, conflicting requirements, or spec needs update

**Trigger**: Supervisor escalates to Overlord with context

**Overlord review**:
1. Review operational spec vs test vs implementation
2. Determine root cause:
   - Spec needs clarification → Update operational spec
   - Test needs update → Create test fix ticket
   - Implementation needs fix → Create implementation ticket
3. May need to update multiple artifacts in order:
   - Spec → Tests → Code (if spec changed)
   - Tests → Code (if test was wrong)
   - Code (if implementation was wrong)

## CI Evolution Strategy

### Phase 1: Naive Full Regression (Waves 0-2)

**Implementation**: `check.sh` runs all tests, always.

```bash
#!/usr/bin/env bash
set -euo pipefail

echo "==> Running full regression suite"

# Run all tests that exist (cumulative)
[ -d "tests/interfaces" ] && python -m pytest tests/interfaces/ -v
[ -d "tests/unit" ] && python -m pytest tests/unit/ -v
[ -d "tests/integration" ] && python -m pytest tests/integration/ -v
[ -d "tests/system" ] && python -m pytest tests/system/ -v
```

**Why**: Simple, safe, establishes the baseline. No complexity, no missed regressions.

### Phase 2: Wave-Aware Execution (Wave 3+)

**Implementation**: `check.sh` detects wave context and runs relevant tests, but always includes full regression on main branch.

**Evolution via tickets**: Created as needed when system matures.

### Phase 3: Smart Incremental (Mature System)

**Implementation**: Test dependency graph + change detection.

**Evolution via tickets**: Created as needed for performance optimization.

## Progressive Strictness Thresholds

### Coverage Requirements

| Wave | Minimum Coverage | Target Coverage |
|------|-----------------|----------------|
| Wave 0 | 50% | 60% |
| Wave 1 | 60% | 70% |
| Wave 2 | 70% | 80% |
| Wave 3+ | 80% | 90% |

### Implementation in `check.sh`

```bash
# Progressive coverage thresholds
WAVE="${WAVE:-0}"
case "$WAVE" in
  "0")
    COVERAGE_THRESHOLD=60
    ;;
  "1")
    COVERAGE_THRESHOLD=70
    ;;
  "2")
    COVERAGE_THRESHOLD=80
    ;;
  *)
    COVERAGE_THRESHOLD=80
    ;;
esac

python -m pytest tests/ --cov=src --cov-report=term-missing --cov-fail-under=$COVERAGE_THRESHOLD
```

## Definition of Done

### For Test Tickets

- [ ] Test file created and executable
- [ ] Tests cover interface contract (types, shapes, errors)
- [ ] Tests are documented (what they verify, why)
- [ ] Tests pass (may be failing initially if TDD)
- [ ] Tests are integrated into `check.sh`
- [ ] Test implementation is NOT exposed to implementation workers

### For Implementation Tickets

- [ ] **Implementation matches operational specification** (PRIMARY)
- [ ] Unit tests written (TDD) and passing
- [ ] All interface contract tests pass
- [ ] Code coverage meets wave threshold
- [ ] `./scripts/check.sh` passes
- [ ] Integration tests pass (if applicable)
- [ ] Black box system tests pass (if applicable)
- [ ] Review verifies implementation delivers value per spec, not just passes tests

### For Black Box System Test Tickets

- [ ] Black box tests created from operational spec
- [ ] Tests verify user workflows
- [ ] Tests use realistic inputs
- [ ] Tests execute actual CLI commands
- [ ] Tests verify output format matches spec
- [ ] Tests verify exit codes
- [ ] Tests pass
- [ ] Tests integrated into `check.sh`

## Summary: The Testing Strategy

The testing strategy creates a **testing mechanism** that ensures provable correctness:

1. **Tests define the target** - Agents work to make tests pass
2. **Tests are cumulative** - Once added, they stay forever
3. **Tests are wave-aware** - Organized by layer, executed cumulatively
4. **Tests get stricter** - Thresholds increase, tests never weaken
5. **Tests are comprehensive** - Interface → Unit → Integration → System
6. **Tests are traceable** - Linked to specifications, contracts, and requirements

**Result**: The system self-organizes to meet test requirements, and progress only moves forward. Progress is permanent. Correctness is provable.
