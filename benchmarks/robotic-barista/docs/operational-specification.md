# Operational Specification: Robotic Barista CLI

## Overview

This document describes how the Robotic Barista CLI system works from an operational perspective. It defines commands, interfaces, user workflows, and expected behaviors. This specification is the **source of truth** for implementation and testing.



## Development Environment

Always use a Python virtual environment for development:

```bash
python3 -m venv .venv
source .venv/bin/activate
pip install -e ".[dev]"
```

This avoids `externally-managed-environment` errors on modern Python installations.

## System Architecture

The system is a command-line application that simulates an automated coffee barista. It models behavior and state but does not control real hardware.

**Components**:
- **CLI Interface**: Command parsing and output formatting
- **Services**: Business logic orchestration (Recipe, Inventory, Order services)
- **Domain**: Business rules and entities
- **Storage**: JSON file persistence (`barista.json`)



**Installation & Entry Point**: After `pip install -e .`, the `barista` command must be available system-wide via the terminal. Configure a `[project.scripts]` entry in `pyproject.toml` (e.g., `barista = "robotic_barista.cli:cli"`). The application must also be runnable via `python -m robotic_barista.cli`.

## CLI Commands

**Data Path**: All commands that read or write persistent data (inventory, recipes, orders) MUST resolve the data file as `barista.json` in the `BARISTA_DATA_DIR` directory. See Persistence section for details.

**Architecture note:** All command groups (`inventory`, `recipes`, `order`, `orders`) must be registered in the CLI entry point module referenced by `[project.scripts]`. They share a single Click group. Implementing command groups in separate PRs requires that each PR registers all groups (or the entry point already imports them) to avoid CI failures from missing commands.


### Inventory Commands

#### `inventory list`
**Purpose**: Display all ingredients and their current quantities.

**Usage**: `barista inventory list`

**Output Format**:
```
espresso: 500ml
milk: 1000ml
sugar: 200g
```

**Exit Code**: 0 on success, non-zero on error

**Error Cases**: None (always succeeds, may show empty list)

#### `inventory add <ingredient> <quantity> <unit>`
**Purpose**: Add ingredients to inventory. Creates ingredient if new.

**Usage**: `barista inventory add espresso 500 ml`

**Output Format**: Success message confirming the ingredient and quantity added. Example:
```
Added espresso: 500 ml (total: 500 ml)
```
When adding to an existing ingredient: `Added espresso: 200 ml (total: 700 ml)`

**Exit Code**: 0 on success, non-zero on error

**Error Cases**:
- Unit mismatch: If ingredient exists with different unit, fail with: "Error: Unit mismatch: ingredient 'espresso' already exists with unit 'ml', cannot add with unit 'g'"
- Invalid quantity: Fail with clear error message

**Behavior**:
- If ingredient doesn't exist, create it with the specified unit
- If ingredient exists, add to existing quantity (validate unit match)

### Recipe Commands

#### `recipes list`
**Purpose**: List all recipes (id + name).

**Usage**: `barista recipes list`

**Output Format**:
```
recipe-1  Latte
recipe-2  Cappuccino
recipe-3  Espresso
```

**Exit Code**: 0 on success, non-zero on error

#### `recipes show <recipeName>`
**Purpose**: Show full recipe details.

**Usage**: `barista recipes show latte` (accepts recipe name, case-insensitive)

**Output Format**:
```
Recipe: Latte (recipe-1)
Ingredients:
  espresso: 30ml
  milk: 150ml
Steps:
  1. Grind beans
  2. Pull espresso shot
  3. Steam milk
  4. Combine and serve
```

**Exit Code**: 0 on success, non-zero on error

**Error Cases**:
- Recipe not found: "Error: Recipe 'latte' not found"
- Multiple recipes with same name: "Error: Multiple recipes found with name 'latte'. Use recipe ID instead."

#### `recipes add --name "<name>" --ingredient "<ingredient>:<qty><unit>" [--ingredient ...] --step "<text>" [--step ...]`
**Purpose**: Create a new recipe.

**Usage**: `barista recipes add --name "Latte" --ingredient "espresso:30ml" --ingredient "milk:150ml" --step "Grind beans" --step "Pull espresso shot"`

**Output Format**: Success message with recipe name and ID. Example:
```
Recipe 'Latte' added (ID: recipe-1)
```

**Exit Code**: 0 on success, non-zero on error

**Error Cases**:
- Missing name: Fail with error
- No ingredients: "Error: Recipe must have at least 1 ingredient"
- No steps: "Error: Recipe must have at least 1 step"
- Invalid ingredient format: Fail with clear error
- Duplicate name: "Error: Recipe '<name>' already exists" (non-zero exit)

### Order Commands

#### `order place <recipeName> --size S|M|L`
**Purpose**: Place an order for a drink.

**Usage**: `barista order place latte --size M` (accepts recipe name, case-insensitive)

**Output Format**: Order ID on success
```
order-1
```

**Exit Code**: 0 on success, non-zero on error

**Error Cases**:
- Recipe not found: "Error: Recipe 'latte' not found"
- Invalid size: "Error: Size must be S, M, or L"
- Missing size: "Error: --size is required (S, M, or L)"
- Multiple recipes with same name: "Error: Multiple recipes found with name 'latte'. Use recipe ID instead."

**Behavior**:
- Creates order in PLACED state
- Generates unique order ID (format: `ord-<uuid>`, e.g., `ord-a1b2c3d4`). IDs must start with a letter prefix so they are safe to pass as positional CLI arguments
- Stores recipe reference and size

#### `order validate <orderId>`
**Purpose**: Validate an order against inventory.

**Usage**: `barista order validate order-1`

**Output Format**: 
- Success: "✓ Order order-1 validated"
- Failure: "Error: Order order-1 validation failed\nMissing ingredients:\n  - espresso: need 45ml, have 20ml"

**Exit Code**: 0 on success (VALIDATED), non-zero on error (FAILED)

**Error Cases**:
- Order not found: "Error: Order 'order-1' not found"
- Invalid state: "Error: Order 'order-1' is not in PLACED state"
- Insufficient inventory: Show missing ingredients with quantities needed vs available

**Behavior**:
- Checks inventory against scaled ingredient requirements (S=1.0x, M=1.5x, L=2.0x)
- Transitions to VALIDATED if sufficient inventory
- Transitions to FAILED if insufficient inventory
- Does NOT consume inventory

#### `order brew <orderId>`
**Purpose**: Brew a validated order.

**Usage**: `barista order brew order-1`

**Output Format**: Brew plan (matches spec example exactly)
```
Order: order-1 (Latte, M)
Use:
  espresso: 45ml
  milk: 225ml
Steps:
  1. Grind beans
  2. Pull espresso shot
  3. Steam milk
  4. Combine and serve
```

**Exit Code**: 0 on success (COMPLETED), non-zero on error (FAILED)

**Error Cases**:
- Order not found: "Error: Order 'order-1' not found"
- Invalid state: "Error: Order 'order-1' is not in VALIDATED state"
- Inventory changed: "Error: Order order-1 cannot be brewed - inventory changed since validation\nMissing ingredients:\n  - espresso: need 45ml, have 30ml"

**Behavior**:
- Re-validates inventory (may have changed since validation)
- If sufficient, generates brew plan with scaled ingredients
- Consumes inventory atomically (all or nothing)
- Transitions to COMPLETED
- If insufficient, transitions to FAILED without consuming inventory

#### `orders list [--status <status>]`

**Note**: The `orders` command group is separate from `order`. Use `barista orders list`, not `barista order list`.

**Purpose**: List orders with optional status filter.

**Usage**: 
- `barista orders list`
- `barista orders list --status PLACED`

**Output Format**:
```
order-1  Latte  M  COMPLETED  2026-01-28T14:30:00Z
order-2  Cappuccino  L  VALIDATED  2026-01-28T14:35:00Z
```

**Exit Code**: 0 on success, non-zero on error

**Error Cases**:
- Invalid status: "Error: Invalid status 'INVALID'. Valid statuses: PLACED, VALIDATED, BREWING, COMPLETED, FAILED"

## User Workflows

### Workflow 1: Setting Up Recipes and Inventory

```bash
# Add inventory
barista inventory add espresso 500 ml
barista inventory add milk 1000 ml

# Create recipe
barista recipes add --name "Latte" \
  --ingredient "espresso:30ml" \
  --ingredient "milk:150ml" \
  --step "Grind beans" \
  --step "Pull espresso shot" \
  --step "Steam milk" \
  --step "Combine and serve"
```

### Workflow 2: Placing and Processing an Order

```bash
# Place order
barista order place latte --size M
# Output: order-1

# Validate order
barista order validate order-1
# Output: ✓ Order order-1 validated

# Brew order
barista order brew order-1
# Output: Brew plan with scaled ingredients and steps
```

### Workflow 3: Handling Insufficient Inventory

```bash
# Place order
barista order place latte --size L
# Output: order-2

# Validate (fails if insufficient)
barista order validate order-2
# Output: Error: Order order-2 validation failed
# Missing ingredients:
#   - espresso: need 60ml, have 20ml
```

## Data Format

### JSON Structure

All data stored in `barista.json`:

```json
{
  "ingredients": [
    {"name": "espresso", "unit": "ml", "is_perishable": false}
  ],
  "inventory": [
    {"ingredient_name": "espresso", "quantity": 500}
  ],
  "recipes": [
    {
      "id": "recipe-1",
      "name": "Latte",
      "ingredients": [
        {"ingredient_name": "espresso", "quantity": 30, "unit": "ml"},
        {"ingredient_name": "milk", "quantity": 150, "unit": "ml"}
      ],
      "steps": ["Grind beans", "Pull espresso shot", "Steam milk", "Combine and serve"]
    }
  ],
  "orders": [
    {
      "id": "order-1",
      "recipe_id": "recipe-1",
      "size": "M",
      "status": "COMPLETED",
      "created_at": "2026-01-28T14:30:00Z",
      "updated_at": "2026-01-28T14:35:00Z"
    }
  ]
}
```

## Order Lifecycle

**States**: PLACED → VALIDATED → BREWING → COMPLETED (or FAILED)

**State Transitions**:
- `order place`: Creates order in PLACED state
- `order validate`: PLACED → VALIDATED (if inventory sufficient) or PLACED → FAILED (if insufficient)
- `order brew`: VALIDATED → BREWING → COMPLETED (if inventory still sufficient) or VALIDATED → FAILED (if inventory changed)

**Rules**:
- Orders can only be validated from PLACED state
- Orders can only be brewed from VALIDATED state
- Failed orders cannot be validated or brewed again
- Completed orders are final

## Size Scaling

**Multipliers**:
- S (Small): 1.0x base quantities
- M (Medium): 1.5x base quantities
- L (Large): 2.0x base quantities

**Example**: Latte with base 30ml espresso, 150ml milk:
- S: 30ml espresso, 150ml milk
- M: 45ml espresso, 225ml milk
- L: 60ml espresso, 300ml milk

## Inventory Rules

1. **Validation Before Brewing**: Inventory must be checked before brewing
2. **Atomic Consumption**: All ingredients consumed together, or none consumed
3. **No Consumption on Failure**: If inventory insufficient, order fails without consuming
4. **Re-validation on Brew**: Inventory re-checked at brew time (may have changed)

## Error Handling

**General Rules**:
- All errors print clear, descriptive messages
- Errors exit with non-zero code
- Success exits with code 0
- Error messages go to stderr (or stdout, but consistently)

**Error Message Format**: "Error: [clear description of what went wrong]"

**Common Error Patterns**:
- Not found: "Error: [Entity] '[name]' not found"
- Invalid state: "Error: [Entity] '[id]' is not in [required state] state"
- Validation failure: "Error: [Entity] '[id]' validation failed\n[Details]"
- Unit mismatch: "Error: Unit mismatch: ingredient '[name]' already exists with unit '[unit]', cannot add with unit '[different-unit]'"

## Persistence

**Location**: `barista.json` in the directory specified by the `BARISTA_DATA_DIR` environment variable. If `BARISTA_DATA_DIR` is not set, default to a `data/` subdirectory relative to the current working directory. Create the directory if it does not exist.

**Atomicity**: Writes use temp file pattern:
1. Write to `barista.json.tmp`
2. Rename to `barista.json`

**Initialization**: Data directory created on first use if it doesn't exist.

**Persistence Scope**: All state persists:
- Ingredients catalog
- Inventory levels
- Recipes
- Orders (all states)

## Expected Behaviors

**Input-Output Relationships**:
- Valid commands produce expected output and exit 0
- Invalid commands produce error messages and exit non-zero
- Commands are idempotent where appropriate (e.g., `inventory add` adds to existing)

**Observables**:
- File system: `barista.json` reflects current state
- CLI output: Matches spec format exactly
- Exit codes: 0 for success, non-zero for errors

**Side Effects**:
- Inventory consumption on successful brew
- Order state transitions
- Data file updates

## Interface Contracts

**CLI Interface**: Defined in `contracts/cli-commands.yaml`
- All commands, arguments, output formats
- Error message formats
- Exit code conventions

**Data Schema**: Defined in `contracts/data-schema.json`
- JSON structure validation
- Type requirements
- Required fields

**Service Interfaces**: Defined in `contracts/service-interfaces.yaml`
- Service method signatures
- Expected behaviors
- Error conditions
