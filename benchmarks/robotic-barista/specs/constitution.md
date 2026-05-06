# Project Constitution: Robotic Barista

## Core Principles

### 1. Simplicity
- Keep the CLI interface clean and intuitive
- Minimize cognitive load for users
- Clear, consistent command structure

### 2. Modularity
- Separate domain logic from CLI parsing and persistence
- Clear boundaries between layers (domain/services/storage/cli)
- Interfaces over implementations

### 3. Correctness
- Enforce business rules strictly (inventory checks, order lifecycle)
- Fail fast with clear error messages
- Maintain data integrity

### 4. Testability
- Design for testability with clear interfaces
- Dependency injection for external dependencies
- Pure functions where possible

### 5. Persistence
- State must persist across runs
- Data integrity is critical
- Support for future migration (JSON → SQLite)

### 6. Spec-First Development
- Implement to operational specification
- Tests validate compliance, not guide implementation
- Operational spec is the source of truth

## Technical Constraints

- **Language**: Python 3.11+
- **Type**: CLI application (no web UI)
- **Persistence**: Local file storage (JSON or SQLite)
- **Hardware**: No real hardware control (simulation only)
- **Concurrency**: Must handle concurrent operations safely (no global mutable state)
- **Platform**: Cross-platform (macOS, Linux, Windows)

## Development Standards

- **Code Quality**: Linting (ruff), type checking (mypy), formatting (ruff format)
- **Testing**: Comprehensive test suite (unit, integration, system)
- **Documentation**: Clear README, operational specification, user manual
- **Architecture**: Modular design (domain/services/storage/cli separation)

## Non-Goals

- Web interface
- Real hardware integration
- Multi-user support (single-user CLI)
- Network/remote operations
- Advanced features (perishable ingredients, expiry dates) - future consideration
