#!/usr/bin/env python3
"""Native Incus agent VM client.

The client sends one bounded JSON request to the configured control host. VM
access then uses a generation-bound native OpenSSH configuration.
"""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re
import select
import shlex
import subprocess
import sys
import threading
import time
from typing import Any, Mapping, Sequence
import uuid


PROTOCOL_VERSION = 4
VERSION = "0.4.0-native"
REMOTE_CONTROL = "/opt/agent-vm-native/agent_vm_control.py"
DEFAULT_CONTROL_TIMEOUT_SECONDS = 240
LONG_CONTROL_TIMEOUT_SECONDS = 600
REMOTE_STDOUT_LIMIT_BYTES = 2 * 1024 * 1024
REMOTE_STDERR_LIMIT_BYTES = 64 * 1024
SESSION_ENV = "AGENT_VM_SESSION_ID"
CONTROL_HOST_ENV = "AGENT_VM_CONTROL_HOST"
REMOTE_CONTROL_ENV = "AGENT_VM_REMOTE_CONTROL"
DEFAULT_CONTROL_HOST = "agent-vm-host"
DEFAULT_REMOTE_CONTROL = "/opt/agent-vm-native/agent_vm_control.py"
RELEASE_MANIFEST_ENV = "AGENT_VM_RELEASE_MANIFEST"
CLIENT_SIDECAR_NAME = "client-release.json"
DEFAULT_RELEASE_MANIFEST = Path(__file__).resolve().with_name(CLIENT_SIDECAR_NAME)
LEGACY_RELEASE_ID = "legacy-protocol-4"
MANIFEST_FORMAT = 1
SCHEMA_VERSION = 1
SEMANTIC_REVISION = 1
LOCAL_CONFIG_ROOT = Path(os.environ.get("XDG_CONFIG_HOME", str(Path.home() / ".config"))) / "agent-vm"
LOCAL_STATE_ROOT = Path(os.environ.get("XDG_STATE_HOME", str(Path.home() / ".local/state"))) / "agent-vm"
SESSION_ID_RE = re.compile(r"^[A-Za-z0-9_.:@/-]{1,160}$")
NAME_RE = re.compile(r"^[a-z][a-z0-9-]{0,30}$")

# A present sidecar must describe the complete public control surface.  This
# is deliberately duplicated here because the client is shipped independently
# and must not import the operations-side manifest parser.
CLIENT_ACTION_NAMES = (
    "acquire",
    "admin-gc",
    "admin-metrics",
    "admin-reconcile",
    "admin-session-list",
    "admin-status",
    "destroy",
    "doctor",
    "drain-enter",
    "drain-exit",
    "drain-status",
    "port-add",
    "port-list",
    "port-remove",
    "release",
    "renew",
    "resize",
    "restart",
    "ssh-info",
    "status",
    "timer-age-gc",
    "timer-expire",
    "timer-reconcile",
)


class CliError(Exception):
    def __init__(self, code: str, message: str, detail: Any = None, exit_code: int = 2):
        super().__init__(message)
        self.code = code
        self.message = message
        self.detail = detail
        self.exit_code = exit_code

    def as_dict(self) -> dict[str, Any]:
        value = {"code": self.code, "message": self.message}
        if self.detail is not None:
            value["detail"] = self.detail
        return value


def fail(code: str, message: str, detail: Any = None, exit_code: int = 2) -> None:
    raise CliError(code, message, detail, exit_code)


def session_id(explicit: str | None = None, *, required: bool = True) -> str | None:
    value = explicit if explicit is not None else os.environ.get(SESSION_ENV)
    if value is None and not required:
        return None
    if not value:
        fail("missing_session", f"{SESSION_ENV} must be set before a remote operation")
    if SESSION_ID_RE.fullmatch(value) is None:
        fail("invalid_session", f"{SESSION_ENV} contains unsupported characters")
    return value


def json_output(value: Any) -> None:
    sys.stdout.write(json.dumps(value, sort_keys=True, indent=2) + "\n")


def local_session_dir(value: str) -> Path:
    digest = hashlib.sha256(value.encode("utf-8")).hexdigest()[:24]
    return LOCAL_STATE_ROOT / "sessions" / digest


def read_session_state(value: str) -> dict[str, Any]:
    path = local_session_dir(value) / "state.json"
    try:
        with path.open("r", encoding="utf-8") as handle:
            state = json.load(handle)
    except FileNotFoundError:
        fail("local_state_missing", "local session state is missing; run acquire first")
    except (OSError, json.JSONDecodeError):
        fail("local_state_invalid", "local session state cannot be read")
    if not isinstance(state, dict):
        fail("local_state_invalid", "local session state is not an object")
    return state


def write_session_state(value: str, state: dict[str, Any]) -> None:
    directory = local_session_dir(value)
    directory.mkdir(mode=0o700, parents=True, exist_ok=True)
    path = directory / "state.json"
    temp = path.with_name(f".{path.name}.{os.getpid()}")
    with temp.open("w", encoding="utf-8") as handle:
        json.dump(state, handle, sort_keys=True)
        handle.write("\n")
        handle.flush()
        os.fchmod(handle.fileno(), 0o600)
        os.fsync(handle.fileno())
    os.replace(temp, path)


def current_generation(value: str) -> int:
    state = read_session_state(value)
    generation = state.get("generation")
    if isinstance(generation, bool) or not isinstance(generation, int) or generation < 1:
        fail("local_state_invalid", "local session state has no valid generation")
    return generation


def control_timeout(action: str, fields: dict[str, Any] | None = None) -> int:
    if action in {"acquire", "restart"}:
        return LONG_CONTROL_TIMEOUT_SECONDS
    if action == "resize" and fields and fields.get("allow_restart") is True:
        return LONG_CONTROL_TIMEOUT_SECONDS
    return DEFAULT_CONTROL_TIMEOUT_SECONDS


def _client_warning(code: str, message: str, action: str | None = None, detail: Any = None) -> dict[str, Any]:
    value: dict[str, Any] = {"code": code, "message": message}
    if action is not None:
        value["action"] = action
    if detail is not None:
        value["detail"] = detail
    return value


def _manifest_sidecar_path(path: Path | None = None) -> Path:
    candidate = path
    if candidate is None:
        configured = os.environ.get(RELEASE_MANIFEST_ENV)
        candidate = Path(configured) if configured else DEFAULT_RELEASE_MANIFEST
    if not candidate.is_absolute() or any(part in {"..", "."} for part in candidate.parts) or "\x00" in str(candidate):
        fail("release_metadata_invalid", "release sidecar path must be absolute and canonical")
    return candidate


def _safe_sidecar_path(value: Any, label: str) -> str:
    if not isinstance(value, str) or not value or "\\" in value or "\x00" in value:
        fail("release_metadata_invalid", f"{label} is not a safe relative path")
    path = Path(value)
    if path.is_absolute() or ".." in path.parts or "." in path.parts:
        fail("release_metadata_invalid", f"{label} is not a safe relative path")
    return path.as_posix()


def _client_schema(value: Any, label: str) -> dict[str, int]:
    if not isinstance(value, dict) or set(value) != {"schema", "semantic_revision"}:
        fail("release_metadata_invalid", f"{label} must declare schema and semantic revision")
    schema = value.get("schema")
    semantic = value.get("semantic_revision")
    if isinstance(schema, bool) or not isinstance(schema, int) or schema < 1:
        fail("release_metadata_invalid", f"{label}.schema is invalid")
    if isinstance(semantic, bool) or not isinstance(semantic, int) or semantic < 1:
        fail("release_metadata_invalid", f"{label}.semantic_revision is invalid")
    return {"schema": schema, "semantic_revision": semantic}


def _client_window(value: Any, label: str) -> dict[str, int]:
    expected = {"schema_min", "schema_max", "semantic_min", "semantic_max"}
    if not isinstance(value, dict) or set(value) != expected:
        fail("release_metadata_invalid", f"{label} must declare schema and semantic min/max")
    result: dict[str, int] = {}
    for key in expected:
        item = value[key]
        if isinstance(item, bool) or not isinstance(item, int) or item < 1:
            fail("release_metadata_invalid", f"{label}.{key} is invalid")
        result[key] = item
    if result["schema_min"] > result["schema_max"] or result["semantic_min"] > result["semantic_max"]:
        fail("release_metadata_invalid", f"{label} is inverted")
    return result


def _validate_client_artifact(sidecar: Path, artifact: dict[str, Any]) -> None:
    import stat

    required = {"name", "path", "role", "sha256", "mode"}
    if set(artifact) != required or artifact.get("role") != "client":
        fail("release_metadata_invalid", "client artifact declaration is incomplete")
    relative = _safe_sidecar_path(artifact["path"], "client artifact path")
    if artifact.get("name") != "agent_vm.py" or relative != "agent_vm.py":
        fail("release_metadata_invalid", "client artifact must identify agent_vm.py")
    digest = artifact["sha256"]
    if not isinstance(digest, str) or re.fullmatch(r"[0-9a-f]{64}", digest) is None:
        fail("release_metadata_invalid", "client artifact checksum is invalid")
    mode = artifact["mode"]
    if isinstance(mode, bool) or not isinstance(mode, int) or mode < 0 or mode > 0o777:
        fail("release_metadata_invalid", "client artifact mode is invalid")
    if mode != 0o755:
        fail("release_metadata_invalid", "client artifact mode must be 0755")
    path = sidecar.parent.resolve() / relative
    try:
        info = path.lstat()
    except (FileNotFoundError, OSError) as exc:
        fail("artifact_unverified", "declared client artifact is missing")
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
        fail("artifact_unverified", "declared client artifact is not a regular file")
    if stat.S_IMODE(info.st_mode) != mode:
        fail("artifact_mode_mismatch", "declared client artifact mode differs")
    try:
        digest_actual = hashlib.sha256(path.read_bytes()).hexdigest()
    except OSError as exc:
        fail("artifact_unverified", "declared client artifact cannot be read")
    if digest_actual != digest:
        fail("artifact_checksum_mismatch", "declared client artifact checksum differs")


def load_client_release_metadata(path: Path | None = None) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    """Read only the local client view and verify a present sidecar fail-closed."""
    sidecar = _manifest_sidecar_path(path)
    try:
        sidecar_info = sidecar.lstat()
    except FileNotFoundError:
        return {
            "release_id": LEGACY_RELEASE_ID,
            "source_revision": None,
            "protocol": PROTOCOL_VERSION,
            "manifest_present": False,
            "artifacts_verified": True,
            "actions": {},
        }, [_client_warning("legacy_compatibility", "release sidecar is absent; using legacy protocol-4 compatibility")]
    except OSError as exc:
        fail("release_metadata_invalid", "release sidecar cannot be inspected")
    import stat
    if stat.S_ISLNK(sidecar_info.st_mode) or not stat.S_ISREG(sidecar_info.st_mode):
        fail("release_metadata_invalid", "release sidecar is not a regular file")
    try:
        with sidecar.open("r", encoding="utf-8") as handle:
            raw = json.load(handle)
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        fail("release_metadata_invalid", "release sidecar is not valid UTF-8 JSON")
    if not isinstance(raw, dict):
        fail("release_metadata_invalid", "release sidecar root must be an object")
    expected_keys = {"format", "release_id", "source_revision", "protocol", "client_artifact", "client_view"}
    if set(raw) != expected_keys:
        fail("release_metadata_invalid", "release sidecar has an unexpected shape")
    if raw.get("format") != MANIFEST_FORMAT:
        fail("release_metadata_invalid", "release sidecar format is unsupported")
    source_revision = raw.get("source_revision")
    release_id = raw.get("release_id")
    if not isinstance(source_revision, str) or re.fullmatch(r"[0-9a-f]{40}", source_revision) is None:
        fail("release_metadata_invalid", "release sidecar source_revision is invalid")
    if release_id != source_revision:
        fail("release_metadata_invalid", "release sidecar release_id must equal source_revision")
    if raw.get("protocol") != PROTOCOL_VERSION:
        fail("protocol_unsupported", f"release sidecar protocol must be {PROTOCOL_VERSION}")
    view = raw.get("client_view")
    if not isinstance(view, dict):
        fail("release_metadata_invalid", "client_view must be an object")
    if set(view) != {"release_id", "source_revision", "protocol", "actions"}:
        fail("release_metadata_invalid", "client_view has an unexpected shape")
    if view.get("release_id") != release_id or view.get("source_revision") != source_revision or view.get("protocol") != PROTOCOL_VERSION:
        fail("release_metadata_invalid", "client_view identity does not match sidecar")
    actions = view.get("actions")
    if not isinstance(actions, dict) or set(actions) != set(CLIENT_ACTION_NAMES):
        fail("release_metadata_invalid", "client_view.actions must contain the fixed action set")
    normalized: dict[str, Any] = {}
    for action, spec in actions.items():
        if not isinstance(action, str) or re.fullmatch(r"[a-z][a-z0-9-]{0,63}", action) is None:
            fail("release_metadata_invalid", "client_view contains an invalid action")
        if not isinstance(spec, dict) or set(spec) != {"request", "response", "client_to_control", "control_to_client"}:
            fail("release_metadata_invalid", f"client_view action {action} has an invalid shape")
        request = _client_schema(spec["request"], f"client_view.{action}.request")
        response = _client_schema(spec["response"], f"client_view.{action}.response")
        c2c = _client_window(spec["client_to_control"], f"client_view.{action}.client_to_control")
        c2l = _client_window(spec["control_to_client"], f"client_view.{action}.control_to_client")
        if not c2c["schema_min"] <= request["schema"] <= c2c["schema_max"] or not c2c["semantic_min"] <= request["semantic_revision"] <= c2c["semantic_max"]:
            fail("release_metadata_invalid", f"client_view action {action} request is outside its window")
        if not c2l["schema_min"] <= response["schema"] <= c2l["schema_max"] or not c2l["semantic_min"] <= response["semantic_revision"] <= c2l["semantic_max"]:
            fail("release_metadata_invalid", f"client_view action {action} response is outside its window")
        normalized[action] = {
            "request": request,
            "response": response,
            "client_to_control": c2c,
            "control_to_client": c2l,
        }
    client_artifact = raw.get("client_artifact")
    if client_artifact is not None:
        if not isinstance(client_artifact, dict):
            fail("release_metadata_invalid", "client_artifact must be an object")
        _validate_client_artifact(sidecar, client_artifact)
    else:
        fail("release_metadata_invalid", "release sidecar must declare one client artifact")
    return {
        "release_id": release_id,
        "source_revision": source_revision,
        "protocol": PROTOCOL_VERSION,
        "manifest_present": True,
        "artifacts_verified": client_artifact is not None,
        "actions": normalized,
    }, []


def client_capability(action: str, metadata: dict[str, Any]) -> dict[str, Any]:
    spec = metadata.get("actions", {}).get(action)
    if spec is None:
        if metadata.get("manifest_present"):
            fail("unknown_action", f"action is not declared in the local release sidecar: {action}")
        return {
            "release_id": LEGACY_RELEASE_ID,
            "protocol": PROTOCOL_VERSION,
            "action": action,
            "request": {"schema": SCHEMA_VERSION, "semantic_revision": SEMANTIC_REVISION},
            "accepted_response": {
                "schema_min": SCHEMA_VERSION,
                "schema_max": SCHEMA_VERSION,
                "semantic_min": SEMANTIC_REVISION,
                "semantic_max": SEMANTIC_REVISION,
            },
            "legacy": True,
        }
    return {
        "release_id": metadata["release_id"],
        "protocol": metadata["protocol"],
        "action": action,
        "request": dict(spec["request"]),
        "accepted_response": dict(spec["control_to_client"]),
        "legacy": False,
    }


def _validate_response_warnings(value: Any, action: str) -> list[dict[str, Any]]:
    if value is None:
        return []
    if not isinstance(value, list):
        fail("invalid_remote_response", "control warnings must be an array")
    warnings: list[dict[str, Any]] = []
    for item in value:
        if not isinstance(item, dict) or not isinstance(item.get("code"), str) or not item.get("code") or not isinstance(item.get("message"), str) or not item.get("message"):
            fail("invalid_remote_response", "control warning entries must have code and message")
        if "action" in item and item["action"] != action:
            fail("invalid_remote_response", "control warning action does not match request")
        warnings.append(dict(item))
    return warnings


def _validate_response_release(value: Any) -> dict[str, Any]:
    if not isinstance(value, dict):
        fail("invalid_remote_response", "control release metadata must be an object")
    release_id = value.get("release_id")
    if not isinstance(release_id, str) or (release_id != LEGACY_RELEASE_ID and re.fullmatch(r"[0-9a-f]{40}", release_id) is None):
        fail("invalid_remote_response", "control release metadata has an invalid release_id")
    source_revision = value.get("source_revision")
    if release_id == LEGACY_RELEASE_ID:
        if source_revision is not None:
            fail("invalid_remote_response", "legacy control release must not declare source_revision")
    elif source_revision != release_id:
        fail("invalid_remote_response", "control release source_revision does not match release_id")
    protocol = value.get("protocol")
    if protocol != PROTOCOL_VERSION:
        fail("protocol_mismatch", "control release metadata protocol differs")
    for field in ("manifest_present", "artifacts_verified"):
        if field in value and not isinstance(value[field], bool):
            fail("invalid_remote_response", f"control release metadata {field} must be boolean")
    return dict(value)


def _validate_response_compatibility(value: Any, action: str, accepted: Any) -> dict[str, Any] | None:
    """Validate the control response's declared schema and semantic window."""
    accepted_window = _client_window(accepted, "compatibility.accepted_response")
    if value is None:
        return _client_warning(
            "legacy_response_compatibility",
            "control response omitted schema metadata; accepted as legacy protocol-4 response",
            action,
        )
    if not isinstance(value, dict) or set(value) != {"schema", "semantic_revision"}:
        fail("invalid_remote_response", "control response compatibility metadata is invalid")
    schema = value.get("schema")
    semantic = value.get("semantic_revision")
    if isinstance(schema, bool) or not isinstance(schema, int) or schema < 1:
        fail("invalid_remote_response", "control response schema is invalid")
    if isinstance(semantic, bool) or not isinstance(semantic, int) or semantic < 1:
        fail("invalid_remote_response", "control response semantic revision is invalid")
    if not accepted_window["schema_min"] <= schema <= accepted_window["schema_max"]:
        fail("response_schema_incompatible", "control response schema is outside the accepted window", {"action": action})
    if not accepted_window["semantic_min"] <= semantic <= accepted_window["semantic_max"]:
        fail("response_semantic_incompatible", "control response semantic revision is outside the accepted window", {"action": action})
    return None


def _append_warning(warnings: list[dict[str, Any]], item: dict[str, Any]) -> None:
    identity = (item.get("code"), item.get("action"), item.get("message"))
    if not any((existing.get("code"), existing.get("action"), existing.get("message")) == identity for existing in warnings):
        warnings.append(item)


def _finalize_local_operation(path: Path, request: Mapping[str, Any], response: Mapping[str, Any]) -> None:
    _write_local_operation(path, request, {"outcome": "success", "response": dict(response)})


def _finalize_local_error(path: Path, request: Mapping[str, Any], error: Mapping[str, Any]) -> None:
    _write_local_operation(path, request, {"outcome": "error", "error": dict(error)})


def _write_local_operation(path: Path, request: Mapping[str, Any], result: Mapping[str, Any]) -> None:
    temporary = path.with_name("." + path.name + ".tmp")
    try:
        with temporary.open("w", encoding="utf-8") as handle:
            json.dump({"request": dict(request), **dict(result)}, handle, sort_keys=True)
            handle.write("\n")
            handle.flush()
            os.fchmod(handle.fileno(), 0o600)
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        directory_fd = os.open(path.parent, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    except OSError:
        try:
            if temporary.exists():
                temporary.unlink()
        except OSError:
            pass
        fail(
            "local_journal_update_failed",
            "remote operation outcome is known but the local operation journal could not be finalized",
            {"operation_id": request.get("operation_id")},
        )


def _reuse_local_operation(path: Path, request: Mapping[str, Any], *, wait_for_writer: bool = False) -> dict[str, Any]:
    attempts = 50 if wait_for_writer else 1
    for attempt in range(attempts):
        try:
            old = json.loads(path.read_text())
        except (ValueError, OSError):
            if attempt + 1 == attempts:
                fail("local_journal_invalid", "operation journal is unreadable")
            time.sleep(0.01)
            continue
        old_request = old.get("request") if isinstance(old, dict) else None
        if not isinstance(old_request, dict):
            fail("local_journal_invalid", "operation journal request is invalid")
        current_intent = {key: value for key, value in request.items() if key != "compatibility"}
        old_intent = {key: value for key, value in old_request.items() if key != "compatibility"}
        if old_intent != current_intent:
            fail("operation_conflict", "operation ID already describes another request")
        # Replay the exact request recorded before the client sidecar changed.
        return dict(old_request)
    fail("local_journal_invalid", "operation journal is unreadable")


def _json_value_matches(actual: Any, expected: Any) -> bool:
    return type(actual) is type(expected) and actual == expected


def _parse_remote_error(raw: bytes | None, *, request: Mapping[str, Any]) -> dict[str, Any] | None:
    if not raw:
        return None
    try:
        lines = raw.decode("utf-8", "replace").splitlines()
    except Exception:
        return None
    matches: list[dict[str, Any]] = []
    for line in lines:
        try:
            payload = json.loads(line)
        except (TypeError, ValueError):
            continue
        if not isinstance(payload, dict):
            continue
        if payload.get("ok") is not False or not _json_value_matches(payload.get("protocol"), PROTOCOL_VERSION):
            continue
        # A write journal may be finalized only from an error envelope emitted
        # for this exact request.  SSH/wrapper diagnostics are not outcomes.
        identity = ("action", "session_id", "generation", "operation_id")
        if any(key not in payload or not _json_value_matches(payload[key], request[key]) for key in identity if key in request):
            continue
        error = payload.get("error")
        if not isinstance(error, dict):
            continue
        code = error.get("code")
        message = error.get("message")
        if isinstance(code, str) and code and isinstance(message, str) and message:
            matches.append(error)
    return matches[0] if len(matches) == 1 else None


_DETERMINISTIC_REMOTE_ERROR_CODES = frozenset({
    # Release and compatibility preflight happens before the control process
    # opens the state store or performs a native effect.
    "artifact_checksum_mismatch",
    "artifact_missing",
    "artifact_mode_mismatch",
    "artifact_path_invalid",
    "artifact_unverified",
    "bundle_invalid",
    "cache_ineligible",
    "config_invalid",
    "config_missing",
    "compatibility_action_mismatch",
    "compatibility_invalid",
    "capacity_insufficient",
    "client_sidecar_invalid",
    "draining",
    "generation_mismatch",
    "invalid_request",
    "manifest_action_unsupported",
    "manifest_format_unsupported",
    "manifest_invalid",
    "manifest_missing",
    "manifest_path_invalid",
    "manifest_self_reference",
    "manifest_unverified",
    "operation_conflict",
    "protocol_mismatch",
    "protocol_unsupported",
    "request_schema_incompatible",
    "request_semantic_incompatible",
    "response_schema_incompatible",
    "response_semantic_incompatible",
    "resource_insufficient",
    "resource_pressure",
    "resource_budget_invalid",
    "resource_unverified",
    "lease_window_short",
    "task_missing",
    "task_not_active",
    "unknown_action",
})


def _remote_error_outcome_uncertain(error: Mapping[str, Any], *, action: str | None = None) -> bool:
    code = error.get("code")
    if code == "sqlite_unavailable":
        detail = error.get("detail")
        return not isinstance(detail, Mapping) or detail.get("state_committed") is not False
    detail = error.get("detail")
    if isinstance(detail, Mapping) and isinstance(detail.get("outcome_verified"), bool):
        return not detail["outcome_verified"]
    if code in {"acquire_partial", "resize_partial", "restart_partial", "cleanup_incomplete", "operation_unverified"}:
        return True
    if isinstance(detail, Mapping) and any(
        isinstance(detail.get(key), str)
        and detail.get(key) in {
            "native-generation", "native-init", "native-config", "native-stop", "native-start",
            "guest-readiness", "guest-post-check", "access-exposure", "port-add", "port-remove", "cache-delete",
        }
        for key in ("phase", "operation_phase")
    ):
        return True
    if action == "port-add" and code in {"generation_mismatch", "task_not_active"}:
        # Older protocol-4 controllers do not report the native phase. Keep
        # the conservative classification because a proxy may already exist.
        return True
    if code in _DETERMINISTIC_REMOTE_ERROR_CODES:
        return False
    return True


def _drain_bounded_stream(fd: int, limit: int, output: list[bytes], truncated: list[bool],
                          stop_event: threading.Event | None = None,
                          eof: list[bool] | None = None) -> None:
    retained = bytearray()
    try:
        try:
            while True:
                if stop_event is not None and stop_event.is_set():
                    break
                ready, _, _ = select.select([fd], [], [], 0.1)
                if not ready:
                    continue
                chunk = os.read(fd, 65536)
                if not chunk:
                    if eof is not None:
                        eof[0] = True
                    break
                remaining = limit + 1 - len(retained)
                if remaining > 0:
                    retained.extend(chunk[:remaining])
                if len(retained) > limit:
                    truncated[0] = True
        except OSError:
            # The reader owns the descriptor; cancellation is handled by the
            # bounded select loop so a late reader cannot close a reused fd.
            pass
    finally:
        try:
            os.close(fd)
        except OSError:
            pass
    output.append(bytes(retained))


def _bounded_process_output(value: Any, limit: int) -> tuple[bytes, bool]:
    if not isinstance(value, bytes):
        return b"", False
    return value[:limit + 1], len(value) > limit


def control_request(action: str, *, session: str | None, generation: int | None = None,
                    write: bool = False, operation_id: str | None = None,
                    fields: dict[str, Any] | None = None) -> dict[str, Any]:
    if write and session is None: fail("missing_session", f"{SESSION_ENV} is required")
    request = {"protocol": PROTOCOL_VERSION, "action": action}
    if session is not None: request["session_id"] = session
    if generation is not None: request["generation"] = generation
    if write: request["operation_id"] = operation_id or str(uuid.uuid4())
    if fields: request.update(fields)
    metadata, local_warnings = load_client_release_metadata()
    request["compatibility"] = client_capability(action, metadata)
    op_path: Path | None = None
    if write:
        directory = local_session_dir(session) / "operations"
        directory.mkdir(mode=0o700, parents=True, exist_ok=True)
        op_path = directory / (hashlib.sha256(request["operation_id"].encode()).hexdigest() + ".json")
        try:
            with op_path.open("x", encoding="utf-8") as handle:
                os.fchmod(handle.fileno(), 0o600)
                json.dump({"request": request, "outcome": "pending"}, handle, sort_keys=True)
                handle.flush()
                os.fsync(handle.fileno())
        except FileExistsError:
            # Another client may have created the journal just before us.  The
            # bounded retry also avoids reading its file between create and fsync.
            request = _reuse_local_operation(op_path, request, wait_for_writer=True)
    try:
        control_host = os.environ.get(CONTROL_HOST_ENV, DEFAULT_CONTROL_HOST)
        remote_control = os.environ.get(REMOTE_CONTROL_ENV, DEFAULT_REMOTE_CONTROL)
        stdout_read, stdout_write = os.pipe()
        stderr_read, stderr_write = os.pipe()
        stdout_sink = os.fdopen(stdout_write, "wb")
        stderr_sink = os.fdopen(stderr_write, "wb")
        stdout_output: list[bytes] = []
        stderr_output: list[bytes] = []
        stdout_truncated_flag = [False]
        stderr_truncated_flag = [False]
        stdout_eof = [False]
        stderr_eof = [False]
        stream_stop = threading.Event()
        stdout_reader = threading.Thread(
            target=_drain_bounded_stream,
            args=(stdout_read, REMOTE_STDOUT_LIMIT_BYTES, stdout_output, stdout_truncated_flag, stream_stop, stdout_eof),
            daemon=True,
        )
        stderr_reader = threading.Thread(
            target=_drain_bounded_stream,
            args=(stderr_read, REMOTE_STDERR_LIMIT_BYTES, stderr_output, stderr_truncated_flag, stream_stop, stderr_eof),
            daemon=True,
        )
        stdout_reader.start()
        stderr_reader.start()
        process = None
        remote_timeout = False
        stream_cleanup_verified = True
        stream_cleanup_error = None
        request_timeout = control_timeout(action, fields)
        request_started = time.monotonic()
        try:
            try:
                process = subprocess.run(["ssh", "-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "StrictHostKeyChecking=yes", "--", control_host,
                                          shlex.join(["python3", remote_control])],
                                         input=json.dumps(request, sort_keys=True).encode(), stdout=stdout_sink,
                                         stderr=stderr_sink, check=False, timeout=request_timeout)
            except subprocess.TimeoutExpired:
                remote_timeout = True
                raise
        finally:
            stdout_sink.close()
            stderr_sink.close()
            if remote_timeout:
                stream_stop.set()
                stdout_reader.join(timeout=1)
                stderr_reader.join(timeout=1)
                if stdout_reader.is_alive() or stderr_reader.is_alive():
                    stream_cleanup_verified = False
                    stream_cleanup_error = "reader_join_timeout"
                elif not stdout_eof[0] or not stderr_eof[0]:
                    stream_cleanup_verified = False
                    stream_cleanup_error = "reader_cancelled_before_eof"
            else:
                reader_deadline = request_started + request_timeout
                for reader in (stdout_reader, stderr_reader):
                    reader.join(timeout=max(0.0, reader_deadline - time.monotonic()))
                    if reader.is_alive():
                        stream_stop.set()
                        reader.join(timeout=0.2)
                    if reader.is_alive():
                        stream_cleanup_verified = False
                        stream_cleanup_error = "reader_join_timeout"
                if not stdout_eof[0] or not stderr_eof[0]:
                    stream_cleanup_verified = False
                    stream_cleanup_error = stream_cleanup_error or "reader_cancelled_before_eof"
        stdout = stdout_output[0] if stdout_output else b""
        stderr = stderr_output[0] if stderr_output else b""
        if process is not None:
            mocked_stdout, mocked_stdout_truncated = _bounded_process_output(process.stdout, REMOTE_STDOUT_LIMIT_BYTES)
            mocked_stderr, mocked_stderr_truncated = _bounded_process_output(process.stderr, REMOTE_STDERR_LIMIT_BYTES)
            if isinstance(process.stdout, bytes):
                stdout = mocked_stdout
                stdout_truncated_flag[0] = stdout_truncated_flag[0] or mocked_stdout_truncated
            if isinstance(process.stderr, bytes):
                stderr = mocked_stderr
                stderr_truncated_flag[0] = stderr_truncated_flag[0] or mocked_stderr_truncated
    except FileNotFoundError: fail("command_missing", "ssh is not installed")
    except subprocess.TimeoutExpired:
        detail = {
            "operation_id": request.get("operation_id"),
            "stream_cleanup_verified": stream_cleanup_verified,
        }
        if stream_cleanup_error is not None:
            detail["stream_cleanup_error"] = stream_cleanup_error
        fail("remote_timeout", "control outcome is uncertain; retry the same operation ID",
             detail)
    stdout_truncated = stdout_truncated_flag[0]
    stderr_truncated = stderr_truncated_flag[0]
    stderr = stderr[:REMOTE_STDERR_LIMIT_BYTES]
    if stderr:
        sys.stderr.buffer.write(stderr)
        sys.stderr.flush()
    if not stream_cleanup_verified:
        detail = {
            "operation_id": request.get("operation_id"),
            "stream_cleanup_verified": False,
        }
        if stream_cleanup_error is not None:
            detail["stream_cleanup_error"] = stream_cleanup_error
        exit_code = process.returncode if process is not None and process.returncode else 1
        fail("remote_failed", "control response stream cleanup is not verified", detail, exit_code)
    if process.returncode:
        if stdout_truncated or stderr_truncated or not stream_cleanup_verified:
            detail = {
                "operation_id": request.get("operation_id"),
                "stdout_truncated": stdout_truncated,
                "stderr_truncated": stderr_truncated,
                "stdout_limit_bytes": REMOTE_STDOUT_LIMIT_BYTES,
                "stderr_limit_bytes": REMOTE_STDERR_LIMIT_BYTES,
                "stream_cleanup_verified": stream_cleanup_verified,
            }
            if stream_cleanup_error is not None:
                detail["stream_cleanup_error"] = stream_cleanup_error
            fail("remote_failed", "remote request output was truncated or stream cleanup is not verified",
                 detail, process.returncode)
        remote_error = _parse_remote_error(stderr, request=request)
        if remote_error is not None:
            if op_path is not None and not _remote_error_outcome_uncertain(remote_error, action=action):
                _finalize_local_error(op_path, request, remote_error)
            fail(
                str(remote_error["code"]),
                str(remote_error["message"]),
                remote_error.get("detail"),
                process.returncode,
            )
        fail("remote_failed", "remote request failed; control outcome is not verified",
             {"operation_id": request.get("operation_id")}, process.returncode)
    if stdout_truncated:
        fail("invalid_remote_response", "control response exceeded the bounded stdout limit",
             {"stdout_truncated": True, "stdout_limit_bytes": REMOTE_STDOUT_LIMIT_BYTES})
    try: response = json.loads(stdout)
    except (ValueError, UnicodeDecodeError):
        detail = {"stream_cleanup_verified": stream_cleanup_verified}
        if stream_cleanup_error is not None:
            detail["stream_cleanup_error"] = stream_cleanup_error
        fail("invalid_remote_response", "control did not return JSON", detail)
    if not isinstance(response, dict) or response.get("protocol") != PROTOCOL_VERSION or response.get("action") != action or response.get("session_id") != session:
        fail("protocol_mismatch", "response protocol or request identity differs")
    if "generation" in request and not _json_value_matches(response.get("generation"), request["generation"]):
        fail("protocol_mismatch", "response generation differs")
    compatibility = request.get("compatibility")
    if not isinstance(compatibility, dict):
        fail("protocol_mismatch", "request compatibility capability is missing")
    warnings = _validate_response_warnings(response.get("warnings"), action)
    legacy_outcome = compatibility.get("legacy") is True and "outcome" not in response
    if legacy_outcome:
        response["outcome"] = "success"
        _append_warning(
            warnings,
            _client_warning(
                "legacy_response_outcome",
                "control response omitted outcome; accepted as legacy protocol-4 success",
                action,
            ),
        )
    elif not isinstance(response.get("outcome"), str) or response["outcome"] not in {"success", "replayed"}:
        fail("invalid_remote_response", "control response outcome is missing or invalid")
    if response.get("ok") is not True or not isinstance(response.get("result"), dict) or not isinstance(response.get("server_time"), str):
        fail("invalid_remote_response", "invalid control response envelope")
    if write and response.get("operation_id") != request["operation_id"]:
        fail("protocol_mismatch", "operation identity differs")
    response_warning = _validate_response_compatibility(
        response.get("response"), action, compatibility.get("accepted_response")
    )
    if response_warning is not None:
        _append_warning(warnings, response_warning)
    if "release" in response:
        response["release"] = _validate_response_release(response["release"])
    for item in local_warnings:
        enriched = dict(item)
        enriched["action"] = action
        _append_warning(warnings, enriched)
    if "release" not in response:
        _append_warning(warnings, _client_warning("legacy_control_release", "control response did not identify a release", action))
    response["warnings"] = warnings
    if write and op_path is not None:
        _finalize_local_operation(op_path, request, response)
    return response



def run_command(argv: Sequence[str], *, input_data: bytes | None = None, output: bool = False) -> int:
    try:
        process = subprocess.run(list(argv), input=input_data, check=False)
    except FileNotFoundError:
        fail("command_missing", f"command not found: {argv[0]}")
    return process.returncode


def make_ssh_key(session: str) -> tuple[Path, str]:
    directory = local_session_dir(session) / "keys"
    directory.mkdir(mode=0o700, parents=True, exist_ok=True)
    private_key = directory / "id_ed25519"
    public_key = private_key.with_suffix(".pub")
    if not private_key.exists() or not public_key.exists():
        try:
            process = subprocess.run(
                ["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(private_key)],
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                check=False,
            )
        except FileNotFoundError as exc:
            fail("command_missing", "ssh-keygen is not installed")
        if process.returncode != 0:
            fail("keygen_failed", "ssh-keygen failed", process.stderr.decode("utf-8", "replace"))
    os.chmod(private_key, 0o600)
    try:
        key = public_key.read_text(encoding="utf-8").strip()
    except OSError:
        fail("key_missing", "generated public key cannot be read")
    if not key:
        fail("key_invalid", "generated public key is empty")
    return private_key, key


def ssh_paths(session: str, info: dict[str, Any]) -> tuple[Path, Path]:
    task_id = str(info["task_id"])
    generation = int(info["generation"])
    directory = LOCAL_CONFIG_ROOT / "sessions" / hashlib.sha256(session.encode("utf-8")).hexdigest()[:24]
    directory.mkdir(mode=0o700, parents=True, exist_ok=True)
    return directory / f"agent-vm-{task_id}-g{generation}.ssh", directory / f"agent-vm-{task_id}-g{generation}.known_hosts"


def write_ssh_config(session: str, info: dict[str, Any], private_key: Path) -> Path:
    host = str(info["host"])
    port = int(info["port"])
    alias = str(info["host_key_alias"])
    try: ipaddress.IPv4Address(host)
    except ValueError: fail("invalid_remote_response", "invalid VM endpoint address")
    if not 1024 <= port <= 65535 or not re.fullmatch(r"agent-vm-[a-z0-9-]+-g[1-9][0-9]*", alias):
        fail("invalid_remote_response", "invalid VM endpoint identity")
    if not re.fullmatch(r"avm-[a-z0-9-]+", str(info["task_id"])) or int(info["generation"]) < 1:
        fail("invalid_remote_response", "invalid task identity")
    host_key = str(info["host_key"])
    fields = host_key.split()
    if len(fields) != 2 or fields[0] != "ssh-ed25519" or not re.fullmatch(r"[A-Za-z0-9+/=]+", fields[1]):
        fail("invalid_host_key", "control returned an invalid trusted host key")
    config_path, known_hosts_path = ssh_paths(session, info)
    known_hosts = f"{alias} {host_key}\n"
    config = (
        "Host agent-vm\n"
        f"  HostName {host}\n"
        f"  Port {port}\n"
        "  User root\n"
        f"  HostKeyAlias {alias}\n"
        "  StrictHostKeyChecking yes\n"
        "  IdentitiesOnly yes\n"
        f"  IdentityFile {ssh_config_path(private_key)}\n"
        f"  UserKnownHostsFile {ssh_config_path(known_hosts_path)}\n"
        "  ControlMaster no\n"
        "  ControlPath none\n"
    )
    for path, content in ((known_hosts_path, known_hosts), (config_path, config)):
        temp = path.with_name(f".{path.name}.{os.getpid()}")
        with temp.open("w", encoding="utf-8") as handle:
            handle.write(content)
            handle.flush()
            os.fchmod(handle.fileno(), 0o600)
            os.fsync(handle.fileno())
        os.replace(temp, path)
    return config_path


def ssh_config_path(path: Path) -> str:
    value = str(path.resolve())
    if any(ch in value for ch in ('\n', '\r', '\0')):
        fail("invalid_path", "SSH configuration path contains a control character")
    return '"' + value.replace('\\', '\\\\').replace('"', '\\"').replace('%', '%%') + '"'


def fetch_ssh_info(session: str) -> tuple[dict[str, Any], dict[str, Any], Path]:
    response = control_request("ssh-info", session=session)
    info = response["result"]
    if not isinstance(info, dict):
        fail("invalid_remote_response", "ssh-info result is not an object")
    state = read_session_state(session)
    private_key = Path(str(state["private_key"]))
    if state.get("task_id") != info["task_id"] or state.get("generation") != info["generation"]:
        fail("generation_mismatch", "connection material belongs to another lease; acquire the current lease first")
    if not private_key.is_file():
        fail("key_missing", f"private key is missing: {private_key}")
    config_path = write_ssh_config(session, info, private_key)
    _, known_hosts = ssh_paths(session, info)
    info.update(identity_file=str(private_key), known_hosts_file=str(known_hosts), ssh_config=str(config_path))
    return response, info, config_path


def parse_memory(value: str) -> int:
    match = re.fullmatch(r"([0-9]+)(MiB|GiB|M|G)", value, re.IGNORECASE)
    if match is None:
        fail("invalid_size", "memory must use MiB, GiB, M or G")
    amount = int(match.group(1))
    unit = match.group(2).lower()
    return amount if unit == "mib" or unit == "m" else amount * 1024


def parse_disk(value: str) -> int:
    match = re.fullmatch(r"([0-9]+)(GiB|G)", value, re.IGNORECASE)
    if match is None:
        fail("invalid_size", "disk must use GiB or G")
    return int(match.group(1))


def add_common_mutation_options(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--operation-id", help="reuse this ID when retrying a mutation")


def add_session_option(parser: argparse.ArgumentParser, *, suppress_default: bool = False) -> None:
    options = {"help": f"session identity override (defaults to {SESSION_ENV})"}
    if suppress_default:
        options["default"] = argparse.SUPPRESS
    parser.add_argument("--session-id", **options)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="agent-vm")
    parser.add_argument("--version", action="version", version=f"%(prog)s {VERSION}")
    add_session_option(parser)
    subparsers = parser.add_subparsers(dest="command", required=True)
    version = subparsers.add_parser("version")
    add_session_option(version, suppress_default=True)
    help_parser = subparsers.add_parser("help")
    add_session_option(help_parser, suppress_default=True)

    doctor = subparsers.add_parser("doctor")
    add_session_option(doctor, suppress_default=True)
    status = subparsers.add_parser("status")
    add_session_option(status, suppress_default=True)
    acquire = subparsers.add_parser("acquire")
    add_session_option(acquire, suppress_default=True)
    acquire.add_argument("--ssh-host-port", type=int)
    acquire.add_argument("--cpu", type=int)
    acquire.add_argument("--memory")
    acquire.add_argument("--disk")
    add_common_mutation_options(acquire)
    for name in ("renew", "release", "destroy", "restart"):
        command = subparsers.add_parser(name)
        add_session_option(command, suppress_default=True)
        add_common_mutation_options(command)
    resize = subparsers.add_parser("resize")
    add_session_option(resize, suppress_default=True)
    resize.add_argument("--cpu", type=int)
    resize.add_argument("--memory")
    resize.add_argument("--disk")
    mode = resize.add_mutually_exclusive_group()
    mode.add_argument("--allow-restart", action="store_true")
    mode.add_argument("--live", action="store_true", help="adjust memory only without restarting; no automatic fallback")
    add_common_mutation_options(resize)
    ssh_info = subparsers.add_parser("ssh-info")
    add_session_option(ssh_info, suppress_default=True)
    ssh_info.add_argument("--format", choices=("json", "ssh-config"), default="json")
    ssh = subparsers.add_parser("ssh")
    add_session_option(ssh, suppress_default=True)
    ssh.add_argument("ssh_args", nargs=argparse.REMAINDER)
    execute = subparsers.add_parser("exec")
    add_session_option(execute, suppress_default=True)
    execute.add_argument("argv", nargs=argparse.REMAINDER)
    sync = subparsers.add_parser("sync")
    add_session_option(sync, suppress_default=True)
    sync.add_argument("--name", required=True)
    upload = subparsers.add_parser("upload")
    add_session_option(upload, suppress_default=True)
    upload.add_argument("source")
    upload.add_argument("destination")
    download = subparsers.add_parser("download")
    add_session_option(download, suppress_default=True)
    download.add_argument("source")
    download.add_argument("destination")
    export = subparsers.add_parser("export")
    add_session_option(export, suppress_default=True)
    export.add_argument("path")

    port = subparsers.add_parser("port")
    add_session_option(port, suppress_default=True)
    port_sub = port.add_subparsers(dest="port_command", required=True)
    port_add = port_sub.add_parser("add")
    add_session_option(port_add, suppress_default=True)
    port_add.add_argument("name")
    port_add.add_argument("--guest-port", type=int, required=True)
    port_add.add_argument("--protocol", choices=("tcp", "udp"), default="tcp")
    port_add.add_argument("--host-port", type=int)
    add_common_mutation_options(port_add)
    port_list = port_sub.add_parser("list")
    add_session_option(port_list, suppress_default=True)
    port_remove = port_sub.add_parser("remove")
    add_session_option(port_remove, suppress_default=True)
    port_remove.add_argument("name")
    port_remove.add_argument("--protocol", choices=("tcp", "udp"), default="tcp")
    add_common_mutation_options(port_remove)

    admin = subparsers.add_parser("admin")
    add_session_option(admin, suppress_default=True)
    admin_sub = admin.add_subparsers(dest="admin_command", required=True)
    admin_status = admin_sub.add_parser("status")
    add_session_option(admin_status, suppress_default=True)
    admin_metrics = admin_sub.add_parser("metrics")
    add_session_option(admin_metrics, suppress_default=True)
    admin_session = admin_sub.add_parser("session")
    add_session_option(admin_session, suppress_default=True)
    session_sub = admin_session.add_subparsers(dest="session_command", required=True)
    session_list = session_sub.add_parser("list")
    add_session_option(session_list, suppress_default=True)
    drain = admin_sub.add_parser("drain")
    add_session_option(drain, suppress_default=True)
    drain.add_argument("drain_command", choices=("enter", "status", "exit"))
    reconcile = admin_sub.add_parser("reconcile")
    add_session_option(reconcile, suppress_default=True)
    reconcile.add_argument("--apply", action="store_true")
    add_common_mutation_options(reconcile)
    gc = admin_sub.add_parser("gc")
    add_session_option(gc, suppress_default=True)
    gc.add_argument("--apply", action="store_true")
    add_common_mutation_options(gc)
    return parser


def operation_id(args: argparse.Namespace) -> str:
    value = getattr(args, "operation_id", None)
    return value or str(uuid.uuid4())


def mutation(
    action: str,
    args: argparse.Namespace,
    *,
    fields: dict[str, Any] | None = None,
    generation: int | None = None,
    session: str | None = None,
) -> dict[str, Any]:
    session = session if session is not None else session_id(getattr(args, "session_id", None))
    if generation is None and action not in {"acquire", "drain-enter", "drain-exit", "admin-reconcile", "admin-gc"}:
        assert session is not None
        generation = current_generation(session)
    return control_request(
        action,
        session=session,
        generation=0 if generation is None and action == "acquire" else generation,
        write=True,
        operation_id=operation_id(args),
        fields=fields,
    )


def command_acquire(args: argparse.Namespace) -> int:
    session = session_id(args.session_id)
    private_key, public_key = make_ssh_key(session)
    # Observation never renews. Reuse a saved operation's original generation
    # when retrying after a lost acknowledgement.
    generation = control_request("status", session=session)["result"]["generation"]
    requested_op = getattr(args, "operation_id", None)
    if requested_op:
        journal = local_session_dir(session) / "operations" / (hashlib.sha256(requested_op.encode()).hexdigest() + ".json")
        if journal.is_file(): generation = json.loads(journal.read_text())["request"]["generation"]
    fields = {"public_key": public_key}
    if args.ssh_host_port is not None: fields["ssh_host_port"] = args.ssh_host_port
    if args.cpu is not None: fields["cpu"] = args.cpu
    if args.memory is not None: fields["memory_mib"] = parse_memory(args.memory)
    if args.disk is not None: fields["disk_gib"] = parse_disk(args.disk)
    response = mutation("acquire", args, generation=generation, session=session, fields=fields)
    result = response.get("current_state", response["result"])
    if result is None or not result.get("effective_lease"):
        fail("lease_unavailable", "replayed allocation no longer has an active lease; inspect status")
    write_session_state(session, {"session_id": session, "task_id": result["task_id"],
                                 "generation": result["generation"], "private_key": str(private_key)})
    json_output(response)
    return 0



def command_ssh_info(args: argparse.Namespace) -> int:
    session = session_id(args.session_id)
    assert session is not None
    response, info, config_path = fetch_ssh_info(session)
    if args.format == "json":
        json_output(response)
    else:
        sys.stdout.write(config_path.read_text(encoding="utf-8"))
    return 0


def command_ssh(args: argparse.Namespace) -> int:
    session = session_id(args.session_id)
    assert session is not None
    _, _, config_path = fetch_ssh_info(session)
    ssh_args = list(args.ssh_args)
    if ssh_args and ssh_args[0] == "--":
        ssh_args = ssh_args[1:]
    return run_command(["ssh", "-F", str(config_path), "agent-vm", *ssh_args])


def command_exec(args: argparse.Namespace) -> int:
    session = session_id(args.session_id)
    argv = list(args.argv)
    if not argv or argv[0] != "--": fail("invalid_request", "exec requires -- followed by a program and its arguments")
    argv = argv[1:]
    if not argv or any("\0" in value for value in argv): fail("invalid_request", "invalid execution argv")
    _, _, config_path = fetch_ssh_info(session)
    return run_command(["ssh", "-F", str(config_path), "-o", "BatchMode=yes", "--", "agent-vm", shlex.join(argv)])



def command_transfer(args: argparse.Namespace, direction: str) -> int:
    session = session_id(args.session_id)
    remote = args.destination if direction == "upload" else args.source
    if not remote.startswith("/") or "\0" in remote: fail("invalid_path", "remote path must be absolute")
    _, _, config_path = fetch_ssh_info(session)
    local = str(Path(args.source if direction == "upload" else args.destination).absolute())
    source, destination = (local, "agent-vm:" + remote) if direction == "upload" else ("agent-vm:" + remote, local)
    return run_command(["scp", "-F", str(config_path), "-o", "BatchMode=yes", "--", source, destination])



def command_export(args: argparse.Namespace) -> int:
    if not args.path.startswith("/") or "\0" in args.path: fail("invalid_path", "export path must be absolute")
    _, _, config_path = fetch_ssh_info(session_id(args.session_id))
    relative = args.path.lstrip("/") or "."
    # The tar stream alone owns stdout. No server-side archive is created.
    return run_command(["ssh", "-F", str(config_path), "-o", "BatchMode=yes", "--", "agent-vm",
                        shlex.join(["tar", "-C", "/", "-cf", "-", "--", relative])])



def command_sync(args: argparse.Namespace) -> int:
    session = session_id(args.session_id)
    if re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_-]{0,63}", args.name) is None:
        fail("invalid_name", "--name must be one safe path segment")
    helper = Path(__file__).with_name("sync_worktree.py")
    if not helper.is_file(): fail("sync_helper_missing", "sync helper is missing")
    return run_command([
        sys.executable,
        str(helper),
        "--name",
        args.name,
        "--cli",
        str(Path(__file__).resolve()),
        "--session-id",
        session,
    ])



def command_port(args: argparse.Namespace) -> int:
    session = session_id(args.session_id)
    assert session is not None
    generation = current_generation(session)
    if args.port_command == "list":
        response = control_request("port-list", session=session, generation=generation)
    elif args.port_command == "add":
        response = mutation(
            "port-add",
            args,
            fields={
                "name": args.name,
                "vm_port": args.guest_port,
                "port_protocol": args.protocol,
                **({"host_port": args.host_port} if args.host_port is not None else {}),
            },
            generation=generation,
            session=session,
        )
    else:
        response = mutation(
            "port-remove",
            args,
            fields={"name": args.name, "port_protocol": args.protocol},
            generation=generation,
            session=session,
        )
    json_output(response)
    return 0


def command_admin(args: argparse.Namespace) -> int:
    if args.admin_command == "metrics":
        if args.session_id is not None:
            fail("invalid_request", "admin metrics does not accept --session-id")
        response = control_request("admin-metrics", session=None)
        json_output(response)
        return 0
    session = session_id(args.session_id, required=True)
    if args.admin_command == "status":
        response = control_request("admin-status", session=session)
    elif args.admin_command == "session":
        response = control_request("admin-session-list", session=session)
    elif args.admin_command == "drain":
        if args.drain_command == "status":
            response = control_request("drain-status", session=session)
        else:
            response = mutation(
                f"drain-{args.drain_command}",
                args,
                generation=0,
                session=session,
            )
    elif args.admin_command == "reconcile":
        response = mutation(
            "admin-reconcile",
            args,
            fields={"apply": bool(args.apply)},
            generation=0,
            session=session,
        )
    else:
        response = mutation(
            "admin-gc",
            args,
            fields={"apply": bool(args.apply)},
            generation=0,
            session=session,
        )
    json_output(response)
    return 0


def dispatch(args: argparse.Namespace) -> int:
    if args.command == "version":
        metadata, warnings = load_client_release_metadata()
        json_output({"version": VERSION, "protocol": PROTOCOL_VERSION, "release": metadata, "warnings": warnings})
        return 0
    if args.command == "help":
        build_parser().print_help()
        return 0
    if args.command == "doctor":
        response = control_request("doctor", session=session_id(args.session_id, required=False))
        json_output(response)
        return 0
    if args.command == "status":
        session = session_id(args.session_id)
        response = control_request("status", session=session)
        json_output(response)
        return 0
    if args.command == "acquire":
        return command_acquire(args)
    if args.command in {"renew", "release", "destroy", "restart"}:
        response = mutation(args.command, args)
        json_output(response)
        return 0
    if args.command == "resize":
        if args.live and (args.memory is None or args.cpu is not None or args.disk is not None):
            raise CliError("invalid_request", "--live requires only --memory; CPU/disk changes use --allow-restart")
        fields: dict[str, Any] = {"allow_restart": bool(args.allow_restart), "live_memory": bool(args.live)}
        if args.cpu is not None:
            fields["cpu"] = args.cpu
        if args.memory is not None:
            fields["memory_mib"] = parse_memory(args.memory)
        if args.disk is not None:
            fields["disk_gib"] = parse_disk(args.disk)
        response = mutation("resize", args, fields=fields)
        json_output(response)
        return 0
    if args.command == "ssh-info":
        return command_ssh_info(args)
    if args.command == "ssh":
        return command_ssh(args)
    if args.command == "exec":
        return command_exec(args)
    if args.command == "sync":
        return command_sync(args)
    if args.command == "upload":
        return command_transfer(args, "upload")
    if args.command == "download":
        return command_transfer(args, "download")
    if args.command == "export":
        return command_export(args)
    if args.command == "port":
        return command_port(args)
    if args.command == "admin":
        return command_admin(args)
    fail("unknown_command", "unknown command")
    return 2


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    try:
        args = parser.parse_args(list(argv) if argv is not None else None)
        return dispatch(args)
    except CliError as exc:
        sys.stderr.write(json.dumps({"ok": False, "error": exc.as_dict()}, sort_keys=True) + "\n")
        return exc.exit_code
    except KeyboardInterrupt:
        return 130


if __name__ == "__main__":
    raise SystemExit(main())
