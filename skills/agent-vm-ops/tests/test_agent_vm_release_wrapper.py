"""Permission and failure-state tests for the fixed release wrapper."""
from __future__ import annotations

import importlib.util
import io
import json
import os
from pathlib import Path
import sqlite3
import stat
import subprocess
import tarfile
import tempfile
import unittest
from unittest import mock


ROOT = Path(__file__).resolve().parents[3]
SCRIPTS = ROOT / "skills/agent-vm-ops/scripts"
if str(SCRIPTS) not in os.sys.path:
    os.sys.path.insert(0, str(SCRIPTS))
import agent_vm_release as RELEASE  # noqa: E402
import release_manifest as MANIFEST  # noqa: E402
import agent_vm_release_wrapper as WRAPPER  # noqa: E402


class WrapperTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.bundle = self.root / "bundle.tar"
        RELEASE.build_release(ROOT, self.bundle, "b" * 40)
        uid = os.getuid()
        self.config = {
            "release_root": str(self.root / "releases"),
            "staging_root": str(self.root / "staging"),
            "current_link": str(self.root / "current"),
            "previous_link": str(self.root / "previous"),
            "control_path": str(self.root / "installed" / "agent_vm_control.ts"),
            "unit_dir": str(self.root / "units"),
            "state_root": str(self.root / "wrapper-state"),
            "owner_uid": uid,
            "require_drained": True,
        }
        self.config_path = self.root / "wrapper.json"
        self.config_path.write_text(json.dumps(self.config), encoding="utf-8")
        self.loaded = {key: Path(value) for key, value in self.config.items() if key in {
            "release_root", "staging_root", "current_link", "previous_link", "control_path", "unit_dir", "state_root",
        }}
        self.loaded.update({
            "owner_uid": uid,
            "require_drained": True,
            "unit_names": tuple(WRAPPER.FIXED_UNIT_NAMES),
            "fence_path": self.root / "release-activation.fence",
        })
        self.payload = self.bundle.read_bytes()
        self.drain_patch = mock.patch.object(WRAPPER, "_ensure_drained")
        self.drain_patch.start()
        self.addCleanup(self.drain_patch.stop)
        syntax_patch = mock.patch.object(WRAPPER, "verify_typescript_syntax")
        self.typescript_check = syntax_patch.start()
        self.addCleanup(syntax_patch.stop)

    def test_drain_verification_uses_fixed_bun_controller(self):
        self.drain_patch.stop()
        response = json.dumps({"ok": True, "result": {"draining": True}}).encode()
        with mock.patch.object(
            WRAPPER.subprocess, "run",
            return_value=subprocess.CompletedProcess([], 0, response, b""),
        ) as run:
            WRAPPER._ensure_drained(self.loaded)
        self.assertEqual(
            run.call_args.args[0],
            ["/usr/local/bin/bun", str(self.loaded["control_path"])],
        )

    def test_drain_verification_accepts_installed_format1_layout(self):
        self.drain_patch.stop()
        extracted = self.root / "legacy-installed"
        WRAPPER.safe_extract_archive(self.payload, extracted)
        manifest = MANIFEST.load_release_manifest(extracted / MANIFEST.MANIFEST_NAME)
        WRAPPER._install_release_files(self.loaded, extracted, manifest)
        response = json.dumps({"ok": True, "result": {"draining": True}}).encode()
        with mock.patch.object(
            WRAPPER.subprocess, "run",
            return_value=subprocess.CompletedProcess([], 0, response, b""),
        ) as run:
            WRAPPER._ensure_drained(self.loaded)
        self.assertEqual(
            run.call_args.args[0],
            ["/usr/local/bin/bun", str(self.loaded["control_path"])],
        )

    def test_drain_verification_rejects_source_product_layout(self):
        self.drain_patch.stop()
        installed_root = Path(self.loaded["control_path"]).parent
        installed_root.mkdir(parents=True, exist_ok=True)
        revision = "d" * 40
        artifacts = [
            {"name": name, "path": path, "role": role, "sha256": "0" * 64, "mode": mode}
            for name, path, role, mode in sorted(MANIFEST.PRODUCT_CLIENT_ARTIFACTS, key=lambda item: item[1])
        ]
        actions = MANIFEST.default_operations()
        manifest = {
            "format": MANIFEST.LEGACY_PRODUCT_MANIFEST_FORMAT,
            "version": "1.0.0",
            "release_id": revision,
            "source_revision": revision,
            "protocol": 4,
            "target": {"os": "source", "arch": "any"},
            "toolchain": {"bun": "1.4.4", "go": "go1.27.3", "cgo_enabled": False},
            "artifacts": artifacts,
            "actions": actions,
            "client_view": {"version": "1.0.0", "source_revision": revision, "protocol": 4, "transport": "legacy-bun", "actions": actions},
        }
        (installed_root / MANIFEST.MANIFEST_NAME).write_text(json.dumps(manifest), encoding="utf-8")
        client_artifact = next(item for item in artifacts if item["role"] == "client")
        (installed_root / "client-release.json").write_text(json.dumps({
            "format": manifest["format"],
            "version": manifest["version"],
            "source_revision": revision,
            "protocol": 4,
            "client_artifact": client_artifact,
            "client_dependencies": [item for item in artifacts if item["role"] == "support"],
            "client_view": manifest["client_view"],
        }), encoding="utf-8")
        with mock.patch.object(WRAPPER.subprocess, "run") as run:
            with self.assertRaises(WRAPPER.WrapperError) as failure:
                WRAPPER._ensure_drained(self.loaded)
        self.assertEqual(failure.exception.code, "drain_unverified")
        run.assert_not_called()

    def test_drain_verification_uses_fixed_go_request_after_cutover(self):
        self.drain_patch.stop()
        self._install_go_runtime("go-request")
        response = json.dumps({"ok": True, "result": {"draining": True}}).encode()
        with mock.patch.object(
            WRAPPER.subprocess, "run",
            return_value=subprocess.CompletedProcess([], 0, response, b""),
        ) as run:
            WRAPPER._ensure_drained(self.loaded)
        self.assertEqual(
            run.call_args.args[0],
            [str(WRAPPER.GO_CONTROL_PATH), "request", "--socket", str(WRAPPER.GO_CONTROL_SOCKET), "--timeout", "30s"],
        )
        self.assertEqual(
            json.loads(run.call_args_list[0].kwargs["input"]),
            {"protocol": 4, "action": "drain-status"},
        )
        self.assertEqual(
            json.loads(run.call_args_list[1].kwargs["input"]),
            {"protocol": 4, "action": "admin-metrics"},
        )
        self.assertEqual(run.call_args.kwargs["timeout"], 30)

    def test_drain_verification_uses_fixed_go_request_for_https_metadata(self):
        self.drain_patch.stop()
        self._install_go_runtime("https")
        response = json.dumps({"ok": True, "result": {"draining": True}}).encode()
        with mock.patch.object(
            WRAPPER.subprocess, "run",
            return_value=subprocess.CompletedProcess([], 0, response, b""),
        ) as run:
            WRAPPER._ensure_drained(self.loaded)
        self.assertEqual(
            run.call_args.args[0],
            [str(WRAPPER.GO_CONTROL_PATH), "request", "--socket", str(WRAPPER.GO_CONTROL_SOCKET), "--timeout", "30s"],
        )

    def test_drain_verification_accepts_installed_go_runtime_layout(self):
        self.drain_patch.stop()
        self._install_go_runtime("https")
        response = json.dumps({"ok": True, "result": {"draining": True}}).encode()
        with mock.patch.object(
            WRAPPER.subprocess, "run",
            return_value=subprocess.CompletedProcess([], 0, response, b""),
        ) as run:
            WRAPPER._ensure_drained(self.loaded)
        self.assertEqual(
            run.call_args.args[0],
            [str(WRAPPER.GO_CONTROL_PATH), "request", "--socket", str(WRAPPER.GO_CONTROL_SOCKET), "--timeout", "30s"],
        )

    def test_drain_verification_rejects_tampered_active_native_unit(self):
        self.drain_patch.stop()
        self._install_go_runtime("https")
        unit = Path(self.loaded["unit_dir"]) / "agent-vm-native.service"
        unit.write_bytes(unit.read_bytes() + b"\n# tampered\n")
        with mock.patch.object(WRAPPER.subprocess, "run") as run:
            with self.assertRaises(WRAPPER.WrapperError) as failure:
                WRAPPER._ensure_drained(self.loaded)
        self.assertEqual(failure.exception.code, "drain_unverified")
        run.assert_not_called()

    def test_drain_verification_rejects_malformed_present_sidecar(self):
        self.drain_patch.stop()
        self._install_go_payload("https")
        sidecar = Path(self.loaded["control_path"]).with_name("client-release.json")
        sidecar.write_text(json.dumps({"format": 2, "client_view": {"transport": "https"}}), encoding="utf-8")
        with mock.patch.object(WRAPPER.subprocess, "run") as run:
            with self.assertRaises(WRAPPER.WrapperError) as failure:
                WRAPPER._ensure_drained(self.loaded)
        self.assertEqual(failure.exception.code, "drain_unverified")
        run.assert_not_called()

    def test_drain_verification_rejects_tampered_installed_control_artifact(self):
        self.drain_patch.stop()
        self._install_go_payload("https")
        binary = Path(self.loaded["control_path"]).parent / "bin" / "agent-vm-control"
        binary.write_bytes(binary.read_bytes() + b"tampered")
        with mock.patch.object(WRAPPER.subprocess, "run") as run:
            with self.assertRaises(WRAPPER.WrapperError) as failure:
                WRAPPER._ensure_drained(self.loaded)
        self.assertEqual(failure.exception.code, "drain_unverified")
        run.assert_not_called()

    def test_drain_verification_rejects_manifest_without_sidecar(self):
        self.drain_patch.stop()
        self._install_go_payload("https")
        sidecar = Path(self.loaded["control_path"]).with_name("client-release.json")
        sidecar.unlink()
        with mock.patch.object(WRAPPER.subprocess, "run") as run:
            with self.assertRaises(WRAPPER.WrapperError) as failure:
                WRAPPER._ensure_drained(self.loaded)
        self.assertEqual(failure.exception.code, "drain_unverified")
        run.assert_not_called()

    def test_drain_verification_rejects_inflight_management_operations(self):
        self.drain_patch.stop()
        responses = [
            json.dumps({"ok": True, "result": {"draining": True}}).encode(),
            json.dumps({"ok": True, "result": {"pending_operations": [{"operation_id": "running"}]}}).encode(),
        ]
        with mock.patch.object(
            WRAPPER.subprocess,
            "run",
            side_effect=[subprocess.CompletedProcess([], 0, response, b"") for response in responses],
        ):
            with self.assertRaises(WRAPPER.WrapperError) as failure:
                WRAPPER._ensure_drained(self.loaded)
        self.assertEqual(failure.exception.code, "drain_unverified")

    def test_drain_verification_checks_target_state_compatibility(self):
        self.drain_patch.stop()
        response = json.dumps({
            "ok": True,
            "result": {
                "draining": True,
                "state_capabilities": list(MANIFEST.STATE_CAPABILITY_MARKERS),
            },
        }).encode()
        target = {
            "format": MANIFEST.PRODUCT_MANIFEST_FORMAT,
            "state_compatibility": {
                "reads": list(MANIFEST.STATE_CAPABILITY_MARKERS),
                "writes": list(MANIFEST.STATE_CAPABILITY_MARKERS),
            },
        }
        with mock.patch.object(
            WRAPPER.subprocess, "run",
            return_value=subprocess.CompletedProcess([], 0, response, b""),
        ):
            WRAPPER._ensure_drained(self.loaded, target)

        incompatible = {
            **target,
            "state_compatibility": {
                "reads": [MANIFEST.STATE_CAPABILITY_MARKERS[0]],
                "writes": [MANIFEST.STATE_CAPABILITY_MARKERS[0]],
            },
        }
        with mock.patch.object(
            WRAPPER.subprocess, "run",
            return_value=subprocess.CompletedProcess([], 0, response, b""),
        ):
            with self.assertRaises(WRAPPER.WrapperError) as failure:
                WRAPPER._ensure_drained(self.loaded, incompatible)
        self.assertEqual(failure.exception.code, "state_compatibility_mismatch")

    def test_drain_verification_reads_fixed_state_db_for_old_controller(self):
        self.drain_patch.stop()
        response = json.dumps({"ok": True, "result": {"draining": True}}).encode()
        target = {
            "format": MANIFEST.PRODUCT_MANIFEST_FORMAT,
            "state_compatibility": {
                "reads": list(MANIFEST.STATE_CAPABILITY_MARKERS),
                "writes": list(MANIFEST.STATE_CAPABILITY_MARKERS),
            },
        }
        with mock.patch.object(
            WRAPPER.subprocess, "run",
            return_value=subprocess.CompletedProcess([], 0, response, b""),
        ), mock.patch.object(
            WRAPPER,
            "_read_live_state_capabilities",
            return_value=list(MANIFEST.STATE_CAPABILITY_MARKERS),
        ) as read_capabilities:
            WRAPPER._ensure_drained(self.loaded, target)
        read_capabilities.assert_called_once_with()

    def test_legacy_product_rollback_rejects_management_state(self):
        self.drain_patch.stop()
        response = json.dumps({
            "ok": True,
            "result": {
                "draining": True,
                "state_capabilities": list(MANIFEST.STATE_CAPABILITY_MARKERS),
            },
        }).encode()
        target = {"format": MANIFEST.LEGACY_PRODUCT_MANIFEST_FORMAT}
        with mock.patch.object(
            WRAPPER.subprocess, "run",
            return_value=subprocess.CompletedProcess([], 0, response, b""),
        ):
            with self.assertRaises(WRAPPER.WrapperError) as failure:
                WRAPPER._ensure_drained(self.loaded, target)
        self.assertEqual(failure.exception.code, "state_compatibility_mismatch")

    def test_missing_capability_table_requires_a_valid_protocol4_schema(self):
        database_path = self.root / "legacy.sqlite3"
        with sqlite3.connect(database_path) as database:
            database.executescript("""
                CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
                CREATE TABLE tasks (task_id TEXT, session_id TEXT, generation INTEGER, state TEXT, instance_name TEXT,
                    vm_ip TEXT, ssh_host_port INTEGER, lease_expires_at TEXT, inactive_since TEXT, cleanup_after TEXT,
                    created_at TEXT, updated_at TEXT, last_error TEXT, image_fingerprint TEXT, resources_json TEXT,
                    reservation_bytes INTEGER, previous_generation INTEGER, resource_reservation_json TEXT);
                CREATE TABLE ports (task_id TEXT, generation INTEGER, port_name TEXT, protocol TEXT, host_port INTEGER,
                    vm_port INTEGER, device_name TEXT, state TEXT, created_at TEXT);
                CREATE TABLE operations (operation_id TEXT, action TEXT, session_id TEXT, generation INTEGER,
                    response_json TEXT, created_at TEXT);
                CREATE TABLE audit (id INTEGER, action TEXT, task_id TEXT, session_id TEXT, generation INTEGER,
                    outcome TEXT, detail_json TEXT, created_at TEXT);
            """)
        with mock.patch.object(WRAPPER, "STATE_DB_PATH", database_path):
            self.assertEqual(WRAPPER._read_live_state_capabilities(), [MANIFEST.STATE_CAPABILITY_MARKERS[0]])

    def test_missing_capability_table_rejects_an_empty_database(self):
        database_path = self.root / "empty.sqlite3"
        with sqlite3.connect(database_path):
            pass
        with mock.patch.object(WRAPPER, "STATE_DB_PATH", database_path):
            with self.assertRaises(WRAPPER.WrapperError) as failure:
                WRAPPER._read_live_state_capabilities()
        self.assertEqual(failure.exception.code, "state_compatibility_unverified")

    def test_legacy_product_runtime_stops_the_web_unit(self):
        with mock.patch.object(
            WRAPPER.subprocess, "run",
            return_value=subprocess.CompletedProcess([], 0, b"", b""),
        ) as run:
            WRAPPER._activate_product_runtime({"format": MANIFEST.LEGACY_PRODUCT_MANIFEST_FORMAT})
        commands = [call.args[0] for call in run.call_args_list]
        self.assertIn(["systemctl", "stop", "agent-vm-web.service"], commands)
        self.assertIn(["systemctl", "disable", "agent-vm-web.service"], commands)

    def test_go_runtime_stops_the_web_unit_before_old_control_plane(self):
        with mock.patch.object(
            WRAPPER.subprocess, "run",
            return_value=subprocess.CompletedProcess([], 0, b"", b""),
        ) as run:
            WRAPPER._activate_go_runtime()
        commands = [call.args[0] for call in run.call_args_list]
        self.assertIn(["systemctl", "stop", "agent-vm-web.service"], commands)
        self.assertIn(["systemctl", "disable", "agent-vm-web.service"], commands)

    def test_product_runtime_restores_an_enabled_web_unit_after_native_restart(self):
        with mock.patch.object(
            WRAPPER.subprocess, "run",
            return_value=subprocess.CompletedProcess([], 0, b"", b""),
        ) as run:
            WRAPPER._activate_product_runtime({"format": MANIFEST.PRODUCT_MANIFEST_FORMAT})
        commands = [call.args[0] for call in run.call_args_list]
        self.assertIn(["systemctl", "is-enabled", "--quiet", "agent-vm-web.service"], commands)
        native_restart = commands.index(["systemctl", "restart", "agent-vm-native.service"])
        web_start = commands.index(["systemctl", "start", "agent-vm-web.service"])
        self.assertLess(native_restart, web_start)

    def test_product_runtime_does_not_start_a_disabled_web_unit(self):
        def run(command, **kwargs):
            if command == ["systemctl", "is-enabled", "--quiet", "agent-vm-web.service"]:
                return subprocess.CompletedProcess(command, 1, b"", b"")
            return subprocess.CompletedProcess(command, 0, b"", b"")

        with mock.patch.object(WRAPPER.subprocess, "run", side_effect=run) as process:
            WRAPPER._activate_product_runtime({"format": MANIFEST.PRODUCT_MANIFEST_FORMAT})
        commands = [call.args[0] for call in process.call_args_list]
        self.assertNotIn(["systemctl", "start", "agent-vm-web.service"], commands)

    def test_product_runtime_uses_the_activation_snapshot_after_web_was_disabled(self):
        def run(command, **kwargs):
            if command == ["systemctl", "is-enabled", "--quiet", "agent-vm-web.service"]:
                return subprocess.CompletedProcess(command, 1, b"", b"")
            return subprocess.CompletedProcess(command, 0, b"", b"")

        with mock.patch.object(WRAPPER.subprocess, "run", side_effect=run) as process:
            WRAPPER._activate_product_runtime(
                {"format": MANIFEST.PRODUCT_MANIFEST_FORMAT},
                web_was_enabled=True,
            )
        commands = [call.args[0] for call in process.call_args_list]
        self.assertNotIn(["systemctl", "is-enabled", "--quiet", "agent-vm-web.service"], commands)
        self.assertIn(["systemctl", "start", "agent-vm-web.service"], commands)

    def test_product_bundle_targets_use_the_control_bundle_root(self):
        targets = WRAPPER._activation_targets(self.loaded)
        bundle_root = Path(self.loaded["control_path"]).parent
        self.assertIn(bundle_root / "config" / "agent-vm-web.json.example", targets)
        self.assertIn(bundle_root / "systemd" / "agent-vm-web.service", targets)
        self.assertNotIn(bundle_root / "bin" / "config" / "agent-vm-web.json.example", targets)

    def test_missing_legacy_enable_and_disable_are_allowed_but_other_failures_are_not(self):
        missing = subprocess.CompletedProcess(
            [], 1, b"", b"Failed to disable unit: Unit agent-vm-native-age-gc.service does not exist\n",
        )
        with mock.patch.object(WRAPPER.subprocess, "run", return_value=missing):
            WRAPPER._systemctl("disable", "agent-vm-native-age-gc.service", allow_missing=True)
            WRAPPER._systemctl("enable", "agent-vm-native-age-gc.service", allow_missing=True)

        missing_template = subprocess.CompletedProcess(
            [], 1, b"", b"Failed to stop agent-vm-native-expire@.service: Unit name is missing the instance name\n",
        )
        with mock.patch.object(WRAPPER.subprocess, "run", return_value=missing_template):
            WRAPPER._systemctl("stop", "agent-vm-native-expire@.service", allow_missing=True)

        failed = subprocess.CompletedProcess([], 1, b"", b"Failed to disable unit: permission denied\n")
        with mock.patch.object(WRAPPER.subprocess, "run", return_value=failed):
            with self.assertRaises(WRAPPER.WrapperError) as failure:
                WRAPPER._systemctl("disable", "agent-vm-native-age-gc.service", allow_missing=True)
        self.assertEqual(failure.exception.code, "activation_failed")

    def test_python_syntax_check_only_compiles_python_files(self):
        python = self.root / "candidate.py"
        typescript = self.root / "candidate.ts"
        python.write_text("raise RuntimeError('must not execute')\n", encoding="utf-8")
        typescript.write_text("export const value: number = 1;\n", encoding="utf-8")
        manifest = {"artifacts": [
            {"role": "support", "path": python.name},
            {"role": "control", "path": typescript.name},
        ]}
        WRAPPER._verify_python_syntax(self.root, manifest)
        python.write_text("def broken(:\n", encoding="utf-8")
        with self.assertRaises(WRAPPER.WrapperError) as failure:
            WRAPPER._verify_python_syntax(self.root, manifest)
        self.assertEqual(failure.exception.code, "python_syntax_invalid")

    def test_transaction_and_retained_release_reject_typescript_syntax_failure(self):
        WRAPPER.stage(self.loaded, "tx-invalid-ts", "b" * 40, self.payload)
        self.typescript_check.side_effect = WRAPPER.ManifestError(
            "typescript_syntax_invalid", "injected syntax failure",
        )
        with self.assertRaises(WRAPPER.WrapperError) as failure:
            WRAPPER.verify_transaction(self.loaded, "tx-invalid-ts", "b" * 40)
        self.assertEqual(failure.exception.code, "typescript_syntax_invalid")
        staged = Path(self.loaded["staging_root"]) / "tx-invalid-ts"
        self.assertFalse((staged / ".verified.json").exists())
        with self.assertRaises(WRAPPER.WrapperError) as retained_failure:
            WRAPPER._verify_retained_release(self.loaded, staged)
        self.assertEqual(retained_failure.exception.code, "typescript_syntax_invalid")
        self.assertEqual(self.typescript_check.call_count, 2)
        self.assertFalse(Path(self.loaded["control_path"]).exists())

    def test_support_targets_reject_every_non_allowlisted_path_before_copying(self):
        paths = (
            "other.ts",
            "agent_vm_control.ts",
            "nested/control-types.ts",
            "./control-types.ts",
            "../control-types.ts",
            "/control-types.ts",
            "nested/release_manifest.py",
            "control-types.ts/extra",
            "nested\\control-types.ts",
        )
        for path in paths:
            with self.subTest(path=path):
                manifest = {"artifacts": [
                    {"role": "support", "path": name, "mode": 0o644}
                    for name in sorted(WRAPPER.SUPPORT_ARTIFACT_NAMES)
                ]}
                manifest["artifacts"].append({"role": "support", "path": path, "mode": 0o644})
                with mock.patch.object(WRAPPER, "_copy_atomic") as copy:
                    with self.assertRaises(WRAPPER.WrapperError) as failure:
                        WRAPPER._install_release_files(self.loaded, self.root, manifest)
                self.assertEqual(failure.exception.code, "activation_unverified")
                copy.assert_not_called()

    def test_unsafe_archive_members_are_rejected(self):
        output = io.BytesIO()
        with tarfile.open(fileobj=output, mode="w") as archive:
            member = tarfile.TarInfo("../escape")
            member.size = 1
            archive.addfile(member, io.BytesIO(b"x"))
        with self.assertRaises(WRAPPER.WrapperError) as failure:
            WRAPPER.safe_extract_archive(output.getvalue(), self.root / "unsafe")
        self.assertEqual(failure.exception.code, "unsafe_archive")

    def test_manifest_failure_removes_temporary_staging(self):
        output = io.BytesIO()
        with tarfile.open(fileobj=output, mode="w") as archive:
            member = tarfile.TarInfo("agent_vm.ts")
            member.mode = 0o755
            member.size = 7
            archive.addfile(member, io.BytesIO(b"client\n"))
        with self.assertRaises(WRAPPER.WrapperError) as failure:
            WRAPPER.stage(self.loaded, "tx-manifest", "b" * 40, output.getvalue())
        self.assertEqual(failure.exception.code, "manifest_missing")
        self.assertFalse((Path(self.loaded["staging_root"]) / ".tx-manifest.staging").exists())

    def test_wrapper_config_and_cli_cannot_disable_or_redirect_activation(self):
        invalid = dict(self.config)
        invalid["require_drained"] = False
        invalid_path = self.root / "invalid-wrapper.json"
        invalid_path.write_text(json.dumps(invalid), encoding="utf-8")
        with self.assertRaises(WRAPPER.WrapperError) as failure:
            WRAPPER.load_config(invalid_path)
        self.assertEqual(failure.exception.code, "wrapper_config_invalid")
        with self.assertRaises(SystemExit):
            WRAPPER.build_parser().parse_args(["--config", str(invalid_path), "status"])
        redirected = dict(self.config)
        redirected_path = self.root / "redirected-wrapper.json"
        redirected_path.write_text(json.dumps(redirected), encoding="utf-8")
        with self.assertRaises(WRAPPER.WrapperError) as redirected_failure:
            WRAPPER.load_config(redirected_path)
        self.assertEqual(redirected_failure.exception.code, "wrapper_config_invalid")

    def test_wrapper_rejects_overlapping_protected_paths(self):
        invalid = dict(self.config)
        invalid["staging_root"] = invalid["release_root"]
        invalid_path = self.root / "overlap-wrapper.json"
        invalid_path.write_text(json.dumps(invalid), encoding="utf-8")
        with self.assertRaises(WRAPPER.WrapperError) as failure:
            WRAPPER.load_config(invalid_path)
        self.assertEqual(failure.exception.code, "wrapper_config_invalid")

    def _tampered_unit_payload(self):
        extracted = self.root / "tampered"
        WRAPPER.safe_extract_archive(self.payload, extracted)
        unit = extracted / "systemd" / "agent-vm-native-reconcile.service"
        unit.write_text(unit.read_text(encoding="utf-8") + "\nExecStartPre=/bin/echo injected\n", encoding="utf-8")
        manifest_path = extracted / "release-manifest.json"
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        for item in manifest["artifacts"]:
            if item["path"] == "systemd/agent-vm-native-reconcile.service":
                item["sha256"] = RELEASE.sha256_file(unit)
        manifest_path.write_text(json.dumps(manifest, sort_keys=True, indent=2) + "\n", encoding="utf-8")
        output = io.BytesIO()
        with tarfile.open(fileobj=output, mode="w") as archive:
            for path in sorted(extracted.rglob("*")):
                if path.is_file():
                    archive.add(path, arcname=path.relative_to(extracted).as_posix(), recursive=False)
        return output.getvalue()

    def _install_go_payload(self, transport):
        payload = self._go_payload("c" * 40, transport=transport)
        WRAPPER.safe_extract_archive(payload, Path(self.loaded["control_path"]).parent)

    def _install_go_runtime(self, transport):
        extracted = self.root / "go-installed"
        WRAPPER.safe_extract_archive(self._go_payload("e" * 40, transport=transport), extracted)
        manifest = MANIFEST.load_release_manifest(extracted / MANIFEST.MANIFEST_NAME)
        WRAPPER._install_go_release_files(self.loaded, extracted, manifest)

    def _go_payload(self, release_id, *, transport="go-request"):
        extracted = self.root / "go-candidate"
        client_root = ROOT / "skills/agent-vm/scripts"
        ops_root = ROOT / "skills/agent-vm-ops"
        artifacts = []
        for name, relative, role, mode in sorted(MANIFEST.GO_FIXED_ARTIFACTS):
            path = extracted / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            if relative == "bin/agent-vm-control":
                # Minimal little-endian ELF64 header for the amd64 target.
                path.write_bytes(b"\x7fELF\x02\x01\x01\x00" + b"\x00" * 10 + b"\x3e\x00" + b"\x00" * 42)
            elif relative == "systemd/agent-vm-native.service":
                path.write_bytes((ops_root / "assets/systemd/agent-vm-native.service").read_bytes())
            elif relative == "agent_vm_control_bridge.ts":
                path.write_bytes((ops_root / "scripts/agent_vm_control_bridge.ts").read_bytes())
            else:
                path.write_bytes((client_root / relative).read_bytes())
            path.chmod(mode)
            artifacts.append({
                "name": name,
                "path": relative,
                "role": role,
                "sha256": MANIFEST.sha256_file(path),
                "mode": mode,
            })
        manifest = MANIFEST.build_go_manifest(release_id, artifacts, transport=transport)
        (extracted / "release-manifest.json").write_text(json.dumps(manifest, sort_keys=True) + "\n", encoding="utf-8")
        client_artifact = next(item for item in artifacts if item["role"] == "client")
        sidecar = {
            "format": 2,
            "release_id": release_id,
            "source_revision": release_id,
            "protocol": 4,
            "client_artifact": client_artifact,
            "client_dependencies": [item for item in artifacts if item["path"] in MANIFEST.CLIENT_DEPENDENCY_MODES],
            "client_view": manifest["client_view"],
        }
        (extracted / "client-release.json").write_text(json.dumps(sidecar, sort_keys=True) + "\n", encoding="utf-8")
        output = io.BytesIO()
        with tarfile.open(fileobj=output, mode="w") as archive:
            for path in sorted(extracted.rglob("*")):
                if path.is_file():
                    archive.add(path, arcname=path.relative_to(extracted).as_posix(), recursive=False)
        return output.getvalue()

    def test_format_two_installs_go_binary_bridge_and_retires_old_units(self):
        release_id = "a" * 40
        payload = self._go_payload(release_id)
        unit_dir = Path(self.loaded["unit_dir"])
        unit_dir.mkdir(parents=True)
        for name in MANIFEST.GO_RETIRED_UNITS:
            old = unit_dir / name
            old.write_bytes(b"old unit\n")
            old.chmod(0o644)
        WRAPPER.stage(self.loaded, "tx-go", release_id, payload)
        verified = WRAPPER.verify_transaction(self.loaded, "tx-go", release_id)
        self.assertTrue(verified["verified"])
        with mock.patch.object(WRAPPER.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, b"", b"")):
            activated = WRAPPER.activate(self.loaded, "tx-go", release_id)
        self.assertTrue(activated["activated"])
        self.assertEqual(
            (Path(self.loaded["control_path"]).parent / "bin" / "agent-vm-control").read_bytes(),
            b"\x7fELF\x02\x01\x01\x00" + b"\x00" * 10 + b"\x3e\x00" + b"\x00" * 42,
        )
        self.assertEqual(Path(self.loaded["control_path"]).read_bytes(), (ROOT / "skills/agent-vm-ops/scripts/agent_vm_control_bridge.ts").read_bytes())
        self.assertEqual(
            (Path(self.loaded["control_path"]).parent / "agent_vm_control_bridge.ts").read_bytes(),
            (ROOT / "skills/agent-vm-ops/scripts/agent_vm_control_bridge.ts").read_bytes(),
        )
        self.assertEqual(
            (Path(self.loaded["control_path"]).parent / "systemd" / "agent-vm-native.service").read_bytes(),
            (ROOT / "skills/agent-vm-ops/assets/systemd/agent-vm-native.service").read_bytes(),
        )
        self.assertTrue((unit_dir / "agent-vm-native.service").is_file())
        for name in MANIFEST.GO_RETIRED_UNITS:
            self.assertFalse((unit_dir / name).exists())

    def test_native_install_removes_format_one_predecessor_files(self):
        legacy_root = self.root / "legacy-before-native"
        WRAPPER.safe_extract_archive(self.payload, legacy_root)
        legacy_manifest = MANIFEST.load_release_manifest(legacy_root / MANIFEST.MANIFEST_NAME)
        WRAPPER._install_release_files(self.loaded, legacy_root, legacy_manifest)

        self._install_go_runtime("https")
        installed_root = Path(self.loaded["control_path"]).parent
        for name in WRAPPER.LEGACY_ONLY_SUPPORT_NAMES:
            self.assertFalse((installed_root / name).exists(), name)
        for name in WRAPPER.FIXED_UNIT_NAMES:
            self.assertFalse((installed_root / "systemd" / name).exists(), name)
        for name in WRAPPER.GO_RETIRED_UNITS:
            self.assertFalse((Path(self.loaded["unit_dir"]) / name).exists(), name)
        installed_manifest = MANIFEST.load_release_manifest(
            installed_root / MANIFEST.MANIFEST_NAME,
        )
        self.assertEqual(
            WRAPPER._verify_installed_release(self.loaded, installed_root, installed_manifest)["format"],
            MANIFEST.GO_MANIFEST_FORMAT,
        )

    def test_native_to_format_one_activation_hands_off_runtime_before_cleanup(self):
        self.loaded["require_drained"] = False
        go_release_id = "a" * 40
        legacy_release_id = "b" * 40
        go_payload = self._go_payload(go_release_id, transport="https")
        with mock.patch.object(
            WRAPPER.subprocess,
            "run",
            return_value=subprocess.CompletedProcess([], 0, b"", b""),
        ) as run:
            WRAPPER.stage(self.loaded, "tx-native", go_release_id, go_payload)
            WRAPPER.verify_transaction(self.loaded, "tx-native", go_release_id)
            WRAPPER.activate(self.loaded, "tx-native", go_release_id)
            WRAPPER.stage(self.loaded, "tx-legacy", legacy_release_id, self.payload)
            WRAPPER.verify_transaction(self.loaded, "tx-legacy", legacy_release_id)
            WRAPPER.activate(self.loaded, "tx-legacy", legacy_release_id)
        commands = [call.args[0] for call in run.call_args_list]
        native_stop = commands.index(["systemctl", "stop", "agent-vm-native.service"])
        legacy_start = commands.index(["systemctl", "start", "agent-vm-native-reconcile.service"])
        self.assertLess(native_stop, legacy_start)
        self.assertEqual(Path(self.loaded["current_link"]).resolve().name, legacy_release_id)
        self.assertFalse((Path(self.loaded["unit_dir"]) / "agent-vm-native.service").exists())

    def test_cross_format_rollback_hands_off_runtime_before_cleanup(self):
        self.loaded["require_drained"] = False
        legacy_release_id = "b" * 40
        native_release_id = "a" * 40
        native_payload = self._go_payload(native_release_id, transport="https")
        with mock.patch.object(
            WRAPPER.subprocess,
            "run",
            return_value=subprocess.CompletedProcess([], 0, b"", b""),
        ):
            WRAPPER.stage(self.loaded, "tx-legacy", legacy_release_id, self.payload)
            WRAPPER.verify_transaction(self.loaded, "tx-legacy", legacy_release_id)
            WRAPPER.activate(self.loaded, "tx-legacy", legacy_release_id)
            WRAPPER.stage(self.loaded, "tx-native", native_release_id, native_payload)
            WRAPPER.verify_transaction(self.loaded, "tx-native", native_release_id)
            WRAPPER.activate(self.loaded, "tx-native", native_release_id)

        events = []
        original_install = WRAPPER._install_release_files

        def install(*args, **kwargs):
            events.append("install")
            return original_install(*args, **kwargs)

        def stop_native():
            events.append("deactivate-native")

        def daemon_reload():
            events.append("daemon-reload")

        def start_legacy(*, native_already_stopped=False):
            events.append(("activate-legacy", native_already_stopped))

        with mock.patch.object(WRAPPER, "_install_release_files", side_effect=install), mock.patch.object(
            WRAPPER, "_deactivate_native_runtime", side_effect=stop_native,
        ), mock.patch.object(WRAPPER, "_daemon_reload", side_effect=daemon_reload), mock.patch.object(
            WRAPPER, "_activate_legacy_runtime", side_effect=start_legacy,
        ):
            result = WRAPPER.rollback(self.loaded, "tx-native", native_release_id)

        self.assertTrue(result["rolled_back"])
        self.assertLess(events.index("deactivate-native"), events.index("install"))
        self.assertLess(events.index("install"), events.index("daemon-reload"))
        self.assertEqual(events[-1], ("activate-legacy", True))

    def test_native_rollback_runtime_failure_restores_previous_authority(self):
        self.loaded["require_drained"] = False
        first_release_id = "a" * 40
        second_release_id = "b" * 40
        first_payload = self._go_payload(first_release_id, transport="https")
        second_payload = self._go_payload(second_release_id, transport="https")
        with mock.patch.object(
            WRAPPER.subprocess,
            "run",
            return_value=subprocess.CompletedProcess([], 0, b"", b""),
        ):
            WRAPPER.stage(self.loaded, "tx-first-native", first_release_id, first_payload)
            WRAPPER.verify_transaction(self.loaded, "tx-first-native", first_release_id)
            WRAPPER.activate(self.loaded, "tx-first-native", first_release_id)
            WRAPPER.stage(self.loaded, "tx-second-native", second_release_id, second_payload)
            WRAPPER.verify_transaction(self.loaded, "tx-second-native", second_release_id)
            WRAPPER.activate(self.loaded, "tx-second-native", second_release_id)

            with mock.patch.object(
                WRAPPER,
                "_activate_go_runtime",
                side_effect=[WRAPPER.WrapperError("activation_failed", "injected"), None],
            ) as activate_go:
                with self.assertRaises(WRAPPER.WrapperError) as failure:
                    WRAPPER.rollback(self.loaded, "tx-second-native", second_release_id)

        self.assertEqual(failure.exception.code, "activation_failed")
        self.assertEqual(activate_go.call_count, 2)
        record = json.loads(
            (Path(self.loaded["state_root"]) / "tx-second-native.activation.json").read_text(),
        )
        self.assertEqual(record["rollback_state"], "failed")
        self.assertEqual(Path(self.loaded["current_link"]).resolve().name, second_release_id)

    def test_cross_format_rollback_to_native_hands_off_legacy_before_cleanup(self):
        self.loaded["require_drained"] = False
        native_release_id = "a" * 40
        legacy_release_id = "b" * 40
        native_payload = self._go_payload(native_release_id, transport="https")
        with mock.patch.object(
            WRAPPER.subprocess,
            "run",
            return_value=subprocess.CompletedProcess([], 0, b"", b""),
        ):
            WRAPPER.stage(self.loaded, "tx-native", native_release_id, native_payload)
            WRAPPER.verify_transaction(self.loaded, "tx-native", native_release_id)
            WRAPPER.activate(self.loaded, "tx-native", native_release_id)
            WRAPPER.stage(self.loaded, "tx-legacy", legacy_release_id, self.payload)
            WRAPPER.verify_transaction(self.loaded, "tx-legacy", legacy_release_id)
            WRAPPER.activate(self.loaded, "tx-legacy", legacy_release_id)

        events = []
        original_install = WRAPPER._install_release_files

        def install(*args, **kwargs):
            events.append("install")
            return original_install(*args, **kwargs)

        def stop_legacy():
            events.append("deactivate-legacy")

        def daemon_reload():
            events.append("daemon-reload")

        def start_native():
            events.append("activate-native")

        with mock.patch.object(WRAPPER, "_install_release_files", side_effect=install), mock.patch.object(
            WRAPPER, "_deactivate_legacy_runtime", side_effect=stop_legacy,
        ), mock.patch.object(WRAPPER, "_daemon_reload", side_effect=daemon_reload), mock.patch.object(
            WRAPPER, "_activate_go_runtime", side_effect=start_native,
        ):
            result = WRAPPER.rollback(self.loaded, "tx-legacy", legacy_release_id)

        self.assertTrue(result["rolled_back"])
        self.assertLess(events.index("deactivate-legacy"), events.index("install"))
        self.assertLess(events.index("install"), events.index("daemon-reload"))
        self.assertEqual(events[-1], "activate-native")

    def test_format_one_install_removes_native_predecessor_files(self):
        self._install_go_runtime("https")
        installed_root = Path(self.loaded["control_path"]).parent
        legacy_root = self.root / "legacy-after-native"
        WRAPPER.safe_extract_archive(self.payload, legacy_root)
        legacy_manifest = MANIFEST.load_release_manifest(legacy_root / MANIFEST.MANIFEST_NAME)
        WRAPPER._install_release_files(self.loaded, legacy_root, legacy_manifest)

        self.assertFalse((installed_root / "agent_vm_control_bridge.ts").exists())
        self.assertFalse((installed_root / "bin" / "agent-vm-control").exists())
        self.assertFalse((installed_root / "systemd" / "agent-vm-native.service").exists())
        self.assertFalse((installed_root / "systemd" / "agent-vm-web.service").exists())
        self.assertFalse((installed_root / "config" / "agent-vm-web.json.example").exists())
        self.assertFalse((installed_root / "web-assets.manifest.json").exists())
        self.assertFalse((Path(self.loaded["unit_dir"]) / "agent-vm-native.service").exists())
        self.assertFalse((Path(self.loaded["unit_dir"]) / "agent-vm-web.service").exists())
        self.drain_patch.stop()
        response = json.dumps({"ok": True, "result": {"draining": True}}).encode()
        with mock.patch.object(
            WRAPPER.subprocess,
            "run",
            return_value=subprocess.CompletedProcess([], 0, response, b""),
        ) as run:
            WRAPPER._ensure_drained(self.loaded)
        self.assertEqual(run.call_args.args[0], ["/usr/local/bin/bun", str(self.loaded["control_path"])])

    def test_unit_policy_bytes_are_pinned_even_when_manifest_checksum_matches(self):
        payload = self._tampered_unit_payload()
        WRAPPER.stage(self.loaded, "tx-tampered-unit", "b" * 40, payload)
        with self.assertRaises(WRAPPER.WrapperError) as failure:
            WRAPPER.verify_transaction(self.loaded, "tx-tampered-unit", "b" * 40)
        self.assertEqual(failure.exception.code, "unit_unverified")

    def test_stage_verify_and_activate_only_install_fixed_targets(self):
        staged = WRAPPER.stage(self.loaded, "tx-one", "b" * 40, self.payload)
        self.assertTrue(staged["staged"])
        verified = WRAPPER.verify_transaction(self.loaded, "tx-one", "b" * 40)
        self.assertTrue(verified["verified"])
        with mock.patch.object(WRAPPER.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, b"", b"")) as run:
            activated = WRAPPER.activate(self.loaded, "tx-one", "b" * 40)
        self.assertTrue(activated["activated"])
        self.assertEqual(Path(self.loaded["current_link"]).resolve().name, "b" * 40)
        self.assertTrue(Path(self.loaded["control_path"]).is_file())
        self.assertTrue(Path(self.loaded["control_path"]).with_name("agent_vm.ts").is_file())
        self.assertTrue(Path(self.loaded["control_path"]).with_name("release_manifest.py").is_file())
        self.assertTrue(Path(self.loaded["control_path"]).with_name("release-manifest.json").is_file())
        self.assertTrue(Path(self.loaded["control_path"]).with_name("client-release.json").is_file())
        for name in WRAPPER.SUPPORT_ARTIFACT_NAMES:
            with self.subTest(support=name):
                installed = Path(self.loaded["control_path"]).with_name(name)
                source_scripts = ROOT / "skills/agent-vm/scripts" if name in WRAPPER.CLIENT_PACKAGE_MODES else SCRIPTS
                self.assertEqual(installed.read_bytes(), (source_scripts / name).read_bytes())
                self.assertEqual(stat.S_IMODE(installed.stat().st_mode), WRAPPER.SUPPORT_ARTIFACT_MODES[name])
                self.assertEqual(installed.stat().st_uid, os.getuid())
                self.assertIn(installed, WRAPPER._activation_targets(self.loaded))
        commands = [call.args[0] for call in run.call_args_list]
        self.assertIn(["systemctl", "daemon-reload"], commands)
        self.assertIn(["systemctl", "start", "agent-vm-native-reconcile.service"], commands)
        self.assertIn(["systemctl", "enable", "agent-vm-native-age-gc.timer"], commands)

    def test_activation_retires_only_the_exact_replaced_python_entrypoints(self):
        installed = Path(self.loaded["control_path"]).parent
        installed.mkdir(parents=True)
        retired = installed / WRAPPER.RETIRED_CONTROL_NAME
        retired.write_bytes(b"old controller\n")
        retired.chmod(0o755)
        retired_client = installed / WRAPPER.RETIRED_CLIENT_NAME
        retired_client.write_bytes(b"old client\n")
        retired_client.chmod(0o755)
        unrelated = installed / "operator-owned.py"
        unrelated.write_bytes(b"untouched\n")
        WRAPPER.stage(self.loaded, "tx-retire", "b" * 40, self.payload)
        with mock.patch.object(
            WRAPPER.subprocess,
            "run",
            return_value=subprocess.CompletedProcess([], 0, b"", b""),
        ):
            with mock.patch.object(WRAPPER, "_daemon_reload"):
                WRAPPER.activate(self.loaded, "tx-retire", "b" * 40)
        self.assertFalse(retired.exists())
        self.assertFalse(retired_client.exists())
        self.assertEqual(unrelated.read_bytes(), b"untouched\n")
        self.assertTrue(Path(self.loaded["control_path"]).exists())

    def test_failed_activation_restores_retired_entrypoint_bytes_and_mode(self):
        installed = Path(self.loaded["control_path"]).parent
        installed.mkdir(parents=True)
        retired = installed / WRAPPER.RETIRED_CONTROL_NAME
        retired.write_bytes(b"old controller\n")
        retired.chmod(0o755)
        retired_client = installed / WRAPPER.RETIRED_CLIENT_NAME
        retired_client.write_bytes(b"old client\n")
        retired_client.chmod(0o755)
        WRAPPER.stage(self.loaded, "tx-retire-fail", "b" * 40, self.payload)
        with mock.patch.object(WRAPPER, "_daemon_reload", side_effect=WRAPPER.WrapperError("injected", "reload failed")):
            with self.assertRaises(WRAPPER.WrapperError):
                WRAPPER.activate(self.loaded, "tx-retire-fail", "b" * 40)
        self.assertEqual(retired.read_bytes(), b"old controller\n")
        self.assertEqual(stat.S_IMODE(retired.stat().st_mode), 0o755)
        self.assertEqual(retired_client.read_bytes(), b"old client\n")
        self.assertEqual(stat.S_IMODE(retired_client.stat().st_mode), 0o755)
        self.assertFalse(Path(self.loaded["control_path"]).exists())

    def test_activation_failure_restores_fixed_targets_and_pointers(self):
        WRAPPER.stage(self.loaded, "tx-failure", "b" * 40, self.payload)
        WRAPPER.verify_transaction(self.loaded, "tx-failure", "b" * 40)
        with mock.patch.object(
            WRAPPER,
            "_daemon_reload",
            side_effect=[WRAPPER.WrapperError("activation_failed", "injected"), None],
        ):
            with self.assertRaises(WRAPPER.WrapperError) as failure:
                WRAPPER.activate(self.loaded, "tx-failure", "b" * 40)
        self.assertEqual(failure.exception.code, "activation_failed")
        self.assertFalse(Path(self.loaded["current_link"]).exists())
        self.assertFalse(Path(self.loaded["control_path"]).exists())
        self.assertFalse(Path(self.loaded["control_path"]).with_name("agent_vm.ts").exists())
        self.assertFalse(Path(self.loaded["control_path"]).with_name("client-release.json").exists())
        for name in WRAPPER.SUPPORT_ARTIFACT_NAMES:
            self.assertFalse(Path(self.loaded["control_path"]).with_name(name).exists())

    def test_package_install_and_restore_do_not_touch_unowned_temporary_symlinks(self):
        source = self.root / "source.ts"
        source.write_bytes(b"new")
        target = self.root / "client.ts"
        adjacent = self.root / "operator-file"
        adjacent.write_bytes(b"keep")
        for suffix in ("release-tmp", "restore-tmp"):
            (self.root / (".client.ts." + suffix)).symlink_to(adjacent)
        WRAPPER._copy_atomic(source, target, os.getuid(), 0o644)
        self.assertEqual(target.read_bytes(), b"new")
        WRAPPER._restore_targets({target: (b"old", 0o600, os.getuid())})
        self.assertEqual(target.read_bytes(), b"old")
        self.assertEqual(stat.S_IMODE(target.stat().st_mode), 0o600)
        self.assertEqual(adjacent.read_bytes(), b"keep")
        for suffix in ("release-tmp", "restore-tmp"):
            self.assertTrue((self.root / (".client.ts." + suffix)).is_symlink())

    def test_install_cleanup_and_restore_reject_symlinked_parent_directories(self):
        source = self.root / "source.ts"
        source.write_bytes(b"new")
        bundle_root = Path(self.loaded["control_path"]).parent
        bundle_root.mkdir(parents=True)
        outside = self.root / "operator-owned"
        outside.mkdir()
        foreign = outside / "foreign.ts"
        foreign.write_bytes(b"keep")
        systemd = bundle_root / "systemd"
        systemd.symlink_to(outside, target_is_directory=True)

        with self.assertRaises(WRAPPER.WrapperError) as copy_failure:
            WRAPPER._copy_atomic(source, systemd / "new.service", os.getuid(), 0o644)
        self.assertEqual(copy_failure.exception.code, "activation_unverified")
        self.assertFalse((outside / "new.service").exists())

        with self.assertRaises(WRAPPER.WrapperError) as remove_failure:
            WRAPPER._remove_owned_target(systemd / foreign.name, os.getuid(), (0o644,))
        self.assertEqual(remove_failure.exception.code, "activation_unverified")
        self.assertEqual(foreign.read_bytes(), b"keep")

        with self.assertRaises(WRAPPER.WrapperError) as restore_failure:
            WRAPPER._restore_targets({systemd / "restored.service": (b"old", 0o644, os.getuid())})
        self.assertEqual(restore_failure.exception.code, "activation_restore_failed")
        self.assertFalse((outside / "restored.service").exists())

        with mock.patch.object(WRAPPER, "_copy_atomic") as copy:
            with self.assertRaises(WRAPPER.WrapperError) as install_failure:
                WRAPPER._install_release_files(self.loaded, self.root, {})
        self.assertEqual(install_failure.exception.code, "activation_unverified")
        copy.assert_not_called()

    def test_activation_failure_restores_existing_support_bytes_modes_and_owners(self):
        installed = Path(self.loaded["control_path"]).parent
        installed.mkdir(parents=True)
        for index, name in enumerate(sorted(WRAPPER.SUPPORT_ARTIFACT_NAMES)):
            if index % 2 == 0:
                path = installed / name
                path.write_bytes(f"previous:{name}\n".encode())
                path.chmod(0o600)
        before = WRAPPER._snapshot_targets(self.loaded)
        WRAPPER.stage(self.loaded, "tx-support-restore", "b" * 40, self.payload)
        with mock.patch.object(
            WRAPPER, "_daemon_reload",
            side_effect=[WRAPPER.WrapperError("activation_failed", "injected"), None],
        ):
            with self.assertRaises(WRAPPER.WrapperError) as failure:
                WRAPPER.activate(self.loaded, "tx-support-restore", "b" * 40)
        self.assertEqual(failure.exception.code, "activation_failed")
        self.assertEqual(WRAPPER._snapshot_targets(self.loaded), before)
        self.assertFalse(Path(self.loaded["current_link"]).exists())
        self.assertFalse(Path(self.loaded["fence_path"]).exists())

    def test_activation_rejects_a_modified_retained_release(self):
        first = self.root / "retained-first.tar"
        second = self.root / "retained-second.tar"
        RELEASE.build_release(ROOT, first, "e" * 40)
        RELEASE.build_release(ROOT, second, "f" * 40)
        with mock.patch.object(WRAPPER.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, b"", b"")):
            WRAPPER.stage(self.loaded, "tx-retained-first", "e" * 40, first.read_bytes())
            WRAPPER.activate(self.loaded, "tx-retained-first", "e" * 40)
            retained_unit = Path(self.loaded["release_root"]) / ("e" * 40) / "systemd" / "agent-vm-native-reconcile.service"
            retained_unit.write_text(retained_unit.read_text(encoding="utf-8") + "\n", encoding="utf-8")
            WRAPPER.stage(self.loaded, "tx-retained-second", "f" * 40, second.read_bytes())
            WRAPPER.verify_transaction(self.loaded, "tx-retained-second", "f" * 40)
            with self.assertRaises(WRAPPER.WrapperError) as failure:
                WRAPPER.activate(self.loaded, "tx-retained-second", "f" * 40)
        self.assertEqual(failure.exception.code, "activation_unverified")

    def test_rollback_is_bounded_and_keeps_previous_release(self):
        first = self.root / "first.tar"
        second = self.root / "second.tar"
        RELEASE.build_release(ROOT, first, "c" * 40)
        RELEASE.build_release(ROOT, second, "d" * 40)
        # Give every support artifact distinct bytes in the newer release so
        # rollback must actually restore the retained modules.
        newer = self.root / "newer-support"
        WRAPPER.safe_extract_archive(second.read_bytes(), newer)
        manifest_path = newer / "release-manifest.json"
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        previous_support = {}
        for item in manifest["artifacts"]:
            if item["role"] != "support":
                continue
            path = newer / item["path"]
            previous_support[item["path"]] = path.read_bytes()
            comment = b"\n# newer release\n" if path.suffix == ".py" else b"\n// newer release\n"
            path.write_bytes(path.read_bytes() + comment)
            item["sha256"] = RELEASE.sha256_file(path)
        manifest_path.write_text(json.dumps(manifest), encoding="utf-8")
        sidecar_path = newer / "client-release.json"
        sidecar = json.loads(sidecar_path.read_text(encoding="utf-8"))
        sidecar["client_dependencies"] = [
            item for item in manifest["artifacts"] if item["path"] in RELEASE.CLIENT_DEPENDENCY_MODES
        ]
        sidecar_path.write_text(json.dumps(sidecar), encoding="utf-8")
        RELEASE._create_archive(newer, second)
        with mock.patch.object(WRAPPER.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, b"", b"")):
            WRAPPER.stage(self.loaded, "tx-first", "c" * 40, first.read_bytes())
            WRAPPER.activate(self.loaded, "tx-first", "c" * 40)
            WRAPPER.stage(self.loaded, "tx-second", "d" * 40, second.read_bytes())
            WRAPPER.activate(self.loaded, "tx-second", "d" * 40)
            for name, content in previous_support.items():
                self.assertNotEqual(
                    Path(self.loaded["control_path"]).with_name(name).read_bytes(), content,
                )
            result = WRAPPER.rollback(self.loaded, "tx-second", "d" * 40)
        self.assertTrue(result["rolled_back"])
        self.assertEqual(result["release_id"], "c" * 40)
        self.assertEqual(Path(self.loaded["current_link"]).resolve().name, "c" * 40)
        for name, content in previous_support.items():
            self.assertEqual(
                Path(self.loaded["control_path"]).with_name(name).read_bytes(), content,
            )
        with self.assertRaises(WRAPPER.WrapperError) as failure:
            WRAPPER.rollback(self.loaded, "tx-second", "d" * 40)
        self.assertEqual(failure.exception.code, "rollback_already_attempted")

    def test_interrupted_rollback_after_pointer_switch_can_be_retried(self):
        first = self.root / "retry-first.tar"
        second = self.root / "retry-second.tar"
        RELEASE.build_release(ROOT, first, "c" * 40)
        RELEASE.build_release(ROOT, second, "d" * 40)
        with mock.patch.object(WRAPPER.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, b"", b"")):
            WRAPPER.stage(self.loaded, "tx-retry-first", "c" * 40, first.read_bytes())
            WRAPPER.activate(self.loaded, "tx-retry-first", "c" * 40)
            WRAPPER.stage(self.loaded, "tx-retry-second", "d" * 40, second.read_bytes())
            WRAPPER.activate(self.loaded, "tx-retry-second", "d" * 40)
        activation_path = Path(self.loaded["state_root"]) / "tx-retry-second.activation.json"
        record = json.loads(activation_path.read_text(encoding="utf-8"))
        record.update({"rollback_attempts": 1, "rollback_state": "in-progress", "rollback_phase": "started"})
        activation_path.write_text(json.dumps(record), encoding="utf-8")
        WRAPPER._atomic_symlink(Path(self.loaded["release_root"]) / ("c" * 40), Path(self.loaded["current_link"]))
        with mock.patch.object(WRAPPER.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, b"", b"")):
            result = WRAPPER.rollback(self.loaded, "tx-retry-second", "d" * 40)
        self.assertTrue(result["rolled_back"])
        self.assertEqual(Path(self.loaded["current_link"]).resolve().name, "c" * 40)
        self.assertEqual(Path(self.loaded["previous_link"]).resolve().name, "d" * 40)
        self.assertFalse(Path(self.loaded["fence_path"]).exists())
        saved = json.loads(activation_path.read_text(encoding="utf-8"))
        self.assertEqual(saved["rollback_state"], "complete")

    def test_rollback_rejects_a_drifted_previous_pointer(self):
        first = self.root / "pointer-first.tar"
        second = self.root / "pointer-second.tar"
        RELEASE.build_release(ROOT, first, "8" * 40)
        RELEASE.build_release(ROOT, second, "9" * 40)
        with mock.patch.object(WRAPPER.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, b"", b"")):
            WRAPPER.stage(self.loaded, "tx-pointer-first", "8" * 40, first.read_bytes())
            WRAPPER.activate(self.loaded, "tx-pointer-first", "8" * 40)
            WRAPPER.stage(self.loaded, "tx-pointer-second", "9" * 40, second.read_bytes())
            WRAPPER.activate(self.loaded, "tx-pointer-second", "9" * 40)
            WRAPPER._atomic_symlink(Path(self.loaded["release_root"]) / ("9" * 40), Path(self.loaded["previous_link"]))
            with self.assertRaises(WRAPPER.WrapperError) as failure:
                WRAPPER.rollback(self.loaded, "tx-pointer-second", "9" * 40)
        self.assertEqual(failure.exception.code, "rollback_stale")
        self.assertEqual(Path(self.loaded["current_link"]).resolve().name, "9" * 40)

    def test_stale_rollback_cannot_downgrade_a_newer_release(self):
        first = self.root / "stale-first.tar"
        second = self.root / "stale-second.tar"
        third = self.root / "stale-third.tar"
        RELEASE.build_release(ROOT, first, "5" * 40)
        RELEASE.build_release(ROOT, second, "6" * 40)
        RELEASE.build_release(ROOT, third, "7" * 40)
        with mock.patch.object(WRAPPER.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, b"", b"")):
            WRAPPER.stage(self.loaded, "tx-stale-first", "5" * 40, first.read_bytes())
            WRAPPER.activate(self.loaded, "tx-stale-first", "5" * 40)
            WRAPPER.stage(self.loaded, "tx-stale-second", "6" * 40, second.read_bytes())
            WRAPPER.activate(self.loaded, "tx-stale-second", "6" * 40)
            WRAPPER.stage(self.loaded, "tx-stale-third", "7" * 40, third.read_bytes())
            WRAPPER.activate(self.loaded, "tx-stale-third", "7" * 40)
            with self.assertRaises(WRAPPER.WrapperError) as failure:
                WRAPPER.rollback(self.loaded, "tx-stale-second", "6" * 40)
        self.assertEqual(failure.exception.code, "rollback_stale")
        self.assertEqual(Path(self.loaded["current_link"]).resolve().name, "7" * 40)

    def test_interrupted_activation_record_can_be_rolled_back_once(self):
        first = self.root / "interrupted-first.tar"
        second = self.root / "interrupted-second.tar"
        RELEASE.build_release(ROOT, first, "1" * 40)
        RELEASE.build_release(ROOT, second, "2" * 40)
        with mock.patch.object(WRAPPER.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, b"", b"")):
            WRAPPER.stage(self.loaded, "tx-interrupted-first", "1" * 40, first.read_bytes())
            WRAPPER.activate(self.loaded, "tx-interrupted-first", "1" * 40)
            WRAPPER.stage(self.loaded, "tx-interrupted-second", "2" * 40, second.read_bytes())
            WRAPPER.activate(self.loaded, "tx-interrupted-second", "2" * 40)
        activation_path = Path(self.loaded["state_root"]) / "tx-interrupted-second.activation.json"
        record = json.loads(activation_path.read_text(encoding="utf-8"))
        record["activated"] = False
        record["activation_phase"] = "current-pointer-updated"
        activation_path.write_text(json.dumps(record), encoding="utf-8")
        with mock.patch.object(WRAPPER.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, b"", b"")):
            result = WRAPPER.rollback(self.loaded, "tx-interrupted-second", "2" * 40)
        self.assertTrue(result["rolled_back"])
        self.assertEqual(Path(self.loaded["current_link"]).resolve().name, "1" * 40)

    def test_rollback_failure_keeps_structured_error_and_drain_state(self):
        first = self.root / "failure-first.tar"
        second = self.root / "failure-second.tar"
        RELEASE.build_release(ROOT, first, "3" * 40)
        RELEASE.build_release(ROOT, second, "4" * 40)
        with mock.patch.object(WRAPPER.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, b"", b"")):
            WRAPPER.stage(self.loaded, "tx-failure-first", "3" * 40, first.read_bytes())
            WRAPPER.activate(self.loaded, "tx-failure-first", "3" * 40)
            WRAPPER.stage(self.loaded, "tx-failure-second", "4" * 40, second.read_bytes())
            WRAPPER.activate(self.loaded, "tx-failure-second", "4" * 40)
            with mock.patch.object(WRAPPER, "_install_release_files", side_effect=WRAPPER.WrapperError("injected", "rollback failure")):
                with self.assertRaises(WRAPPER.WrapperError):
                    WRAPPER.rollback(self.loaded, "tx-failure-second", "4" * 40)
        record = json.loads((Path(self.loaded["state_root"]) / "tx-failure-second.activation.json").read_text(encoding="utf-8"))
        self.assertEqual(record["rollback_state"], "failed")
        self.assertTrue(record["draining"])
        self.assertEqual(record["rollback_error"]["code"], "injected")


if __name__ == "__main__":
    unittest.main()
