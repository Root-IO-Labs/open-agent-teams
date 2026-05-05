"""Test importing files."""


def test_imports() -> None:
    """Test importing oat_sdk modules."""
    from oat_cli import (
        agent,
        integrations,
    )
    from oat_cli.main import cli_main
