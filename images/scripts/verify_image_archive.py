#!/usr/bin/env python3
"""Reject build-host devices and Docker runtime state before publishing an image."""

import argparse
import tarfile
from pathlib import Path, PurePosixPath


def verify_archive(path: Path) -> None:
    state_roots = ('rootfs/var/lib/docker', 'rootfs/var/lib/containerd')
    with tarfile.open(path, 'r|gz') as archive:
        for member in archive:
            name = PurePosixPath(member.name)
            if name.is_absolute() or '..' in name.parts:
                raise ValueError('non-portable archive path: ' + member.name)
            if member.isblk() or member.ischr():
                raise ValueError('build-host device in guest image: ' + member.name)
            normalized = str(name)
            for root in state_roots:
                if normalized.startswith(root + '/') or (normalized == root and not member.isdir()):
                    raise ValueError('Docker runtime state in guest image: ' + member.name)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('archive', type=Path)
    verify_archive(parser.parse_args().archive)
