# Specification: Robotic Barista CLI

## Goal

Build a command-line application that simulates an automated coffee barista. Users can:
- Define recipes (drink name + ingredients + steps)
- Place drink orders
- Track inventory (ingredients on hand)
- Produce a brew plan (what the machine will do) or fail with clear reasons

The system does not control real hardware. It only models behavior and state.

## Functional Requirements

### FR1: Recipe Management
- **FR1.1**: Users can create recipes with name, ingredients (with quantities and units), and ordered steps
- **FR1.2**: Users can list all recipes (id + name)
- **FR1.3**: Users can view a recipe's full details (ingredients, steps, base quantities)
- **FR1.4**: Recipes must have at least 1 ingredient and 1 step

### FR2: Inventory Management
- **FR2.1**: Users can add ingredients to inventory (creates ingredient if new)
- **FR2.2**: Users can list all inventory items (ingredient + quantity)
- **FR2.3**: Units must match existing ingredient unit if ingredient exists
- **FR2.4**: Inventory persists across runs

### FR3: Order Processing
- **FR3.1**: Users can place orders (recipe + size: S/M/L)
- **FR3.2**: Orders start in PLACED state
- **FR3.3**: Users can validate orders (checks inventory, transitions to VALIDATED or FAILED)
- **FR3.4**: Users can brew validated orders (produces brew plan, consumes inventory, transitions to COMPLETED)
- **FR3.5**: Users can list orders (with optional status filter)
- **FR3.6**: Order lifecycle: PLACED → VALIDATED → BREWING → COMPLETED (or FAILED at any validation step)

### FR4: Size Scaling
- **FR4.1**: Size S = 1.0x base quantities
- **FR4.2**: Size M = 1.5x base quantities
- **FR4.3**: Size L = 2.0x base quantities

### FR5: Brew Planning
- **FR5.1**: Brew plan shows order details (id, recipe name, size)
- **FR5.2**: Brew plan shows scaled ingredient usage
- **FR5.3**: Brew plan shows ordered steps
- **FR5.4**: Brew plan format is human-readable and consistent

### FR6: Inventory Rules
- **FR6.1**: Inventory must be checked before brewing
- **FR6.2**: Brewing consumes inventory
- **FR6.3**: If inventory is insufficient, order fails and does NOT consume inventory
- **FR6.4**: If inventory changes after validation but before brew, brew fails without consuming

## Non-Functional Requirements

### NFR1: Persistence
- All state (inventory, recipes, orders) must persist across runs
- Use JSON or SQLite file in local data directory
- In-memory storage is not sufficient

### NFR2: Architecture
- Code must be modular (separate domain logic from CLI parsing and persistence)
- Suggested structure: domain/, services/, storage/, cli/

### NFR3: Concurrency Safety
- Code should not make concurrency impossible
- Avoid global mutable state that would break ordering
- Design for future concurrent operations

### NFR4: Error Handling
- Errors must print clear messages
- Errors exit with non-zero code
- Success returns zero

### NFR5: Testing
- Include test suite covering:
  - Size scaling
  - Validation fails with correct missing items
  - Brew consumes inventory exactly once
  - Brew fails if inventory changed after validation

## User Stories

### US1: Recipe Creation
**As a** barista operator  
**I want** to create recipes with ingredients and steps  
**So that** I can define drink offerings

### US2: Inventory Management
**As a** barista operator  
**I want** to add and track ingredients  
**So that** I know what's available for brewing

### US3: Order Placement
**As a** customer  
**I want** to place an order for a drink in a specific size  
**So that** I can get my coffee

### US4: Order Validation
**As a** barista operator  
**I want** to validate orders against inventory  
**So that** I know if I can fulfill them

### US5: Order Brewing
**As a** barista operator  
**I want** to brew validated orders  
**So that** I can produce drinks and track inventory consumption

## Acceptance Criteria

A submission is considered complete when:
- ✅ All commands work as described
- ✅ Persistence works across app restarts
- ✅ Order lifecycle and inventory rules are enforced
- ✅ Tests pass and cover the core behaviors
- ✅ Reasonable error handling and input validation exist

## Out of Scope (Future)

- `inventory set` command (overwrite quantity)
- Perishable ingredients with expiry dates
- `order cancel` command
- Multi-user support
- Network/remote operations
