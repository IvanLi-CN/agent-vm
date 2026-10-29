#!/usr/bin/env python3
"""Narrow root-owned activation wrapper for Agent VM control releases.

The wrapper is a deployment prerequisite.  It intentionally exposes only
stage, verify, activate, rollback, and status actions; it never accepts an
arbitrary destination, shell command, configuration file, or state database
path from the caller.
"""

from __future__ import annotations

import argparse
import base64
from contextlib import contextmanager
import fcntl
import hashlib
import io
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import sqlite3
import stat
import subprocess
import sys
import tarfile
import tempfile
from typing import Any, Mapping


try:
    from release_manifest import (
        BUN_PATH,
        MANIFEST_NAME,
        SUPPORT_ARTIFACT_NAMES,
        SUPPORT_ARTIFACT_MODES,
        CLIENT_PACKAGE_MODES,
        UNIT_CONTENT_SHA256,
        UNIT_EXEC_START,
        GO_FIXED_ARTIFACTS,
        GO_MANIFEST_FORMAT,
        GO_RETIRED_UNITS,
        GO_UNIT_CONTENT_SHA256,
        HISTORICAL_GO_UNIT_CONTENT_SHA256,
        HISTORICAL_PRODUCT_UNIT_CONTENT_SHA256,
        GO_UNIT_EXEC_START,
        LEGACY_PRODUCT_MANIFEST_FORMAT,
        PRODUCT_MANIFEST_FORMAT,
        STATE_CAPABILITY_MARKERS,
        PRODUCT_CLIENT_ARTIFACTS,
        PRODUCT_LINUX_ARTIFACTS,
        PRODUCT_LINUX_LEGACY_ARTIFACTS,
        PRODUCT_UNIT_CONTENT_SHA256,
        PRODUCT_UNIT_EXEC_START,
        ManifestError,
        load_release_manifest,
        sha256_file,
        verify_client_sidecar,
        verify_bundle,
        verify_go_bundle,
        verify_runtime_bundle,
        verify_typescript_syntax,
    )
except ModuleNotFoundError:
    sys.path.insert(0, str(Path(__file__).resolve().parent))
    from release_manifest import (
        BUN_PATH,
        MANIFEST_NAME,
        SUPPORT_ARTIFACT_NAMES,
        SUPPORT_ARTIFACT_MODES,
        CLIENT_PACKAGE_MODES,
        UNIT_CONTENT_SHA256,
        UNIT_EXEC_START,
        GO_FIXED_ARTIFACTS,
        GO_MANIFEST_FORMAT,
        GO_RETIRED_UNITS,
        GO_UNIT_CONTENT_SHA256,
        HISTORICAL_GO_UNIT_CONTENT_SHA256,
        HISTORICAL_PRODUCT_UNIT_CONTENT_SHA256,
        GO_UNIT_EXEC_START,
        LEGACY_PRODUCT_MANIFEST_FORMAT,
        PRODUCT_MANIFEST_FORMAT,
        STATE_CAPABILITY_MARKERS,
        PRODUCT_CLIENT_ARTIFACTS,
        PRODUCT_LINUX_ARTIFACTS,
        PRODUCT_LINUX_LEGACY_ARTIFACTS,
        PRODUCT_UNIT_CONTENT_SHA256,
        PRODUCT_UNIT_EXEC_START,
        ManifestError,
        load_release_manifest,
        sha256_file,
        verify_client_sidecar,
        verify_bundle,
        verify_go_bundle,
        verify_runtime_bundle,
        verify_typescript_syntax,
    )


DEFAULT_CONFIG_PATH = Path("/etc/agent-vm-native/release-wrapper.json")
CANONICAL_CONTROL_PATH = Path("/opt/agent-vm-native/agent_vm_control.ts")
RETIRED_CONTROL_NAME = "agent_vm_control.py"
RETIRED_CLIENT_NAME = "agent_vm.py"
CANONICAL_UNIT_DIR = Path("/etc/systemd/system")
RELEASE_FENCE_PATH = Path("/run/agent-vm-native/release-activation.fence")
GO_CONTROL_PATH = Path("/opt/agent-vm-native/bin/agent-vm-control")
GO_CONTROL_SOCKET = Path("/run/agent-vm-native/control.sock")
STATE_DB_PATH = Path("/var/lib/agent-vm-native/state.sqlite3")
LEGACY_EXPIRY_TIMER_RE = re.compile(r"^agent-vm-native-expire-[A-Za-z0-9_.:-]+\.timer$")
LEGACY_EXPIRY_SERVICE_RE = re.compile(r"^agent-vm-native-expire@[A-Za-z0-9_.:-]+\.service$")
ID_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$")
RELEASE_ID_RE = re.compile(r"^[0-9a-f]{40}$")
MAX_ARCHIVE_BYTES = 128 * 1024 * 1024
MAX_FILE_BYTES = 64 * 1024 * 1024
FORBIDDEN_CONFIG_NAMES = {"config.json", "state.sqlite3", "control.lock", "authority.lock", "admission.lock"}
FIXED_UNIT_NAMES = tuple(sorted(set(UNIT_EXEC_START) | {"agent-vm-native-age-gc.timer"}))
PRODUCT_UNIT_NAMES = ("agent-vm-native.service", "agent-vm-web.service")
GO_SUPPORT_ARTIFACT_NAMES = frozenset(
    path for _, path, role, _ in GO_FIXED_ARTIFACTS if role == "support"
)
LEGACY_ONLY_SUPPORT_NAMES = tuple(sorted(SUPPORT_ARTIFACT_NAMES - GO_SUPPORT_ARTIFACT_NAMES))
FIXED_ARTIFACTS = {
    ("client", "agent_vm.ts"),
    ("control", "agent_vm_control.ts"),
    *{("support", name) for name in SUPPORT_ARTIFACT_NAMES},
    *{("unit", "systemd/" + name) for name in FIXED_UNIT_NAMES},
}
FIXED_ARTIFACT_MODES = {
    ("client", "agent_vm.ts"): 0o755,
    ("control", "agent_vm_control.ts"): 0o755,
    **{("support", name): mode for name, mode in SUPPORT_ARTIFACT_MODES.items()},
    **{("unit", "systemd/" + name): 0o644 for name in FIXED_UNIT_NAMES},
}


def _is_go_manifest(manifest: Mapping[str, Any]) -> bool:
    return manifest.get("format") == GO_MANIFEST_FORMAT


def _is_product_manifest(manifest: Mapping[str, Any]) -> bool:
    return manifest.get("format") in {LEGACY_PRODUCT_MANIFEST_FORMAT, PRODUCT_MANIFEST_FORMAT}


def _legacy_runtime_artifact_paths(
    config: Mapping[str, Any],
    installed_root: Path,
    manifest: Mapping[str, Any],
) -> dict[str, Path]:
    """Map format-1 bundle paths to their split installed runtime paths."""
    paths: dict[str, Path] = {}
    for artifact in manifest["artifacts"]:
        relative = str(artifact["path"])
        if artifact["role"] == "unit":
            paths[relative] = Path(config["unit_dir"]) / Path(relative).name
        elif relative == "agent_vm_control.ts":
            paths[relative] = Path(config["control_path"])
        else:
            paths[relative] = installed_root / Path(*PurePosixPath(relative).parts)
    return paths


def _native_runtime_artifact_paths(
    config: Mapping[str, Any],
    manifest: Mapping[str, Any],
) -> dict[str, Path]:
    """Map native unit artifacts to the active systemd installation."""
    return {
        str(artifact["path"]): Path(config["unit_dir"]) / Path(str(artifact["path"])).name
        for artifact in manifest["artifacts"]
        if artifact["role"] == "unit"
    }


def _manifest_release_id(manifest: Mapping[str, Any]) -> str:
    return str(manifest["source_revision"] if _is_product_manifest(manifest) else manifest["release_id"])


def _go_control_target(config: Mapping[str, Any]) -> Path:
    configured = Path(config["control_path"])
    if configured == CANONICAL_CONTROL_PATH:
        return GO_CONTROL_PATH
    return configured.parent / "bin" / GO_CONTROL_PATH.name


def _go_bridge_target(config: Mapping[str, Any]) -> Path:
    configured = Path(config["control_path"])
    return configured.with_name("agent_vm_control_bridge.ts")


class WrapperError(Exception):
    def __init__(self, code: str, message: str, detail: Any = None):
        super().__init__(message)
        self.code = code
        self.message = message
        self.detail = detail

    def as_dict(self) -> dict[str, Any]:
        value: dict[str, Any] = {"code": self.code, "message": self.message}
        if self.detail is not None:
            value["detail"] = self.detail
        return value


def _fail(code: str, message: str, detail: Any = None) -> None:
    raise WrapperError(code, message, detail)


def _safe_id(value: Any, label: str) -> str:
    if not isinstance(value, str) or ID_RE.fullmatch(value) is None:
        _fail("invalid_request", f"{label} is invalid")
    return value


def _release_id(value: Any) -> str:
    if not isinstance(value, str) or RELEASE_ID_RE.fullmatch(value) is None:
        _fail("invalid_request", "release_id must be a 40-character lowercase Git revision")
    return value


def _absolute_path(value: Any, label: str) -> Path:
    if not isinstance(value, str) or not value.startswith("/") or "\x00" in value:
        _fail("wrapper_config_invalid", f"{label} must be an absolute path")
    path = Path(value)
    if any(part == ".." for part in path.parts):
        _fail("wrapper_config_invalid", f"{label} cannot contain parent traversal")
    return path


def _require_root_owned_path(path: Path, label: str, *, regular: bool = False, allow_symlink: bool = False) -> None:
    """Reject symlinked or writable-by-others configuration boundaries."""
    current = Path(path.anchor)
    for part in path.parts[1:]:
        current /= part
        try:
            info = current.lstat()
        except FileNotFoundError:
            break
        except OSError:
            _fail("wrapper_config_invalid", f"cannot inspect {label}")
        if stat.S_ISLNK(info.st_mode):
            if current == path and allow_symlink and info.st_uid == 0:
                return
            _fail("wrapper_config_invalid", f"{label} contains a symlink")
        if info.st_uid != 0 or stat.S_IMODE(info.st_mode) & 0o022:
            _fail("wrapper_config_invalid", f"{label} is not root-owned and private")
        if current == path and regular and not stat.S_ISREG(info.st_mode):
            _fail("wrapper_config_invalid", f"{label} is not a regular file")


def _require_directory_chain(path: Path, code: str, label: str) -> None:
    """Reject untrusted, writable, symlinked, or non-directory path boundaries."""
    current = path
    visited: set[Path] = set()
    allowed_owners = {0, os.geteuid()}
    while True:
        try:
            info = current.lstat()
        except FileNotFoundError:
            parent = current.parent
            if parent == current:
                return
            current = parent
            continue
        except OSError:
            _fail(code, f"cannot inspect {label}")
        if stat.S_ISLNK(info.st_mode):
            # macOS exposes /var as a root-owned link to /private/var. It is
            # safe to traverse only this fixed platform boundary; activation
            # directories themselves must never be symlinked.
            try:
                resolved = current.resolve(strict=True)
            except OSError:
                _fail(code, f"{label} contains an unresolved symlink")
            if current != Path("/var") or resolved != Path("/private/var") or info.st_uid not in allowed_owners:
                _fail(code, f"{label} contains a symlink")
            if resolved in visited:
                _fail(code, f"{label} contains a symlink cycle")
            visited.add(resolved)
            current = resolved
            continue
        if not stat.S_ISDIR(info.st_mode):
            _fail(code, f"{label} is not a directory")
        mode = stat.S_IMODE(info.st_mode)
        sticky_shared = info.st_uid == 0 and mode & 0o1000 and mode & 0o022
        if info.st_uid not in allowed_owners or (mode & 0o022 and not sticky_shared):
            _fail(code, f"{label} is not owned and private")
        if current == current.parent:
            return
        current = current.parent


def _activation_parent_dirs(config: Mapping[str, Any]) -> tuple[Path, ...]:
    bundle_root = Path(config["control_path"]).parent
    return tuple(dict.fromkeys((
        bundle_root,
        bundle_root / "bin",
        bundle_root / "config",
        bundle_root / "systemd",
        Path(config["unit_dir"]),
    )))


def _require_activation_parent_dirs(config: Mapping[str, Any], code: str) -> None:
    for path in _activation_parent_dirs(config):
        _require_directory_chain(path, code, f"activation parent directory: {path}")


def _paths_overlap(left: Path, right: Path) -> bool:
    # Compare configured path entries without following a current/previous
    # symlink into release_root.  Pointer targets are verified separately.
    left_absolute = Path(os.path.abspath(left))
    right_absolute = Path(os.path.abspath(right))
    return left_absolute == right_absolute or left_absolute in right_absolute.parents or right_absolute in left_absolute.parents


def _read_json(path: Path, code: str) -> dict[str, Any]:
    try:
        with path.open("r", encoding="utf-8") as handle:
            value = json.load(handle)
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        _fail(code, f"cannot read JSON: {path.name}")
    if not isinstance(value, dict):
        _fail(code, f"JSON root must be an object: {path.name}")
    return value


def load_config(path: Path = DEFAULT_CONFIG_PATH) -> dict[str, Any]:
    _require_root_owned_path(path, "wrapper config", regular=True)
    raw = _read_json(path, "wrapper_config_invalid")
    required = {
        "release_root",
        "staging_root",
        "current_link",
        "previous_link",
        "control_path",
        "unit_dir",
        "state_root",
    }
    missing = sorted(required - set(raw))
    if missing:
        _fail("wrapper_config_invalid", "wrapper config is missing required fields", {"fields": missing})
    config = {key: _absolute_path(raw[key], key) for key in required}
    owner_uid = raw.get("owner_uid", 0)
    if isinstance(owner_uid, bool) or not isinstance(owner_uid, int) or owner_uid < 0:
        _fail("wrapper_config_invalid", "owner_uid must be a non-negative integer")
    if owner_uid != 0:
        _fail("wrapper_config_invalid", "owner_uid must be root")
    config["owner_uid"] = owner_uid
    if raw.get("require_drained", True) is not True:
        _fail("wrapper_config_invalid", "release activation must require admission drain")
    config["require_drained"] = True
    if config["control_path"] != CANONICAL_CONTROL_PATH or config["unit_dir"] != CANONICAL_UNIT_DIR:
        _fail("wrapper_config_invalid", "control_path and unit_dir must use the fixed activation targets")
    configured_fence = _absolute_path(raw.get("fence_path", str(RELEASE_FENCE_PATH)), "fence_path")
    if configured_fence != RELEASE_FENCE_PATH:
        _fail("wrapper_config_invalid", "fence_path must use the fixed activation fence")
    config["fence_path"] = RELEASE_FENCE_PATH
    unit_names = raw.get("unit_names", list(FIXED_UNIT_NAMES))
    if not isinstance(unit_names, list) or not all(isinstance(item, str) for item in unit_names) or sorted(unit_names) != list(FIXED_UNIT_NAMES):
        _fail("wrapper_config_invalid", "unit_names must equal the fixed systemd unit set")
    config["unit_names"] = tuple(unit_names)
    for path_key in ("release_root", "staging_root", "state_root"):
        if config[path_key].name in FORBIDDEN_CONFIG_NAMES:
            _fail("wrapper_config_invalid", f"{path_key} cannot target durable control state")
    if config["current_link"].parent != config["release_root"].parent or config["previous_link"].parent != config["release_root"].parent:
        _fail("wrapper_config_invalid", "current and previous links must be release-root siblings")
    if _paths_overlap(config["current_link"], config["previous_link"]):
        _fail("wrapper_config_invalid", "current and previous links must be distinct")
    protected = tuple(config[key] for key in (
        "release_root", "staging_root", "state_root", "current_link", "previous_link", "control_path", "unit_dir", "fence_path",
    ))
    for index, left in enumerate(protected):
        for right in protected[index + 1:]:
            if _paths_overlap(left, right):
                _fail("wrapper_config_invalid", "wrapper paths must not overlap", {"left": str(left), "right": str(right)})
    for key, configured_path in ((key, config[key]) for key in config if isinstance(config[key], Path)):
        _require_root_owned_path(configured_path, key, allow_symlink=key in {"current_link", "previous_link"})
    _require_activation_parent_dirs(config, "wrapper_config_invalid")
    return config


def _mkdir_private(path: Path, mode: int = 0o700) -> None:
    try:
        if path.exists() or path.is_symlink():
            info = path.lstat()
            if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
                _fail("wrapper_io_error", f"private path is not a directory: {path.name}")
        path.mkdir(mode=mode, parents=True, exist_ok=True)
        os.chmod(path, mode)
    except OSError as exc:
        _fail("wrapper_io_error", f"cannot prepare private directory: {path.name}")


@contextmanager
def _release_lock(config: Mapping[str, Any]):
    """Serialize all root-owned release staging and activation transitions."""
    lock_root = Path(config["state_root"])
    _mkdir_private(lock_root)
    lock_path = lock_root / ".release-wrapper.lock"
    flags = os.O_RDWR | os.O_CREAT | getattr(os, "O_NOFOLLOW", 0)
    try:
        descriptor = os.open(lock_path, flags, 0o600)
    except OSError as exc:
        _fail("wrapper_lock_unavailable", "cannot open the release transaction lock")
    try:
        try:
            os.fchmod(descriptor, 0o600)
            fcntl.flock(descriptor, fcntl.LOCK_EX)
        except OSError as exc:
            _fail("wrapper_lock_unavailable", "cannot acquire the release transaction lock")
        yield
    finally:
        try:
            fcntl.flock(descriptor, fcntl.LOCK_UN)
        except OSError:
            pass
        try:
            os.close(descriptor)
        except OSError:
            pass


def _safe_member_path(name: str) -> Path:
    if not name or "\x00" in name or "\\" in name:
        _fail("unsafe_archive", "archive member name is invalid")
    parsed = PurePosixPath(name)
    if parsed.is_absolute() or any(part in {"", ".", ".."} for part in parsed.parts):
        _fail("unsafe_archive", "archive member escapes staging root", {"member": name})
    return Path(*parsed.parts)


def safe_extract_archive(payload: bytes, destination: Path) -> None:
    if len(payload) > MAX_ARCHIVE_BYTES:
        _fail("archive_too_large", "staging archive exceeds the fixed size limit")
    _mkdir_private(destination)
    seen: set[str] = set()
    total = 0
    try:
        archive = tarfile.open(fileobj=io.BytesIO(payload), mode="r:")
    except (OSError, tarfile.TarError) as exc:
        _fail("unsafe_archive", "staging input is not a readable tar archive")
    try:
        for member in archive.getmembers():
            relative = _safe_member_path(member.name)
            normalized = relative.as_posix()
            if normalized in seen:
                _fail("unsafe_archive", "archive contains duplicate members", {"member": normalized})
            seen.add(normalized)
            if not member.isfile() or member.issym() or member.islnk() or member.isdev() or member.isdir():
                _fail("unsafe_archive", "archive may contain regular files only", {"member": normalized})
            if member.size < 0 or member.size > MAX_FILE_BYTES:
                _fail("archive_too_large", "archive member exceeds the fixed size limit", {"member": normalized})
            total += member.size
            if total > MAX_ARCHIVE_BYTES:
                _fail("archive_too_large", "staging archive contents exceed the fixed size limit")
            target = destination / relative
            parent = target.parent
            _mkdir_private(parent, 0o700)
            source = archive.extractfile(member)
            if source is None:
                _fail("unsafe_archive", "archive member cannot be read", {"member": normalized})
            with target.open("xb") as handle:
                shutil.copyfileobj(source, handle, length=1024 * 1024)
                handle.flush()
                os.fsync(handle.fileno())
            os.chmod(target, member.mode & 0o777)
    finally:
        archive.close()


def _transaction_dir(config: Mapping[str, Any], transaction_id: str) -> Path:
    root = Path(config["staging_root"])
    _mkdir_private(root)
    return root / transaction_id


def _metadata_path(config: Mapping[str, Any], transaction_id: str) -> Path:
    return _transaction_dir(config, transaction_id) / ".wrapper-state.json"


def _fsync_directory(path: Path, code: str = "wrapper_io_error") -> None:
    """Persist a directory entry after an atomic rename or unlink."""
    flags = os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0)
    try:
        descriptor = os.open(path, flags)
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)
    except OSError:
        _fail(code, f"cannot persist release directory: {path.name}")


def _fsync_tree(root: Path) -> None:
    """Persist a newly copied release before publishing its directory entry."""
    try:
        files = [path for path in root.rglob("*") if path.is_file() and not path.is_symlink()]
        directories = [path for path in root.rglob("*") if path.is_dir() and not path.is_symlink()]
        for path in files:
            descriptor = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
            try:
                os.fsync(descriptor)
            finally:
                os.close(descriptor)
        for path in sorted(directories, key=lambda item: len(item.parts), reverse=True):
            _fsync_directory(path, "activation_failed")
        _fsync_directory(root, "activation_failed")
    except OSError:
        _fail("activation_failed", "cannot persist copied release files")


@contextmanager
def _activation_fence(config: Mapping[str, Any], transaction_id: str, release_id: str):
    """Keep control-plane drain exit fenced for the whole activation window."""
    path = Path(config.get("fence_path", RELEASE_FENCE_PATH))
    _mkdir_private(path.parent, 0o755)
    if path == RELEASE_FENCE_PATH:
        _require_root_owned_path(path.parent, "release activation fence directory")
    try:
        info = path.lstat()
    except FileNotFoundError:
        info = None
    except OSError:
        _fail("release_fence_unverified", "release activation fence cannot be inspected")
    if info is not None:
        if (path == RELEASE_FENCE_PATH and info.st_uid != 0) or stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o600:
            _fail("release_fence_unverified", "release activation fence is not a private regular file")
        existing = _read_json(path, "release_fence_unverified")
        if (
            existing.get("format") != 1
            or existing.get("transaction_id") != transaction_id
            or existing.get("release_id") != release_id
        ):
            _fail("release_activation_in_progress", "another release activation fence is active")
    else:
        flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0)
        try:
            descriptor = os.open(path, flags, 0o600)
            with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
                json.dump({"format": 1, "transaction_id": transaction_id, "release_id": release_id}, handle, sort_keys=True)
                handle.write("\n")
                handle.flush()
                os.fsync(handle.fileno())
            _fsync_directory(path.parent)
        except FileExistsError:
            _fail("release_activation_in_progress", "another release activation fence is active")
        except OSError:
            _fail("release_fence_unavailable", "release activation fence cannot be created")
    try:
        yield
    finally:
        try:
            path.unlink()
            _fsync_directory(path.parent)
        except FileNotFoundError:
            pass
        except OSError:
            _fail("release_fence_cleanup_failed", "release activation fence could not be cleared")


def _write_json(path: Path, value: Mapping[str, Any], mode: int = 0o600) -> None:
    _mkdir_private(path.parent)
    temporary = path.with_name("." + path.name + ".tmp")
    try:
        with temporary.open("w", encoding="utf-8") as handle:
            json.dump(dict(value), handle, sort_keys=True)
            handle.write("\n")
            handle.flush()
            os.fchmod(handle.fileno(), mode)
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        _fsync_directory(path.parent)
    except OSError as exc:
        _fail("wrapper_io_error", f"cannot write wrapper record: {path.name}")


def _load_transaction(config: Mapping[str, Any], transaction_id: str) -> dict[str, Any]:
    path = _metadata_path(config, transaction_id)
    try:
        info = path.lstat()
    except OSError:
        _fail("transaction_missing", "release transaction has not been staged")
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
        _fail("transaction_missing", "release transaction has not been staged")
    return _read_json(path, "transaction_invalid")


def _require_owned_regular(path: Path, owner_uid: int, mode: int | None = None) -> None:
    try:
        info = path.lstat()
    except OSError as exc:
        _fail("artifact_unverified", f"artifact cannot be inspected: {path.name}")
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
        _fail("artifact_unverified", f"artifact is not a regular file: {path.name}")
    if info.st_uid != owner_uid:
        _fail("artifact_owner_mismatch", f"artifact owner differs: {path.name}", {"expected": owner_uid, "actual": info.st_uid})
    if mode is not None and stat.S_IMODE(info.st_mode) != mode:
        _fail("artifact_mode_mismatch", f"artifact mode differs: {path.name}", {"expected": mode, "actual": stat.S_IMODE(info.st_mode)})


def _require_owned_regular_modes(path: Path, owner_uid: int, modes: tuple[int, ...]) -> None:
    """Validate a fixed cleanup target against a small historical mode set."""
    _require_owned_regular(path, owner_uid)
    try:
        actual = stat.S_IMODE(path.lstat().st_mode)
    except OSError:
        _fail("artifact_unverified", f"artifact cannot be inspected: {path.name}")
    if actual not in modes:
        _fail("artifact_mode_mismatch", f"artifact mode differs: {path.name}", {"expected": list(modes), "actual": actual})


def _artifact_runtime_path(
    root: Path,
    artifact: Mapping[str, Any],
    artifact_paths: Mapping[str, Path] | None = None,
) -> Path:
    if artifact_paths is not None:
        path = artifact_paths.get(str(artifact["path"]))
        if path is not None:
            return path
    return root / Path(*PurePosixPath(str(artifact["path"])).parts)


def _verify_unit_contents(
    root: Path,
    manifest: Mapping[str, Any],
    *,
    allow_historical_content: bool = False,
    artifact_paths: Mapping[str, Path] | None = None,
) -> None:
    if _is_product_manifest(manifest):
        expected_units = PRODUCT_UNIT_EXEC_START if manifest["format"] == PRODUCT_MANIFEST_FORMAT else {"agent-vm-native.service": PRODUCT_UNIT_EXEC_START["agent-vm-native.service"]}
        expected_hashes = PRODUCT_UNIT_CONTENT_SHA256
        historical_hashes = HISTORICAL_PRODUCT_UNIT_CONTENT_SHA256
        for item in manifest["artifacts"]:
            if item["role"] != "unit":
                continue
            name = Path(item["path"]).name
            if name not in expected_units:
                _fail("unit_unverified", f"product unit is not in the fixed allowlist: {name}")
            path = _artifact_runtime_path(root, item, artifact_paths)
            try:
                content = path.read_text(encoding="utf-8")
            except (OSError, UnicodeError):
                _fail("unit_unverified", f"unit cannot be read: {name}")
            accepted_hashes = {expected_hashes[name]}
            if allow_historical_content and name in historical_hashes:
                accepted_hashes.update(historical_hashes[name])
            if sha256_file(path) not in accepted_hashes:
                _fail("unit_unverified", f"unit bytes differ from the fixed product policy: {name}")
            if expected_units[name] not in content or "ExecStart=" not in content:
                _fail("unit_unverified", f"unit does not contain its fixed ExecStart: {name}")
            if any(token in content for token in ("systemctl stop", "systemctl disable", "ExecStop=", "ssh ", "curl ", "wget ")):
                _fail("unit_unverified", f"unit contains a forbidden control path: {name}")
        return
    if _is_go_manifest(manifest):
        for item in manifest["artifacts"]:
            if item["role"] != "unit":
                continue
            name = Path(item["path"]).name
            if name not in GO_UNIT_EXEC_START:
                _fail("unit_unverified", f"Go unit is not in the fixed allowlist: {name}")
            path = _artifact_runtime_path(root, item, artifact_paths)
            try:
                content = path.read_text(encoding="utf-8")
            except (OSError, UnicodeError):
                _fail("unit_unverified", f"unit cannot be read: {name}")
            accepted_hashes = {GO_UNIT_CONTENT_SHA256[name]}
            if allow_historical_content and name in HISTORICAL_GO_UNIT_CONTENT_SHA256:
                accepted_hashes.update(HISTORICAL_GO_UNIT_CONTENT_SHA256[name])
            if sha256_file(path) not in accepted_hashes:
                _fail("unit_unverified", f"unit bytes differ from the fixed Go policy: {name}")
            if GO_UNIT_EXEC_START[name] not in content or "ExecStart=" not in content:
                _fail("unit_unverified", f"unit does not contain its fixed ExecStart: {name}")
            if any(token in content for token in ("systemctl stop", "systemctl disable", "ExecStop=", "ssh ", "curl ", "wget ")):
                _fail("unit_unverified", f"unit contains a forbidden control path: {name}")
        return
    for item in manifest["artifacts"]:
        if item["role"] != "unit":
            continue
        name = Path(item["path"]).name
        if name not in UNIT_EXEC_START and name != "agent-vm-native-age-gc.timer":
            _fail("unit_unverified", f"unit is not in the fixed allowlist: {name}")
        path = _artifact_runtime_path(root, item, artifact_paths)
        try:
            content = path.read_text(encoding="utf-8")
        except (OSError, UnicodeError):
            _fail("unit_unverified", f"unit cannot be read: {name}")
        try:
            digest = sha256_file(path)
        except ManifestError:
            _fail("unit_unverified", f"unit cannot be hashed: {name}")
        if digest != UNIT_CONTENT_SHA256[name]:
            _fail("unit_unverified", f"unit bytes differ from the fixed policy: {name}")
        required = UNIT_EXEC_START.get(name, "Unit=agent-vm-native-age-gc.service")
        if required not in content or (name != "agent-vm-native-age-gc.timer" and "ExecStart=" not in content):
            _fail("unit_unverified", f"unit does not contain its fixed ExecStart: {name}")
        if any(token in content for token in ("systemctl stop", "systemctl disable", "ExecStop=", "ssh ", "curl ", "wget ")):
            _fail("unit_unverified", f"unit contains a forbidden control path: {name}")


def _reject_transaction_extras(
    root: Path,
    manifest: Mapping[str, Any],
    *,
    allowed_extra_paths: set[str] | None = None,
) -> None:
    allowed = {MANIFEST_NAME, "client-release.json", ".wrapper-state.json", ".verified.json"} | {
        item["path"] for item in manifest["artifacts"]
    }
    allowed.update(allowed_extra_paths or set())
    for path in root.rglob("*"):
        if path.is_symlink():
            _fail("artifact_unverified", "transaction contains a symlink", {"path": path.relative_to(root).as_posix()})
        if path.is_file() and path.relative_to(root).as_posix() not in allowed:
            _fail("artifact_unverified", "transaction contains an unallowlisted file", {"path": path.relative_to(root).as_posix()})


def _verify_fixed_artifacts(
    config: Mapping[str, Any],
    manifest: Mapping[str, Any],
    *,
    allow_historical_web_config_mode: bool = False,
) -> None:
    declared = {(item["role"], item["path"]) for item in manifest["artifacts"]}
    if _is_product_manifest(manifest):
        expected_values = PRODUCT_LINUX_ARTIFACTS if manifest["format"] == PRODUCT_MANIFEST_FORMAT else PRODUCT_LINUX_LEGACY_ARTIFACTS
        expected = {(role, path) for _, path, role, _ in expected_values}
        if declared != expected:
            _fail("artifact_unverified", "manifest artifact set differs from the fixed product release allowlist", {"expected": sorted(expected), "actual": sorted(declared)})
        expected_modes = {(role, path): mode for _, path, role, mode in expected_values}
        for item in manifest["artifacts"]:
            expected_mode = expected_modes[(item["role"], item["path"])]
            historical_mode = (
                allow_historical_web_config_mode
                and manifest["format"] == PRODUCT_MANIFEST_FORMAT
                and item["path"] == "config/agent-vm-web.json.example"
                and item["mode"] == 0o644
            )
            if item["mode"] != expected_mode and not historical_mode:
                _fail("artifact_mode_mismatch", f"fixed product release artifact mode differs: {item['path']}")
        return
    if _is_go_manifest(manifest):
        expected = {(role, path) for _, path, role, _ in GO_FIXED_ARTIFACTS}
        if declared != expected:
            _fail(
                "artifact_unverified",
                "manifest artifact set differs from the fixed Go release allowlist",
                {"expected": sorted(expected), "actual": sorted(declared)},
            )
        expected_modes = {(role, path): mode for _, path, role, mode in GO_FIXED_ARTIFACTS}
        for item in manifest["artifacts"]:
            expected_mode = expected_modes[(item["role"], item["path"])]
            if item["mode"] != expected_mode:
                _fail(
                    "artifact_mode_mismatch",
                    f"fixed Go release artifact mode differs: {item['path']}",
                    {"expected": expected_mode, "actual": item["mode"]},
                )
        return
    if declared != FIXED_ARTIFACTS:
        _fail(
            "artifact_unverified",
            "manifest artifact set differs from the fixed release allowlist",
            {"expected": sorted(FIXED_ARTIFACTS), "actual": sorted(declared)},
        )
    for item in manifest["artifacts"]:
        expected_mode = FIXED_ARTIFACT_MODES[(item["role"], item["path"])]
        if item["mode"] != expected_mode:
            _fail(
                "artifact_mode_mismatch",
                f"fixed release artifact mode differs: {item['path']}",
                {"expected": expected_mode, "actual": item["mode"]},
            )


def _verify_python_syntax(root: Path, manifest: Mapping[str, Any]) -> None:
    for item in manifest["artifacts"]:
        if item["role"] not in {"client", "control", "support"}:
            continue
        if PurePosixPath(item["path"]).suffix != ".py":
            continue
        path = root / Path(*PurePosixPath(item["path"]).parts)
        try:
            source = path.read_text(encoding="utf-8")
            compile(source, str(path), "exec")
        except (OSError, UnicodeError, SyntaxError) as exc:
            _fail("python_syntax_invalid", f"Python artifact cannot be compiled: {item['path']}")


def _verify_typescript_syntax(root: Path, manifest: Mapping[str, Any]) -> None:
    try:
        verify_typescript_syntax(root, manifest, bun_path=BUN_PATH)
    except ManifestError as exc:
        _fail(exc.code, exc.message, exc.detail)


def _verify_transaction(config: Mapping[str, Any], transaction_id: str, release_id: str) -> dict[str, Any]:
    transaction = _load_transaction(config, transaction_id)
    if transaction.get("release_id") != release_id:
        _fail("transaction_conflict", "transaction is bound to another release")
    root = _transaction_dir(config, transaction_id)
    try:
        manifest = load_release_manifest(root / MANIFEST_NAME)
        if _is_go_manifest(manifest):
            verify_go_bundle(root, manifest)
        elif _is_product_manifest(manifest):
            from release_manifest import verify_product_bundle
            verify_product_bundle(root, manifest)
        else:
            verify_bundle(root, manifest)
        verify_client_sidecar(root, manifest)
    except ManifestError as exc:
        _fail(exc.code, exc.message, exc.detail)
    _reject_transaction_extras(root, manifest)
    _verify_fixed_artifacts(config, manifest)
    _verify_python_syntax(root, manifest)
    _verify_typescript_syntax(root, manifest)
    _verify_unit_contents(root, manifest)
    owner_uid = int(config["owner_uid"])
    _require_owned_regular(root / MANIFEST_NAME, owner_uid, 0o644)
    _require_owned_regular(root / "client-release.json", owner_uid, 0o644)
    for item in manifest["artifacts"]:
        path = root / Path(*PurePosixPath(item["path"]).parts)
        _require_owned_regular(path, owner_uid, item["mode"])
    checked = {
        "transaction_id": transaction_id,
        "release_id": _manifest_release_id(manifest),
        "manifest_sha256": sha256_file(root / MANIFEST_NAME),
        "verified": True,
    }
    _write_json(root / ".verified.json", checked)
    return checked


def verify_transaction(config: Mapping[str, Any], transaction_id: str, release_id: str) -> dict[str, Any]:
    with _release_lock(config):
        return _verify_transaction(config, transaction_id, release_id)


def stage(config: Mapping[str, Any], transaction_id: str, release_id: str, payload: bytes) -> dict[str, Any]:
    transaction_id = _safe_id(transaction_id, "transaction_id")
    release_id = _release_id(release_id)
    with _release_lock(config):
        destination = _transaction_dir(config, transaction_id)
        if destination.exists():
            _fail("transaction_exists", "transaction staging directory already exists")
        temporary = destination.with_name("." + transaction_id + ".staging")
        if temporary.exists():
            _fail("transaction_exists", "temporary transaction staging directory already exists")
        try:
            safe_extract_archive(payload, temporary)
            manifest = load_release_manifest(temporary / MANIFEST_NAME)
            if _manifest_release_id(manifest) != release_id:
                _fail("release_mismatch", "staged manifest does not match requested release")
            _write_json(temporary / ".wrapper-state.json", {"transaction_id": transaction_id, "release_id": release_id, "staged": True})
            os.replace(temporary, destination)
            _fsync_directory(destination.parent)
        except ManifestError as exc:
            _fail(exc.code, exc.message, exc.detail)
        except OSError as exc:
            _fail("wrapper_io_error", "cannot commit staged release")
        finally:
            if temporary.exists():
                shutil.rmtree(temporary, ignore_errors=True)
        return {"staged": True, "transaction_id": transaction_id, "release_id": release_id}


def _current_target(link: Path, release_root: Path | None = None) -> Path | None:
    if not link.exists() and not link.is_symlink():
        return None
    if not link.is_symlink():
        _fail("activation_unverified", f"release pointer is not a symlink: {link.name}")
    target = link.resolve()
    if not target.is_dir():
        _fail("activation_unverified", f"release pointer target is missing: {link.name}")
    if release_root is not None:
        root = release_root.resolve()
        try:
            target.relative_to(root)
        except ValueError:
            _fail("activation_unverified", f"release pointer escapes the release root: {link.name}")
        if RELEASE_ID_RE.fullmatch(target.name) is None:
            _fail("activation_unverified", f"release pointer target has an invalid release id: {link.name}")
    return target


def _verify_retained_release(config: Mapping[str, Any], target: Path) -> dict[str, Any]:
    """Re-verify a release before using it as an activation or rollback target."""
    try:
        manifest = load_release_manifest(target / MANIFEST_NAME, allow_historical_web_config_mode=True)
        if _is_go_manifest(manifest):
            verify_go_bundle(target, manifest)
        elif _is_product_manifest(manifest):
            from release_manifest import verify_product_bundle
            verify_product_bundle(target, manifest, allow_historical_web_config_mode=True)
        else:
            verify_bundle(target, manifest)
        verify_client_sidecar(target, manifest, allow_historical_web_config_mode=True)
    except ManifestError as exc:
        _fail("activation_unverified", "retained release cannot be verified", exc.as_dict())
    _reject_transaction_extras(target, manifest)
    _verify_fixed_artifacts(config, manifest, allow_historical_web_config_mode=True)
    _verify_python_syntax(target, manifest)
    _verify_typescript_syntax(target, manifest)
    _verify_unit_contents(target, manifest, allow_historical_content=True)
    owner_uid = int(config["owner_uid"])
    _require_owned_regular(target / MANIFEST_NAME, owner_uid, 0o644)
    _require_owned_regular(target / "client-release.json", owner_uid, 0o644)
    for item in manifest["artifacts"]:
        _require_owned_regular(target / Path(*PurePosixPath(item["path"]).parts), owner_uid, item["mode"])
    return manifest


def _atomic_symlink(target: Path, link: Path) -> None:
    temporary = link.with_name("." + link.name + ".tmp")
    _require_directory_chain(link.parent, "activation_failed", f"release pointer parent: {link.parent}")
    try:
        if temporary.exists() or temporary.is_symlink():
            temporary.unlink()
        os.symlink(target, temporary)
        os.replace(temporary, link)
        _fsync_directory(link.parent, "activation_failed")
    except OSError as exc:
        _fail("activation_failed", f"cannot atomically update release pointer: {link.name}")


def _copy_atomic(source: Path, target: Path, owner_uid: int, mode: int) -> None:
    _require_directory_chain(target.parent, "activation_unverified", f"activation target parent: {target.parent}")
    target.parent.mkdir(mode=0o755, parents=True, exist_ok=True)
    _require_directory_chain(target.parent, "activation_unverified", f"activation target parent: {target.parent}")
    if target.exists() or target.is_symlink():
        try:
            info = target.lstat()
        except OSError:
            _fail("activation_unverified", f"cannot inspect activation target: {target.name}")
        if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
            _fail("activation_unverified", f"activation target is not a regular file: {target.name}")
    descriptor, name = tempfile.mkstemp(prefix="." + target.name + ".", suffix=".release-tmp", dir=target.parent)
    temporary = Path(name)
    try:
        with os.fdopen(descriptor, "wb") as target_handle, source.open("rb") as source_handle:
            shutil.copyfileobj(source_handle, target_handle, length=1024 * 1024)
            target_handle.flush()
            os.fchmod(target_handle.fileno(), mode)
            if hasattr(os, "fchown"):
                os.fchown(target_handle.fileno(), owner_uid, -1)
            os.fsync(target_handle.fileno())
        os.replace(temporary, target)
        _fsync_directory(target.parent, "activation_failed")
    except OSError as exc:
        try:
            if temporary.exists():
                temporary.unlink()
        except OSError:
            pass
        _fail("activation_failed", f"cannot atomically install artifact: {target.name}")


def _remove_owned_target(path: Path, owner_uid: int, modes: tuple[int, ...]) -> None:
    """Remove one fixed predecessor artifact after checking its ownership and mode."""
    _require_directory_chain(path.parent, "activation_unverified", f"activation target parent: {path.parent}")
    if not path.exists() and not path.is_symlink():
        return
    _require_owned_regular_modes(path, owner_uid, modes)
    try:
        path.unlink()
        _fsync_directory(path.parent, "activation_failed")
    except OSError:
        _fail("activation_failed", f"cannot retire the exact predecessor artifact: {path.name}")


def _remove_native_predecessor_files(config: Mapping[str, Any], owner_uid: int) -> None:
    """Remove the exact native artifacts when returning to the format-1 runtime."""
    bundle_root = Path(config["control_path"]).parent
    targets: list[tuple[Path, tuple[int, ...]]] = [
        (bundle_root / "agent_vm_control_bridge.ts", (0o755,)),
        (_go_control_target(config), (0o755,)),
        (Path(config["unit_dir"]) / "agent-vm-native.service", (0o644,)),
        (Path(config["unit_dir"]) / "agent-vm-web.service", (0o644,)),
        (bundle_root / "systemd" / "agent-vm-native.service", (0o644,)),
        (bundle_root / "systemd" / "agent-vm-web.service", (0o644,)),
        (bundle_root / "config" / "agent-vm-web.json.example", (0o640, 0o644)),
        (bundle_root / "web-assets.manifest.json", (0o644,)),
    ]
    for path, modes in targets:
        _remove_owned_target(path, owner_uid, modes)


def _remove_legacy_predecessor_files(config: Mapping[str, Any], owner_uid: int) -> None:
    """Remove exact format-1-only files before installing a native runtime."""
    bundle_root = Path(config["control_path"]).parent
    targets = [
        (bundle_root / name, (SUPPORT_ARTIFACT_MODES[name],))
        for name in LEGACY_ONLY_SUPPORT_NAMES
    ]
    targets.extend(
        (bundle_root / "systemd" / name, (0o644,))
        for name in FIXED_UNIT_NAMES
    )
    for path, modes in targets:
        _remove_owned_target(path, owner_uid, modes)


def _activation_targets(config: Mapping[str, Any]) -> tuple[Path, ...]:
    bundle_root = Path(config["control_path"]).parent
    targets = (
        Path(config["control_path"]),
        _go_control_target(config),
        Path(config["control_path"]).with_name("agent_vm_control_bridge.ts"),
        Path(config["control_path"]).with_name(RETIRED_CONTROL_NAME),
        Path(config["control_path"]).with_name(RETIRED_CLIENT_NAME),
        Path(config["control_path"]).with_name("agent_vm.ts"),
        *(Path(config["control_path"]).with_name(name) for name in sorted(SUPPORT_ARTIFACT_NAMES)),
        Path(config["control_path"]).with_name(MANIFEST_NAME),
        Path(config["control_path"]).with_name("client-release.json"),
    ) + tuple(Path(config["unit_dir"]) / name for name in (*config["unit_names"], "agent-vm-native.service", "agent-vm-web.service")) + tuple(
        [bundle_root / "systemd" / name for name in (*FIXED_UNIT_NAMES, *PRODUCT_UNIT_NAMES)]
        + [
            bundle_root / "config" / "agent-vm-web.json.example",
            bundle_root / "web-assets.manifest.json",
        ]
    )
    return tuple(dict.fromkeys(targets))


def _snapshot_targets(config: Mapping[str, Any]) -> dict[Path, tuple[bytes, int, int] | None]:
    snapshot: dict[Path, tuple[bytes, int, int] | None] = {}
    for target in _activation_targets(config):
        _require_directory_chain(target.parent, "activation_unverified", f"activation target parent: {target.parent}")
        if not target.exists() and not target.is_symlink():
            snapshot[target] = None
            continue
        try:
            info = target.lstat()
            if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
                _fail("activation_unverified", f"activation target is not a regular file: {target.name}")
            snapshot[target] = (target.read_bytes(), stat.S_IMODE(info.st_mode), info.st_uid)
        except OSError as exc:
            _fail("activation_unverified", f"activation target cannot be snapshotted: {target.name}")
    return snapshot


def _restore_targets(snapshot: Mapping[Path, tuple[bytes, int, int] | None]) -> None:
    for target, saved in snapshot.items():
        _require_directory_chain(target.parent, "activation_restore_failed", f"activation target parent: {target.parent}")
        if saved is None:
            try:
                if target.exists() or target.is_symlink():
                    target.unlink()
                    _fsync_directory(target.parent, "activation_restore_failed")
            except OSError as exc:
                _fail("activation_restore_failed", f"cannot remove a partially installed target: {target.name}")
            continue
        data, mode, owner_uid = saved
        temporary = None
        try:
            target.parent.mkdir(mode=0o755, parents=True, exist_ok=True)
            _require_directory_chain(target.parent, "activation_restore_failed", f"activation target parent: {target.parent}")
            descriptor, name = tempfile.mkstemp(prefix="." + target.name + ".", suffix=".restore-tmp", dir=target.parent)
            temporary = Path(name)
            with os.fdopen(descriptor, "wb") as handle:
                handle.write(data)
                handle.flush()
                os.fchmod(handle.fileno(), mode)
                if hasattr(os, "fchown"):
                    os.fchown(handle.fileno(), owner_uid, -1)
                os.fsync(handle.fileno())
            os.replace(temporary, target)
            _fsync_directory(target.parent, "activation_restore_failed")
        except OSError as exc:
            try:
                if temporary is not None and temporary.exists():
                    temporary.unlink()
            except OSError:
                pass
            _fail("activation_restore_failed", f"cannot restore activation target: {target.name}")


def _restore_pointer(link: Path, target: Path | None) -> None:
    if target is None:
        _require_directory_chain(link.parent, "activation_restore_failed", f"release pointer parent: {link.parent}")
        try:
            if link.exists() or link.is_symlink():
                link.unlink()
                _fsync_directory(link.parent, "activation_restore_failed")
        except OSError as exc:
            _fail("activation_restore_failed", f"cannot remove release pointer: {link.name}")
    else:
        _atomic_symlink(target, link)


def _restore_activation_state(
    config: Mapping[str, Any],
    current_link: Path,
    previous_link: Path,
    old_current: Path | None,
    old_previous: Path | None,
    target_snapshot: Mapping[Path, tuple[bytes, int, int] | None],
) -> None:
    _restore_targets(target_snapshot)
    _restore_pointer(current_link, old_current)
    _restore_pointer(previous_link, old_previous)
    _daemon_reload()


def _validate_state_compatibility(manifest: Mapping[str, Any], capabilities: Any) -> None:
    if not isinstance(capabilities, list) or not capabilities or not all(isinstance(item, str) and item for item in capabilities):
        _fail("state_compatibility_unverified", "control response did not provide readable state capabilities")
    if len(set(capabilities)) != len(capabilities):
        _fail("state_compatibility_unverified", "control response did not provide readable state capabilities")
    if manifest.get("format") == PRODUCT_MANIFEST_FORMAT:
        compatibility = manifest.get("state_compatibility")
        if not isinstance(compatibility, Mapping):
            _fail("state_compatibility_unverified", "target product manifest has no state compatibility declaration")
        readable = compatibility.get("reads")
        writable = compatibility.get("writes")
        if (
            not isinstance(readable, list)
            or not isinstance(writable, list)
            or not all(isinstance(item, str) and item for item in readable)
            or not all(isinstance(item, str) and item for item in writable)
        ):
            _fail("state_compatibility_unverified", "target product manifest state compatibility is unreadable")
        supported = set(readable) & set(writable)
    elif manifest.get("format") == LEGACY_PRODUCT_MANIFEST_FORMAT:
        # Format 3 predates the management schema and can preserve only the
        # legacy core state. It must never be allowed to activate over a
        # database containing newer management capabilities.
        supported = {STATE_CAPABILITY_MARKERS[0]}
    else:
        supported = {STATE_CAPABILITY_MARKERS[0]}
    for capability in capabilities:
        if capability not in supported:
            _fail(
                "state_compatibility_mismatch",
                "target release cannot preserve a database capability",
                {"capability": capability, "release_format": manifest.get("format")},
            )


def _read_live_state_capabilities() -> list[str]:
    """Read the fixed state registry when an older controller omits it in drain-status."""
    uri = f"file:{STATE_DB_PATH}?mode=ro"
    try:
        with sqlite3.connect(uri, uri=True, timeout=30) as database:
            rows = database.execute(
                "SELECT name, version FROM state_capabilities ORDER BY name, version",
            ).fetchall()
    except sqlite3.OperationalError as exc:
        if "no such table" in str(exc).lower() and STATE_CAPABILITY_MARKERS:
            try:
                with sqlite3.connect(uri, uri=True, timeout=30) as database:
                    _validate_legacy_state_schema(database)
            except (OSError, sqlite3.Error):
                _fail("state_compatibility_unverified", "live state schema could not be validated")
            return [STATE_CAPABILITY_MARKERS[0]]
        _fail("state_compatibility_unverified", "live state capabilities could not be read")
    except (OSError, sqlite3.Error):
        _fail("state_compatibility_unverified", "live state capabilities could not be read")
    return [f"{name}@{version}" for name, version in rows]


def _validate_legacy_state_schema(database: sqlite3.Connection) -> None:
    required = {
        "meta": {"key", "value"},
        "tasks": {
            "task_id", "session_id", "generation", "state", "instance_name", "vm_ip",
            "ssh_host_port", "lease_expires_at", "inactive_since", "cleanup_after", "created_at",
            "updated_at", "last_error", "image_fingerprint", "resources_json", "reservation_bytes",
            "previous_generation", "resource_reservation_json",
        },
        "ports": {
            "task_id", "generation", "port_name", "protocol", "host_port", "vm_port",
            "device_name", "state", "created_at",
        },
        "operations": {"operation_id", "action", "session_id", "generation", "response_json", "created_at"},
        "audit": {"id", "action", "task_id", "session_id", "generation", "outcome", "detail_json", "created_at"},
    }
    for table, columns in required.items():
        row = database.execute("SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?", (table,)).fetchone()
        if row is None:
            _fail("state_compatibility_unverified", "live state schema is incomplete")
        present = {str(item[1]) for item in database.execute(f"PRAGMA table_info({table})").fetchall()}
        if not columns.issubset(present):
            _fail("state_compatibility_unverified", "live state schema is incomplete")


def _verify_installed_release(
    config: Mapping[str, Any],
    installed_root: Path,
    manifest: Mapping[str, Any],
) -> dict[str, Any]:
    """Verify the split runtime layout before using its control authority."""
    _require_activation_parent_dirs(config, "drain_unverified")
    runtime_paths: Mapping[str, Path] | None = None
    if _is_go_manifest(manifest):
        runtime_paths = _native_runtime_artifact_paths(config, manifest)
        verify_go_bundle(installed_root, manifest)
    elif _is_product_manifest(manifest):
        target = manifest.get("target")
        if not isinstance(target, Mapping) or target.get("os") != "linux":
            _fail("drain_unverified", "installed product release is not a native Linux package")
        runtime_paths = _native_runtime_artifact_paths(config, manifest)
        from release_manifest import verify_product_bundle
        verify_product_bundle(
            installed_root,
            manifest,
            allow_historical_web_config_mode=True,
        )
    else:
        runtime_paths = _legacy_runtime_artifact_paths(config, installed_root, manifest)
        verify_runtime_bundle(manifest, runtime_paths)
    checked = verify_client_sidecar(
        installed_root,
        manifest,
        allow_historical_web_config_mode=True,
    )
    historical_legacy_residue = {
        name for name in LEGACY_ONLY_SUPPORT_NAMES
        if (installed_root / name).exists() or (installed_root / name).is_symlink()
    } if (_is_go_manifest(checked) or _is_product_manifest(checked)) else set()
    bridge_alias = installed_root / "agent_vm_control.ts"
    allowed_extra_paths = set(historical_legacy_residue)
    if (_is_go_manifest(checked) or _is_product_manifest(checked)) and (bridge_alias.exists() or bridge_alias.is_symlink()):
        allowed_extra_paths.add(bridge_alias.name)
    _reject_transaction_extras(installed_root, checked, allowed_extra_paths=allowed_extra_paths)
    _verify_fixed_artifacts(config, checked, allow_historical_web_config_mode=True)
    _verify_python_syntax(installed_root, checked)
    _verify_typescript_syntax(installed_root, checked)
    _verify_unit_contents(
        installed_root,
        checked,
        allow_historical_content=True,
        artifact_paths=runtime_paths,
    )
    owner_uid = int(config["owner_uid"])
    _require_owned_regular(installed_root / MANIFEST_NAME, owner_uid, 0o644)
    _require_owned_regular(installed_root / "client-release.json", owner_uid, 0o644)
    for name in historical_legacy_residue:
        _require_owned_regular(installed_root / name, owner_uid, SUPPORT_ARTIFACT_MODES[name])
    for item in checked["artifacts"]:
        _require_owned_regular(
            _artifact_runtime_path(installed_root, item, runtime_paths),
            owner_uid,
            item["mode"],
        )
    if (_is_go_manifest(checked) or _is_product_manifest(checked)) and (bridge_alias.exists() or bridge_alias.is_symlink()):
        bridge = next(
            (item for item in checked["artifacts"] if item["path"] == "agent_vm_control_bridge.ts"),
            None,
        )
        if bridge is None:
            _fail("artifact_unverified", "installed compatibility bridge alias has no declared bridge")
        _require_owned_regular(bridge_alias, owner_uid, bridge["mode"])
    return checked


def _ensure_drained(config: Mapping[str, Any], target_manifest: Mapping[str, Any] | None = None) -> None:
    if not config["require_drained"]:
        return
    _require_activation_parent_dirs(config, "drain_unverified")
    command = [BUN_PATH, str(config["control_path"])]
    sidecar = Path(config["control_path"]).with_name("client-release.json")
    manifest_path = sidecar.parent / MANIFEST_NAME
    if sidecar.exists() or sidecar.is_symlink():
        try:
            installed_root = sidecar.parent
            installed_manifest = load_release_manifest(
                installed_root / MANIFEST_NAME,
                allow_historical_web_config_mode=True,
            )
            checked = _verify_installed_release(config, installed_root, installed_manifest)
        except ManifestError as exc:
            _fail("drain_unverified", "installed client release metadata is invalid", exc.as_dict())
        except WrapperError as exc:
            if exc.code == "drain_unverified":
                raise
            _fail("drain_unverified", "installed client release integrity is not verified", exc.as_dict())
        if checked.get("format") in {GO_MANIFEST_FORMAT, LEGACY_PRODUCT_MANIFEST_FORMAT, PRODUCT_MANIFEST_FORMAT}:
            view = checked.get("client_view")
            transport = view.get("transport") if isinstance(view, Mapping) else None
            if transport not in {"legacy-bun", "go-request", "https"}:
                _fail("drain_unverified", "installed client release transport is invalid")
            if transport in {"go-request", "https"}:
                command = [str(GO_CONTROL_PATH), "request", "--socket", str(GO_CONTROL_SOCKET), "--timeout", "30s"]
            elif _is_go_manifest(checked) or _is_product_manifest(checked):
                # Historical native packages may declare legacy-bun while
                # carrying only the declared bridge, without the old alias.
                # The bridge is the verified native compatibility authority.
                command = [BUN_PATH, str(_go_bridge_target(config))]
    elif manifest_path.exists() or manifest_path.is_symlink():
        _fail("drain_unverified", "installed release manifest is present but the client sidecar is missing")
    def control_result(action: str, *, allow_legacy_unknown: bool = False) -> Mapping[str, Any] | None:
        try:
            process = subprocess.run(
                command,
                input=json.dumps({"protocol": 4, "action": action}).encode(),
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                check=False,
                timeout=30,
            )
        except (OSError, subprocess.TimeoutExpired):
            _fail("drain_unverified", "current control process could not verify release state")
        try:
            response = json.loads(process.stdout)
        except (UnicodeDecodeError, json.JSONDecodeError):
            _fail("drain_unverified", "control response is invalid")
        if not isinstance(response, dict):
            _fail("drain_unverified", "control response is invalid")
        if response.get("ok") is not True:
            error = response.get("error")
            code = error.get("code") if isinstance(error, Mapping) else None
            if allow_legacy_unknown and code in {"unknown_action", "unsupported_action"}:
                return None
            _fail("drain_unverified", "control response was not successful")
        if process.returncode != 0:
            _fail("drain_unverified", "current control process did not verify release state")
        result = response.get("result")
        if not isinstance(result, Mapping):
            _fail("drain_unverified", "control response result is invalid")
        return result

    result = control_result("drain-status")
    if result is None:
        _fail("drain_unverified", "drain status response is unavailable")
    if not isinstance(result, Mapping):
        _fail("drain_unverified", "drain status result is invalid")
    if result.get("draining") is not True:
        _fail("drain_required", "release activation requires admission drain")
    if target_manifest is not None:
        capabilities = result.get("state_capabilities")
        if capabilities is None:
            capabilities = _read_live_state_capabilities()
        _validate_state_compatibility(target_manifest, capabilities)

    # A wrapper activation is a second, independent fence around the
    # publisher's convergence loop. Newer authorities expose active
    # management operations; older protocol-4 controllers may not know this
    # read-only action, in which case the publisher's legacy task checks remain
    # the available contract.
    metrics = control_result("admin-metrics", allow_legacy_unknown=True)
    if metrics is not None and "pending_operations" in metrics:
        pending = metrics["pending_operations"]
        if not isinstance(pending, list):
            _fail("drain_unverified", "control returned an invalid pending-operation list")
        if pending:
            _fail("drain_unverified", "release activation requires management operations to converge", {"pending_operations": pending[:100]})


def _daemon_reload() -> None:
    try:
        process = subprocess.run(["systemctl", "daemon-reload"], stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False, timeout=30)
    except (OSError, subprocess.TimeoutExpired) as exc:
        _fail("activation_failed", "systemd daemon-reload could not be verified")
    if process.returncode != 0:
        _fail("activation_failed", "systemd daemon-reload failed")


def _systemctl(action: str, unit: str, *, allow_missing: bool = False) -> None:
    """Run one fixed systemd transition and keep activation fail-closed."""
    try:
        process = subprocess.run(
            ["systemctl", action, unit],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
            timeout=30,
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        _fail("activation_failed", f"systemd {action} could not be verified: {unit}")
    missing_unit = process.returncode == 5 or (
        action in {"stop", "disable", "enable"}
        and process.returncode == 1
        and any(
            marker in process.stderr.lower()
            for marker in (b"does not exist", b"missing the instance name")
        )
    )
    if process.returncode != 0 and not (allow_missing and missing_unit):
        _fail("activation_failed", f"systemd {action} failed: {unit}")


def _legacy_expiry_timers() -> tuple[str, ...]:
    """Return only fixed-shape generation expiry timers owned by legacy control."""
    try:
        process = subprocess.run(
            ["systemctl", "list-unit-files", "--no-legend", "--no-pager", "agent-vm-native-expire-*.timer"],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
            timeout=30,
        )
    except (OSError, subprocess.TimeoutExpired):
        _fail("activation_failed", "systemd legacy expiry timer inventory could not be verified")
    if process.returncode not in (0, 1) or (process.returncode == 1 and process.stderr.strip()):
        _fail("activation_failed", "systemd legacy expiry timer inventory failed")
    units: set[str] = set()
    for line in process.stdout.decode("utf-8", errors="strict").splitlines():
        fields = line.split()
        name = fields[0] if fields else ""
        if LEGACY_EXPIRY_TIMER_RE.fullmatch(name):
            units.add(name)
    return tuple(sorted(units))


def _legacy_expiry_services(timers: tuple[str, ...]) -> tuple[str, ...]:
    prefix = "agent-vm-native-expire-"
    suffix = ".timer"
    services = []
    for timer in timers:
        if not timer.startswith(prefix) or not timer.endswith(suffix):
            _fail("activation_failed", "systemd legacy expiry timer inventory contained an invalid unit")
        instance = timer[len(prefix):-len(suffix)]
        services.append(f"agent-vm-native-expire@{instance}.service")
    try:
        process = subprocess.run(
            ["systemctl", "list-units", "--all", "--no-legend", "--no-pager", "agent-vm-native-expire@*.service"],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
            timeout=30,
        )
    except (OSError, subprocess.TimeoutExpired):
        _fail("activation_failed", "systemd legacy expiry service inventory could not be verified")
    if process.returncode not in (0, 1) or (process.returncode == 1 and process.stderr.strip()):
        _fail("activation_failed", "systemd legacy expiry service inventory failed")
    try:
        lines = process.stdout.decode("utf-8", errors="strict").splitlines()
    except UnicodeDecodeError:
        _fail("activation_failed", "systemd legacy expiry service inventory is invalid")
    for line in lines:
        name = line.split()[0] if line.split() else ""
        if LEGACY_EXPIRY_SERVICE_RE.fullmatch(name):
            services.append(name)
    return tuple(sorted(set(services)))


def _deactivate_units(units: tuple[tuple[str, str], ...]) -> None:
    errors: list[dict[str, Any]] = []
    for action, unit in units:
        try:
            _systemctl(action, unit, allow_missing=True)
        except WrapperError as error:
            errors.append(error.as_dict())
    if errors:
        _fail("activation_failed", "one or more runtime deactivation steps failed", {"errors": errors})


def _deactivate_web_runtime() -> None:
    _deactivate_units((
        ("stop", "agent-vm-web.service"),
        ("disable", "agent-vm-web.service"),
    ))


def _deactivate_native_runtime() -> None:
    """Stop native authorities before their fixed runtime files are retired."""
    errors: list[dict[str, Any]] = []
    try:
        _deactivate_web_runtime()
    except WrapperError as error:
        errors.append(error.as_dict())
    try:
        _deactivate_units((
            ("stop", "agent-vm-native.service"),
            ("disable", "agent-vm-native.service"),
        ))
    except WrapperError as error:
        errors.append(error.as_dict())
    if errors:
        _fail("activation_failed", "one or more native deactivation steps failed", {"errors": errors})


def _deactivate_legacy_runtime() -> None:
    """Stop the fixed legacy triggers before their files are retired."""
    errors: list[dict[str, Any]] = []
    try:
        expiry_timers = _legacy_expiry_timers()
    except WrapperError as error:
        errors.append(error.as_dict())
        expiry_timers = ()
    try:
        expiry_services = _legacy_expiry_services(expiry_timers)
    except WrapperError as error:
        errors.append(error.as_dict())
        expiry_services = ()
    try:
        retired_timers = tuple(unit for unit in GO_RETIRED_UNITS if unit.endswith(".timer"))
        retired_services = tuple(unit for unit in GO_RETIRED_UNITS if not unit.endswith(".timer"))
        _deactivate_units(tuple(
            (action, unit)
            for unit in (*retired_timers, *expiry_timers, *retired_services, *expiry_services)
            for action in ("stop", "disable")
        ))
    except WrapperError as error:
        errors.append(error.as_dict())
    if errors:
        _fail("activation_failed", "one or more legacy deactivation steps failed", {"errors": errors})


def _stop_runtime_authorities() -> list[dict[str, Any]]:
    """Stop every runtime authority before reporting an incomplete recovery."""
    shutdown_errors: list[dict[str, Any]] = []
    for deactivate in (_deactivate_native_runtime, _deactivate_legacy_runtime):
        try:
            deactivate()
        except WrapperError as shutdown_error:
            shutdown_errors.append(shutdown_error.as_dict())
    return shutdown_errors


def _unit_is_enabled(unit: str) -> bool:
    """Capture whether a unit was enabled before a dependency restart."""
    try:
        process = subprocess.run(
            ["systemctl", "is-enabled", "--quiet", unit],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
            timeout=30,
        )
    except (OSError, subprocess.TimeoutExpired):
        _fail("activation_failed", f"systemd enabled-state check could not be verified: {unit}")
    return process.returncode == 0


def _activate_go_runtime(*, web_enabled: bool = False) -> None:
    """Retire legacy triggers, enable Go, and verify that it is active."""
    if not web_enabled:
        _deactivate_web_runtime()
    _deactivate_legacy_runtime()
    _systemctl("enable", "agent-vm-native.service")
    _systemctl("restart", "agent-vm-native.service")
    _systemctl("is-active", "agent-vm-native.service")


def _activate_product_runtime(
    manifest: Mapping[str, Any],
    *,
    web_was_enabled: bool | None = None,
) -> None:
    web_enabled = manifest.get("format") == PRODUCT_MANIFEST_FORMAT
    if web_enabled and web_was_enabled is None:
        web_was_enabled = _unit_is_enabled("agent-vm-web.service")
    if web_enabled and web_was_enabled is False:
        _deactivate_web_runtime()
    _activate_go_runtime(web_enabled=web_enabled)
    if web_was_enabled:
        # Requires=agent-vm-native.service stops Web during native restart.
        _systemctl("enable", "agent-vm-web.service", allow_missing=True)
        _systemctl("start", "agent-vm-web.service", allow_missing=True)


def _activate_legacy_runtime(*, native_already_stopped: bool = False) -> None:
    """Restore only the fixed legacy reconciliation triggers for rollback."""
    if not native_already_stopped:
        _deactivate_native_runtime()
    _systemctl("start", "agent-vm-native-reconcile.service", allow_missing=True)
    _systemctl("enable", "agent-vm-native-reconcile.service", allow_missing=True)
    _systemctl("start", "agent-vm-native-age-gc.timer", allow_missing=True)
    _systemctl("enable", "agent-vm-native-age-gc.timer", allow_missing=True)
    for unit in _legacy_expiry_timers():
        _systemctl("start", unit, allow_missing=True)
        _systemctl("enable", unit, allow_missing=True)


def _install_release_files(config: Mapping[str, Any], release_root: Path, manifest: Mapping[str, Any]) -> None:
    _require_activation_parent_dirs(config, "activation_unverified")
    if _is_product_manifest(manifest):
        _install_product_release_files(config, release_root, manifest)
        return
    if _is_go_manifest(manifest):
        _install_go_release_files(config, release_root, manifest)
        return
    owner_uid = int(config["owner_uid"])
    support_paths = {item["path"] for item in manifest["artifacts"] if item["role"] == "support"}
    if support_paths != SUPPORT_ARTIFACT_NAMES:
        _fail("activation_unverified", "support artifact paths differ from the fixed module set")
    support_installed: set[str] = set()
    for item in manifest["artifacts"]:
        source = release_root / Path(*PurePosixPath(item["path"]).parts)
        if item["role"] == "control":
            if item["path"] != "agent_vm_control.ts":
                _fail("activation_unverified", "control artifact name is not fixed")
            _copy_atomic(source, Path(config["control_path"]), owner_uid, item["mode"])
        elif item["role"] == "client":
            if item["path"] != "agent_vm.ts":
                _fail("activation_unverified", "client artifact name is not fixed")
            _copy_atomic(source, Path(config["control_path"]).with_name("agent_vm.ts"), owner_uid, item["mode"])
        elif item["role"] == "unit":
            name = Path(item["path"]).name
            if name not in config["unit_names"]:
                _fail("activation_unverified", f"unit target is not fixed: {name}")
            _copy_atomic(source, Path(config["unit_dir"]) / name, owner_uid, item["mode"])
        elif item["role"] == "support":
            if item["path"] not in SUPPORT_ARTIFACT_NAMES:
                _fail("activation_unverified", "support artifact path is not fixed")
            _copy_atomic(source, Path(config["control_path"]).with_name(item["path"]), owner_uid, item["mode"])
            support_installed.add(item["path"])
    if support_installed != SUPPORT_ARTIFACT_NAMES:
        _fail("activation_unverified", "fixed support modules are missing")
    _remove_native_predecessor_files(config, owner_uid)
    manifest_source = release_root / MANIFEST_NAME
    _require_owned_regular(manifest_source, owner_uid, 0o644)
    _copy_atomic(manifest_source, Path(config["control_path"]).with_name(MANIFEST_NAME), owner_uid, 0o644)
    client_sidecar = release_root / "client-release.json"
    _require_owned_regular(client_sidecar, owner_uid, 0o644)
    _copy_atomic(client_sidecar, Path(config["control_path"]).with_name("client-release.json"), owner_uid, 0o644)
    # This exact predecessor is snapshotted before activation, so failure can
    # restore it. It is never executed or retained as a second control plane.
    for name in (RETIRED_CONTROL_NAME, RETIRED_CLIENT_NAME):
        retired = Path(config["control_path"]).with_name(name)
        if retired.exists() or retired.is_symlink():
            _require_owned_regular(retired, owner_uid, 0o755)
            try:
                retired.unlink()
                _fsync_directory(retired.parent, "activation_failed")
            except OSError:
                _fail("activation_failed", "cannot retire the exact replaced executable")


def _install_product_release_files(config: Mapping[str, Any], release_root: Path, manifest: Mapping[str, Any]) -> None:
    """Install the complete format-4 product without replacing live Web config."""
    owner_uid = int(config["owner_uid"])
    bundle_root = Path(config["control_path"]).parent
    expected_values = PRODUCT_LINUX_ARTIFACTS if manifest["format"] == PRODUCT_MANIFEST_FORMAT else PRODUCT_LINUX_LEGACY_ARTIFACTS
    expected_support = {path for _, path, role, _ in expected_values if role == "support"}
    installed_support: set[str] = set()
    installed_controls: set[str] = set()
    for item in manifest["artifacts"]:
        source = release_root / Path(*PurePosixPath(item["path"]).parts)
        if item["role"] == "client":
            if item["path"] != "agent_vm.ts":
                _fail("activation_unverified", "product client artifact name is not fixed")
            _copy_atomic(source, Path(config["control_path"]).with_name("agent_vm.ts"), owner_uid, item["mode"])
        elif item["role"] == "support":
            if item["path"] in {"web-assets.manifest.json", "config/agent-vm-web.json.example"}:
                _copy_atomic(source, bundle_root / item["path"], owner_uid, item["mode"])
            elif item["path"] in {path for _, path, role, _ in PRODUCT_CLIENT_ARTIFACTS if role == "support"}:
                _copy_atomic(source, Path(config["control_path"]).with_name(item["path"]), owner_uid, item["mode"])
            else:
                _fail("activation_unverified", "product support artifact path is not fixed")
            installed_support.add(item["path"])
        elif item["role"] == "control":
            if item["path"] == "agent_vm_control_bridge.ts":
                _copy_atomic(source, _go_bridge_target(config), owner_uid, item["mode"])
                # Keep the historical Bun entrypoint aligned with the bridge
                # so the publisher and installed-release verifier share one
                # compatibility authority across native package formats.
                legacy_target = Path(config["control_path"])
                if legacy_target != _go_bridge_target(config):
                    _copy_atomic(source, legacy_target, owner_uid, item["mode"])
            elif item["path"] == "bin/agent-vm-control":
                _copy_atomic(source, _go_control_target(config), owner_uid, item["mode"])
            else:
                _fail("activation_unverified", "product control artifact path is not fixed")
            installed_controls.add(item["path"])
        elif item["role"] == "unit":
            if item["path"] not in {"systemd/agent-vm-native.service", "systemd/agent-vm-web.service"}:
                _fail("activation_unverified", "product unit artifact path is not fixed")
            name = Path(item["path"]).name
            _copy_atomic(source, Path(config["unit_dir"]) / name, owner_uid, item["mode"])
            _copy_atomic(source, bundle_root / item["path"], owner_uid, item["mode"])
    expected_client_support = {path for _, path, role, _ in PRODUCT_CLIENT_ARTIFACTS if role == "support"}
    if installed_support != expected_support or not expected_client_support.issubset(installed_support) or installed_controls != {"agent_vm_control_bridge.ts", "bin/agent-vm-control"}:
        _fail("activation_unverified", "fixed product control artifacts are missing")
    _remove_legacy_predecessor_files(config, owner_uid)
    for name in GO_RETIRED_UNITS:
        retired = Path(config["unit_dir"]) / name
        if retired.exists() or retired.is_symlink():
            _require_owned_regular(retired, owner_uid, 0o644)
            try:
                retired.unlink()
                _fsync_directory(retired.parent, "activation_failed")
            except OSError:
                _fail("activation_failed", f"cannot retire the exact old unit: {name}")
    if manifest["format"] == LEGACY_PRODUCT_MANIFEST_FORMAT:
        for target in (
            Path(config["unit_dir"]) / "agent-vm-web.service",
            bundle_root / "systemd" / "agent-vm-web.service",
            bundle_root / "config" / "agent-vm-web.json.example",
            bundle_root / "web-assets.manifest.json",
        ):
            if target.exists() or target.is_symlink():
                if target.name == "agent-vm-web.json.example":
                    _require_owned_regular_modes(target, owner_uid, (0o640, 0o644))
                else:
                    _require_owned_regular(target, owner_uid, 0o644)
                try:
                    target.unlink()
                    _fsync_directory(target.parent, "activation_failed")
                except OSError:
                    _fail("activation_failed", f"cannot retire the exact Web artifact: {target.name}")
    manifest_source = release_root / MANIFEST_NAME
    _require_owned_regular(manifest_source, owner_uid, 0o644)
    _copy_atomic(manifest_source, Path(config["control_path"]).with_name(MANIFEST_NAME), owner_uid, 0o644)
    client_sidecar = release_root / "client-release.json"
    _require_owned_regular(client_sidecar, owner_uid, 0o644)
    _copy_atomic(client_sidecar, Path(config["control_path"]).with_name("client-release.json"), owner_uid, 0o644)
    for name in (RETIRED_CONTROL_NAME, RETIRED_CLIENT_NAME):
        retired = Path(config["control_path"]).with_name(name)
        if retired.exists() or retired.is_symlink():
            _require_owned_regular(retired, owner_uid, 0o755)
            try:
                retired.unlink()
                _fsync_directory(retired.parent, "activation_failed")
            except OSError:
                _fail("activation_failed", "cannot retire the exact replaced executable")


def _install_go_release_files(config: Mapping[str, Any], release_root: Path, manifest: Mapping[str, Any]) -> None:
    """Install the format-2 client, forwarding bridge, binary, and one unit."""
    owner_uid = int(config["owner_uid"])
    go_bundle_root = Path(config["control_path"]).parent
    expected_support = {path for _, path, role, _ in GO_FIXED_ARTIFACTS if role == "support"}
    installed_support: set[str] = set()
    installed_controls: set[str] = set()
    for item in manifest["artifacts"]:
        source = release_root / Path(*PurePosixPath(item["path"]).parts)
        if item["role"] == "client":
            if item["path"] != "agent_vm.ts":
                _fail("activation_unverified", "Go client artifact name is not fixed")
            _copy_atomic(source, Path(config["control_path"]).with_name("agent_vm.ts"), owner_uid, item["mode"])
        elif item["role"] == "support":
            if item["path"] not in expected_support:
                _fail("activation_unverified", "Go support artifact path is not fixed")
            _copy_atomic(source, Path(config["control_path"]).with_name(item["path"]), owner_uid, item["mode"])
            installed_support.add(item["path"])
        elif item["role"] == "control":
            if item["path"] == "agent_vm_control_bridge.ts":
                _copy_atomic(source, _go_bridge_target(config), owner_uid, item["mode"])
                # Keep the historical filename for the SSH compatibility
                # bridge while retaining the manifest's explicit artifact
                # path for Go's installed-release verification.
                legacy_target = Path(config["control_path"])
                if legacy_target != _go_bridge_target(config):
                    _copy_atomic(source, legacy_target, owner_uid, item["mode"])
                installed_controls.add(item["path"])
            elif item["path"] == "bin/agent-vm-control":
                _copy_atomic(source, _go_control_target(config), owner_uid, item["mode"])
                installed_controls.add(item["path"])
            else:
                _fail("activation_unverified", "Go control artifact path is not fixed")
        elif item["role"] == "unit":
            if item["path"] != "systemd/agent-vm-native.service":
                _fail("activation_unverified", "Go unit artifact path is not fixed")
            _copy_atomic(source, Path(config["unit_dir"]) / "agent-vm-native.service", owner_uid, item["mode"])
            _copy_atomic(source, go_bundle_root / item["path"], owner_uid, item["mode"])
    if installed_support != expected_support or installed_controls != {"agent_vm_control_bridge.ts", "bin/agent-vm-control"}:
        _fail("activation_unverified", "fixed Go control artifacts are missing")
    _remove_legacy_predecessor_files(config, owner_uid)
    for name in GO_RETIRED_UNITS:
        retired = Path(config["unit_dir"]) / name
        if retired.exists() or retired.is_symlink():
            _require_owned_regular(retired, owner_uid, 0o644)
            try:
                retired.unlink()
                _fsync_directory(retired.parent, "activation_failed")
            except OSError:
                _fail("activation_failed", f"cannot retire the exact old unit: {name}")
    for target in (
        Path(config["unit_dir"]) / "agent-vm-web.service",
        go_bundle_root / "systemd" / "agent-vm-web.service",
        go_bundle_root / "config" / "agent-vm-web.json.example",
        go_bundle_root / "web-assets.manifest.json",
    ):
        if target.exists() or target.is_symlink():
            if target.name == "agent-vm-web.json.example":
                _require_owned_regular_modes(target, owner_uid, (0o640, 0o644))
            else:
                _require_owned_regular(target, owner_uid, 0o644)
            try:
                target.unlink()
                _fsync_directory(target.parent, "activation_failed")
            except OSError:
                _fail("activation_failed", f"cannot retire the exact Web artifact: {target.name}")
    manifest_source = release_root / MANIFEST_NAME
    _require_owned_regular(manifest_source, owner_uid, 0o644)
    _copy_atomic(manifest_source, Path(config["control_path"]).with_name(MANIFEST_NAME), owner_uid, 0o644)
    client_sidecar = release_root / "client-release.json"
    _require_owned_regular(client_sidecar, owner_uid, 0o644)
    _copy_atomic(client_sidecar, Path(config["control_path"]).with_name("client-release.json"), owner_uid, 0o644)
    for name in (RETIRED_CONTROL_NAME, RETIRED_CLIENT_NAME):
        retired = Path(config["control_path"]).with_name(name)
        if retired.exists() or retired.is_symlink():
            _require_owned_regular(retired, owner_uid, 0o755)
            try:
                retired.unlink()
                _fsync_directory(retired.parent, "activation_failed")
            except OSError:
                _fail("activation_failed", "cannot retire the exact replaced executable")


def activate(config: Mapping[str, Any], transaction_id: str, release_id: str) -> dict[str, Any]:
    with _release_lock(config):
        with _activation_fence(config, transaction_id, release_id):
            return _activate_unlocked(config, transaction_id, release_id)


def _activate_unlocked(config: Mapping[str, Any], transaction_id: str, release_id: str) -> dict[str, Any]:
    transaction_id = _safe_id(transaction_id, "transaction_id")
    release_id = _release_id(release_id)
    verified = _verify_transaction(config, transaction_id, release_id)
    staged = _transaction_dir(config, transaction_id)
    try:
        candidate_manifest = load_release_manifest(staged / MANIFEST_NAME)
    except ManifestError as exc:
        _fail(exc.code, exc.message, exc.detail)
    _ensure_drained(config, candidate_manifest)
    activation_path = Path(config["state_root"]) / (transaction_id + ".activation.json")
    if activation_path.exists() or activation_path.is_symlink():
        _fail("transaction_exists", "activation record already exists for this transaction")
    release_root = Path(config["release_root"])
    current_link = Path(config["current_link"])
    previous_link = Path(config["previous_link"])
    old_current = _current_target(current_link, release_root)
    old_previous = _current_target(previous_link, release_root)
    if old_current is not None and old_current == old_previous:
        _fail("activation_unverified", "current and previous pointers must not target the same release")
    old_runtime_manifest = None
    if old_current is not None:
        old_runtime_manifest = _verify_retained_release(config, old_current)
    else:
        # A legacy wrapper may have installed a runtime before the release
        # pointers were introduced. Treat that installed control plane as the
        # predecessor so cross-format activation stops it before retiring its
        # files, instead of leaving an orphaned authority behind.
        installed_root = Path(config["control_path"]).parent
        installed_sidecar = installed_root / "client-release.json"
        installed_manifest_path = installed_root / MANIFEST_NAME
        if installed_sidecar.exists() or installed_sidecar.is_symlink():
            try:
                installed_manifest = load_release_manifest(
                    installed_manifest_path,
                    allow_historical_web_config_mode=True,
                )
                old_runtime_manifest = _verify_installed_release(
                    config,
                    installed_root,
                    installed_manifest,
                )
            except ManifestError as exc:
                _fail("activation_unverified", "installed release metadata is invalid", exc.as_dict())
            except WrapperError as exc:
                _fail("activation_unverified", "installed release integrity is not verified", exc.as_dict())
        elif installed_manifest_path.exists() or installed_manifest_path.is_symlink():
            _fail("activation_unverified", "installed release manifest is present but the client sidecar is missing")
    if old_previous is not None:
        _verify_retained_release(config, old_previous)
    web_unit_enabled = None
    if _is_product_manifest(candidate_manifest) or _is_go_manifest(candidate_manifest):
        web_unit_enabled = _unit_is_enabled("agent-vm-web.service")
    elif old_runtime_manifest is not None and _is_product_manifest(old_runtime_manifest):
        web_unit_enabled = _unit_is_enabled("agent-vm-web.service")
    _mkdir_private(release_root, 0o755)
    release_dir = release_root / release_id
    if release_dir.exists():
        _fail("release_exists", "release directory already exists")
    temporary = release_root / ("." + release_id + ".tmp")
    if temporary.exists():
        _fail("activation_failed", "temporary release directory already exists")
    try:
        shutil.copytree(
            staged,
            temporary,
            symlinks=False,
            ignore=shutil.ignore_patterns(".wrapper-state.json", ".verified.json"),
        )
        _fsync_tree(temporary)
        os.replace(temporary, release_dir)
        _fsync_directory(release_root, "activation_failed")
    except OSError as exc:
        if temporary.exists():
            shutil.rmtree(temporary, ignore_errors=True)
        _fail("activation_failed", "cannot commit verified release directory")
    previous_id = old_current.name if old_current else None
    target_snapshot = _snapshot_targets(config)
    record = {
        "transaction_id": transaction_id,
        "release_id": release_id,
        "previous_release_id": previous_id,
        "old_current_release_id": old_current.name if old_current else None,
        "old_previous_release_id": old_previous.name if old_previous else None,
        "manifest_sha256": verified["manifest_sha256"],
        "activated": False,
        "activation_phase": "prepared",
        "rollback_attempts": 0,
        "web_unit_enabled": web_unit_enabled,
    }
    _write_json(activation_path, record)
    runtime_switch_attempted = False
    predecessor_runtime_stopped = False
    try:
        manifest = load_release_manifest(release_dir / MANIFEST_NAME)
        target_is_native = _is_product_manifest(manifest) or _is_go_manifest(manifest)
        old_is_native = old_runtime_manifest is not None and (
            _is_product_manifest(old_runtime_manifest) or _is_go_manifest(old_runtime_manifest)
        )
        if not target_is_native and old_is_native:
            runtime_switch_attempted = True
            record["activation_phase"] = "runtime-switching"
            _write_json(activation_path, record)
            _deactivate_native_runtime()
            predecessor_runtime_stopped = True
            record["activation_phase"] = "predecessor-stopped"
            _write_json(activation_path, record)
        elif target_is_native and old_runtime_manifest is not None and not old_is_native:
            runtime_switch_attempted = True
            record["activation_phase"] = "runtime-switching"
            _write_json(activation_path, record)
            _deactivate_legacy_runtime()
            predecessor_runtime_stopped = True
            record["activation_phase"] = "predecessor-stopped"
            _write_json(activation_path, record)
        _install_release_files(config, release_dir, manifest)
        record["activation_phase"] = "files-installed"
        _write_json(activation_path, record)
        _atomic_symlink(release_dir, current_link)
        record["activation_phase"] = "current-pointer-updated"
        _write_json(activation_path, record)
        if old_current is not None:
            _atomic_symlink(old_current, previous_link)
        record["activation_phase"] = "previous-pointer-updated"
        _write_json(activation_path, record)
        _daemon_reload()
        record["activation_phase"] = "daemon-reloaded"
        _write_json(activation_path, record)
        if _is_product_manifest(manifest):
            runtime_switch_attempted = True
            _activate_product_runtime(manifest, web_was_enabled=web_unit_enabled)
            record["activation_phase"] = "runtime-switched"
            _write_json(activation_path, record)
        elif _is_go_manifest(manifest):
            runtime_switch_attempted = True
            _activate_go_runtime()
            record["activation_phase"] = "runtime-switched"
            _write_json(activation_path, record)
        else:
            runtime_switch_attempted = True
            _activate_legacy_runtime(native_already_stopped=predecessor_runtime_stopped)
            record["activation_phase"] = "runtime-switched"
            _write_json(activation_path, record)
        record["activated"] = True
        record["activation_phase"] = "active"
        _write_json(activation_path, record)
    except (WrapperError, ManifestError) as raw_failure:
        failure = raw_failure if isinstance(raw_failure, WrapperError) else WrapperError(
            raw_failure.code, raw_failure.message, raw_failure.detail,
        )
        restored = True
        old_manifest = None
        old_compatibility_error = None
        if old_current is not None:
            try:
                old_manifest = load_release_manifest(old_current / MANIFEST_NAME)
                _validate_state_compatibility(old_manifest, _read_live_state_capabilities())
            except (WrapperError, ManifestError) as compatibility_error:
                old_compatibility_error = compatibility_error
        if old_compatibility_error is not None:
            restored = False
            failure = WrapperError(
                "activation_restore_blocked",
                "the previous release cannot read the migrated state; the compatible candidate remains selected",
                {"activation_error": failure.as_dict(), "compatibility_error": getattr(old_compatibility_error, "as_dict", lambda: {"message": str(old_compatibility_error)})()},
            )
            try:
                _systemctl("stop", "agent-vm-native.service", allow_missing=True)
            except WrapperError:
                pass
        else:
            try:
                _restore_activation_state(
                    config,
                    current_link,
                    previous_link,
                    old_current,
                    old_previous,
                    target_snapshot,
                )
            except WrapperError as restore_error:
                restored = False
                failure = WrapperError(
                    "activation_restore_failed",
                    "activation failed and the previous release could not be fully restored",
                    {"activation_error": failure.as_dict(), "restore_error": restore_error.as_dict()},
                )
        if not restored or (old_current is None and runtime_switch_attempted):
            shutdown_errors = _stop_runtime_authorities()
            if shutdown_errors:
                failure = WrapperError(
                    "activation_restore_failed",
                    "activation failed and the authorities could not be stopped safely",
                    {"activation_error": failure.as_dict(), "shutdown_errors": shutdown_errors},
                )
        elif runtime_switch_attempted:
            try:
                assert old_manifest is not None
                if _is_product_manifest(old_manifest):
                    snapshot = record.get("web_unit_enabled")
                    _activate_product_runtime(
                        old_manifest,
                        web_was_enabled=snapshot if isinstance(snapshot, bool) else None,
                    )
                elif _is_go_manifest(old_manifest):
                    _activate_go_runtime()
                else:
                    _activate_legacy_runtime()
            except (WrapperError, ManifestError) as runtime_error:
                failure = WrapperError(
                    "activation_restore_failed",
                    "activation failed and the previous runtime could not be restored",
                    {"activation_error": failure.as_dict(), "runtime_error": getattr(runtime_error, "as_dict", lambda: {"message": str(runtime_error)})()},
                )
                shutdown_errors = _stop_runtime_authorities()
                if shutdown_errors:
                    failure = WrapperError(
                        "activation_restore_failed",
                        "activation failed and the authorities could not be stopped safely",
                        {"activation_error": failure.as_dict(), "shutdown_errors": shutdown_errors},
                    )
        try:
            _write_json(activation_path, {
                **record,
                "activated": False,
                "activation_phase": "failed",
                "activation_error": failure.as_dict(),
            })
        except WrapperError:
            pass
        raise failure
    return record


def rollback(config: Mapping[str, Any], transaction_id: str, release_id: str) -> dict[str, Any]:
    with _release_lock(config):
        with _activation_fence(config, transaction_id, release_id):
            return _rollback_unlocked(config, transaction_id, release_id)


def _rollback_unlocked(config: Mapping[str, Any], transaction_id: str, release_id: str) -> dict[str, Any]:
    transaction_id = _safe_id(transaction_id, "transaction_id")
    release_id = _release_id(release_id)
    activation_path = Path(config["state_root"]) / (transaction_id + ".activation.json")
    try:
        activation_info = activation_path.lstat()
    except OSError:
        _fail("rollback_unavailable", "there is no activation record for this transaction")
    if stat.S_ISLNK(activation_info.st_mode) or not stat.S_ISREG(activation_info.st_mode):
        _fail("rollback_unavailable", "there is no activation record for this transaction")
    record = _read_json(activation_path, "rollback_unavailable")
    interrupted_phases = {
        "prepared",
        "runtime-switching",
        "predecessor-stopped",
        "files-installed",
        "previous-pointer-updated",
        "current-pointer-updated",
        "daemon-reloaded",
        "runtime-switched",
    }
    interrupted = record.get("activated") is False and record.get("activation_phase") in interrupted_phases
    if record.get("release_id") != release_id or not (record.get("activated") is True or interrupted):
        _fail("rollback_unavailable", "rollback transaction does not match the active release")
    rollback_state = record.get("rollback_state")
    recovering_in_progress = record.get("activated") is True and rollback_state == "in-progress"
    if record.get("rollback_attempts", 0) >= 1 and not recovering_in_progress:
        _fail("rollback_already_attempted", "automatic rollback is limited to one attempt")
    previous_id = record.get("previous_release_id")
    if not isinstance(previous_id, str) or RELEASE_ID_RE.fullmatch(previous_id) is None:
        _fail("rollback_unavailable", "no previous verified release is retained")
    previous_dir = Path(config["release_root"]) / previous_id
    previous_manifest = _verify_retained_release(config, previous_dir)
    current_link = Path(config["current_link"])
    previous_link = Path(config["previous_link"])
    actual_current = _current_target(current_link, Path(config["release_root"]))
    actual_previous = _current_target(previous_link, Path(config["release_root"]))
    old_current_id = record.get("old_current_release_id")
    old_previous_id = record.get("old_previous_release_id")
    if record.get("activated") is True:
        old_current_id = old_current_id or previous_id
    elif not isinstance(old_current_id, str):
        _fail("rollback_unavailable", "interrupted activation has no retained current pointer record")
    for value, label in ((old_current_id, "old_current_release_id"), (old_previous_id, "old_previous_release_id")):
        if value is not None and (not isinstance(value, str) or RELEASE_ID_RE.fullmatch(value) is None):
            _fail("rollback_unavailable", f"{label} is invalid")
    if old_current_id == old_previous_id:
        _fail("activation_unverified", "current and previous pointers must not target the same release")
    if old_current_id != previous_id:
        _fail("rollback_unavailable", "retained current release does not match the activation record")
    active_manifest = None
    if actual_current is not None:
        active_manifest = _verify_retained_release(config, actual_current)
    active_web_unit_enabled = None
    if active_manifest is not None and active_manifest.get("format") == PRODUCT_MANIFEST_FORMAT:
        active_web_unit_enabled = _unit_is_enabled("agent-vm-web.service")
    if old_previous_id is not None:
        _verify_retained_release(config, Path(config["release_root"]) / old_previous_id)
    if record.get("activated") is True:
        actual_current_name = actual_current.name if actual_current is not None else None
        actual_previous_name = actual_previous.name if actual_previous is not None else None
        if recovering_in_progress:
            before_current_switch = {old_current_id, old_previous_id, None}
            after_current_switch = {old_current_id, old_previous_id, release_id, None}
            if actual_current_name == release_id and actual_previous_name not in before_current_switch:
                _fail("rollback_stale", "in-progress rollback release pointers are no longer expected")
            if actual_current_name == previous_id and actual_previous_name not in after_current_switch:
                _fail("rollback_stale", "in-progress rollback release pointers are no longer expected")
            if actual_current_name not in {release_id, previous_id}:
                _fail("rollback_stale", "in-progress rollback transaction no longer matches the active release")
        else:
            if actual_current is None or actual_current.name != release_id:
                _fail("rollback_stale", "rollback transaction is no longer the active release")
            if actual_previous is None or actual_previous.name != old_current_id:
                _fail("rollback_stale", "rollback transaction previous pointer no longer matches the activation record")
    else:
        allowed_current = {release_id, old_current_id}
        allowed_previous = {None, old_current_id, old_previous_id, release_id}
        if actual_current is None or actual_current.name not in allowed_current:
            _fail("rollback_stale", "interrupted rollback transaction no longer matches release pointers")
        if actual_previous is not None and actual_previous.name not in allowed_previous:
            _fail("rollback_stale", "interrupted rollback transaction no longer matches release pointers")
    # The activation record is written only after the original activation has
    # drained the control plane. An interrupted activation or rollback may
    # have already stopped that authority, so asking it to drain again would
    # make recovery impossible; the release fence and retained pointers are
    # the recovery proof for this bounded retry.
    if interrupted or recovering_in_progress:
        if config["require_drained"]:
            _validate_state_compatibility(previous_manifest, _read_live_state_capabilities())
    else:
        _ensure_drained(config, previous_manifest)
    target_snapshot = _snapshot_targets(config)
    record["rollback_attempts"] = 1
    record["rollback_started"] = True
    record["rollback_phase"] = "started"
    record["rollback_state"] = "in-progress"
    record["draining"] = True
    _write_json(activation_path, record)
    predecessor_runtime_stopped = False
    runtime_switch_attempted = False
    try:
        target_is_native = _is_product_manifest(previous_manifest) or _is_go_manifest(previous_manifest)
        active_is_native = active_manifest is not None and (
            _is_product_manifest(active_manifest) or _is_go_manifest(active_manifest)
        )
        if not target_is_native and active_is_native:
            runtime_switch_attempted = True
            _deactivate_native_runtime()
            predecessor_runtime_stopped = True
        elif target_is_native and active_manifest is not None and not active_is_native:
            runtime_switch_attempted = True
            _deactivate_legacy_runtime()
            predecessor_runtime_stopped = True
        _install_release_files(config, previous_dir, previous_manifest)
        _atomic_symlink(previous_dir, current_link)
        if record.get("activated") is True:
            _atomic_symlink(Path(config["release_root"]) / release_id, previous_link)
        else:
            _restore_pointer(
                previous_link,
                Path(config["release_root"]) / old_previous_id if old_previous_id is not None else None,
            )
        _daemon_reload()
        runtime_switch_attempted = True
        if _is_product_manifest(previous_manifest):
            snapshot = record.get("web_unit_enabled")
            _activate_product_runtime(
                previous_manifest,
                web_was_enabled=snapshot if isinstance(snapshot, bool) else None,
            )
        elif _is_go_manifest(previous_manifest):
            _activate_go_runtime()
        else:
            _activate_legacy_runtime(native_already_stopped=predecessor_runtime_stopped)
    except WrapperError as failure:
        restore_succeeded = False
        try:
            _restore_activation_state(
                config,
                current_link,
                previous_link,
                actual_current,
                actual_previous,
                target_snapshot,
            )
            restore_succeeded = True
        except WrapperError as restore_error:
            failure = WrapperError(
                "activation_restore_failed",
                "rollback failed and the previous release could not be fully restored",
                {"rollback_error": failure.as_dict(), "restore_error": restore_error.as_dict()},
            )
        if not restore_succeeded:
            shutdown_errors = _stop_runtime_authorities()
            if shutdown_errors:
                failure = WrapperError(
                    "activation_restore_failed",
                    "rollback failed and the authorities could not be stopped safely",
                    {"rollback_error": failure.as_dict(), "shutdown_errors": shutdown_errors},
                )
        elif runtime_switch_attempted:
            try:
                if active_manifest is None:
                    _systemctl("stop", "agent-vm-native.service", allow_missing=True)
                elif _is_product_manifest(active_manifest):
                    _activate_product_runtime(
                        active_manifest,
                        web_was_enabled=active_web_unit_enabled,
                    )
                elif _is_go_manifest(active_manifest):
                    _activate_go_runtime()
                else:
                    _activate_legacy_runtime()
            except (WrapperError, ManifestError) as runtime_error:
                failure = WrapperError(
                    "activation_restore_failed",
                    "rollback failed and the previous runtime could not be restored",
                    {"rollback_error": failure.as_dict(), "runtime_error": getattr(runtime_error, "as_dict", lambda: {"message": str(runtime_error)})()},
                )
                shutdown_errors = _stop_runtime_authorities()
                if shutdown_errors:
                    failure = WrapperError(
                        "activation_restore_failed",
                        "rollback failed and the authorities could not be stopped safely",
                        {"rollback_error": failure.as_dict(), "shutdown_errors": shutdown_errors},
                    )
        record["rollback_phase"] = "failed"
        record["rollback_state"] = "failed"
        record["rollback_error"] = failure.as_dict()
        record["draining"] = True
        try:
            _write_json(activation_path, record)
        except WrapperError:
            pass
        raise failure
    record["rolled_back_to"] = previous_id
    record["rollback_phase"] = "complete"
    record["rollback_state"] = "complete"
    record["draining"] = True
    try:
        _write_json(activation_path, record)
    except WrapperError as record_error:
        shutdown_errors = _stop_runtime_authorities()
        restore_error = None
        try:
            _restore_activation_state(
                config,
                current_link,
                previous_link,
                actual_current,
                actual_previous,
                target_snapshot,
            )
        except WrapperError as error:
            restore_error = error
        if shutdown_errors or restore_error is not None:
            detail = {"record_error": record_error.as_dict()}
            if shutdown_errors:
                detail["shutdown_errors"] = shutdown_errors
            if restore_error is not None:
                detail["restore_error"] = restore_error.as_dict()
            raise WrapperError(
                "activation_restore_failed",
                "rollback record could not be persisted and the active release could not be fully restored",
                detail,
            ) from restore_error
        try:
            if active_manifest is None:
                _systemctl("stop", "agent-vm-native.service", allow_missing=True)
            elif _is_product_manifest(active_manifest):
                _activate_product_runtime(
                    active_manifest,
                    web_was_enabled=active_web_unit_enabled,
                )
            elif _is_go_manifest(active_manifest):
                _activate_go_runtime()
            else:
                _activate_legacy_runtime()
        except (WrapperError, ManifestError) as runtime_error:
            shutdown_errors = _stop_runtime_authorities()
            detail = {
                "record_error": record_error.as_dict(),
                "runtime_error": getattr(runtime_error, "as_dict", lambda: {"message": str(runtime_error)})(),
            }
            if shutdown_errors:
                detail["shutdown_errors"] = shutdown_errors
            raise WrapperError(
                "activation_restore_failed",
                "rollback record could not be persisted and the active runtime could not be restored",
                detail,
            ) from runtime_error
        record["rollback_error"] = record_error.as_dict()
        try:
            _write_json(activation_path, record)
        except WrapperError:
            pass
        raise record_error
    return {
        "rolled_back": True,
        "release_id": previous_id,
        "previous_release_id": release_id if record.get("activated") is True else None,
    }


def status(config: Mapping[str, Any]) -> dict[str, Any]:
    release_root = Path(config["release_root"])
    current = _current_target(Path(config["current_link"]), release_root)
    previous = _current_target(Path(config["previous_link"]), release_root)
    return {
        "current_release_id": current.name if current else None,
        "previous_release_id": previous.name if previous else None,
        "release_root": str(config["release_root"]),
    }


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="agent-vm-release-wrapper")
    subparsers = parser.add_subparsers(dest="action", required=True)
    stage_parser = subparsers.add_parser("stage")
    stage_parser.add_argument("--transaction-id", required=True)
    stage_parser.add_argument("--release-id", required=True)
    verify_parser = subparsers.add_parser("verify")
    verify_parser.add_argument("--transaction-id", required=True)
    verify_parser.add_argument("--release-id", required=True)
    activate_parser = subparsers.add_parser("activate")
    activate_parser.add_argument("--transaction-id", required=True)
    activate_parser.add_argument("--release-id", required=True)
    rollback_parser = subparsers.add_parser("rollback")
    rollback_parser.add_argument("--transaction-id", required=True)
    rollback_parser.add_argument("--release-id", required=True)
    subparsers.add_parser("status")
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        config = load_config(DEFAULT_CONFIG_PATH)
        if args.action == "stage":
            payload = sys.stdin.buffer.read(MAX_ARCHIVE_BYTES + 1)
            result = stage(config, args.transaction_id, args.release_id, payload)
        elif args.action == "verify":
            result = verify_transaction(config, _safe_id(args.transaction_id, "transaction_id"), _release_id(args.release_id))
        elif args.action == "activate":
            result = activate(config, args.transaction_id, args.release_id)
        elif args.action == "rollback":
            result = rollback(config, args.transaction_id, args.release_id)
        else:
            result = status(config)
        sys.stdout.write(json.dumps(result, sort_keys=True) + "\n")
        return 0
    except WrapperError as exc:
        sys.stderr.write(json.dumps({"ok": False, "error": exc.as_dict()}, sort_keys=True) + "\n")
        return 2
    except Exception as exc:
        sys.stderr.write(json.dumps({"ok": False, "error": {"code": "internal_error", "message": type(exc).__name__}}, sort_keys=True) + "\n")
        return 3


if __name__ == "__main__":
    raise SystemExit(main())
