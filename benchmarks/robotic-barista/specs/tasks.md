# Tasks: Robotic Barista CLI

## Task Breakdown

Tasks are organized by implementation phase (Wave 0, Wave 1, Wave 2, Wave 3) with dependencies clearly identified.

## Wave 0: Foundation

### T0.1: Interface Contract Tests - CLI Commands
**Type**: Test  
**Layer**: Interface  
**Dependencies**: None

**Description**: Create interface contract tests for all CLI commands. Define expected input/output formats, error message formats, and exit codes.

**Test Specification**:
- Define CLI command interfaces in `contracts/cli-commands.yaml`
- Test all command argument parsing
- Test output format consistency
- Test error message formats
- Test exit codes (0 for success, non-zero for errors)

**Verification**:
- Contract file created
- Interface tests created in `tests/interfaces/cli-commands-contract.test.py`
- Tests integrated into `check.sh`

### T0.2: Interface Contract Tests - Data Schema
**Type**: Test  
**Layer**: Interface  
**Dependencies**: None

**Description**: Create interface contract tests for JSON data schema. Define expected data structure, types, and validation rules.

**Test Specification**:
- Define JSON schema in `contracts/data-schema.json`
- Test data structure validation
- Test type validation
- Test required fields

**Verification**:
- Schema file created
- Interface tests created in `tests/interfaces/data-schema-contract.test.py`
- Tests integrated into `check.sh`

### T0.3: Domain Entities
**Type**: Implementation  
**Dependencies**: [T0.1, T0.2]

**Description**: Implement domain entities: Ingredient, InventoryItem, Recipe, Order with business rules.

**Implementation**:
- Create `src/robotic_barista/domain/ingredient.py`
- Create `src/robotic_barista/domain/inventory_item.py`
- Create `src/robotic_barista/domain/recipe.py`
- Create `src/robotic_barista/domain/order.py`
- Implement size scaling logic (S=1.0x, M=1.5x, L=2.0x)
- Implement order state machine

**Verification**:
- All entities created with proper validation
- Unit tests written (TDD) and passing
- Code coverage >= 60%
- `./scripts/check.sh` passes

### T0.4: Repository Interfaces
**Type**: Implementation  
**Dependencies**: [T0.3]

**Description**: Create repository interfaces for abstraction.

**Implementation**:
- Create `src/robotic_barista/storage/repositories.py` with interfaces
- Define RecipeRepository, InventoryRepository, OrderRepository interfaces

**Verification**:
- Interfaces defined
- Unit tests written and passing
- `./scripts/check.sh` passes

### T0.5: JSON Storage Implementation
**Type**: Implementation  
**Dependencies**: [T0.4]

**Description**: Implement JSON file storage with atomic writes.

**Implementation**:
- Create `src/robotic_barista/storage/json_repository.py`
- Implement atomic write pattern (temp file → rename)
- Implement read/write operations
- Handle data directory creation

**Verification**:
- JSON storage works correctly
- Atomic writes prevent corruption
- Unit tests written and passing
- Integration tests for persistence
- `./scripts/check.sh` passes

## Wave 1: Core Functionality

### T1.1: Recipe Service
**Type**: Implementation  
**Dependencies**: [T0.5]

**Description**: Implement RecipeService for recipe management.

**Implementation**:
- Create `src/robotic_barista/services/recipe_service.py`
- Implement create, list, show operations
- Support recipe name lookup (enhancement)
- Write unit tests first (TDD)

**Verification**:
- Recipe service works correctly
- Unit tests written (TDD) and passing
- Code coverage >= 70%
- `./scripts/check.sh` passes

### T1.2: Inventory Service
**Type**: Implementation  
**Dependencies**: [T0.5]

**Description**: Implement InventoryService for inventory management.

**Implementation**:
- Create `src/robotic_barista/services/inventory_service.py`
- Implement add, list operations
- Handle ingredient creation if new
- Validate unit matching
- Write unit tests first (TDD)

**Verification**:
- Inventory service works correctly
- Unit tests written (TDD) and passing
- Code coverage >= 70%
- `./scripts/check.sh` passes

### T1.3: Recipe CLI Commands
**Type**: Implementation  
**Dependencies**: [T1.1]

**Description**: Implement CLI commands for recipe management.

**Implementation**:
- Create `src/robotic_barista/cli/recipes.py`
- Implement `recipes list`, `recipes show`, `recipes add` commands
- Support recipe names (not just IDs)
- Format output per spec
- Handle errors with clear messages

**Verification**:
- All recipe commands work
- Output format matches spec
- Error handling works correctly
- Black box tests pass
- `./scripts/check.sh` passes

### T1.4: Inventory CLI Commands
**Type**: Implementation  
**Dependencies**: [T1.2]

**Description**: Implement CLI commands for inventory management.

**Implementation**:
- Create `src/robotic_barista/cli/inventory.py`
- Implement `inventory list`, `inventory add` commands
- Format output per spec
- Handle errors (unit mismatch, etc.)

**Verification**:
- All inventory commands work
- Output format matches spec
- Error handling works correctly
- Black box tests pass
- `./scripts/check.sh` passes

### T1.5: Order Placement
**Type**: Implementation  
**Dependencies**: [T1.1]

**Description**: Implement order placement functionality.

**Implementation**:
- Create `src/robotic_barista/services/order_service.py` (partial)
- Implement order placement (PLACED state)
- Support recipe names (not just IDs)
- Write unit tests first (TDD)

**Verification**:
- Order placement works correctly
- Unit tests written (TDD) and passing
- Code coverage >= 70%
- `./scripts/check.sh` passes

### T1.6: Order Placement CLI Command
**Type**: Implementation  
**Dependencies**: [T1.5]

**Description**: Implement `order place` CLI command.

**Implementation**:
- Create `src/robotic_barista/cli/order.py` (partial)
- Implement `order place` command
- Support recipe names
- Print order ID on success

**Verification**:
- Order place command works
- Output format matches spec
- Black box tests pass
- `./scripts/check.sh` passes

## Wave 2: Order Processing

### T2.1: Order Validation
**Type**: Implementation  
**Dependencies**: [T1.5, T1.2]

**Description**: Implement order validation with inventory checks.

**Implementation**:
- Extend `OrderService` with validation logic
- Check inventory against scaled ingredient requirements
- Transition to VALIDATED or FAILED
- Provide clear error messages with missing ingredients
- Write unit tests first (TDD)

**Verification**:
- Order validation works correctly
- Size scaling applied correctly
- Error messages are clear
- Unit tests written (TDD) and passing
- Code coverage >= 80%
- `./scripts/check.sh` passes

### T2.2: Order Brewing
**Type**: Implementation  
**Dependencies**: [T2.1]

**Description**: Implement order brewing with brew plan generation and inventory consumption.

**Implementation**:
- Extend `OrderService` with brewing logic
- Generate brew plan (scaled ingredients + steps)
- Re-validate inventory before brewing
- Consume inventory atomically
- Transition to COMPLETED or FAILED
- Write unit tests first (TDD)

**Verification**:
- Order brewing works correctly
- Brew plan format matches spec
- Inventory consumed correctly
- Re-validation works
- Unit tests written (TDD) and passing
- Code coverage >= 80%
- `./scripts/check.sh` passes

### T2.3: Order Validation and Brew CLI Commands
**Type**: Implementation  
**Dependencies**: [T2.1, T2.2]

**Description**: Implement `order validate` and `order brew` CLI commands.

**Implementation**:
- Extend `src/robotic_barista/cli/order.py`
- Implement `order validate` command
- Implement `order brew` command
- Format output per spec (especially brew plan)

**Verification**:
- Order validate command works
- Order brew command works
- Brew plan format matches spec exactly
- Error handling works correctly
- Black box tests pass
- `./scripts/check.sh` passes

### T2.4: Orders List Command
**Type**: Implementation  
**Dependencies**: [T1.5]

**Description**: Implement `orders list` command with optional status filter.

**Implementation**:
- Extend `src/robotic_barista/cli/order.py`
- Implement `orders list` command
- Support `--status` filter
- Format output per spec

**Verification**:
- Orders list command works
- Status filter works correctly
- Output format matches spec
- Black box tests pass
- `./scripts/check.sh` passes

### T2.5: Integration Tests
**Type**: Test  
**Layer**: Integration  
**Dependencies**: [T2.3, T2.4]

**Description**: Create integration tests for complete workflows.

**Test Specification**:
- Test complete order workflow (place → validate → brew)
- Test persistence across operations
- Test cross-component interactions
- Test error scenarios

**Verification**:
- Integration tests created
- Tests pass
- Tests integrated into `check.sh`

### T2.6: Black Box System Tests
**Type**: Test  
**Layer**: System (Black Box)  
**Dependencies**: [T2.3, T2.4]

**Description**: Create black box system tests derived from operational specification.

**Test Specification**:
- Test all CLI commands as black box (execute via subprocess)
- Test complete user workflows end-to-end
- Test error scenarios
- Test edge cases
- Verify output format matches spec
- Verify exit codes

**Verification**:
- Black box tests created in `tests/system/black-box/cli-commands.test.py`
- Tests derived from operational specification
- Tests pass
- Tests integrated into `check.sh`

## Wave 3: Testing & Refinement

### T3.1: Comprehensive Test Coverage
**Type**: Test  
**Dependencies**: [T2.6]

**Description**: Ensure comprehensive test coverage across all layers.

**Verification**:
- Code coverage >= 80%
- All test layers have adequate coverage
- Edge cases covered
- `./scripts/check.sh` passes

### T3.2: Documentation Completion
**Type**: Documentation  
**Dependencies**: [T2.6]

**Description**: Complete all documentation.

**Verification**:
- README updated with examples
- User manual complete
- Operational specification complete
- All documentation reviewed

### T3.3: Performance and Edge Case Handling
**Type**: Implementation  
**Dependencies**: [T2.6]

**Description**: Handle remaining edge cases and optimize if needed.

**Verification**:
- All edge cases handled
- Performance acceptable
- `./scripts/check.sh` passes
