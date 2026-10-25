#!/usr/bin/env python3
"""Release-manifest parsing and compatibility checks for Agent VM releases.

This standard-library-only module supplies the Python release tools with the
same artifact and unit policy as release-manifest.ts. TypeScript syntax checks
require the fixed, trusted Bun executable and never execute candidate modules.
"""

from __future__ import annotations

from hashlib import sha256
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import stat
import subprocess
from typing import Any, Mapping


MANIFEST_FORMAT = 1
PROTOCOL_VERSION = 4
SOURCE_REVISION_RE = re.compile(r"^[0-9a-f]{40}$")
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
RELEASE_ID_RE = SOURCE_REVISION_RE
LEGACY_RELEASE_ID = "legacy-protocol-4"
MANIFEST_NAME = "release-manifest.json"
CLIENT_SIDECAR_NAME = "client-release.json"
BUN_PATH = "/usr/local/bin/bun"
TYPESCRIPT_SUPPORT_ARTIFACT_NAMES = (
    "control-types.ts",
    "control-store.ts",
    "control-native.ts",
    "control-core.ts",
    "control-observation.ts",
    "control-resources.ts",
    "control-systemd.ts",
    "control-lifecycle.ts",
    "release-manifest.ts",
    "guest-root-growth.ts",
)
SUPPORT_ARTIFACT_NAMES = frozenset({
    "release_manifest.py",
    *TYPESCRIPT_SUPPORT_ARTIFACT_NAMES,
})

# Only this trusted program is evaluated. Candidate source is stdin data;
# transformSync parses it without importing modules or running its output.
TYPESCRIPT_SYNTAX_CHECK = (
    "const source = await Bun.stdin.text();\n"
    'new Bun.Transpiler({loader: "ts", target: "bun"}).transformSync(source);\n'
)

# Keep this list in sync with the one-shot control dispatch table.  The list is
# intentionally explicit: JSON parseability must never make a new action
# executable on an old control process.
ACTION_NAMES = (
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

UNIT_EXEC_START = {
    "agent-vm-native-age-gc.service":
        "ExecStart=/usr/local/bin/bun /opt/agent-vm-native/agent_vm_control.ts --timer-age-gc",
    "agent-vm-native-expire@.service":
        "ExecStart=/usr/local/bin/bun /opt/agent-vm-native/agent_vm_control.ts --timer-expire %i",
    "agent-vm-native-reconcile.service":
        "ExecStart=/usr/local/bin/bun /opt/agent-vm-native/agent_vm_control.ts --timer-reconcile",
}

# Unit files are deployment policy, not caller-provided configuration.  The
# wrapper and publisher both require these exact bytes in addition to the
# manifest checksum so a modified manifest cannot authorize new scheduling or
# control commands.
UNIT_CONTENT_SHA256 = {
    "agent-vm-native-age-gc.service": "6a6387459c3f4b5f888fc7d4fe6d49cf571ff4eabd3a96edde72a05d37588460",
    "agent-vm-native-age-gc.timer": "4eb1c7b012fc3251ef4427c56b8c1b0236deff28762cc4293278f56d02c4cb4b",
    "agent-vm-native-expire@.service": "e0cfb3c7eaf4aaadcfe5e296a0698f0cb235c4bbecd22275ad5c6826d7cbd3e2",
    "agent-vm-native-reconcile.service": "76ac0a8e730ffdfa4c0fe0b0c0cd6ad75dd11e627b747e843ded9fae5fd8ddc8",
}

FIXED_ARTIFACTS = frozenset({
    ("agent_vm.py", "agent_vm.py", "client", 0o755),
    ("agent_vm_control.ts", "agent_vm_control.ts", "control", 0o755),
    *{(name, name, "support", 0o644) for name in SUPPORT_ARTIFACT_NAMES},
    ("unit:agent-vm-native-age-gc.service", "systemd/agent-vm-native-age-gc.service", "unit", 0o644),
    ("unit:agent-vm-native-age-gc.timer", "systemd/agent-vm-native-age-gc.timer", "unit", 0o644),
    ("unit:agent-vm-native-expire@.service", "systemd/agent-vm-native-expire@.service", "unit", 0o644),
    ("unit:agent-vm-native-reconcile.service", "systemd/agent-vm-native-reconcile.service", "unit", 0o644),
})


class ManifestError(Exception):
    """A fail-closed manifest or artifact validation error."""

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


def _error(code: str, message: str, detail: Any = None) -> ManifestError:
    return ManifestError(code, message, detail)


def verify_typescript_syntax(root: Path, manifest: Mapping[str, Any], *, bun_path: str | None = None) -> None:
    """Parse candidate TypeScript as data; the protected wrapper fixes bun_path."""
    executable = bun_path or shutil.which("bun")
    if executable is None:
        raise _error("typescript_syntax_unverified", "Bun is required to verify TypeScript syntax")
    for item in manifest["artifacts"]:
        if item["role"] not in {"client", "control", "support"}:
            continue
        relative = PurePosixPath(item["path"])
        if relative.suffix != ".ts":
            continue
        path = root / Path(*relative.parts)
        try:
            source = path.read_text(encoding="utf-8").encode("utf-8")
        except (OSError, UnicodeError) as exc:
            raise _error(
                "typescript_syntax_invalid",
                f"TypeScript artifact cannot be read: {item['path']}",
            ) from exc
        try:
            process = subprocess.run(
                [executable, "--no-macros", "--eval", TYPESCRIPT_SYNTAX_CHECK],
                input=source,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.PIPE,
                check=False,
                timeout=30,
                cwd="/",
                # Do not inherit preloads, runtime options, or candidate config.
                env={"PATH": "/usr/local/bin:/usr/bin:/bin"},
            )
        except (OSError, subprocess.TimeoutExpired) as exc:
            raise _error(
                "typescript_syntax_unverified",
                "fixed Bun transpiler could not verify TypeScript syntax",
                {"path": item["path"]},
            ) from exc
        if process.returncode != 0:
            raise _error(
                "typescript_syntax_invalid",
                f"TypeScript artifact cannot be transpiled: {item['path']}",
            )


def _require_mapping(value: Any, label: str) -> Mapping[str, Any]:
    if not isinstance(value, Mapping):
        raise _error("manifest_invalid", f"{label} must be an object")
    return value


def _require_string(value: Any, label: str, pattern: re.Pattern[str] | None = None) -> str:
    if not isinstance(value, str) or not value:
        raise _error("manifest_invalid", f"{label} must be a non-empty string")
    if pattern is not None and pattern.fullmatch(value) is None:
        raise _error("manifest_invalid", f"{label} has an invalid value")
    return value


def _require_int(value: Any, label: str, minimum: int | None = None, maximum: int | None = None) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        raise _error("manifest_invalid", f"{label} must be an integer")
    if minimum is not None and value < minimum:
        raise _error("manifest_invalid", f"{label} is below its minimum")
    if maximum is not None and value > maximum:
        raise _error("manifest_invalid", f"{label} is above its maximum")
    return value


def _safe_relative_path(value: Any, label: str) -> str:
    path = _require_string(value, label)
    if "\\" in path or "\x00" in path:
        raise _error("manifest_path_invalid", f"{label} contains an unsupported separator")
    parsed = PurePosixPath(path)
    if parsed.is_absolute() or any(part in {"", ".", ".."} for part in parsed.parts):
        raise _error("manifest_path_invalid", f"{label} must be a relative regular-file path")
    return parsed.as_posix()


def _window(value: Any, label: str) -> dict[str, int]:
    raw = _require_mapping(value, label)
    expected = ("schema_min", "schema_max", "semantic_min", "semantic_max")
    if set(raw) != set(expected):
        raise _error("manifest_invalid", f"{label} must declare schema and semantic min/max")
    result = {
        key: _require_int(raw[key], f"{label}.{key}", 1)
        for key in expected
    }
    if result["schema_min"] > result["schema_max"] or result["semantic_min"] > result["semantic_max"]:
        raise _error("manifest_invalid", f"{label} has an inverted compatibility window")
    return result


def _schema(value: Any, label: str) -> dict[str, int]:
    raw = _require_mapping(value, label)
    if set(raw) != {"schema", "semantic_revision"}:
        raise _error("manifest_invalid", f"{label} must declare schema and semantic_revision")
    return {
        "schema": _require_int(raw["schema"], f"{label}.schema", 1),
        "semantic_revision": _require_int(raw["semantic_revision"], f"{label}.semantic_revision", 1),
    }


def operation_spec(
    *,
    request_schema: int = 1,
    response_schema: int = 1,
    request_semantic_revision: int = 1,
    response_semantic_revision: int = 1,
    client_to_control: Mapping[str, int] | None = None,
    control_to_client: Mapping[str, int] | None = None,
) -> dict[str, Any]:
    """Create one explicit action compatibility declaration."""
    c2c = dict(client_to_control or {
        "schema_min": request_schema,
        "schema_max": request_schema,
        "semantic_min": request_semantic_revision,
        "semantic_max": request_semantic_revision,
    })
    c2l = dict(control_to_client or {
        "schema_min": response_schema,
        "schema_max": response_schema,
        "semantic_min": response_semantic_revision,
        "semantic_max": response_semantic_revision,
    })
    return {
        "request": {"schema": request_schema, "semantic_revision": request_semantic_revision},
        "response": {"schema": response_schema, "semantic_revision": response_semantic_revision},
        "client_to_control": c2c,
        "control_to_client": c2l,
    }


def default_operations() -> dict[str, dict[str, Any]]:
    return {action: operation_spec() for action in ACTION_NAMES}


def legacy_manifest() -> dict[str, Any]:
    """Return the explicit protocol-4 fallback used by pre-manifest installs."""
    artifacts = [
        {"name": name, "path": path, "role": role, "sha256": "0" * 64, "mode": mode}
        for name, path, role, mode in sorted(FIXED_ARTIFACTS)
    ]
    actions = default_operations()
    return {
        "format": MANIFEST_FORMAT,
        "release_id": "0" * 40,
        "source_revision": "0" * 40,
        "protocol": PROTOCOL_VERSION,
        "artifacts": artifacts,
        "actions": actions,
        "client_view": {
            "release_id": "0" * 40,
            "source_revision": "0" * 40,
            "protocol": PROTOCOL_VERSION,
            "actions": actions,
        },
    }


def build_manifest(
    source_revision: str,
    artifacts: list[Mapping[str, Any]],
    *,
    operations: Mapping[str, Mapping[str, Any]] | None = None,
) -> dict[str, Any]:
    """Build and validate a release manifest without self-referencing it."""
    manifest: dict[str, Any] = {
        "format": MANIFEST_FORMAT,
        "release_id": source_revision,
        "source_revision": source_revision,
        "protocol": PROTOCOL_VERSION,
        "artifacts": [dict(item) for item in artifacts],
        "actions": {key: dict(value) for key, value in (operations or default_operations()).items()},
    }
    manifest["client_view"] = {
        "release_id": source_revision,
        "source_revision": source_revision,
        "protocol": PROTOCOL_VERSION,
        "actions": manifest["actions"],
    }
    return validate_manifest(manifest)


def validate_manifest(value: Any) -> dict[str, Any]:
    """Validate the complete manifest and return a JSON-safe copy."""
    root = _require_mapping(value, "manifest")
    required = {"format", "release_id", "source_revision", "protocol", "artifacts", "actions", "client_view"}
    if not required.issubset(root):
        raise _error("manifest_invalid", "manifest is missing required fields", {"missing": sorted(required - set(root))})
    allowed = required
    if set(root) - allowed:
        raise _error("manifest_invalid", "manifest contains unknown fields", {"fields": sorted(set(root) - allowed)})
    if _require_int(root["format"], "format", 1) != MANIFEST_FORMAT:
        raise _error("manifest_format_unsupported", f"manifest format must be {MANIFEST_FORMAT}")
    source_revision = _require_string(root["source_revision"], "source_revision", SOURCE_REVISION_RE)
    release_id = _require_string(root["release_id"], "release_id", RELEASE_ID_RE)
    if release_id != source_revision:
        raise _error("manifest_invalid", "release_id must equal source_revision")
    if _require_int(root["protocol"], "protocol", 1) != PROTOCOL_VERSION:
        raise _error("protocol_unsupported", f"manifest protocol must be {PROTOCOL_VERSION}")

    raw_artifacts = root["artifacts"]
    if not isinstance(raw_artifacts, list) or not raw_artifacts:
        raise _error("manifest_invalid", "artifacts must be a non-empty list")
    artifacts: list[dict[str, Any]] = []
    names: set[str] = set()
    paths: set[str] = set()
    for index, raw in enumerate(raw_artifacts):
        item = _require_mapping(raw, f"artifacts[{index}]")
        if set(item) != {"name", "path", "role", "sha256", "mode"}:
            raise _error("manifest_invalid", f"artifacts[{index}] has an unexpected shape")
        name = _require_string(item["name"], f"artifacts[{index}].name", re.compile(r"^[A-Za-z0-9_.:@-]{1,120}$"))
        path = _safe_relative_path(item["path"], f"artifacts[{index}].path")
        role = _require_string(item["role"], f"artifacts[{index}].role", re.compile(r"^(client|control|support|unit)$"))
        digest = _require_string(item["sha256"], f"artifacts[{index}].sha256", SHA256_RE)
        mode = _require_int(item["mode"], f"artifacts[{index}].mode", 0, 0o777)
        if name in names or path in paths:
            raise _error("manifest_invalid", "artifact names and paths must be unique")
        if path in {MANIFEST_NAME, CLIENT_SIDECAR_NAME}:
            raise _error("manifest_self_reference", "release metadata cannot checksum itself")
        names.add(name)
        paths.add(path)
        artifacts.append({"name": name, "path": path, "role": role, "sha256": digest, "mode": mode})
    actual_artifacts = {(item["name"], item["path"], item["role"], item["mode"]) for item in artifacts}
    if actual_artifacts != FIXED_ARTIFACTS:
        raise _error(
            "manifest_artifact_set_invalid",
            "manifest artifact set differs from the fixed release allowlist",
            {"expected": sorted(FIXED_ARTIFACTS), "actual": sorted(actual_artifacts)},
        )

    raw_actions = _require_mapping(root["actions"], "actions")
    unsupported_actions = set(raw_actions) - set(ACTION_NAMES)
    if unsupported_actions:
        raise _error(
            "manifest_action_unsupported",
            "manifest declares an unsupported action",
            {"actions": sorted(unsupported_actions)},
        )
    if set(raw_actions) != set(ACTION_NAMES):
        raise _error(
            "manifest_action_set_invalid",
            "manifest action set differs from the fixed control surface",
            {"expected": list(ACTION_NAMES), "actual": sorted(raw_actions)},
        )
    actions: dict[str, dict[str, Any]] = {}
    for action, raw_spec in raw_actions.items():
        action_name = _require_string(action, "action", re.compile(r"^[a-z][a-z0-9-]{0,63}$"))
        if action_name not in ACTION_NAMES:
            raise _error("manifest_action_unsupported", f"manifest declares an unsupported action: {action_name}")
        spec = _require_mapping(raw_spec, f"actions.{action_name}")
        expected = {"request", "response", "client_to_control", "control_to_client"}
        if set(spec) != expected:
            raise _error("manifest_invalid", f"actions.{action_name} has an unexpected shape")
        request = _schema(spec["request"], f"actions.{action_name}.request")
        response = _schema(spec["response"], f"actions.{action_name}.response")
        c2c = _window(spec["client_to_control"], f"actions.{action_name}.client_to_control")
        c2l = _window(spec["control_to_client"], f"actions.{action_name}.control_to_client")
        if not c2c["schema_min"] <= request["schema"] <= c2c["schema_max"] or not c2c["semantic_min"] <= request["semantic_revision"] <= c2c["semantic_max"]:
            raise _error("manifest_invalid", f"actions.{action_name} request is outside its declared window")
        if not c2l["schema_min"] <= response["schema"] <= c2l["schema_max"] or not c2l["semantic_min"] <= response["semantic_revision"] <= c2l["semantic_max"]:
            raise _error("manifest_invalid", f"actions.{action_name} response is outside its declared window")
        actions[action_name] = {
            "request": request,
            "response": response,
            "client_to_control": c2c,
            "control_to_client": c2l,
        }

    result: dict[str, Any] = {
        "format": MANIFEST_FORMAT,
        "release_id": release_id,
        "source_revision": source_revision,
        "protocol": PROTOCOL_VERSION,
        "artifacts": artifacts,
        "actions": actions,
    }
    client_view = _require_mapping(root["client_view"], "client_view")
    expected_view = {"release_id", "source_revision", "protocol", "actions"}
    if set(client_view) != expected_view:
        raise _error("manifest_invalid", "client_view has an unexpected shape")
    if client_view["release_id"] != release_id or client_view["source_revision"] != source_revision or client_view["protocol"] != PROTOCOL_VERSION:
        raise _error("manifest_invalid", "client_view identity does not match manifest")
    view_actions = _require_mapping(client_view["actions"], "client_view.actions")
    if dict(view_actions) != actions:
        raise _error("manifest_invalid", "client_view actions do not match manifest actions")
    result["client_view"] = {
        "release_id": release_id,
        "source_revision": source_revision,
        "protocol": PROTOCOL_VERSION,
        "actions": actions,
    }
    return result


def load_manifest(path: Path) -> dict[str, Any]:
    try:
        info = path.lstat()
        if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
            raise _error("manifest_unverified", "release manifest is not a regular file")
        with path.open("r", encoding="utf-8") as handle:
            value = json.load(handle)
    except FileNotFoundError as exc:
        raise _error("manifest_missing", "release manifest is missing") from exc
    except ManifestError:
        raise
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise _error("manifest_invalid", "release manifest is not valid UTF-8 JSON") from exc
    return validate_manifest(value)


def _resolve_regular(root: Path, relative_path: str) -> Path:
    candidate = root / Path(*PurePosixPath(relative_path).parts)
    cursor = root
    parts = PurePosixPath(relative_path).parts
    for index, part in enumerate(parts):
        cursor = cursor / part
        try:
            info = cursor.lstat()
        except FileNotFoundError as exc:
            raise _error("artifact_missing", f"artifact is missing: {relative_path}") from exc
        except OSError as exc:
            raise _error("artifact_unverified", f"artifact cannot be inspected: {relative_path}") from exc
        if stat.S_ISLNK(info.st_mode):
            raise _error("artifact_unverified", f"artifact path contains a symlink: {relative_path}")
        if index < len(parts) - 1 and not stat.S_ISDIR(info.st_mode):
            raise _error("artifact_unverified", f"artifact path contains a non-directory parent: {relative_path}")
    try:
        candidate.resolve().relative_to(root)
    except ValueError as exc:
        raise _error("artifact_path_invalid", "artifact escapes bundle root") from exc
    try:
        info = candidate.lstat()
    except FileNotFoundError as exc:
        raise _error("artifact_missing", f"artifact is missing: {relative_path}") from exc
    except OSError as exc:
        raise _error("artifact_unverified", f"artifact cannot be inspected: {relative_path}") from exc
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
        raise _error("artifact_unverified", f"artifact is not a regular file: {relative_path}")
    return candidate


def sha256_file(path: Path) -> str:
    digest = sha256()
    try:
        with path.open("rb") as handle:
            for chunk in iter(lambda: handle.read(1024 * 1024), b""):
                digest.update(chunk)
    except OSError as exc:
        raise _error("artifact_unverified", f"artifact cannot be read: {path.name}") from exc
    return digest.hexdigest()


def verify_bundle(root: Path, manifest: Mapping[str, Any] | None = None) -> dict[str, Any]:
    """Verify exact artifact bytes, regular-file type, and declared modes."""
    root = root.resolve()
    if not root.is_dir():
        raise _error("bundle_invalid", "bundle root is not a directory")
    checked = validate_manifest(manifest if manifest is not None else load_manifest(root / MANIFEST_NAME))
    for item in checked["artifacts"]:
        _verify_artifact_file(_resolve_regular(root, item["path"]), item)
    return checked


def _verify_artifact_file(path: Path, item: Mapping[str, Any]) -> None:
    try:
        info = path.lstat()
    except FileNotFoundError as exc:
        raise _error("artifact_missing", f"artifact is missing: {item['path']}") from exc
    except OSError as exc:
        raise _error("artifact_unverified", f"artifact cannot be inspected: {item['path']}") from exc
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
        raise _error("artifact_unverified", f"artifact is not a regular file: {item['path']}")
    actual_mode = stat.S_IMODE(info.st_mode)
    if actual_mode != item["mode"]:
        raise _error("artifact_mode_mismatch", f"artifact mode differs: {item['path']}", {"expected": item["mode"], "actual": actual_mode})
    actual_digest = sha256_file(path)
    if actual_digest != item["sha256"]:
        raise _error("artifact_checksum_mismatch", f"artifact checksum differs: {item['path']}", {"expected": item["sha256"], "actual": actual_digest})


def verify_runtime_bundle(manifest: Mapping[str, Any], artifact_paths: Mapping[str, Path]) -> dict[str, Any]:
    """Verify installed artifacts whose runtime paths differ from bundle paths."""
    checked = validate_manifest(manifest)
    for item in checked["artifacts"]:
        path = artifact_paths.get(item["path"])
        if path is None:
            raise _error("artifact_missing", f"runtime artifact path is not configured: {item['path']}")
        _verify_artifact_file(path, item)
    return checked


def verify_client_sidecar(root: Path, manifest: Mapping[str, Any] | None = None) -> dict[str, Any]:
    """Verify the derived client-only sidecar without making it an artifact."""
    root = root.resolve()
    checked = validate_manifest(manifest if manifest is not None else load_manifest(root / MANIFEST_NAME))
    sidecar_path = _resolve_regular(root, CLIENT_SIDECAR_NAME)
    try:
        with sidecar_path.open("r", encoding="utf-8") as handle:
            value = json.load(handle)
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise _error("client_sidecar_invalid", "client sidecar is not valid UTF-8 JSON") from exc
    if not isinstance(value, Mapping):
        raise _error("client_sidecar_invalid", "client sidecar root must be an object")
    expected_keys = {"format", "release_id", "source_revision", "protocol", "client_artifact", "client_view"}
    if set(value) != expected_keys:
        raise _error("client_sidecar_invalid", "client sidecar has an unexpected shape")
    if value["format"] != checked["format"] or value["release_id"] != checked["release_id"] or value["source_revision"] != checked["source_revision"] or value["protocol"] != checked["protocol"]:
        raise _error("client_sidecar_invalid", "client sidecar identity does not match manifest")
    client_artifacts = [item for item in checked["artifacts"] if item["role"] == "client"]
    if len(client_artifacts) != 1 or value["client_artifact"] != client_artifacts[0]:
        raise _error("client_sidecar_invalid", "client sidecar artifact does not match manifest")
    if value["client_view"] != checked["client_view"]:
        raise _error("client_sidecar_invalid", "client sidecar view does not match manifest")
    return checked


def compatibility_for(manifest: Mapping[str, Any], action: str) -> dict[str, Any]:
    checked = validate_manifest(manifest)
    if action not in checked["actions"]:
        raise _error("unknown_action", f"action is not declared in the release manifest: {action}")
    spec = checked["actions"][action]
    return {
        "protocol": checked["protocol"],
        "action": action,
        "request": dict(spec["request"]),
        "response": dict(spec["response"]),
        "client_to_control": dict(spec["client_to_control"]),
        "control_to_client": dict(spec["control_to_client"]),
    }


def legacy_capability(action: str) -> dict[str, Any]:
    if action not in ACTION_NAMES:
        raise _error("unknown_action", f"unsupported legacy action: {action}")
    spec = operation_spec()
    return {
        "release_id": LEGACY_RELEASE_ID,
        "protocol": PROTOCOL_VERSION,
        "action": action,
        "request": dict(spec["request"]),
        "accepted_response": {
            "schema_min": 1,
            "schema_max": 1,
            "semantic_min": 1,
            "semantic_max": 1,
        },
        "legacy": True,
    }


def warning(code: str, action: str | None, message: str, detail: Any = None) -> dict[str, Any]:
    value: dict[str, Any] = {"code": code, "message": message}
    if action is not None:
        value["action"] = action
    if detail is not None:
        value["detail"] = detail
    return value


def check_compatibility(
    manifest: Mapping[str, Any],
    action: str,
    peer: Mapping[str, Any] | None,
) -> list[dict[str, Any]]:
    """Validate a peer capability before any journal or native side effect."""
    checked = validate_manifest(manifest)
    local = compatibility_for(checked, action)
    if peer is None:
        return [warning("legacy_compatibility", action, "peer omitted release capability; using legacy protocol-4 compatibility")]
    peer_map = _require_mapping(peer, "compatibility")
    protocol = _require_int(peer_map.get("protocol"), "compatibility.protocol", 1)
    if protocol != checked["protocol"]:
        raise _error("protocol_mismatch", "peer protocol is not supported", {"expected": checked["protocol"], "actual": protocol})
    if peer_map.get("action") != action:
        raise _error("compatibility_action_mismatch", "peer capability action does not match request action")
    request = _schema(peer_map.get("request"), "compatibility.request")
    accepted_response = _window(peer_map.get("accepted_response"), "compatibility.accepted_response")
    local_request_window = local["client_to_control"]
    local_response = local["response"]
    if not local_request_window["schema_min"] <= request["schema"] <= local_request_window["schema_max"]:
        raise _error("request_schema_incompatible", "request schema is outside the control compatibility window", {"action": action})
    if not local_request_window["semantic_min"] <= request["semantic_revision"] <= local_request_window["semantic_max"]:
        raise _error("request_semantic_incompatible", "request semantic revision is outside the control compatibility window", {"action": action})
    if not accepted_response["schema_min"] <= local_response["schema"] <= accepted_response["schema_max"]:
        raise _error("response_schema_incompatible", "control response schema is not accepted by the client", {"action": action})
    if not accepted_response["semantic_min"] <= local_response["semantic_revision"] <= accepted_response["semantic_max"]:
        raise _error("response_semantic_incompatible", "control response semantic revision is not accepted by the client", {"action": action})
    if "release_id" not in peer_map:
        raise _error("compatibility_invalid", "compatibility.release_id is required for a present capability")
    peer_release = peer_map["release_id"]
    if not isinstance(peer_release, str) or not peer_release:
        raise _error("compatibility_invalid", "compatibility.release_id must be a string")
    if peer_release != LEGACY_RELEASE_ID and RELEASE_ID_RE.fullmatch(peer_release) is None:
        raise _error("compatibility_invalid", "compatibility.release_id must be a Git revision or legacy-protocol-4")
    if peer_release == checked["release_id"]:
        return []
    return [warning("compatible_release", action, "peer release differs but the requested operation is explicitly compatible", {"peer_release": peer_release, "control_release": checked["release_id"]})]


def manifest_metadata(manifest: Mapping[str, Any] | None, *, present: bool, verified: bool, legacy: bool = False) -> dict[str, Any]:
    if legacy or manifest is None:
        return {
            "release_id": LEGACY_RELEASE_ID,
            "source_revision": None,
            "protocol": PROTOCOL_VERSION,
            "manifest_present": present,
            "artifacts_verified": verified,
        }
    checked = validate_manifest(manifest)
    return {
        "release_id": checked["release_id"],
        "source_revision": checked["source_revision"],
        "protocol": checked["protocol"],
        "manifest_present": present,
        "artifacts_verified": verified,
    }


def load_runtime_manifest(path: Path) -> tuple[dict[str, Any] | None, bool, bool, list[dict[str, Any]]]:
    """Load a local manifest, distinguishing absent legacy installs from bad installs."""
    try:
        info = path.lstat()
    except FileNotFoundError:
        return None, False, True, [warning("legacy_compatibility", None, "release manifest is absent; using legacy protocol-4 compatibility")]
    except OSError as exc:
        raise _error("manifest_unverified", "release manifest cannot be inspected") from exc
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
        raise _error("manifest_unverified", "release manifest is not a regular file")
    manifest = load_manifest(path)
    return manifest, True, True, []
