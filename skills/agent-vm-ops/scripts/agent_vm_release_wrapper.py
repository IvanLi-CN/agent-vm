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
        GO_UNIT_EXEC_START,
        ManifestError,
        load_release_manifest,
        sha256_file,
        verify_client_sidecar,
        verify_bundle,
        verify_go_bundle,
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
        GO_UNIT_EXEC_START,
        ManifestError,
        load_release_manifest,
        sha256_file,
        verify_client_sidecar,
        verify_bundle,
        verify_go_bundle,
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
LEGACY_EXPIRY_TIMER_RE = re.compile(r"^agent-vm-native-expire-[A-Za-z0-9_.:-]+\.timer$")
ID_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$")
RELEASE_ID_RE = re.compile(r"^[0-9a-f]{40}$")
MAX_ARCHIVE_BYTES = 128 * 1024 * 1024
MAX_FILE_BYTES = 64 * 1024 * 1024
FORBIDDEN_CONFIG_NAMES = {"config.json", "state.sqlite3", "control.lock", "authority.lock", "admission.lock"}
FIXED_UNIT_NAMES = tuple(sorted(set(UNIT_EXEC_START) | {"agent-vm-native-age-gc.timer"}))
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


def _go_control_target(config: Mapping[str, Any]) -> Path:
    configured = Path(config["control_path"])
    if configured == CANONICAL_CONTROL_PATH:
        return GO_CONTROL_PATH
    return configured.parent / "bin" / GO_CONTROL_PATH.name


def _go_bridge_target(config: Mapping[str, Any]) -> Path:
    # The compatibility entrypoint keeps the historical control filename so
    # old SSH clients reach the new UDS request adapter unchanged.
    return Path(config["control_path"])


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


def _verify_unit_contents(root: Path, manifest: Mapping[str, Any]) -> None:
    if _is_go_manifest(manifest):
        for item in manifest["artifacts"]:
            if item["role"] != "unit":
                continue
            name = Path(item["path"]).name
            if name not in GO_UNIT_EXEC_START:
                _fail("unit_unverified", f"Go unit is not in the fixed allowlist: {name}")
            path = root / Path(*PurePosixPath(item["path"]).parts)
            try:
                content = path.read_text(encoding="utf-8")
            except (OSError, UnicodeError):
                _fail("unit_unverified", f"unit cannot be read: {name}")
            if sha256_file(path) != GO_UNIT_CONTENT_SHA256[name]:
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
        path = root / Path(*PurePosixPath(item["path"]).parts)
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


def _reject_transaction_extras(root: Path, manifest: Mapping[str, Any]) -> None:
    allowed = {MANIFEST_NAME, "client-release.json", ".wrapper-state.json", ".verified.json"} | {
        item["path"] for item in manifest["artifacts"]
    }
    for path in root.rglob("*"):
        if path.is_symlink():
            _fail("artifact_unverified", "transaction contains a symlink", {"path": path.relative_to(root).as_posix()})
        if path.is_file() and path.relative_to(root).as_posix() not in allowed:
            _fail("artifact_unverified", "transaction contains an unallowlisted file", {"path": path.relative_to(root).as_posix()})


def _verify_fixed_artifacts(config: Mapping[str, Any], manifest: Mapping[str, Any]) -> None:
    declared = {(item["role"], item["path"]) for item in manifest["artifacts"]}
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
        "release_id": manifest["release_id"],
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
            if manifest["release_id"] != release_id:
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
        manifest = load_release_manifest(target / MANIFEST_NAME)
        if _is_go_manifest(manifest):
            verify_go_bundle(target, manifest)
        else:
            verify_bundle(target, manifest)
        verify_client_sidecar(target, manifest)
    except ManifestError as exc:
        _fail("activation_unverified", "retained release cannot be verified", exc.as_dict())
    _reject_transaction_extras(target, manifest)
    _verify_fixed_artifacts(config, manifest)
    _verify_python_syntax(target, manifest)
    _verify_typescript_syntax(target, manifest)
    _verify_unit_contents(target, manifest)
    owner_uid = int(config["owner_uid"])
    _require_owned_regular(target / MANIFEST_NAME, owner_uid, 0o644)
    _require_owned_regular(target / "client-release.json", owner_uid, 0o644)
    for item in manifest["artifacts"]:
        _require_owned_regular(target / Path(*PurePosixPath(item["path"]).parts), owner_uid, item["mode"])
    return manifest


def _atomic_symlink(target: Path, link: Path) -> None:
    temporary = link.with_name("." + link.name + ".tmp")
    try:
        if temporary.exists() or temporary.is_symlink():
            temporary.unlink()
        os.symlink(target, temporary)
        os.replace(temporary, link)
        _fsync_directory(link.parent, "activation_failed")
    except OSError as exc:
        _fail("activation_failed", f"cannot atomically update release pointer: {link.name}")


def _copy_atomic(source: Path, target: Path, owner_uid: int, mode: int) -> None:
    target.parent.mkdir(mode=0o755, parents=True, exist_ok=True)
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


def _activation_targets(config: Mapping[str, Any]) -> tuple[Path, ...]:
    go_bundle_root = _go_control_target(config).parent
    targets = (
        Path(config["control_path"]),
        _go_control_target(config),
        Path(config["control_path"]).with_name(RETIRED_CONTROL_NAME),
        Path(config["control_path"]).with_name(RETIRED_CLIENT_NAME),
        Path(config["control_path"]).with_name("agent_vm.ts"),
        *(Path(config["control_path"]).with_name(name) for name in sorted(SUPPORT_ARTIFACT_NAMES)),
        Path(config["control_path"]).with_name(MANIFEST_NAME),
        Path(config["control_path"]).with_name("client-release.json"),
    ) + tuple(Path(config["unit_dir"]) / name for name in (*config["unit_names"], "agent-vm-native.service")) + (
        go_bundle_root / "systemd" / "agent-vm-native.service",
    )
    return tuple(dict.fromkeys(targets))


def _snapshot_targets(config: Mapping[str, Any]) -> dict[Path, tuple[bytes, int, int] | None]:
    snapshot: dict[Path, tuple[bytes, int, int] | None] = {}
    for target in _activation_targets(config):
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


def _ensure_drained(config: Mapping[str, Any]) -> None:
    if not config["require_drained"]:
        return
    command = [BUN_PATH, str(config["control_path"])]
    sidecar = Path(config["control_path"]).with_name("client-release.json")
    if sidecar.is_file():
        try:
            metadata = json.loads(sidecar.read_text(encoding="utf-8"))
        except (OSError, UnicodeError, json.JSONDecodeError):
            _fail("drain_unverified", "installed client release metadata is invalid")
        view = metadata.get("client_view") if isinstance(metadata, Mapping) else None
        if isinstance(metadata, Mapping) and metadata.get("format") == GO_MANIFEST_FORMAT and isinstance(view, Mapping) and view.get("transport") == "go-request":
            command = [str(GO_CONTROL_PATH), "request", "--socket", str(GO_CONTROL_SOCKET), "--timeout", "30s"]
    try:
        process = subprocess.run(
            command,
            input=json.dumps({"protocol": 4, "action": "drain-status"}).encode(),
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
            timeout=30,
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        _fail("drain_unverified", "cannot verify admission drain")
    if process.returncode != 0:
        _fail("drain_unverified", "current control process did not verify admission drain")
    try:
        response = json.loads(process.stdout)
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        _fail("drain_unverified", "drain status response is invalid")
    if not isinstance(response, dict) or response.get("ok") is not True:
        _fail("drain_unverified", "drain status response was not successful")
    result = response.get("result")
    if not isinstance(result, Mapping):
        _fail("drain_unverified", "drain status result is invalid")
    if result.get("draining") is not True:
        _fail("drain_required", "release activation requires admission drain")


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
    if process.returncode != 0 and not (allow_missing and process.returncode == 5):
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
    if process.returncode not in (0, 1):
        _fail("activation_failed", "systemd legacy expiry timer inventory failed")
    units: set[str] = set()
    for line in process.stdout.decode("utf-8", errors="strict").splitlines():
        fields = line.split()
        name = fields[0] if fields else ""
        if LEGACY_EXPIRY_TIMER_RE.fullmatch(name):
            units.add(name)
    return tuple(sorted(units))


def _activate_go_runtime() -> None:
    """Retire legacy triggers, enable Go, and verify that it is active."""
    for unit in (*GO_RETIRED_UNITS, *_legacy_expiry_timers()):
        _systemctl("stop", unit, allow_missing=True)
        _systemctl("disable", unit, allow_missing=True)
    _systemctl("enable", "agent-vm-native.service")
    _systemctl("restart", "agent-vm-native.service")
    _systemctl("is-active", "agent-vm-native.service")


def _activate_legacy_runtime() -> None:
    """Restore only the fixed legacy reconciliation triggers for rollback."""
    _systemctl("stop", "agent-vm-native.service", allow_missing=True)
    _systemctl("disable", "agent-vm-native.service", allow_missing=True)
    _systemctl("start", "agent-vm-native-reconcile.service", allow_missing=True)
    _systemctl("enable", "agent-vm-native-reconcile.service", allow_missing=True)
    _systemctl("start", "agent-vm-native-age-gc.timer", allow_missing=True)
    _systemctl("enable", "agent-vm-native-age-gc.timer", allow_missing=True)
    for unit in _legacy_expiry_timers():
        _systemctl("start", unit, allow_missing=True)
        _systemctl("enable", unit, allow_missing=True)


def _install_release_files(config: Mapping[str, Any], release_root: Path, manifest: Mapping[str, Any]) -> None:
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


def _install_go_release_files(config: Mapping[str, Any], release_root: Path, manifest: Mapping[str, Any]) -> None:
    """Install the format-2 client, forwarding bridge, binary, and one unit."""
    owner_uid = int(config["owner_uid"])
    go_bundle_root = _go_control_target(config).parent
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
    for name in GO_RETIRED_UNITS:
        retired = Path(config["unit_dir"]) / name
        if retired.exists() or retired.is_symlink():
            _require_owned_regular(retired, owner_uid, 0o644)
            try:
                retired.unlink()
                _fsync_directory(retired.parent, "activation_failed")
            except OSError:
                _fail("activation_failed", f"cannot retire the exact old unit: {name}")
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
    _ensure_drained(config)
    activation_path = Path(config["state_root"]) / (transaction_id + ".activation.json")
    if activation_path.exists() or activation_path.is_symlink():
        _fail("transaction_exists", "activation record already exists for this transaction")
    staged = _transaction_dir(config, transaction_id)
    release_root = Path(config["release_root"])
    current_link = Path(config["current_link"])
    previous_link = Path(config["previous_link"])
    old_current = _current_target(current_link, release_root)
    old_previous = _current_target(previous_link, release_root)
    if old_current is not None and old_current == old_previous:
        _fail("activation_unverified", "current and previous pointers must not target the same release")
    if old_current is not None:
        _verify_retained_release(config, old_current)
    if old_previous is not None:
        _verify_retained_release(config, old_previous)
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
    }
    _write_json(activation_path, record)
    runtime_switch_attempted = False
    try:
        manifest = load_release_manifest(release_dir / MANIFEST_NAME)
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
        if _is_go_manifest(manifest):
            runtime_switch_attempted = True
            _activate_go_runtime()
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
        if runtime_switch_attempted:
            try:
                if not restored or old_current is None:
                    _systemctl("stop", "agent-vm-native.service", allow_missing=True)
                else:
                    old_manifest = load_release_manifest(old_current / MANIFEST_NAME)
                    if _is_go_manifest(old_manifest):
                        _activate_go_runtime()
                    else:
                        _activate_legacy_runtime()
            except (WrapperError, ManifestError) as runtime_error:
                failure = WrapperError(
                    "activation_restore_failed",
                    "activation failed and the previous runtime could not be restored",
                    {"activation_error": failure.as_dict(), "runtime_error": getattr(runtime_error, "as_dict", lambda: {"message": str(runtime_error)})()},
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
    if old_current_id is not None:
        _verify_retained_release(config, Path(config["release_root"]) / old_current_id)
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
    _ensure_drained(config)
    target_snapshot = _snapshot_targets(config)
    record["rollback_attempts"] = 1
    record["rollback_started"] = True
    record["rollback_phase"] = "started"
    record["rollback_state"] = "in-progress"
    record["draining"] = True
    _write_json(activation_path, record)
    try:
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
        if _is_go_manifest(previous_manifest):
            _activate_go_runtime()
        else:
            _activate_legacy_runtime()
    except WrapperError as failure:
        try:
            _restore_activation_state(
                config,
                current_link,
                previous_link,
                actual_current,
                actual_previous,
                target_snapshot,
            )
        except WrapperError as restore_error:
            failure = WrapperError(
                "activation_restore_failed",
                "rollback failed and the previous release could not be fully restored",
                {"rollback_error": failure.as_dict(), "restore_error": restore_error.as_dict()},
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
        try:
            _restore_activation_state(
                config,
                current_link,
                previous_link,
                actual_current,
                actual_previous,
                target_snapshot,
            )
        except WrapperError as restore_error:
            raise WrapperError(
                "activation_restore_failed",
                "rollback record could not be persisted and the active release could not be fully restored",
                {"record_error": record_error.as_dict(), "restore_error": restore_error.as_dict()},
            ) from restore_error
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
