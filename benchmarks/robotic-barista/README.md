# Robotic Barista

Automated coffee barista CLI application that simulates recipe management, inventory tracking, and order processing.

## Project Status

🚧 **In Development** - Phase 0 Bootstrap Complete

## Features

- Recipe management (define drinks with ingredients and steps)
- Inventory tracking (ingredients with quantities and units)
- Order processing (place, validate, brew orders)
- Brew planning (generates execution plan for orders)

## Development

### Prerequisites

- Python 3.11+
- pip

### Setup

```bash
# Install dependencies
pip install -e ".[dev]"

# Run tests
pytest tests/ -v

# Run gate
./scripts/check.sh
```

### Project Structure

```
robotic-barista/
├── src/robotic_barista/    # Source code
│   ├── domain/             # Domain entities and rules
│   ├── services/           # Business logic services
│   ├── storage/            # Persistence layer
│   └── cli/                # CLI interface
├── tests/                   # Test suite
│   ├── interfaces/         # Interface contract tests
│   ├── unit/              # Unit tests
│   ├── integration/        # Integration tests
│   └── system/            # System-level tests
├── contracts/              # Interface specifications
├── scripts/                # Utility scripts
│   └── check.sh           # Main gate script
└── docs/                   # Documentation
```

## License

[To be determined]
