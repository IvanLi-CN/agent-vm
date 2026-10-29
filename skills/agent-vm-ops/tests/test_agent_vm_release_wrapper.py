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
        self.admission_lock_patch = mock.patch.object(WRAPPER, "_admission_lock")
        self.admission_lock_patch.start()
        self.addCleanup(self.admission_lock_patch.stop)
        self.durable_drain_owner = WRAPPER._verify_durable_drain_owner
        self.drain_owner_patch = mock.patch.object(WRAPPER, "_verify_durable_drain_owner")
        self.drain_owner_patch.start()
        self.addCleanup(self.drain_owner_patch.stop)
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

    def test_interrupted_recovery_accepts_a_consistent_durable_drain_snapshot(self):
        database_path = self._durable_drain_database()
        with mock.patch.object(WRAPPER, "STATE_DB_PATH", database_path):
            WRAPPER._verify_durable_drain_state({"format": MANIFEST.GO_MANIFEST_FORMAT})

    def test_interrupted_recovery_rejects_an_exited_drain(self):
        database_path = self._durable_drain_database(draining="0")
        with mock.patch.object(WRAPPER, "STATE_DB_PATH", database_path):
            with self.assertRaises(WRAPPER.WrapperError) as failure:
                WRAPPER._verify_durable_drain_state()
        self.assertEqual(failure.exception.code, "drain_required")

    def test_interrupted_recovery_rejects_new_pending_work(self):
        database_path = self._durable_drain_database(pending=True)
        with mock.patch.object(WRAPPER, "STATE_DB_PATH", database_path):
            with self.assertRaises(WRAPPER.WrapperError) as failure:
                WRAPPER._verify_durable_drain_state()
        self.assertEqual(failure.exception.code, "drain_unverified")

    def test_atomic_install_converts_temporary_file_creation_failure(self):
        source = self.root / "source.txt"
        source.write_text("source", encoding="utf-8")
        target = self.root / "installed" / "target.txt"
        with mock.patch.object(WRAPPER.tempfile, "mkstemp", side_effect=OSError("no space")):
            with self.assertRaises(WRAPPER.WrapperError) as failure:
                WRAPPER._copy_atomic(source, target, os.getuid(), 0o644)
        self.assertEqual(failure.exception.code, "activation_failed")

    def test_activation_fence_refresh_restores_service_owned_primary_path(self):
        primary = self.loaded["fence_path"]
        persistent = primary.with_name("." + primary.name + ".persistent")
        with WRAPPER._activation_fence(self.loaded, "tx-fence", "a" * 40) as refresh:
            primary.unlink()
            refresh()
            self.assertTrue(primary.is_file())
            self.assertTrue(persistent.is_file())
        self.assertFalse(primary.exists())
        self.assertFalse(persistent.exists())

    def test_activation_fence_cleans_a_partial_initialization(self):
        primary = self.loaded["fence_path"]
        persistent = primary.with_name("." + primary.name + ".persistent")
        original = WRAPPER._write_activation_fence

        def fail_persistent(path, transaction_id, release_id):
            if path == persistent:
                raise WRAPPER.WrapperError("release_fence_unavailable", "injected persistent fence failure")
            return original(path, transaction_id, release_id)

        with mock.patch.object(WRAPPER, "_write_activation_fence", side_effect=fail_persistent):
            with self.assertRaises(WRAPPER.WrapperError) as failure:
                with WRAPPER._activation_fence(self.loaded, "tx-partial-fence", "a" * 40):
                    pass
        self.assertEqual(failure.exception.code, "release_fence_unavailable")
        self.assertFalse(primary.exists())
        self.assertFalse(persistent.exists())

    def test_activation_fence_retry_preserves_the_existing_transaction_fence(self):
        primary = self.loaded["fence_path"]
        persistent = primary.with_name("." + primary.name + ".persistent")
        with WRAPPER._activation_fence(self.loaded, "tx-retry-fence", "a" * 40):
            with WRAPPER._activation_fence(self.loaded, "tx-retry-fence", "a" * 40):
                self.assertTrue(primary.exists())
                self.assertTrue(persistent.exists())
            self.assertTrue(primary.exists())
            self.assertTrue(persistent.exists())
        self.assertFalse(primary.exists())
        self.assertFalse(persistent.exists())

    def test_activation_fence_adopts_a_preexisting_transaction_fence_after_success(self):
        primary = self.loaded["fence_path"]
        persistent = primary.with_name("." + primary.name + ".persistent")
        WRAPPER._write_activation_fence(primary, "tx-adopt-fence", "a" * 40)
        WRAPPER._write_activation_fence(persistent, "tx-adopt-fence", "a" * 40)
        with WRAPPER._activation_fence(self.loaded, "tx-adopt-fence", "a" * 40):
            self.assertTrue(primary.exists())
            self.assertTrue(persistent.exists())
        self.assertFalse(primary.exists())
        self.assertFalse(persistent.exists())

    def test_activation_fence_preserves_a_preexisting_transaction_fence_on_failure(self):
        primary = self.loaded["fence_path"]
        persistent = primary.with_name("." + primary.name + ".persistent")
        WRAPPER._write_activation_fence(primary, "tx-preserve-fence", "a" * 40)
        WRAPPER._write_activation_fence(persistent, "tx-preserve-fence", "a" * 40)
        try:
            with self.assertRaises(RuntimeError):
                with WRAPPER._activation_fence(self.loaded, "tx-preserve-fence", "a" * 40):
                    raise RuntimeError("injected activation failure")
            self.assertTrue(primary.exists())
            self.assertTrue(persistent.exists())
        finally:
            WRAPPER._clear_activation_fence(primary, "tx-preserve-fence", "a" * 40)
            WRAPPER._clear_activation_fence(persistent, "tx-preserve-fence", "a" * 40)

    def test_activation_fence_preserves_a_new_fence_on_process_interrupt(self):
        primary = self.loaded["fence_path"]
        persistent = primary.with_name("." + primary.name + ".persistent")
        try:
            with self.assertRaises(KeyboardInterrupt):
                with WRAPPER._activation_fence(self.loaded, "tx-interrupt-fence", "a" * 40):
                    raise KeyboardInterrupt()
            self.assertTrue(primary.exists())
            self.assertTrue(persistent.exists())
        finally:
            WRAPPER._clear_activation_fence(primary, "tx-interrupt-fence", "a" * 40)
            WRAPPER._clear_activation_fence(persistent, "tx-interrupt-fence", "a" * 40)

    def test_activation_fence_cleans_created_file_when_directory_fsync_fails(self):
        primary = self.loaded["fence_path"]
        persistent = primary.with_name("." + primary.name + ".persistent")
        with mock.patch.object(
            WRAPPER,
            "_fsync_directory",
            side_effect=WRAPPER.WrapperError("wrapper_io_error", "injected directory fsync failure"),
        ):
            with self.assertRaises(WRAPPER.WrapperError) as failure:
                with WRAPPER._activation_fence(self.loaded, "tx-fsync-fence", "a" * 40):
                    pass
        self.assertEqual(failure.exception.code, "wrapper_io_error")
        self.assertFalse(primary.exists())
        self.assertFalse(persistent.exists())

    def test_durable_drain_recovery_requires_the_current_transaction_owner(self):
        database_path = self._durable_drain_database()
        with mock.patch.object(WRAPPER, "STATE_DB_PATH", database_path):
            with self.assertRaises(WRAPPER.WrapperError) as failure:
                WRAPPER._verify_durable_drain_state(
                    {"format": MANIFEST.GO_MANIFEST_FORMAT},
                    expected_owner="release-other-transaction",
                )
        self.assertEqual(failure.exception.code, "drain_unverified")
        with mock.patch.object(WRAPPER, "STATE_DB_PATH", database_path):
            WRAPPER._verify_durable_drain_state(
                {"format": MANIFEST.GO_MANIFEST_FORMAT},
                expected_owner="release-tx-recovery",
            )

    def test_legacy_ownerless_drain_can_be_claimed_by_the_release_transaction(self):
        database_path = self._durable_drain_database(owner="")
        with mock.patch.object(WRAPPER, "STATE_DB_PATH", database_path):
            self.durable_drain_owner("release-tx-legacy", allow_ownerless=True)
        with sqlite3.connect(database_path) as database:
            owner = database.execute("SELECT value FROM meta WHERE key = 'drain_owner'").fetchone()[0]
        self.assertEqual(owner, "release-tx-legacy")

    def test_release_owner_accepts_the_controller_session_id_contract(self):
        owner = WRAPPER._release_drain_owner("tx-session", "release-deploy/@blue-green")
        self.assertEqual(owner, "release-deploy/@blue-green")

    def test_normal_activation_requires_the_current_transaction_owner(self):
        release_id = "b" * 40
        WRAPPER.stage(self.loaded, "tx-owner-check", release_id, self.payload)
        WRAPPER.verify_transaction(self.loaded, "tx-owner-check", release_id)
        database_path = self._durable_drain_database(owner="release-other")
        with mock.patch.object(WRAPPER, "STATE_DB_PATH", database_path), mock.patch.object(
            WRAPPER,
            "_verify_durable_drain_owner",
            side_effect=self.durable_drain_owner,
        ):
            with self.assertRaises(WRAPPER.WrapperError) as failure:
                WRAPPER.activate(self.loaded, "tx-owner-check", release_id)
        self.assertEqual(failure.exception.code, "drain_unverified")
        self.assertFalse(Path(self.loaded["current_link"]).exists())

    def test_legacy_activation_claims_an_ownerless_drain(self):
        release_id = "b" * 40
        WRAPPER.stage(self.loaded, "tx-ownerless-legacy", release_id, self.payload)
        WRAPPER.verify_transaction(self.loaded, "tx-ownerless-legacy", release_id)
        database_path = self._durable_drain_database(owner="")
        WRAPPER._ensure_drained.return_value = False
        with mock.patch.object(WRAPPER, "STATE_DB_PATH", database_path), mock.patch.object(
            WRAPPER,
            "_verify_durable_drain_owner",
            side_effect=self.durable_drain_owner,
        ), mock.patch.object(
            WRAPPER.subprocess,
            "run",
            return_value=subprocess.CompletedProcess([], 0, b"", b""),
        ):
            activation = WRAPPER.activate(self.loaded, "tx-ownerless-legacy", release_id)
        self.assertTrue(activation["activated"])
        with sqlite3.connect(database_path) as database:
            owner = database.execute("SELECT value FROM meta WHERE key = 'drain_owner'").fetchone()[0]
        self.assertEqual(owner, "release-tx-ownerless-legacy")

    def test_normal_rollback_requires_the_activation_transaction_owner(self):
        self.loaded["require_drained"] = False
        first_release_id = "a" * 40
        second_release_id = "c" * 40
        first_bundle = self.root / "owner-first.tar"
        second_bundle = self.root / "owner-second.tar"
        RELEASE.build_release(ROOT, first_bundle, first_release_id)
        RELEASE.build_release(ROOT, second_bundle, second_release_id)
        with mock.patch.object(
            WRAPPER.subprocess,
            "run",
            return_value=subprocess.CompletedProcess([], 0, b"", b""),
        ):
            WRAPPER.stage(self.loaded, "tx-owner-first", first_release_id, first_bundle.read_bytes())
            WRAPPER.verify_transaction(self.loaded, "tx-owner-first", first_release_id)
            WRAPPER.activate(self.loaded, "tx-owner-first", first_release_id)
            WRAPPER.stage(self.loaded, "tx-owner-second", second_release_id, second_bundle.read_bytes())
            WRAPPER.verify_transaction(self.loaded, "tx-owner-second", second_release_id)
            WRAPPER.activate(self.loaded, "tx-owner-second", second_release_id)

        self.loaded["require_drained"] = True
        database_path = self._durable_drain_database(owner="release-other")
        with mock.patch.object(WRAPPER, "STATE_DB_PATH", database_path), mock.patch.object(
            WRAPPER,
            "_verify_durable_drain_owner",
            side_effect=self.durable_drain_owner,
        ):
            with self.assertRaises(WRAPPER.WrapperError) as failure:
                WRAPPER.rollback(self.loaded, "tx-owner-second", second_release_id)
        self.assertEqual(failure.exception.code, "drain_unverified")
        self.assertEqual(Path(self.loaded["current_link"]).resolve().name, second_release_id)

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

    def test_legacy_runtime_refreshes_the_fence_after_native_stop(self):
        events = []

        def stop_native():
            events.append("stop-native")

        def refresh_fence():
            events.append("refresh-fence")

        with mock.patch.object(WRAPPER, "_deactivate_native_runtime", side_effect=stop_native), mock.patch.object(
            WRAPPER,
            "_systemctl",
            side_effect=lambda action, unit, **kwargs: events.append((action, unit)),
        ), mock.patch.object(WRAPPER, "_legacy_expiry_timers", return_value=()):
            WRAPPER._activate_legacy_runtime(refresh_fence=refresh_fence)

        self.assertLess(events.index("stop-native"), events.index("refresh-fence"))
        self.assertLess(events.index("refresh-fence"), events.index(("start", "agent-vm-native-reconcile.service")))

    def test_product_runtime_restores_an_enabled_web_unit_after_native_restart(self):
        with mock.patch.object(
            WRAPPER.subprocess, "run",
            return_value=subprocess.CompletedProcess([], 0, b"", b""),
        ) as run:
            WRAPPER._activate_product_runtime({"format": MANIFEST.PRODUCT_MANIFEST_FORMAT})
        commands = [call.args[0] for call in run.call_args_list]
        self.assertIn(["systemctl", "is-enabled", "--quiet", "agent-vm-web.service"], commands)
        native_restart = commands.index(["systemctl", "restart", "agent-vm-native.service"])
        web_enable = commands.index(["systemctl", "enable", "agent-vm-web.service"])
        web_start = commands.index(["systemctl", "start", "agent-vm-web.service"])
        self.assertLess(native_restart, web_enable)
        self.assertLess(web_enable, web_start)

    def test_product_runtime_does_not_start_a_disabled_web_unit(self):
        def run(command, **kwargs):
            if command == ["systemctl", "is-enabled", "--quiet", "agent-vm-web.service"]:
                return subprocess.CompletedProcess(command, 1, b"", b"")
            return subprocess.CompletedProcess(command, 0, b"", b"")

        with mock.patch.object(WRAPPER.subprocess, "run", side_effect=run) as process:
            WRAPPER._activate_product_runtime({"format": MANIFEST.PRODUCT_MANIFEST_FORMAT})
        commands = [call.args[0] for call in process.call_args_list]
        self.assertIn(["systemctl", "stop", "agent-vm-web.service"], commands)
        self.assertIn(["systemctl", "disable", "agent-vm-web.service"], commands)
        self.assertNotIn(["systemctl", "start", "agent-vm-web.service"], commands)

    def test_product_runtime_restores_a_saved_disabled_web_unit(self):
        with mock.patch.object(
            WRAPPER.subprocess,
            "run",
            return_value=subprocess.CompletedProcess([], 0, b"", b""),
        ) as process:
            WRAPPER._activate_product_runtime(
                {"format": MANIFEST.PRODUCT_MANIFEST_FORMAT},
                web_was_enabled=False,
            )
        commands = [call.args[0] for call in process.call_args_list]
        self.assertIn(["systemctl", "stop", "agent-vm-web.service"], commands)
        self.assertIn(["systemctl", "disable", "agent-vm-web.service"], commands)
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

    def _durable_drain_database(self, *, draining="1", owner="release-tx-recovery", pending=False):
        database_path = self.root / "durable-drain.sqlite3"
        with sqlite3.connect(database_path) as database:
            database.executescript("""
                CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
                CREATE TABLE tasks (state TEXT NOT NULL);
                CREATE TABLE operations (operation_id TEXT, response_json TEXT, created_at TEXT);
                CREATE TABLE state_capabilities (name TEXT NOT NULL, version INTEGER NOT NULL);
            """)
            database.executemany(
                "INSERT INTO meta(key, value) VALUES (?, ?)",
                [("draining", draining), ("drain_owner", owner), ("drain_sealed", "1")],
            )
            database.execute(
                "INSERT INTO state_capabilities(name, version) VALUES (?, ?)",
                ("legacy-agent-vm-core", 1),
            )
            if pending:
                database.execute(
                    "INSERT INTO operations(operation_id, response_json, created_at) VALUES (?, ?, ?)",
                    ("pending-operation", json.dumps({"outcome": "pending"}), "now"),
                )
        return database_path

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

    def test_native_preflight_accepts_fixed_historical_legacy_residue(self):
        self._install_go_runtime("https")
        installed_root = Path(self.loaded["control_path"]).parent
        for name in WRAPPER.LEGACY_ONLY_SUPPORT_NAMES:
            residue = installed_root / name
            residue.write_bytes(b"historical legacy residue\n")
            residue.chmod(WRAPPER.SUPPORT_ARTIFACT_MODES[name])
        installed_manifest = MANIFEST.load_release_manifest(
            installed_root / MANIFEST.MANIFEST_NAME,
        )

        checked = WRAPPER._verify_installed_release(self.loaded, installed_root, installed_manifest)

        self.assertEqual(checked["format"], MANIFEST.GO_MANIFEST_FORMAT)

    def test_native_preflight_accepts_historical_legacy_control_residue(self):
        self._install_go_runtime("https")
        installed_root = Path(self.loaded["control_path"]).parent
        legacy_root = self.root / "legacy-control"
        WRAPPER.safe_extract_archive(self.payload, legacy_root)
        alias = installed_root / "agent_vm_control.ts"
        alias.write_bytes((legacy_root / "agent_vm_control.ts").read_bytes())
        alias.chmod(0o755)
        installed_manifest = MANIFEST.load_release_manifest(
            installed_root / MANIFEST.MANIFEST_NAME,
        )

        checked = WRAPPER._verify_installed_release(self.loaded, installed_root, installed_manifest)

        self.assertEqual(checked["format"], MANIFEST.GO_MANIFEST_FORMAT)

    def test_drain_verification_uses_native_bridge_for_historical_legacy_transport(self):
        self.drain_patch.stop()
        self._install_go_runtime("https")
        installed_root = Path(self.loaded["control_path"]).parent
        (installed_root / "agent_vm_control.ts").unlink()
        manifest_path = installed_root / MANIFEST.MANIFEST_NAME
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        manifest["client_view"]["transport"] = "legacy-bun"
        manifest_path.write_text(json.dumps(manifest, sort_keys=True) + "\n", encoding="utf-8")
        sidecar_path = installed_root / "client-release.json"
        sidecar = json.loads(sidecar_path.read_text(encoding="utf-8"))
        sidecar["client_view"]["transport"] = "legacy-bun"
        sidecar_path.write_text(json.dumps(sidecar, sort_keys=True) + "\n", encoding="utf-8")
        response = json.dumps({"ok": True, "result": {"draining": True}}).encode()
        with mock.patch.object(
            WRAPPER.subprocess,
            "run",
            return_value=subprocess.CompletedProcess([], 0, response, b""),
        ) as run:
            WRAPPER._ensure_drained(self.loaded)

        self.assertEqual(
            run.call_args_list[0].args[0],
            [WRAPPER.BUN_PATH, str(installed_root / "agent_vm_control_bridge.ts")],
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

    def test_interrupted_handoff_can_rollback_without_the_stopped_authority(self):
        self.loaded["require_drained"] = True
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
            WRAPPER.stage(self.loaded, "tx-interrupted", legacy_release_id, self.payload)
            WRAPPER.verify_transaction(self.loaded, "tx-interrupted", legacy_release_id)
            with mock.patch.object(WRAPPER, "_deactivate_native_runtime", side_effect=KeyboardInterrupt):
                with self.assertRaises(KeyboardInterrupt):
                    WRAPPER.activate(self.loaded, "tx-interrupted", legacy_release_id)

            with mock.patch.object(
                WRAPPER, "_verify_durable_drain_state",
            ) as verify_durable_drain, mock.patch.object(WRAPPER, "_ensure_drained", side_effect=AssertionError("interrupted rollback must not use the stopped authority")), mock.patch.object(WRAPPER, "_activate_go_runtime"):
                result = WRAPPER.rollback(self.loaded, "tx-interrupted", legacy_release_id)

        self.assertTrue(result["rolled_back"])
        self.assertEqual(Path(self.loaded["current_link"]).resolve().name, native_release_id)
        verify_durable_drain.assert_called_once()

    def test_activation_restore_failure_stops_all_runtime_authorities(self):
        self.loaded["require_drained"] = False
        native_release_id = "a" * 40
        legacy_release_id = "b" * 40
        native_payload = self._go_payload(native_release_id, transport="https")
        events = []

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
            with mock.patch.object(
                WRAPPER,
                "_deactivate_native_runtime",
                side_effect=lambda: events.append("stop-native"),
            ), mock.patch.object(
                WRAPPER,
                "_deactivate_legacy_runtime",
                side_effect=lambda: events.append("stop-legacy"),
            ), mock.patch.object(
                WRAPPER,
                "_activate_legacy_runtime",
                side_effect=WRAPPER.WrapperError("activation_failed", "injected legacy start failure"),
            ), mock.patch.object(
                WRAPPER,
                "_restore_activation_state",
                side_effect=WRAPPER.WrapperError("activation_restore_failed", "injected restore failure"),
            ), mock.patch.object(
                WRAPPER,
                "_read_live_state_capabilities",
                return_value=[MANIFEST.STATE_CAPABILITY_MARKERS[0]],
            ):
                with self.assertRaises(WRAPPER.WrapperError) as failure:
                    WRAPPER.activate(self.loaded, "tx-legacy", legacy_release_id)

        self.assertEqual(failure.exception.code, "activation_restore_failed")
        self.assertGreaterEqual(events.count("stop-native"), 2)
        self.assertEqual(events[-2:], ["stop-native", "stop-legacy"])

    def test_rollback_to_product_restores_saved_web_unit_state(self):
        self.loaded["require_drained"] = False
        previous_release_id = "a" * 40
        active_release_id = "b" * 40
        previous_dir = Path(self.loaded["release_root"]) / previous_release_id
        active_dir = Path(self.loaded["release_root"]) / active_release_id
        previous_dir.mkdir(parents=True)
        active_dir.mkdir(parents=True)
        Path(self.loaded["current_link"]).symlink_to(active_dir)
        Path(self.loaded["previous_link"]).symlink_to(previous_dir)
        activation_path = Path(self.loaded["state_root"]) / "tx-product.activation.json"
        activation_path.parent.mkdir(parents=True)
        activation_path.write_text(json.dumps({
            "transaction_id": "tx-product",
            "release_id": active_release_id,
            "previous_release_id": previous_release_id,
            "old_current_release_id": previous_release_id,
            "old_previous_release_id": None,
            "manifest_sha256": "0" * 64,
            "activated": True,
            "activation_phase": "active",
            "rollback_attempts": 0,
            "web_unit_enabled": True,
        }), encoding="utf-8")
        previous_manifest = {
            "format": MANIFEST.PRODUCT_MANIFEST_FORMAT,
            "state_compatibility": {
                "reads": list(MANIFEST.STATE_CAPABILITY_MARKERS),
                "writes": list(MANIFEST.STATE_CAPABILITY_MARKERS),
            },
        }
        active_manifest = {"format": MANIFEST.LEGACY_PRODUCT_MANIFEST_FORMAT}

        def retained_manifest(_config, path):
            return previous_manifest if path == previous_dir else active_manifest

        with mock.patch.object(WRAPPER, "_verify_retained_release", side_effect=retained_manifest), mock.patch.object(
            WRAPPER, "_snapshot_targets", return_value={},
        ), mock.patch.object(WRAPPER, "_install_release_files"), mock.patch.object(
            WRAPPER, "_deactivate_legacy_runtime",
        ), mock.patch.object(WRAPPER, "_daemon_reload"), mock.patch.object(
            WRAPPER, "_activate_product_runtime",
        ) as activate_product:
            result = WRAPPER._rollback_unlocked(self.loaded, "tx-product", active_release_id)

        self.assertTrue(result["rolled_back"])
        activate_product.assert_called_once_with(previous_manifest, web_was_enabled=True)

    def test_failed_rollback_restores_current_product_web_unit_state(self):
        self.loaded["require_drained"] = False
        previous_release_id = "a" * 40
        active_release_id = "b" * 40
        previous_dir = Path(self.loaded["release_root"]) / previous_release_id
        active_dir = Path(self.loaded["release_root"]) / active_release_id
        previous_dir.mkdir(parents=True)
        active_dir.mkdir(parents=True)
        Path(self.loaded["current_link"]).symlink_to(active_dir)
        Path(self.loaded["previous_link"]).symlink_to(previous_dir)
        activation_path = Path(self.loaded["state_root"]) / "tx-product-failure.activation.json"
        activation_path.parent.mkdir(parents=True)
        activation_path.write_text(json.dumps({
            "transaction_id": "tx-product-failure",
            "release_id": active_release_id,
            "previous_release_id": previous_release_id,
            "old_current_release_id": previous_release_id,
            "old_previous_release_id": None,
            "manifest_sha256": "0" * 64,
            "activated": True,
            "activation_phase": "active",
            "rollback_attempts": 0,
            "web_unit_enabled": False,
        }), encoding="utf-8")
        active_manifest = {
            "format": MANIFEST.PRODUCT_MANIFEST_FORMAT,
            "state_compatibility": {
                "reads": list(MANIFEST.STATE_CAPABILITY_MARKERS),
                "writes": list(MANIFEST.STATE_CAPABILITY_MARKERS),
            },
        }
        previous_manifest = {"format": MANIFEST.MANIFEST_FORMAT}

        def retained_manifest(_config, path):
            return previous_manifest if path == previous_dir else active_manifest

        with mock.patch.object(WRAPPER, "_verify_retained_release", side_effect=retained_manifest), mock.patch.object(
            WRAPPER, "_snapshot_targets", return_value={},
        ), mock.patch.object(WRAPPER, "_install_release_files"), mock.patch.object(
            WRAPPER, "_deactivate_native_runtime",
        ), mock.patch.object(WRAPPER, "_daemon_reload"), mock.patch.object(
            WRAPPER, "_activate_legacy_runtime",
            side_effect=WRAPPER.WrapperError("activation_failed", "injected legacy start failure"),
        ), mock.patch.object(WRAPPER, "_restore_activation_state"), mock.patch.object(
            WRAPPER, "_unit_is_enabled", return_value=True,
        ) as is_enabled, mock.patch.object(WRAPPER, "_activate_product_runtime") as activate_product:
            with self.assertRaises(WRAPPER.WrapperError) as failure:
                WRAPPER._rollback_unlocked(self.loaded, "tx-product-failure", active_release_id)

        self.assertEqual(failure.exception.code, "activation_failed")
        is_enabled.assert_called_once_with("agent-vm-web.service")
        activate_product.assert_called_once_with(active_manifest, web_was_enabled=True)

    def test_rollback_restore_failure_keeps_all_authorities_stopped(self):
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
                side_effect=WRAPPER.WrapperError("activation_failed", "injected runtime failure"),
            ) as activate_go, mock.patch.object(
                WRAPPER,
                "_restore_activation_state",
                side_effect=WRAPPER.WrapperError("activation_restore_failed", "injected restore failure"),
            ), mock.patch.object(WRAPPER, "_deactivate_native_runtime") as stop_native, mock.patch.object(
                WRAPPER, "_deactivate_legacy_runtime",
            ) as stop_legacy:
                with self.assertRaises(WRAPPER.WrapperError) as failure:
                    WRAPPER.rollback(self.loaded, "tx-second-native", second_release_id)

        self.assertEqual(failure.exception.code, "activation_restore_failed")
        activate_go.assert_called_once_with()
        stop_native.assert_called_once_with()
        stop_legacy.assert_called_once_with()

    def test_rollback_runtime_restore_failure_stops_all_authorities(self):
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
                side_effect=WRAPPER.WrapperError("activation_failed", "injected runtime restore failure"),
            ) as activate_go, mock.patch.object(WRAPPER, "_deactivate_native_runtime") as stop_native, mock.patch.object(
                WRAPPER, "_deactivate_legacy_runtime",
            ) as stop_legacy:
                with self.assertRaises(WRAPPER.WrapperError) as failure:
                    WRAPPER.rollback(self.loaded, "tx-second-native", second_release_id)

        self.assertEqual(failure.exception.code, "activation_restore_failed")
        self.assertEqual(activate_go.call_count, 2)
        activate_go.assert_any_call()
        stop_native.assert_called_once_with()
        stop_legacy.assert_called_once_with()

    def test_rollback_record_failure_restores_original_runtime(self):
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

            activation_path = Path(self.loaded["state_root"]) / "tx-second-native.activation.json"
            original_write = WRAPPER._write_json

            def write_record(path, value):
                if path == activation_path and value.get("rollback_phase") == "complete":
                    raise WRAPPER.WrapperError("activation_failed", "injected record persistence failure")
                return original_write(path, value)

            with mock.patch.object(WRAPPER, "_write_json", side_effect=write_record), mock.patch.object(
                WRAPPER,
                "_activate_go_runtime",
                return_value=None,
            ) as activate_go, mock.patch.object(WRAPPER, "_deactivate_native_runtime") as stop_native, mock.patch.object(
                WRAPPER, "_deactivate_legacy_runtime",
            ) as stop_legacy:
                with self.assertRaises(WRAPPER.WrapperError) as failure:
                    WRAPPER.rollback(self.loaded, "tx-second-native", second_release_id)

        self.assertEqual(failure.exception.code, "activation_failed")
        self.assertEqual(Path(self.loaded["current_link"]).resolve().name, second_release_id)
        self.assertEqual(activate_go.call_count, 2)
        stop_native.assert_called_once_with()
        stop_legacy.assert_called_once_with()

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

    def test_activation_parent_directories_reject_writable_ancestors(self):
        unsafe = self.root / "unsafe-parent"
        nested = unsafe / "nested"
        unsafe.mkdir()
        nested.mkdir()
        unsafe.chmod(0o777)

        with self.assertRaises(WRAPPER.WrapperError) as failure:
            WRAPPER._require_directory_chain(nested, "activation_unverified", "test activation parent")
        self.assertEqual(failure.exception.code, "activation_unverified")

    def test_native_deactivation_attempts_all_units_after_web_failure(self):
        events = []

        def systemctl(action, unit, *, allow_missing=False):
            events.append((action, unit))
            if (action, unit) == ("stop", "agent-vm-web.service"):
                raise WRAPPER.WrapperError("activation_failed", "injected web stop failure")

        with mock.patch.object(WRAPPER, "_systemctl", side_effect=systemctl):
            with self.assertRaises(WRAPPER.WrapperError):
                WRAPPER._deactivate_native_runtime()

        self.assertEqual(events, [
            ("stop", "agent-vm-web.service"),
            ("disable", "agent-vm-web.service"),
            ("stop", "agent-vm-native.service"),
            ("disable", "agent-vm-native.service"),
        ])

    def test_legacy_deactivation_attempts_all_units_after_failure(self):
        expiry = "agent-vm-native-expire-test.timer"
        expiry_service = "agent-vm-native-expire@test.service"
        events = []

        def systemctl(action, unit, *, allow_missing=False):
            events.append((action, unit))
            if unit == WRAPPER.GO_RETIRED_UNITS[0] and action == "stop":
                raise WRAPPER.WrapperError("activation_failed", "injected legacy stop failure")

        with mock.patch.object(WRAPPER, "_legacy_expiry_timers", return_value=(expiry,)), mock.patch.object(
            WRAPPER, "_legacy_expiry_services", return_value=(expiry_service,),
        ), mock.patch.object(WRAPPER, "_systemctl", side_effect=systemctl,
        ):
            with self.assertRaises(WRAPPER.WrapperError):
                WRAPPER._deactivate_legacy_runtime()

        retired_timers = tuple(unit for unit in WRAPPER.GO_RETIRED_UNITS if unit.endswith(".timer"))
        retired_services = tuple(unit for unit in WRAPPER.GO_RETIRED_UNITS if not unit.endswith(".timer"))
        expected_units = (*retired_timers, expiry, *retired_services, expiry_service)
        self.assertEqual(
            events,
            [(action, unit) for unit in expected_units for action in ("stop", "disable")],
        )

    def test_legacy_expiry_inventory_errors_fail_closed(self):
        failed = subprocess.CompletedProcess(
            [], 1, b"", b"Failed to list units: Input/output error\n",
        )
        with mock.patch.object(WRAPPER.subprocess, "run", return_value=failed):
            with self.assertRaises(WRAPPER.WrapperError) as failure:
                WRAPPER._legacy_expiry_services(())
        self.assertEqual(failure.exception.code, "activation_failed")

    def test_legacy_deactivation_stops_timerless_expiry_service_instances(self):
        service = "agent-vm-native-expire@demo-g1.service"
        events = []

        def systemctl(action, unit, *, allow_missing=False):
            events.append((action, unit))

        with mock.patch.object(WRAPPER, "_legacy_expiry_timers", return_value=()), mock.patch.object(
            WRAPPER, "_legacy_expiry_services", return_value=(service,)
        ), mock.patch.object(WRAPPER, "_systemctl", side_effect=systemctl):
            WRAPPER._deactivate_legacy_runtime()

        self.assertIn(("stop", service), events)
        self.assertIn(("disable", service), events)

    def test_initial_activation_hands_off_installed_legacy_runtime_without_current_pointer(self):
        self.loaded["require_drained"] = False
        legacy_root = self.root / "legacy-installed"
        WRAPPER.safe_extract_archive(self.payload, legacy_root)
        legacy_manifest = MANIFEST.load_release_manifest(legacy_root / MANIFEST.MANIFEST_NAME)
        WRAPPER._install_release_files(self.loaded, legacy_root, legacy_manifest)

        release_id = "a" * 40
        native_payload = self._go_payload(release_id, transport="https")
        events = []
        original_install = WRAPPER._install_release_files

        def install(*args, **kwargs):
            events.append("install")
            return original_install(*args, **kwargs)

        def stop_legacy():
            events.append("deactivate-legacy")

        with mock.patch.object(WRAPPER, "_install_release_files", side_effect=install), mock.patch.object(
            WRAPPER, "_deactivate_legacy_runtime", side_effect=stop_legacy,
        ), mock.patch.object(
            WRAPPER.subprocess,
            "run",
            return_value=subprocess.CompletedProcess([], 0, b"", b""),
        ):
            WRAPPER.stage(self.loaded, "tx-initial-native", release_id, native_payload)
            WRAPPER.verify_transaction(self.loaded, "tx-initial-native", release_id)
            WRAPPER.activate(self.loaded, "tx-initial-native", release_id)

        self.assertLess(events.index("deactivate-legacy"), events.index("install"))
        self.assertEqual(Path(self.loaded["current_link"]).resolve().name, release_id)

    def test_initial_activation_failure_restores_installed_legacy_runtime_without_pointer(self):
        self.loaded["require_drained"] = False
        legacy_root = self.root / "legacy-installed-failure"
        WRAPPER.safe_extract_archive(self.payload, legacy_root)
        legacy_manifest = MANIFEST.load_release_manifest(legacy_root / MANIFEST.MANIFEST_NAME)
        WRAPPER._install_release_files(self.loaded, legacy_root, legacy_manifest)

        release_id = "a" * 40
        native_payload = self._go_payload(release_id, transport="https")
        WRAPPER.stage(self.loaded, "tx-initial-native-failure", release_id, native_payload)
        WRAPPER.verify_transaction(self.loaded, "tx-initial-native-failure", release_id)
        with mock.patch.object(WRAPPER, "_deactivate_legacy_runtime") as stop_legacy, mock.patch.object(
            WRAPPER,
            "_install_release_files",
            side_effect=WRAPPER.WrapperError("activation_failed", "injected install failure"),
        ), mock.patch.object(WRAPPER, "_restore_activation_state"), mock.patch.object(
            WRAPPER, "_daemon_reload",
        ), mock.patch.object(WRAPPER, "_unit_is_enabled", return_value=False), mock.patch.object(
            WRAPPER, "_activate_legacy_runtime",
        ) as activate_legacy, mock.patch.object(
            WRAPPER, "_stop_runtime_authorities",
        ) as stop_all, mock.patch.object(
            WRAPPER,
            "_read_live_state_capabilities",
            return_value=[MANIFEST.STATE_CAPABILITY_MARKERS[0]],
        ):
            with self.assertRaises(WRAPPER.WrapperError) as failure:
                WRAPPER.activate(self.loaded, "tx-initial-native-failure", release_id)

        self.assertEqual(failure.exception.code, "activation_failed")
        stop_legacy.assert_called_once_with()
        activate_legacy.assert_called_once_with(refresh_fence=mock.ANY)
        stop_all.assert_not_called()

    def test_interrupted_initial_activation_restores_pointerless_legacy_predecessor(self):
        self.loaded["require_drained"] = False
        legacy_root = self.root / "legacy-installed-interrupted"
        WRAPPER.safe_extract_archive(self.payload, legacy_root)
        legacy_manifest = MANIFEST.load_release_manifest(legacy_root / MANIFEST.MANIFEST_NAME)
        WRAPPER._install_release_files(self.loaded, legacy_root, legacy_manifest)

        release_id = "a" * 40
        native_payload = self._go_payload(release_id, transport="https")
        WRAPPER.stage(self.loaded, "tx-initial-native-interrupted", release_id, native_payload)
        WRAPPER.verify_transaction(self.loaded, "tx-initial-native-interrupted", release_id)
        with mock.patch.object(WRAPPER, "_deactivate_legacy_runtime"), mock.patch.object(
            WRAPPER, "_daemon_reload",
        ), mock.patch.object(
            WRAPPER, "_activate_go_runtime", side_effect=KeyboardInterrupt,
        ), mock.patch.object(
            WRAPPER.subprocess,
            "run",
            return_value=subprocess.CompletedProcess([], 0, b"", b""),
        ):
            with self.assertRaises(KeyboardInterrupt):
                WRAPPER.activate(self.loaded, "tx-initial-native-interrupted", release_id)

        activation_path = Path(self.loaded["state_root"]) / "tx-initial-native-interrupted.activation.json"
        record = json.loads(activation_path.read_text(encoding="utf-8"))
        self.assertTrue(record["predecessor_snapshot"])
        self.assertFalse(Path(self.loaded["previous_link"]).exists())
        with mock.patch.object(WRAPPER, "_deactivate_native_runtime"), mock.patch.object(
            WRAPPER, "_daemon_reload",
        ), mock.patch.object(WRAPPER, "_activate_legacy_runtime") as activate_legacy:
            result = WRAPPER.rollback(self.loaded, "tx-initial-native-interrupted", release_id)

        self.assertTrue(result["rolled_back"])
        self.assertTrue(result["predecessor_restored"])
        self.assertEqual(result["release_id"], legacy_manifest["release_id"])
        self.assertFalse(Path(self.loaded["current_link"]).exists())
        self.assertFalse(Path(self.loaded["previous_link"]).exists())
        self.assertEqual(
            MANIFEST.load_release_manifest(
                Path(self.loaded["control_path"]).with_name(MANIFEST.MANIFEST_NAME),
            ),
            legacy_manifest,
        )
        activate_legacy.assert_called_once_with(native_already_stopped=True)

    def test_completed_initial_activation_can_rollback_to_pointerless_predecessor(self):
        self.loaded["require_drained"] = False
        legacy_root = self.root / "legacy-installed-completed"
        WRAPPER.safe_extract_archive(self.payload, legacy_root)
        legacy_manifest = MANIFEST.load_release_manifest(legacy_root / MANIFEST.MANIFEST_NAME)
        WRAPPER._install_release_files(self.loaded, legacy_root, legacy_manifest)

        release_id = "b" * 40
        native_payload = self._go_payload(release_id, transport="https")
        WRAPPER.stage(self.loaded, "tx-initial-native-completed", release_id, native_payload)
        WRAPPER.verify_transaction(self.loaded, "tx-initial-native-completed", release_id)
        with mock.patch.object(WRAPPER, "_deactivate_legacy_runtime"), mock.patch.object(
            WRAPPER, "_daemon_reload",
        ), mock.patch.object(WRAPPER, "_activate_go_runtime"), mock.patch.object(
            WRAPPER.subprocess,
            "run",
            return_value=subprocess.CompletedProcess([], 0, b"", b""),
        ):
            activation = WRAPPER.activate(self.loaded, "tx-initial-native-completed", release_id)
        self.assertTrue(activation["activated"])
        self.assertFalse(Path(self.loaded["previous_link"]).exists())

        with mock.patch.object(WRAPPER, "_deactivate_native_runtime"), mock.patch.object(
            WRAPPER, "_daemon_reload",
        ), mock.patch.object(WRAPPER, "_activate_legacy_runtime") as activate_legacy:
            result = WRAPPER.rollback(self.loaded, "tx-initial-native-completed", release_id)

        self.assertTrue(result["rolled_back"])
        self.assertTrue(result["predecessor_restored"])
        self.assertEqual(result["release_id"], legacy_manifest["release_id"])
        self.assertFalse(Path(self.loaded["current_link"]).exists())
        self.assertFalse(Path(self.loaded["previous_link"]).exists())
        activate_legacy.assert_called_once_with(native_already_stopped=True)

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
        with mock.patch.object(WRAPPER.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, b"", b"")), mock.patch.object(
            WRAPPER, "_verify_durable_drain_state",
        ):
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
        with mock.patch.object(WRAPPER.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, b"", b"")), mock.patch.object(
            WRAPPER, "_verify_durable_drain_state",
        ):
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
