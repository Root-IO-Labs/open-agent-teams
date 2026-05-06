#!/usr/bin/env python3
"""
Whitebox shim for the robotic-barista CLI.
Saved from the original acceptance-test.sh for future whitebox testing.
This shim introspects the Python module structure to find and invoke
CLI groups directly, bypassing the registered entry point.
"""
import sys

args = sys.argv[1:]
if not args:
    print("Usage: barista <command> [args...]")
    sys.exit(1)

try:
    from robotic_barista.cli.commands import cli
    sys.argv = ["barista"] + args
    cli(standalone_mode=True)
    sys.exit(0)
except (ImportError, SystemExit) as e:
    if isinstance(e, SystemExit):
        sys.exit(e.code)

try:
    from robotic_barista.cli import cli
    sys.argv = ["barista"] + args
    cli(standalone_mode=True)
    sys.exit(0)
except (ImportError, SystemExit) as e:
    if isinstance(e, SystemExit):
        sys.exit(e.code)

import importlib
group_name = args[0]
sys.argv = args
try:
    mod = importlib.import_module(f"robotic_barista.cli.{group_name}")
    getattr(mod, group_name)()
except (ImportError, AttributeError) as e:
    print(f"Error: could not find CLI group '{group_name}': {e}", file=sys.stderr)
    sys.exit(1)
