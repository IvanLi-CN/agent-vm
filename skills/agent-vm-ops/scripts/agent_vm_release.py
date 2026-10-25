#!/usr/bin/env python3
"""Manual Agent VM control-plane release builder and publisher.

This is an operator tool, not part of the public ``agent-vm`` lifecycle CLI.
Remote mutation is limited to the configured SSH alias and fixed release
wrapper actions.  The publisher never edits the control environment directly.
"""

from __future__ import annotations

import argparse
from dataclasses import dataclass
import io
import json
import os
from pathlib import Path, PurePosixPath
import re
import shlex
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile
import time
import uuid
from typing import Any, Callable, Mapping, Protocol


try:
    from release_manifest import (
        MANIFEST_NAME,
        CLIENT_SIDECAR_NAME,
        PROTOCOL_VERSION,
        SUPPORT_ARTIFACT_NAMES,
        SUPPORT_ARTIFACT_MODES,
        CLIENT_PACKAGE_MODES,
        CLIENT_DEPENDENCY_MODES,
        UNIT_EXEC_START,
        UNIT_CONTENT_SHA256,
        ManifestError,
        build_manifest,
        default_operations,
        legacy_manifest,
        load_manifest,
        sha256_file,
        verify_client_sidecar,
        verify_bundle,
        verify_typescript_syntax,
    )
except ModuleNotFoundError:
    sys.path.insert(0, str(Path(__file__).resolve().parent))
    from release_manifest import (
        MANIFEST_NAME,
        CLIENT_SIDECAR_NAME,
        PROTOCOL_VERSION,
        SUPPORT_ARTIFACT_NAMES,
        SUPPORT_ARTIFACT_MODES,
        CLIENT_PACKAGE_MODES,
        CLIENT_DEPENDENCY_MODES,
        UNIT_EXEC_START,
        UNIT_CONTENT_SHA256,
        ManifestError,
        build_manifest,
        default_operations,
        legacy_manifest,
        load_manifest,
        sha256_file,
        verify_client_sidecar,
        verify_bundle,
        verify_typescript_syntax,
    )


try:
    from agent_vm_release_wrapper import WrapperError, safe_extract_archive
except ModuleNotFoundError:
    sys.path.insert(0, str(Path(__file__).resolve().parent))
    from agent_vm_release_wrapper import WrapperError, safe_extract_archive


SOURCE_REVISION_RE = re.compile(r"^[0-9a-f]{40}$")
DEFAULT_TIMEOUT_SECONDS = 300
DEFAULT_POLL_SECONDS = 1.0
MAX_TIMEOUT_SECONDS = 300
MAX_POLL_SECONDS = 1.0
CANONICAL_REMOTE_CONTROL = "/opt/agent-vm-native/agent_vm_control.ts"
CANONICAL_REMOTE_WRAPPER = "/usr/local/sbin/agent-vm-release-wrapper"
CONTROL_TRANSITION_STATES = {"preparing", "closing", "deleting", "failed"}
KNOWN_TASK_STATES = {"absent", "cached", "preparing", "active", "closing", "deleting", "failed"}
RECONCILE_ACTIONS = {
    "inspect",
    "verify-absent-resources",
    "finish-delete",
    "close-incomplete-or-expired",
    "rearm-current-expiry",
    "stop-unexpected-running-cache",
}
SOURCE_ARTIFACTS = (
    ("agent_vm.ts", "skills/agent-vm/scripts/agent_vm.ts", "client", 0o755),
    ("agent_vm_control.ts", "skills/agent-vm-ops/scripts/agent_vm_control.ts", "control", 0o755),
    *(
        (name, f"skills/{'agent-vm' if name in CLIENT_DEPENDENCY_MODES else 'agent-vm-ops'}/scripts/{name}", "support", SUPPORT_ARTIFACT_MODES[name])
        for name in sorted(SUPPORT_ARTIFACT_NAMES)
    ),
    ("systemd/agent-vm-native-age-gc.service", "skills/agent-vm-ops/assets/systemd/agent-vm-native-age-gc.service", "unit", 0o644),
    ("systemd/agent-vm-native-age-gc.timer", "skills/agent-vm-ops/assets/systemd/agent-vm-native-age-gc.timer", "unit", 0o644),
    ("systemd/agent-vm-native-expire@.service", "skills/agent-vm-ops/assets/systemd/agent-vm-native-expire@.service", "unit", 0o644),
    ("systemd/agent-vm-native-reconcile.service", "skills/agent-vm-ops/assets/systemd/agent-vm-native-reconcile.service", "unit", 0o644),
)


class ReleaseError(Exception):
    def __init__(self, code: str, message: str, detail: Any = None):
        super().__init__(message)
        self.code = code
        self.message = message
        self.detail = detail

    def as_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {"code": self.code, "message": self.message}
        if self.detail is not None:
            result["detail"] = self.detail
        return result


def _fail(code: str, message: str, detail: Any = None) -> None:
    raise ReleaseError(code, message, detail)


def _fsync_directory(path: Path) -> None:
    """Persist a directory entry after an atomic rename or unlink."""
    flags = os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0)
    try:
        descriptor = os.open(path, flags)
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)
    except OSError as exc:
        _fail("bundle_durability_failed", f"cannot persist release directory: {path.name}")


def _revision(value: str) -> str:
    if SOURCE_REVISION_RE.fullmatch(value) is None:
        _fail("invalid_source_revision", "source revision must be a 40-character lowercase Git revision")
    return value


def _read_regular(path: Path) -> bytes:
    try:
        info = path.lstat()
    except OSError:
        _fail("source_missing", f"source artifact is missing: {path.name}")
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
        _fail("source_invalid", f"source artifact is not a regular file: {path.name}")
    try:
        return path.read_bytes()
    except OSError:
        _fail("source_unreadable", f"source artifact cannot be read: {path.name}")


def _write_atomic(path: Path, data: bytes, mode: int) -> None:
    path.parent.mkdir(mode=0o755, parents=True, exist_ok=True)
    descriptor, name = tempfile.mkstemp(prefix="." + path.name + ".", suffix=".tmp", dir=path.parent)
    temporary = Path(name)
    try:
        with os.fdopen(descriptor, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fchmod(handle.fileno(), mode)
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        _fsync_directory(path.parent)
    except OSError as exc:
        try:
            temporary.unlink()
        except OSError:
            pass
        _fail("bundle_write_failed", f"cannot write release artifact: {path.name}")


def _git_revision(source_root: Path) -> str:
    try:
        process = subprocess.run(
            ["git", "-C", str(source_root), "rev-parse", "HEAD"],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
            text=True,
        )
    except OSError as exc:
        _fail("git_unavailable", "git is required when source revision is omitted")
    if process.returncode != 0:
        _fail("git_revision_unavailable", "source revision cannot be resolved")
    return _revision(process.stdout.strip())


def _artifact_entry(root: Path, name: str, role: str, mode: int) -> dict[str, Any]:
    path = root / Path(*PurePosixPath(name).parts)
    return {"name": name, "path": name, "role": role, "sha256": sha256_file(path), "mode": mode}


def _write_json(path: Path, value: Mapping[str, Any], mode: int = 0o644) -> None:
    _write_atomic(path, (json.dumps(dict(value), sort_keys=True, indent=2) + "\n").encode(), mode)


def _create_archive(root: Path, output: Path) -> None:
    output.parent.mkdir(mode=0o755, parents=True, exist_ok=True)
    temporary = output.with_name("." + output.name + ".tmp")
    try:
        with tarfile.open(temporary, mode="w") as archive:
            for path in sorted(root.rglob("*")):
                if not path.is_file() or path.is_symlink():
                    continue
                archive.add(path, arcname=path.relative_to(root).as_posix(), recursive=False)
        os.replace(temporary, output)
        _fsync_directory(output.parent)
    except (OSError, tarfile.TarError) as exc:
        try:
            temporary.unlink()
        except OSError:
            pass
        _fail("bundle_write_failed", "cannot create release archive")


def build_release(source_root: Path, output: Path, source_revision: str | None = None) -> dict[str, Any]:
    source_root = source_root.resolve()
    if not source_root.is_dir():
        _fail("source_missing", "source root is not a directory")
    revision = _revision(source_revision) if source_revision is not None else _git_revision(source_root)
    with tempfile.TemporaryDirectory(prefix="agent-vm-release-") as temporary:
        root = Path(temporary)
        artifacts: list[dict[str, Any]] = []
        for destination_name, source_name, role, mode in SOURCE_ARTIFACTS:
            source = source_root / Path(*PurePosixPath(source_name).parts)
            data = _read_regular(source)
            destination = root / Path(*PurePosixPath(destination_name).parts)
            _write_atomic(destination, data, mode)
            artifact = _artifact_entry(root, destination_name, role, mode)
            artifact["name"] = "unit:" + Path(destination_name).name if role == "unit" else artifact["name"]
            artifacts.append(artifact)
        manifest = build_manifest(revision, artifacts, operations=default_operations())
        _write_json(root / MANIFEST_NAME, manifest)
        client_artifact = next(item for item in artifacts if item["role"] == "client")
        _write_json(root / CLIENT_SIDECAR_NAME, {
            "format": manifest["format"],
            "release_id": manifest["release_id"],
            "source_revision": manifest["source_revision"],
            "protocol": manifest["protocol"],
            "client_artifact": client_artifact,
            "client_dependencies": [item for item in artifacts if item["path"] in CLIENT_DEPENDENCY_MODES],
            "client_view": manifest["client_view"],
        })
        _create_archive(root, output)
    return manifest


def _verify_unit_contents(root: Path, manifest: Mapping[str, Any]) -> None:
    for artifact in manifest["artifacts"]:
        if artifact["role"] != "unit":
            continue
        name = Path(artifact["path"]).name
        if name not in UNIT_EXEC_START and name != "agent-vm-native-age-gc.timer":
            _fail("unit_unverified", f"unit is not in the fixed allowlist: {name}")
        try:
            content = (root / Path(*PurePosixPath(artifact["path"]).parts)).read_text(encoding="utf-8")
        except (OSError, UnicodeError):
            _fail("unit_unverified", f"unit cannot be read: {name}")
        path = root / Path(*PurePosixPath(artifact["path"]).parts)
        if sha256_file(path) != UNIT_CONTENT_SHA256[name]:
            _fail("unit_unverified", f"unit bytes differ from the fixed policy: {name}")
        required = UNIT_EXEC_START.get(name, "Unit=agent-vm-native-age-gc.service")
        if required not in content or (name != "agent-vm-native-age-gc.timer" and "ExecStart=" not in content):
            _fail("unit_unverified", f"unit does not contain its fixed ExecStart: {name}")
        if any(token in content for token in ("systemctl stop", "systemctl disable", "ExecStop=", "ssh ", "curl ", "wget ")):
            _fail("unit_unverified", f"unit contains a forbidden control path: {name}")


def _verify_python_syntax(root: Path, manifest: Mapping[str, Any]) -> None:
    for artifact in manifest["artifacts"]:
        if artifact["role"] not in {"client", "control", "support"}:
            continue
        if PurePosixPath(artifact["path"]).suffix != ".py":
            continue
        path = root / Path(*PurePosixPath(artifact["path"]).parts)
        try:
            compile(path.read_text(encoding="utf-8"), str(path), "exec")
        except (OSError, UnicodeError, SyntaxError):
            _fail("python_syntax_invalid", f"Python artifact cannot be compiled: {artifact['path']}")


def _reject_bundle_extras(root: Path, manifest: Mapping[str, Any]) -> None:
    allowed = {MANIFEST_NAME, CLIENT_SIDECAR_NAME} | {item["path"] for item in manifest["artifacts"]}
    for path in root.rglob("*"):
        if path.is_symlink():
            _fail("bundle_invalid", "bundle contains a symlink", {"path": path.relative_to(root).as_posix()})
        if path.is_file() and path.relative_to(root).as_posix() not in allowed:
            _fail("bundle_invalid", "bundle contains an unallowlisted file", {"path": path.relative_to(root).as_posix()})


def _verify_fixed_artifact_set(manifest: Mapping[str, Any]) -> None:
    expected = {(role, destination) for destination, _, role, _ in SOURCE_ARTIFACTS}
    actual = {(item["role"], item["path"]) for item in manifest["artifacts"]}
    if actual != expected:
        _fail("bundle_invalid", "manifest artifact set differs from the fixed release allowlist", {
            "expected": sorted(expected),
            "actual": sorted(actual),
        })


def _open_bundle(bundle: Path) -> tuple[tempfile.TemporaryDirectory[str], Path, dict[str, Any]]:
    temporary = tempfile.TemporaryDirectory(prefix="agent-vm-release-verify-")
    root = Path(temporary.name)
    try:
        if bundle.is_dir():
            for item in bundle.iterdir():
                destination = root / item.name
                if item.is_dir() and not item.is_symlink():
                    shutil.copytree(item, destination, symlinks=True)
                elif item.is_file() and not item.is_symlink():
                    shutil.copy2(item, destination)
                else:
                    _fail("bundle_invalid", "bundle contains a non-regular top-level entry")
        else:
            try:
                safe_extract_archive(bundle.read_bytes(), root)
            except OSError:
                _fail("bundle_invalid", "release bundle cannot be read")
        manifest = load_manifest(root / MANIFEST_NAME)
        verify_bundle(root, manifest)
        verify_client_sidecar(root, manifest)
        _reject_bundle_extras(root, manifest)
        _verify_fixed_artifact_set(manifest)
        _verify_python_syntax(root, manifest)
        verify_typescript_syntax(root, manifest)
        _verify_unit_contents(root, manifest)
        return temporary, root, manifest
    except ManifestError as exc:
        temporary.cleanup()
        _fail(exc.code, exc.message, exc.detail)
    except WrapperError as exc:
        temporary.cleanup()
        _fail(exc.code, exc.message, exc.detail)
    except ReleaseError:
        temporary.cleanup()
        raise


def verify_release(bundle: Path) -> dict[str, Any]:
    temporary, _, manifest = _open_bundle(bundle)
    temporary.cleanup()
    return {
        "verified": True,
        "release_id": manifest["release_id"],
        "source_revision": manifest["source_revision"],
        "protocol": manifest["protocol"],
        "artifacts": len(manifest["artifacts"]),
        "actions": len(manifest["actions"]),
    }


def _archive_bytes(bundle: Path) -> bytes:
    if bundle.is_file():
        try:
            return bundle.read_bytes()
        except OSError:
            _fail("bundle_invalid", "release bundle cannot be read")
    with tempfile.TemporaryDirectory(prefix="agent-vm-release-archive-") as temporary:
        output = Path(temporary) / "release.tar"
        _create_archive(bundle, output)
        return output.read_bytes()


LOCAL_INSTALL_RECORD_NAME = ".agent-vm-client-install.json"


def _local_backup_path(path: Path) -> Path:
    return path.with_name("." + path.name + ".previous")


def _remove_local_path(path: Path) -> None:
    if not path.exists() and not path.is_symlink():
        return
    try:
        info = path.lstat()
    except OSError:
        _fail("local_client_recovery_failed", f"cannot inspect local client path: {path.name}")
    if stat.S_ISDIR(info.st_mode) and not stat.S_ISLNK(info.st_mode):
        _fail("local_client_recovery_failed", f"local client path is a directory: {path.name}")
    try:
        path.unlink()
    except OSError:
        _fail("local_client_recovery_failed", f"cannot remove local client path: {path.name}")


def _remove_local_staging(path: Path) -> None:
    if not path.exists() and not path.is_symlink():
        return
    try:
        info = path.lstat()
    except OSError:
        _fail("local_client_recovery_failed", "cannot inspect local client staging directory")
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
        _fail("local_client_recovery_failed", "local client staging path is not a directory")
    try:
        allowed = set(LOCAL_PACKAGE_MODES)
        children = list(path.iterdir())
        for child in children:
            child_info = child.lstat()
            if child.name not in allowed or stat.S_ISLNK(child_info.st_mode) or not stat.S_ISREG(child_info.st_mode):
                _fail("local_client_recovery_failed", "local client staging directory contains an unexpected entry")
        for child in children:
            child.unlink()
        path.rmdir()
    except OSError:
        _fail("local_client_recovery_failed", "cannot remove local client staging directory")


def _verify_local_install_file(path: Path, *, digest: str, mode: int, label: str) -> None:
    try:
        info = path.lstat()
    except OSError:
        _fail("local_client_recovery_failed", f"completed local client file is unavailable: {label}")
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
        _fail("local_client_recovery_failed", f"completed local client file is not regular: {label}")
    if stat.S_IMODE(info.st_mode) != mode or sha256_file(path) != digest:
        _fail("local_client_recovery_failed", f"completed local client file does not match its recovery marker: {label}")


LOCAL_PACKAGE_MODES = {**CLIENT_PACKAGE_MODES, CLIENT_SIDECAR_NAME: 0o644}


def _present(path: Path) -> bool:
    return path.exists() or path.is_symlink()


def _validate_local_staging(path: Path) -> None:
    if not _present(path):
        return
    if path.is_symlink() or not path.is_dir():
        _fail("local_client_recovery_failed", "local client staging path is not a real directory")
    for child in path.iterdir():
        if child.name not in LOCAL_PACKAGE_MODES or child.is_symlink() or not child.is_file():
            _fail("local_client_recovery_failed", "local client staging directory contains an unexpected entry")


def _recover_local_client_install(client_path: Path, sidecar_path: Path) -> None:
    directory = client_path.parent
    if client_path.name != "agent_vm.ts" or sidecar_path != directory / CLIENT_SIDECAR_NAME:
        _fail("local_client_recovery_failed", "recovery requires the fixed Bun package paths")
    record_path = directory / LOCAL_INSTALL_RECORD_NAME
    if not _present(record_path):
        return
    try:
        info = record_path.lstat()
        if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o600:
            _fail("local_client_recovery_failed", "local client recovery marker is not a private regular file")
        record = json.loads(record_path.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError):
        _fail("local_client_recovery_failed", "local client recovery marker is unreadable")
    if (not isinstance(record, dict) or type(record.get("format")) is not int or record.get("format") != 2
            or set(record) != {"format", "phase", "client_path", "staged_dir", "previous", "target"}
            or record["client_path"] != str(client_path)
            or record["phase"] not in {"installing", "complete"}):
        _fail("local_client_recovery_failed", "invalid complete-package recovery marker; retain it for inspection")
    staging = Path(str(record["staged_dir"]))
    if staging.parent != directory or not staging.name.startswith("agent-vm-client-"):
        _fail("local_client_recovery_failed", "unsafe local client staging path")
    _validate_local_staging(staging)

    def states(value: Any, absent: bool) -> dict[str, Any]:
        if not isinstance(value, dict) or set(value) != set(LOCAL_PACKAGE_MODES):
            _fail("local_client_recovery_failed", "recovery marker must describe the entire client package")
        for name, mode in LOCAL_PACKAGE_MODES.items():
            state = value[name]
            if not isinstance(state, dict) or not isinstance(state.get("exists"), bool):
                _fail("local_client_recovery_failed", "invalid package file state")
            if state["exists"] is False:
                if not absent or set(state) != {"exists"}:
                    _fail("local_client_recovery_failed", "invalid absent package file state")
            elif (set(state) != {"exists", "sha256", "mode"} or type(state["mode"]) is not int
                  or state["mode"] != mode or not isinstance(state["sha256"], str)
                  or re.fullmatch(r"[0-9a-f]{64}", state["sha256"]) is None):
                _fail("local_client_recovery_failed", "invalid package hash or fixed mode")
        return value

    previous, target = states(record["previous"], True), states(record["target"], False)
    if _present(staging):
        for child in staging.iterdir():
            state = target[child.name]
            _verify_local_install_file(child, digest=state["sha256"], mode=state["mode"], label="staging " + child.name)
    complete = record["phase"] == "complete"
    # Validate EVERY path before restoring/deleting ANY path. Recovery itself is
    # repeatable after a crash between individual atomic renames.
    for name in LOCAL_PACKAGE_MODES:
        destination = directory / name
        backup = _local_backup_path(destination)
        old, new = previous[name], target[name]
        if complete:
            _verify_local_install_file(destination, digest=new["sha256"], mode=new["mode"], label=name)
        elif _present(backup):
            if not old["exists"]:
                _fail("local_client_recovery_failed", "unexpected backup for an absent previous file")
            if _present(destination):
                try:
                    _verify_local_install_file(destination, digest=new["sha256"], mode=new["mode"], label=name)
                except ReleaseError:
                    # Exclusive backup creation precedes target replacement.
                    # A crash in between leaves the verified old file at both.
                    _verify_local_install_file(destination, digest=old["sha256"], mode=old["mode"], label=name)
        elif old["exists"]:
            _verify_local_install_file(destination, digest=old["sha256"], mode=old["mode"], label=name)
        elif _present(destination):
            _verify_local_install_file(destination, digest=new["sha256"], mode=new["mode"], label=name)
        if _present(backup):
            if not old["exists"]:
                _fail("local_client_recovery_failed", "unexpected package backup")
            _verify_local_install_file(backup, digest=old["sha256"], mode=old["mode"], label="backup " + name)

    # Sidecar is last during installation; during recovery restore it last too.
    # A mixed package cannot pass the old or new sidecar dependency hashes.
    for name in LOCAL_PACKAGE_MODES:
        destination, backup = directory / name, _local_backup_path(directory / name)
        if complete:
            if _present(backup):
                backup.unlink()
        elif _present(backup):
            os.replace(backup, destination)
        elif not previous[name]["exists"] and _present(destination):
            destination.unlink()
        _fsync_directory(directory)
    _remove_local_staging(staging)
    _fsync_directory(directory)
    record_path.unlink()
    _fsync_directory(directory)


def _client_selfcheck(directory: Path, release_id: str) -> None:
    bun = shutil.which("bun")
    if bun is None:
        _fail("local_client_selfcheck_failed", "Bun is required to self-check the complete client package")
    try:
        process = subprocess.run(
            [bun, "--no-macros", str(directory / "agent_vm.ts"), "version"],
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False, timeout=30,
            cwd="/", env={"PATH": os.environ.get("PATH", "/usr/local/bin:/usr/bin:/bin")},
        )
        value = json.loads(process.stdout)
        if (process.returncode != 0 or value.get("protocol") != PROTOCOL_VERSION
                or value.get("release", {}).get("release_id") != release_id
                or value.get("release", {}).get("artifacts_verified") is not True
                or value.get("warnings") != []):
            raise ValueError("client did not verify the expected complete package")
    except (OSError, subprocess.TimeoutExpired, ValueError, TypeError, AttributeError):
        _fail("local_client_selfcheck_failed", "Bun client package self-check failed")


def _install_client_package(root: Path, client_path: Path, release_id: str) -> None:
    directory = client_path.parent
    directory.mkdir(mode=0o755, parents=True, exist_ok=True)
    # The lock is a fixed private regular file, never a caller-supplied pathname.
    import fcntl
    descriptor = os.open(directory / ".agent-vm-client-install.lock", os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    try:
        info = os.fstat(descriptor)
        if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o600:
            _fail("local_client_target_invalid", "local package lock is not a private regular file")
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            _fail("local_client_install_busy", "another publisher owns this local client package")
        _recover_local_client_install(client_path, directory / CLIENT_SIDECAR_NAME)
        if _present(directory / "agent_vm.py"):
            _fail("local_client_target_invalid", "retired Python client target requires explicit operator migration")
        previous = {}
        for name, mode in LOCAL_PACKAGE_MODES.items():
            destination, backup = directory / name, _local_backup_path(directory / name)
            if _present(backup):
                _fail("local_client_target_invalid", "preexisting package backup is retained: " + backup.name)
            if _present(destination):
                info = destination.lstat()
                if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != mode:
                    _fail("local_client_target_invalid", "existing package file has invalid type or fixed mode: " + name)
                previous[name] = {"exists": True, "sha256": sha256_file(destination), "mode": mode}
            else:
                previous[name] = {"exists": False}
        staging = Path(tempfile.mkdtemp(prefix="agent-vm-client-", dir=directory))
        marker = directory / LOCAL_INSTALL_RECORD_NAME
        recorded = False
        try:
            for name, mode in LOCAL_PACKAGE_MODES.items():
                _write_atomic(staging / name, _read_regular(root / name), mode)
            _client_selfcheck(staging, release_id)
            target = {name: {"exists": True, "sha256": sha256_file(staging / name), "mode": mode}
                      for name, mode in LOCAL_PACKAGE_MODES.items()}
            record = {"format": 2, "phase": "installing", "client_path": str(client_path),
                      "staged_dir": str(staging), "previous": previous, "target": target}
            _write_json(marker, record, 0o600)
            recorded = True
            for name in LOCAL_PACKAGE_MODES:
                destination = directory / name
                if previous[name]["exists"]:
                    _verify_local_install_file(destination, digest=previous[name]["sha256"], mode=previous[name]["mode"], label=name)
                    # link() is exclusive: a backup appearing after preflight
                    # must never be overwritten. Keep the active old file until
                    # the staged candidate replaces it atomically.
                    os.link(destination, _local_backup_path(destination), follow_symlinks=False)
                    _fsync_directory(directory)
                elif _present(destination):
                    _fail("local_client_install_failed", "package target changed during installation: " + name)
                os.replace(staging / name, destination)
                _fsync_directory(directory)
            # Readers fail closed while the complete-package recovery marker
            # exists. Verify installed bytes here without bypassing that fence.
            verify_client_sidecar(directory, load_manifest(root / MANIFEST_NAME))
            record["phase"] = "complete"
            _write_json(marker, record, 0o600)
        except (OSError, ReleaseError, ManifestError) as exc:
            if recorded:
                _recover_local_client_install(client_path, directory / CLIENT_SIDECAR_NAME)
            if isinstance(exc, ManifestError):
                _fail("local_client_install_failed", "installed Bun package failed integrity verification", exc.as_dict())
            raise
        finally:
            # Unverified recovery material is intentionally retained.
            if not recorded:
                _remove_local_staging(staging)
                _fsync_directory(directory)
        _recover_local_client_install(client_path, directory / CLIENT_SIDECAR_NAME)
    finally:
        os.close(descriptor)


class ReleaseTransport(Protocol):
    def wrapper(self, host: str, wrapper_path: str, action: str, transaction_id: str, release_id: str, payload: bytes | None = None) -> Mapping[str, Any]: ...
    def control(self, host: str, remote_control: str, request: Mapping[str, Any]) -> Mapping[str, Any]: ...


class SSHTransport:
    """OpenSSH transport with fixed command boundaries and JSON envelopes."""

    def __init__(self, *, timeout: float = 60):
        self.timeout = timeout

    @staticmethod
    def _parse(process: subprocess.CompletedProcess[bytes], label: str) -> Mapping[str, Any]:
        if process.returncode != 0:
            detail: Any = process.stderr.decode("utf-8", "replace")[-4000:]
            try:
                value = json.loads(process.stderr)
                if isinstance(value, dict):
                    detail = value
            except (UnicodeDecodeError, json.JSONDecodeError):
                pass
            _fail("remote_failed", f"{label} failed", detail)
        try:
            value = json.loads(process.stdout)
        except (UnicodeDecodeError, json.JSONDecodeError):
            _fail("remote_response_invalid", f"{label} did not return JSON")
        if not isinstance(value, dict):
            _fail("remote_response_invalid", f"{label} response is not an object")
        return value

    def _run(self, host: str, command: list[str], payload: bytes | None) -> Mapping[str, Any]:
        try:
            process = subprocess.run(
                ["ssh", "-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "StrictHostKeyChecking=yes", "--", host, shlex.join(command)],
                input=payload,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                check=False,
                timeout=self.timeout,
            )
        except FileNotFoundError:
            _fail("command_missing", "ssh is not installed")
        except subprocess.TimeoutExpired:
            _fail("remote_timeout", "remote release action timed out")
        return self._parse(process, command[0])

    def wrapper(self, host: str, wrapper_path: str, action: str, transaction_id: str, release_id: str, payload: bytes | None = None) -> Mapping[str, Any]:
        command = [wrapper_path, action]
        if action != "status":
            command.extend(["--transaction-id", transaction_id, "--release-id", release_id])
        return self._run(host, command, payload)

    def control(self, host: str, remote_control: str, request: Mapping[str, Any]) -> Mapping[str, Any]:
        response = self._run(host, ["bun", remote_control], json.dumps(dict(request), sort_keys=True).encode())
        if response.get("ok") is False:
            error = response.get("error")
            if isinstance(error, dict):
                _fail(str(error.get("code", "remote_error")), str(error.get("message", "remote control failed")), error.get("detail"))
            _fail("remote_control_failed", "remote control rejected the request")
        return response


def _result(response: Mapping[str, Any]) -> dict[str, Any]:
    if response.get("ok") is False:
        error = response.get("error")
        if isinstance(error, Mapping):
            _fail(str(error.get("code", "remote_error")), str(error.get("message", "remote action failed")), error.get("detail"))
        _fail("remote_error", "remote action failed")
    value = response.get("result", response)
    if not isinstance(value, dict):
        _fail("remote_response_invalid", "remote result is not an object")
    return dict(value)


@dataclass
class ReleasePublisher:
    bundle: Path
    control_host: str
    wrapper_path: str = CANONICAL_REMOTE_WRAPPER
    remote_control: str = CANONICAL_REMOTE_CONTROL
    client_target: Path | None = None
    session_id: str | None = None
    transport: ReleaseTransport | None = None
    sleep: Callable[[float], None] = time.sleep
    monotonic: Callable[[], float] = time.monotonic

    def __post_init__(self) -> None:
        if self.remote_control != CANONICAL_REMOTE_CONTROL:
            _fail("remote_control_invalid", "release publisher uses the fixed control target")
        if self.wrapper_path != CANONICAL_REMOTE_WRAPPER:
            _fail("wrapper_path_invalid", "release publisher uses the fixed wrapper target")

    def _transport(self) -> ReleaseTransport:
        return self.transport or SSHTransport()

    def _request(
        self,
        manifest: Mapping[str, Any],
        action: str,
        *,
        session: str | None = None,
        write: bool = False,
        fields: Mapping[str, Any] | None = None,
    ) -> dict[str, Any]:
        spec = manifest["actions"].get(action)
        if not isinstance(spec, Mapping):
            _fail("unknown_action", f"release manifest does not declare action: {action}")
        request: dict[str, Any] = {
            "protocol": PROTOCOL_VERSION,
            "action": action,
            "compatibility": {
                "release_id": manifest["release_id"],
                "protocol": PROTOCOL_VERSION,
                "action": action,
                "request": dict(spec["request"]),
                "accepted_response": dict(spec["control_to_client"]),
            },
        }
        if session is not None:
            request["session_id"] = session
        if write:
            request["generation"] = 0
            request["operation_id"] = "release-" + uuid.uuid4().hex
        if fields:
            request.update(dict(fields))
        return request

    def _control(
        self,
        manifest: Mapping[str, Any],
        action: str,
        *,
        session: str | None = None,
        write: bool = False,
        fields: Mapping[str, Any] | None = None,
    ) -> dict[str, Any]:
        response = self._transport().control(
            self.control_host,
            self.remote_control,
            self._request(manifest, action, session=session, write=write, fields=fields),
        )
        return _result(response)

    def _wrapper(self, action: str, transaction_id: str, release_id: str, payload: bytes | None = None) -> dict[str, Any]:
        response = self._transport().wrapper(self.control_host, self.wrapper_path, action, transaction_id, release_id, payload)
        return _result(response)

    @staticmethod
    def _healthy(metrics: Mapping[str, Any], doctor: Mapping[str, Any]) -> bool:
        if not isinstance(metrics, Mapping) or not isinstance(doctor, Mapping):
            return False
        if metrics.get("up") is not True or metrics.get("draining") is not True:
            return False
        pending = metrics.get("pending_operations")
        task_states = metrics.get("task_states")
        if not isinstance(pending, list) or any(not isinstance(item, Mapping) for item in pending):
            return False
        if pending != [] or not isinstance(task_states, Mapping):
            return False
        if any(state not in KNOWN_TASK_STATES for state in task_states):
            return False
        if any(isinstance(count, bool) or not isinstance(count, int) or count < 0 for count in task_states.values()):
            return False
        for state in CONTROL_TRANSITION_STATES:
            count = task_states.get(state, 0)
            if isinstance(count, bool) or not isinstance(count, int) or count < 0 or count:
                return False
        issues = doctor.get("issues")
        return doctor.get("ready") is True and isinstance(issues, list) and not issues

    @staticmethod
    def _reconcile_healthy(value: Mapping[str, Any]) -> bool:
        actions = value.get("actions") if isinstance(value, Mapping) else None
        if not isinstance(value, Mapping) or value.get("dry_run") is not True or not isinstance(actions, list):
            return False
        for item in actions:
            if (
                not isinstance(item, Mapping)
                or set(item) != {"task_id", "generation", "action"}
                or not isinstance(item.get("task_id"), str)
                or not item["task_id"]
                or isinstance(item.get("generation"), bool)
                or not isinstance(item.get("generation"), int)
                or item["generation"] < 0
                or item.get("action") not in RECONCILE_ACTIONS
            ):
                return False
        return True

    def _wait_for_convergence(self, manifest: Mapping[str, Any], timeout: float, poll: float) -> dict[str, Any]:
        deadline = self.monotonic() + timeout
        last: dict[str, Any] = {}
        while True:
            last = self._control(manifest, "admin-metrics")
            pending = last.get("pending_operations")
            states = last.get("task_states")
            if last.get("draining") is not True:
                _fail("drain_unverified", "admission drain was not confirmed during convergence", {"draining": last.get("draining")})
            if isinstance(states, Mapping) and any(state not in KNOWN_TASK_STATES for state in states):
                _fail("state_unverified", "control returned an unknown task state during convergence", {"task_states": states})
            converged = False
            if pending == [] and isinstance(states, Mapping):
                converged = all(
                    not isinstance(states.get(state, 0), bool)
                    and isinstance(states.get(state, 0), int)
                    and states.get(state, 0) >= 0
                    and states.get(state, 0) == 0
                    for state in CONTROL_TRANSITION_STATES
                )
            if converged:
                return last
            remaining = deadline - self.monotonic()
            if remaining <= 0:
                _fail("convergence_timeout", "pending operations or task transitions did not converge", {
                    "pending_operations": pending,
                    "task_states": states,
                    "draining": last.get("draining"),
                })
            self.sleep(min(poll, remaining))

    def _verify_automatic_rollback(
        self,
        transaction_id: str,
        failed_release_id: str,
        rollback_result: Mapping[str, Any],
    ) -> None:
        target_release_id = rollback_result.get("release_id")
        if (
            rollback_result.get("rolled_back") is not True
            or not isinstance(target_release_id, str)
            or SOURCE_REVISION_RE.fullmatch(target_release_id) is None
            or target_release_id == failed_release_id
            or rollback_result.get("previous_release_id") != failed_release_id
        ):
            _fail(
                "rollback_unverified",
                "automatic rollback did not identify the retained previous release",
                {"rollback": dict(rollback_result), "failed_release_id": failed_release_id},
            )
        status = self._wrapper("status", transaction_id, failed_release_id)
        if (
            status.get("current_release_id") != target_release_id
            or status.get("previous_release_id") != failed_release_id
        ):
            _fail(
                "rollback_unverified",
                "automatic rollback pointers do not match the retained release record",
                {"rollback": dict(rollback_result), "status": status, "failed_release_id": failed_release_id},
            )

    def _health(self, manifest: Mapping[str, Any]) -> tuple[dict[str, Any], dict[str, Any]]:
        doctor = self._control(manifest, "doctor")
        metrics = self._control(manifest, "admin-metrics")
        reconcile = self._control(manifest, "admin-reconcile", fields={"apply": False})
        actions = reconcile.get("actions")
        reconcile_ok = self._reconcile_healthy(reconcile)
        if not self._healthy(metrics, doctor):
            _fail("remote_health_failed", "remote control health or timer checks failed", {
                "doctor": {"ready": doctor.get("ready"), "issues": doctor.get("issues")},
                "metrics": {"up": metrics.get("up"), "draining": metrics.get("draining"), "pending_operations": metrics.get("pending_operations"), "task_states": metrics.get("task_states")},
            })
        if not reconcile_ok:
            _fail("remote_reconcile_failed", "remote reconciliation check did not produce a verified dry-run result", {
                "dry_run": reconcile.get("dry_run"),
                "actions": actions,
            })
        return doctor, metrics

    def _install_local_client(self, manifest: Mapping[str, Any]) -> None:
        if self.client_target is None:
            _fail("local_client_target_missing", "publish requires an explicit local client target")
        target = self.client_target.absolute()
        if target.name == "agent_vm.ts":
            client_path = target
        elif target.suffix in {".py", ".ts"} or target.is_file() or target.is_symlink():
            _fail("local_client_target_invalid", "client target must be a package directory or agent_vm.ts")
        else:
            client_path = target / "agent_vm.ts"
        temporary, root, checked = _open_bundle(self.bundle)
        try:
            if checked != manifest:
                _fail("bundle_invalid", "bundle changed before local package installation")
            _install_client_package(root, client_path, manifest["release_id"])
        except OSError:
            _fail("local_client_install_failed", "local client package could not be installed")
        finally:
            temporary.cleanup()

    def publish(self, *, timeout: float = DEFAULT_TIMEOUT_SECONDS, poll: float = DEFAULT_POLL_SECONDS, transaction_id: str | None = None) -> dict[str, Any]:
        if self.client_target is None:
            _fail("local_client_target_missing", "publish requires an explicit local client target")
        if timeout <= 0 or timeout > MAX_TIMEOUT_SECONDS:
            _fail("invalid_timeout", f"timeout must be between 0 and {MAX_TIMEOUT_SECONDS} seconds")
        if poll <= 0 or poll > MAX_POLL_SECONDS:
            _fail("invalid_poll_interval", f"poll interval must be no greater than {MAX_POLL_SECONDS} seconds")
        temporary, _, manifest = _open_bundle(self.bundle)
        try:
            payload = _archive_bytes(self.bundle)
        finally:
            temporary.cleanup()
        transaction = transaction_id or ("tx-" + uuid.uuid4().hex)
        session = self.session_id or ("release-" + transaction)
        staged = self._wrapper("stage", transaction, manifest["release_id"], payload)
        if staged.get("staged") is not True:
            _fail("stage_unverified", "remote wrapper did not confirm staging")
        verified = self._wrapper("verify", transaction, manifest["release_id"])
        if verified.get("verified") is not True:
            _fail("verification_unverified", "remote wrapper did not confirm release verification")
        drain = self._control(manifest, "drain-enter", session=session, write=True)
        if drain.get("draining") is not True:
            _fail("drain_unverified", "control did not confirm admission drain")
        self._wait_for_convergence(manifest, timeout, poll)
        activation = self._wrapper("activate", transaction, manifest["release_id"])
        if activation.get("activated") is not True:
            _fail("activation_unverified", "remote wrapper did not confirm activation")
        try:
            self._health(manifest)
        except ReleaseError as health_error:
            try:
                rollback_result = self._wrapper("rollback", transaction, manifest["release_id"])
                self._verify_automatic_rollback(transaction, manifest["release_id"], rollback_result)
                # The wrapper has switched the control files back to the
                # retained previous release.  Validate that peer through the
                # explicit protocol-4 legacy contract, not the failed bundle's
                # release-specific action window.
                self._health(legacy_manifest())
            except ReleaseError as rollback_error:
                raise ReleaseError("rollback_failed", "remote health failed and the single rollback did not restore verified health", {
                    "health_error": health_error.as_dict(),
                    "rollback_error": rollback_error.as_dict(),
                    "draining": True,
                }) from rollback_error
            self._control(manifest, "drain-exit", session=session, write=True)
            return {
                "published": False,
                "rolled_back": True,
                "transaction_id": transaction,
                "release_id": manifest["release_id"],
                "rollback": rollback_result,
                "draining": False,
            }
        try:
            self._install_local_client(manifest)
        except ReleaseError as local_error:
            return {
                "published": False,
                "remote_healthy": True,
                "remote_release_id": activation.get("release_id", manifest["release_id"]),
                "transaction_id": transaction,
                "draining": True,
                "warnings": [{"code": "local_client_update_failed", "message": local_error.message, "detail": local_error.as_dict()}],
            }
        self._control(manifest, "drain-exit", session=session, write=True)
        return {
            "published": True,
            "rolled_back": False,
            "transaction_id": transaction,
            "release_id": manifest["release_id"],
            "local_client_updated": True,
            "draining": False,
        }

    def rollback(self, *, transaction_id: str, release_id: str) -> dict[str, Any]:
        temporary, _, manifest = _open_bundle(self.bundle)
        temporary.cleanup()
        if manifest["release_id"] == release_id:
            _fail("rollback_release_mismatch", "rollback bundle must identify the previous release, not the active release")
        session = self.session_id or ("release-" + transaction_id)
        self._control(manifest, "drain-enter", session=session, write=True)
        result = self._wrapper("rollback", transaction_id, release_id)
        if result.get("release_id") != manifest["release_id"]:
            _fail(
                "rollback_release_mismatch",
                "remote rollback target does not match the supplied previous release bundle",
                {"expected": manifest["release_id"], "actual": result.get("release_id")},
            )
        if result.get("previous_release_id") not in {None, release_id}:
            _fail(
                "rollback_release_mismatch",
                "remote rollback record does not identify the requested active release",
                {"expected": release_id, "actual": result.get("previous_release_id")},
            )
        self._health(manifest)
        try:
            self._install_local_client(manifest)
        except ReleaseError as local_error:
            return {
                "rolled_back": True,
                "remote_healthy": True,
                "draining": True,
                "warnings": [{"code": "local_client_update_failed", "message": local_error.message, "detail": local_error.as_dict()}],
                **result,
            }
        self._control(manifest, "drain-exit", session=session, write=True)
        return {"rolled_back": True, "local_client_updated": True, "draining": False, **result}


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="agent-vm-release")
    subparsers = parser.add_subparsers(dest="action", required=True)
    build = subparsers.add_parser("build")
    build.add_argument("--source-root", type=Path, default=Path.cwd())
    build.add_argument("--source-revision")
    build.add_argument("--output", type=Path, required=True)
    verify = subparsers.add_parser("verify")
    verify.add_argument("--bundle", type=Path, required=True)
    publish = subparsers.add_parser("publish")
    publish.add_argument("--bundle", type=Path, required=True)
    publish.add_argument("--control-host", required=True)
    publish.add_argument("--wrapper", default=CANONICAL_REMOTE_WRAPPER,
                         help=f"fixed remote wrapper path (default: {CANONICAL_REMOTE_WRAPPER})")
    publish.add_argument("--client-target", type=Path, required=True)
    publish.add_argument("--session-id")
    publish.add_argument("--transaction-id")
    publish.add_argument("--timeout", type=float, default=DEFAULT_TIMEOUT_SECONDS)
    publish.add_argument("--poll", type=float, default=DEFAULT_POLL_SECONDS)
    rollback = subparsers.add_parser("rollback")
    rollback.add_argument("--bundle", type=Path, required=True)
    rollback.add_argument("--control-host", required=True)
    rollback.add_argument("--wrapper", default=CANONICAL_REMOTE_WRAPPER,
                          help=f"fixed remote wrapper path (default: {CANONICAL_REMOTE_WRAPPER})")
    rollback.add_argument("--client-target", type=Path, required=True)
    rollback.add_argument("--session-id")
    rollback.add_argument("--transaction-id", required=True)
    rollback.add_argument("--release-id", required=True)
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        if args.action == "build":
            result = build_release(args.source_root, args.output, args.source_revision)
        elif args.action == "verify":
            result = verify_release(args.bundle)
        else:
            publisher = ReleasePublisher(
                bundle=args.bundle,
                control_host=args.control_host,
                wrapper_path=args.wrapper,
                client_target=args.client_target,
                session_id=args.session_id,
            )
            if args.action == "publish":
                result = publisher.publish(timeout=args.timeout, poll=args.poll, transaction_id=args.transaction_id)
            else:
                result = publisher.rollback(transaction_id=args.transaction_id, release_id=args.release_id)
        sys.stdout.write(json.dumps(result, sort_keys=True, indent=2) + "\n")
        return 0
    except ReleaseError as exc:
        sys.stderr.write(json.dumps({"ok": False, "error": exc.as_dict()}, sort_keys=True) + "\n")
        return 2
    except Exception as exc:
        sys.stderr.write(json.dumps({"ok": False, "error": {"code": "internal_error", "message": type(exc).__name__}}, sort_keys=True) + "\n")
        return 3


if __name__ == "__main__":
    raise SystemExit(main())
