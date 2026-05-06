# Robotic Barista CLI - Validated Design

**Date**: 2026-01-28  
**Status**: Validated through brainstorming session

## Design Overview

This document captures the validated design for the Robotic Barista CLI application, following the example specification exactly (with recipe name enhancement for better UX).

## Architecture

**Tech Stack**: Python 3.11+, Click (CLI), JSON (persistence), pytest (testing)

**Architecture Pattern**: Layered architecture with clear separation:
- **Domain Layer** (`domain/`): Entities and business rules (pure Python)
- **Service Layer** (`services/`): Business logic orchestration
- **Storage Layer** (`storage/`): Persistence abstraction (repository pattern)
- **CLI Layer** (`cli/`): Command parsing and presentation

## Data Model

**JSON Structure**: Single file `data/barista.json` with four top-level arrays:
- `ingredients`: Catalog of ingredients with units
- `inventory`: Current stock levels
- `recipes`: Drink definitions with ingredients and steps
- `orders`: Order tracking with lifecycle states

**Persistence**: Atomic writes (write to temp file, then rename) to prevent corruption.

## Order Lifecycle

**State Machine**: PLACED → VALIDATED → BREWING → COMPLETED (or FAILED at validation)

**Business Rules**:
- Size scaling: S=1.0x, M=1.5x, L=2.0x
- Inventory validation before brewing
- Atomic inventory consumption (all or nothing)
- Re-validation on brew if inventory changed

## CLI Interface

**Commands**:
- `inventory`: list, add
- `recipes`: list, show, add (accepts recipe names, not just IDs)
- `order`: place, validate, brew
- `orders`: list (with optional --status filter)

**Error Handling**: Strict validation, fail fast with clear error messages. Exit codes: 0 for success, non-zero for errors.

**Output Format**: Follows spec's example style - structured, consistent, human-readable.

## Testing Strategy

**Four Testing Layers**:
1. **Interface Contract Tests** (Wave 0): CLI command interfaces, data schemas
2. **Unit Tests** (Wave 1): Domain entities, services, repositories
3. **Integration Tests** (Wave 2+): Component interactions, workflows
4. **Black Box System Tests** (Ongoing): Complete CLI workflows, derived from operational specification

**Spec-First Development**: Tests validate implementation matches operational specification. Workers implement to spec, not to pass tests.

**Test Access Restrictions**: Workers cannot access test implementation code - only test specifications and results.

## Design Decisions

- ✅ Follow example spec exactly (with recipe name enhancement)
- ✅ JSON file persistence (start simple)
- ✅ Suggested architecture (open to refactoring)
- ✅ Strict validation (fail fast with clear errors)
- ✅ Single-user, single-threaded execution
- ✅ Spec's output style (structured, consistent)
- ✅ Complete testing strategy with all 4 layers

## Next Steps

1. Create structured artifacts (Constitution, Specification, Plan, Tasks)
2. Create Operational Specification
3. Create Testing Strategy Document
4. Proceed to Phase 2: Work Graph creation
