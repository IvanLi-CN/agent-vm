#!/usr/bin/env python3
"""Verify that downloaded release assets match the staged bytes and provenance."""
import argparse
import json
from pathlib import Path

from verify_assets import digest, verify


def regular_names(directory: Path) -> set[str]:
    if not directory.is_dir() or directory.is_symlink():
        raise ValueError("release asset directory is invalid")
    entries = list(directory.iterdir())
    if any(entry.is_symlink() or not entry.is_file() for entry in entries):
        raise ValueError("release assets must be regular files")
    return {entry.name for entry in entries}


def verify_release_assets(local: Path, remote: Path, revision: str) -> list[dict[str, str]]:
    local_names = regular_names(local)
    remote_names = regular_names(remote)
    if local_names != remote_names:
        raise ValueError("downloaded release asset set differs from staged assets")
    for name in sorted(local_names):
        if digest(local / name) != digest(remote / name):
            raise ValueError("downloaded release asset differs from staged bytes: " + name)
    return verify(remote, revision)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--local-directory", type=Path, required=True)
    parser.add_argument("--remote-directory", type=Path, required=True)
    parser.add_argument("--source-revision", required=True)
    args = parser.parse_args()
    print(json.dumps({"verified": verify_release_assets(args.local_directory, args.remote_directory, args.source_revision)}))
