#!/usr/bin/env python3
"""Compatibility entrypoint; deployment logic lives under deploy/."""
from pathlib import Path
import runpy
import sys
entry = Path(__file__).resolve().parents[3] / "deploy/scripts/release_manifest.py"
sys.path.insert(0, str(entry.parent))
runpy.run_path(str(entry), run_name="__main__")
