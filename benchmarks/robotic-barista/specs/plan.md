# Technical Implementation Plan: Robotic Barista CLI

## Architecture Overview

**Tech Stack**: Python 3.11+, Click (CLI), JSON (persistence, start with JSON), pytest (testing)

**Architecture Pattern**: Layered architecture with clear separation:
- **Domain Layer**: Entities and business rules (pure Python)
- **Service Layer**: Business logic orchestration
- **Storage Layer**: Persistence abstraction (repository pattern)
- **CLI Layer**: Command parsing and presentation

## Component Design

### Domain Layer (`src/robotic_barista/domain/`)

**Entities**:
- `Ingredient`: name, unit (ml, g, count), is_perishable (optional)
- `InventoryItem`: ingredient, quantity_available
- `Recipe`: id, name, ingredients_required (list of ingredient + quantity), steps (ordered list), brew_time_seconds (optional)
- `Order`: id, recipe_id, size (S/M/L), status (enum), timestamps

**Business Rules**:
- Size scaling: S=1.0x, M=1.5x, L=2.0x
- Order lifecycle state machine
- Inventory validation rules
- Inventory consumption rules

### Service Layer (`src/robotic_barista/services/`)

**Services**:
- `RecipeService`: Create, list, show recipes
- `InventoryService`: Add, list inventory items
- `OrderService`: Place, validate, brew orders, list orders

**Responsibilities**:
- Orchestrate domain logic
- Coordinate between domain and storage
- Handle business rule enforcement

### Storage Layer (`src/robotic_barista/storage/`)

**Repositories** (interfaces):
- `RecipeRepository`: CRUD for recipes
- `InventoryRepository`: CRUD for inventory
- `OrderRepository`: CRUD for orders

**Implementation**:
- Start with JSON file storage (simple, fast to implement)
- Design interfaces to allow SQLite migration later
- Single JSON file or separate files per entity type

### CLI Layer (`src/robotic_barista/cli/`)

**Commands** (using Click):
- `inventory`: list, add
- `recipes`: list, show, add (accepts recipe names, not just IDs)
- `order`: place, validate, brew (accepts recipe names)
- `orders`: list (with optional --status filter)

**Responsibilities**:
- Parse command-line arguments
- Call appropriate services
- Format output for display
- Handle errors and exit codes

## Data Model

### JSON Structure (Initial Implementation)

```json
{
  "ingredients": [
    {"name": "espresso", "unit": "ml", "is_perishable": false}
  ],
  "inventory": [
    {"ingredient_name": "espresso", "quantity": 100}
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
      "status": "PLACED",
      "created_at": "2026-01-28T14:00:00Z",
      "updated_at": "2026-01-28T14:00:00Z"
    }
  ]
}
```

## Implementation Phases

### Phase 1: Foundation (Wave 0)
- Domain entities and value objects
- Basic repository interfaces
- JSON storage implementation
- CLI command structure (skeleton)

### Phase 2: Core Functionality (Wave 1)
- Recipe management (create, list, show)
- Inventory management (add, list)
- Order placement
- Basic persistence

### Phase 3: Order Processing (Wave 2)
- Order validation (inventory checks)
- Order brewing (brew plan generation, inventory consumption)
- Order listing with filters
- Error handling and edge cases

### Phase 4: Testing & Refinement (Wave 3)
- Comprehensive test coverage
- Edge case handling
- Performance optimization (if needed)
- Documentation completion

## Risk Mitigation

### Risk 1: Data Corruption
- **Mitigation**: Atomic writes (write to temp file, then rename)
- **Mitigation**: Backup before major operations

### Risk 2: Concurrent Access
- **Mitigation**: File locking for writes
- **Mitigation**: Design for future SQLite migration (handles concurrency better)

### Risk 3: Unit Mismatch
- **Mitigation**: Strong typing for units
- **Mitigation**: Validation at ingredient creation

### Risk 4: Order State Inconsistency
- **Mitigation**: State machine enforcement in domain layer
- **Mitigation**: Validation before state transitions

## Migration Path (Future)

**JSON → SQLite**:
- Design repository interfaces to abstract storage
- Implement SQLite repository alongside JSON
- Migration script to convert JSON data to SQLite
- Feature flag to switch implementations
