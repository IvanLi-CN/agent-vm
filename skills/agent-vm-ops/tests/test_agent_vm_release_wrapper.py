"""Permission and failure-state tests for the fixed release wrapper."""
from __future__ import annotations

import importlib.util
import io
import hashlib
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
        self.drain_patch = mock.patch.object(WRAPPER, "_ensure_drained", return_value=None)
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

    def test_drain_verification_uses_fixed_go_request_after_cutover(self):
        self.drain_patch.stop()
        sidecar = Path(self.loaded["control_path"]).with_name("client-release.json")
        sidecar.parent.mkdir(parents=True, exist_ok=True)
        sidecar.write_text(json.dumps({"format": 2, "client_view": {"transport": "go-request"}}), encoding="utf-8")
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

    def test_clean_activation_accepts_stopped_runtime_without_legacy_drain(self):
        self.drain_patch.stop()
        with mock.patch.object(
            WRAPPER.subprocess,
            "run",
            return_value=subprocess.CompletedProcess([], 3, b"", b""),
        ) as run:
            WRAPPER._ensure_drained(self.loaded, allow_initial=True)
        self.assertEqual(run.call_count, len((
            "agent-vm-native.service",
            "agent-vm-native@blue.service",
            "agent-vm-native@green.service",
            "agent-vm-deployd.service",
        )))
        self.assertTrue(all(call.args[0][:3] == ["systemctl", "is-active", "--quiet"] for call in run.call_args_list))

    def test_clean_activation_rejects_a_running_runtime_without_legacy_drain(self):
        self.drain_patch.stop()
        with mock.patch.object(
            WRAPPER.subprocess,
            "run",
            return_value=subprocess.CompletedProcess([], 0, b"", b""),
        ):
            with self.assertRaises(WRAPPER.WrapperError) as failure:
                WRAPPER._ensure_drained(self.loaded, allow_initial=True)
        self.assertEqual(failure.exception.code, "drain_required")

    def test_drain_verification_uses_active_slot_socket_after_cutover(self):
        self.drain_patch.stop()
        sidecar = Path(self.loaded["control_path"]).with_name("client-release.json")
        sidecar.parent.mkdir(parents=True, exist_ok=True)
        sidecar.write_text(json.dumps({"format": 4, "client_view": {"transport": "go-request"}}), encoding="utf-8")
        response = json.dumps({"ok": True, "result": {"draining": True}}).encode()
        with mock.patch.object(WRAPPER.os.path, "lexists", return_value=True), mock.patch.object(
            WRAPPER.subprocess,
            "run",
            return_value=subprocess.CompletedProcess([], 0, response, b""),
        ) as run:
            WRAPPER._ensure_drained(self.loaded)
        self.assertEqual(
            run.call_args_list[0].args[0],
            [str(WRAPPER.GO_CONTROL_PATH), "request", "--socket", str(WRAPPER.GO_ACTIVE_SOCKET), "--timeout", "30s"],
        )

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

    def test_drain_verification_allows_blocked_drain_enter_without_business_db(self):
        self.drain_patch.stop()
        pending = {"action": "drain-enter", "operation_id": "blocked-drain", "session_id": "release-handoff", "generation": 0, "created_at": "2026-11-05T00:00:00Z"}
        responses = [
            json.dumps({"ok": True, "result": {"draining": True}}).encode(),
            json.dumps({"ok": True, "result": {"pending_operations": [pending], "in_flight_operations": 0}}).encode(),
        ]
        with mock.patch.object(WRAPPER, "_read_activation_safe_pending_ids", side_effect=AssertionError("business DB must not be opened")), mock.patch.object(WRAPPER.subprocess, "run", side_effect=[subprocess.CompletedProcess([], 0, response, b"") for response in responses]):
            WRAPPER._ensure_drained(self.loaded)

    def test_drain_verification_rejects_unverified_drain_intents(self):
        self.drain_patch.stop()
        original = {"action": "drain-enter", "operation_id": "blocked-drain", "session_id": "release-handoff", "generation": 0}
        for changes, inflight in [({"worker": {}}, 0), ({"action": "drain-exit"}, 0), ({"session_id": "maintenance"}, 0), ({"generation": True}, 0), ({"management": True}, 0), ({}, 1), ({}, None)]:
            with self.subTest(changes=changes, inflight=inflight):
                pending = {**original, **changes}
                responses = [json.dumps({"ok": True, "result": {"draining": True}}).encode(), json.dumps({"ok": True, "result": {"pending_operations": [pending], "in_flight_operations": inflight}}).encode()]
                with mock.patch.object(WRAPPER, "_read_activation_safe_pending_ids", return_value=set()), mock.patch.object(WRAPPER.subprocess, "run", side_effect=[subprocess.CompletedProcess([], 0, response, b"") for response in responses]):
                    with self.assertRaises(WRAPPER.WrapperError) as failure:
                        WRAPPER._ensure_drained(self.loaded)
                self.assertEqual(failure.exception.code, "drain_unverified")

    def test_drain_verification_allows_only_queued_prestart_recovery(self):
        self.drain_patch.stop()
        database_path = self.root / "pending.sqlite3"
        queued_request = {
            "protocol": 4,
            "action": "port-add",
            "operation_id": "queued-port",
            "session_id": "session-1",
            "generation": 3,
            "name": "http",
            "vm_port": 8080,
        }
        queued_port = {
            "outcome": "pending",
            "worker": {"state": "exited", "start_attempted": False},
            "_request": queued_request,
            "_request_digest": hashlib.sha256(json.dumps(queued_request, sort_keys=True).encode()).hexdigest(),
        }
        queued_drain_exit = {
            "outcome": "pending",
            "_request": {"session_id": "old-release-owner"},
        }
        with sqlite3.connect(database_path) as database:
            database.executescript("CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL); CREATE TABLE operations (operation_id TEXT, action TEXT, session_id TEXT, generation INTEGER, response_json TEXT);")
            database.execute("INSERT INTO meta(key, value) VALUES ('drain_owner', 'release-current-owner')")
            database.execute("INSERT INTO operations VALUES (?, ?, ?, ?, ?)", ("queued-port", "port-add", "session-1", 3, json.dumps(queued_port)))
            database.execute("INSERT INTO operations VALUES (?, ?, ?, ?, ?)", ("queued-exit", "drain-exit", None, None, json.dumps(queued_drain_exit)))
            database.commit()
        responses = [
            json.dumps({"ok": True, "result": {"draining": True}}).encode(),
            json.dumps({"ok": True, "result": {"pending_operations": [{"operation_id": "queued-port"}, {"operation_id": "queued-exit"}]}}).encode(),
        ]
        with mock.patch.object(
            WRAPPER,
            "STATE_DB_PATH",
            database_path,
        ), mock.patch.object(
            WRAPPER.subprocess,
            "run",
            side_effect=[subprocess.CompletedProcess([], 0, response, b"") for response in responses],
        ):
            WRAPPER._ensure_drained(self.loaded)

    def test_drain_verification_rejects_malformed_queued_port(self):
        self.drain_patch.stop()
        database_path = self.root / "malformed-pending.sqlite3"
        queued_port = {
            "outcome": "pending",
            "worker": {"state": "exited", "start_attempted": False},
        }
        with sqlite3.connect(database_path) as database:
            database.executescript("CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL); CREATE TABLE operations (operation_id TEXT, action TEXT, session_id TEXT, generation INTEGER, response_json TEXT);")
            database.execute("INSERT INTO meta(key, value) VALUES ('drain_owner', 'release-current-owner')")
            database.execute("INSERT INTO operations VALUES (?, ?, ?, ?, ?)", ("malformed-port", "port-add", "session-1", 3, json.dumps(queued_port)))
            database.commit()
        responses = [
            json.dumps({"ok": True, "result": {"draining": True}}).encode(),
            json.dumps({"ok": True, "result": {"pending_operations": [{"operation_id": "malformed-port"}]}}).encode(),
        ]
        with mock.patch.object(
            WRAPPER,
            "STATE_DB_PATH",
            database_path,
        ), mock.patch.object(
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
            WRAPPER._activate_product_runtime(self.loaded, {"format": MANIFEST.LEGACY_PRODUCT_MANIFEST_FORMAT})
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

    def test_go_blue_green_runtime_requires_a_valid_active_slot_pointer(self):
        with self.assertRaises(WRAPPER.WrapperError) as failure:
            WRAPPER._activate_go_runtime(self.loaded, deployd_enabled=True)
        self.assertEqual(failure.exception.code, "activation_unverified")

    def test_initial_go_runtime_migrates_before_creating_active_or_starting_slots(self):
        runtime_root = WRAPPER._go_runtime_root(self.loaded)
        for slot in WRAPPER.GO_SLOT_NAMES:
            (runtime_root / "slots" / slot).mkdir(parents=True)
        legacy_state = runtime_root / "state.sqlite3"
        legacy_state.write_bytes(b"legacy")

        def run(command, *args, **kwargs):
            if command and command[0] == str(WRAPPER.GO_DEPLOY_PATH):
                if command[1] == "migrate":
                    marker_path = WRAPPER._migration_marker_path(self.loaded)
                    marker_path.parent.mkdir(parents=True, exist_ok=True)
                    marker_path.write_text(json.dumps({
                        "format": 2,
                        "transaction_id": "migration-1",
                        "business_authority": "rqlite",
                        "source_sha256": "a" * 64,
                        "source_size": 1,
                        "state_epoch": "epoch-1",
                        "target_sha256": "b" * 64,
                        "retention_until": "2026-11-11T00:00:00Z",
                    }), encoding="utf-8")
                    marker_path.chmod(0o600)
                    return subprocess.CompletedProcess(command, 0, b"", b"")
                if command[1] == "transaction":
                    return subprocess.CompletedProcess(command, 0, json.dumps({
                        "value": {"transaction": {
                            "transaction_id": "migration-1",
                            "target_slot": "state",
                            "phase": "finalize",
                            "state": "finalized",
                            "recovery_required": False,
                        }},
                    }).encode(), b"")
            if command and command[0] == str(WRAPPER.GO_CONTROL_PATH):
                return subprocess.CompletedProcess(command, 0, json.dumps({
                    "ok": True,
                    "result": {"draining": False},
                }).encode(), b"")
            return subprocess.CompletedProcess(command, 0, b"", b"")

        with mock.patch.object(WRAPPER, "STATE_DB_PATH", legacy_state), mock.patch.object(
            WRAPPER.subprocess,
            "run",
            side_effect=run,
        ) as run:
            WRAPPER._activate_go_runtime(self.loaded, deployd_enabled=True)
        commands = [call.args[0] for call in run.call_args_list]
        migration_index = next(index for index, command in enumerate(commands) if command[0] == str(WRAPPER.GO_DEPLOY_PATH) and command[1] == "migrate")
        slot_index = next(index for index, command in enumerate(commands) if command == ["systemctl", "restart", "agent-vm-native@blue.service"])
        self.assertLess(migration_index, slot_index)
        self.assertIn(["systemctl", "restart", "agent-vm-native.service"], commands)
        self.assertTrue((runtime_root / "active").is_symlink())
        drain_exit = next(call for call in run.call_args_list if call.args[0][0] == str(WRAPPER.GO_CONTROL_PATH))
        self.assertEqual(json.loads(drain_exit.kwargs["input"])["session_id"], "release-agent-vm-migration-migration-1")

    def test_clean_go_runtime_allows_bootstrap_without_legacy_sqlite(self):
        runtime_root = WRAPPER._go_runtime_root(self.loaded)
        for slot in WRAPPER.GO_SLOT_NAMES:
            (runtime_root / "slots" / slot).mkdir(parents=True)
        with mock.patch.object(WRAPPER, "STATE_DB_PATH", runtime_root / "state.sqlite3"):
            self.assertTrue(WRAPPER._prepare_initial_go_runtime_layout(self.loaded))

    def test_activation_snapshot_includes_deployd_control_binaries(self):
        targets = WRAPPER._activation_targets(self.loaded)
        bundle_root = Path(self.loaded["control_path"]).parent
        self.assertIn(bundle_root / "bin" / "agent-vm-deploy", targets)
        self.assertIn(bundle_root / "bin" / "agent-vm-deployd", targets)

    def test_go_runtime_waits_for_deployd_socket_before_migration(self):
        runtime_root = WRAPPER._go_runtime_root(self.loaded)
        for slot in WRAPPER.GO_SLOT_NAMES:
            (runtime_root / "slots" / slot).mkdir(parents=True)
        legacy_state = runtime_root / "state.sqlite3"
        legacy_state.write_bytes(b"legacy")

        def run(command, *args, **kwargs):
            if command and command[0] == str(WRAPPER.GO_DEPLOY_PATH):
                if command[1] == "status":
                    return subprocess.CompletedProcess(command, 0, b"{}", b"")
                if command[1] == "migrate":
                    marker_path = WRAPPER._migration_marker_path(self.loaded)
                    marker_path.parent.mkdir(parents=True, exist_ok=True)
                    marker_path.write_text(json.dumps({
                        "format": 2,
                        "transaction_id": "migration-1",
                        "business_authority": "rqlite",
                        "source_sha256": "a" * 64,
                        "source_size": 1,
                        "state_epoch": "epoch-1",
                        "target_sha256": "b" * 64,
                        "retention_until": "2026-11-11T00:00:00Z",
                    }), encoding="utf-8")
                    marker_path.chmod(0o600)
                    return subprocess.CompletedProcess(command, 0, b"", b"")
                if command[1] == "transaction":
                    return subprocess.CompletedProcess(command, 0, json.dumps({
                        "value": {"transaction": {
                            "transaction_id": "migration-1",
                            "target_slot": "state",
                            "phase": "finalize",
                            "state": "finalized",
                            "recovery_required": False,
                        }},
                    }).encode(), b"")
            if command and command[0] == str(WRAPPER.GO_CONTROL_PATH):
                return subprocess.CompletedProcess(command, 0, json.dumps({
                    "ok": True,
                    "result": {"draining": False},
                }).encode(), b"")
            return subprocess.CompletedProcess(command, 0, b"", b"")

        with mock.patch.object(WRAPPER, "STATE_DB_PATH", legacy_state), mock.patch.object(
            WRAPPER.subprocess,
            "run",
            side_effect=run,
        ) as run:
            WRAPPER._activate_go_runtime(self.loaded, deployd_enabled=True)
        commands = [call.args[0] for call in run.call_args_list]
        ready_index = next(index for index, command in enumerate(commands) if command[0] == str(WRAPPER.GO_DEPLOY_PATH) and command[1] == "status")
        migration_index = next(index for index, command in enumerate(commands) if command[0] == str(WRAPPER.GO_DEPLOY_PATH) and command[1] == "migrate")
        self.assertLess(ready_index, migration_index)

    def test_migration_marker_without_finalized_transaction_keeps_runtime_fenced(self):
        marker_path = WRAPPER._migration_marker_path(self.loaded)
        marker_path.parent.mkdir(parents=True, exist_ok=True)
        marker_path.write_text(json.dumps({
            "format": 2,
            "transaction_id": "migration-1",
            "business_authority": "rqlite",
            "source_sha256": "a" * 64,
            "source_size": 1,
            "state_epoch": "epoch-1",
            "target_sha256": "b" * 64,
            "retention_until": "2026-11-11T00:00:00Z",
        }), encoding="utf-8")
        marker_path.chmod(0o600)

        def run(command, *args, **kwargs):
            if command and command[0] == str(WRAPPER.GO_DEPLOY_PATH):
                if command[1] == "migrate":
                    return subprocess.CompletedProcess(command, 0, b"{}", b"")
                if command[1] == "transaction":
                    return subprocess.CompletedProcess(command, 0, json.dumps({
                        "value": {"transaction": {
                            "transaction_id": "migration-1",
                            "target_slot": "state",
                            "phase": "migration_finalize",
                            "state": "needs_recovery",
                            "recovery_required": True,
                        }},
                    }).encode(), b"")
            return subprocess.CompletedProcess(command, 0, b"", b"")

        with mock.patch.object(WRAPPER.subprocess, "run", side_effect=run):
            with self.assertRaises(WRAPPER.WrapperError) as failure:
                WRAPPER._run_initial_state_migration(self.loaded)
        self.assertEqual(failure.exception.code, "activation_unverified")

    def test_migration_completion_verification_retries_a_delayed_finalized_transaction(self):
        marker_path = WRAPPER._migration_marker_path(self.loaded)
        marker_path.parent.mkdir(parents=True, exist_ok=True)
        marker_path.write_text(json.dumps({
            "format": 2,
            "transaction_id": "migration-1",
            "business_authority": "rqlite",
            "source_sha256": "a" * 64,
            "source_size": 1,
            "state_epoch": "epoch-1",
            "target_sha256": "b" * 64,
            "retention_until": "2026-11-11T00:00:00Z",
        }), encoding="utf-8")
        marker_path.chmod(0o600)
        with mock.patch.object(
            WRAPPER,
            "_migration_transaction_is_finalized",
            side_effect=[False, True],
        ) as finalized, mock.patch.object(WRAPPER.time, "sleep") as sleep:
            WRAPPER._verify_migration_completion(self.loaded)
        self.assertEqual(finalized.call_count, 2)
        sleep.assert_called_once_with(WRAPPER.MIGRATION_VERIFY_POLL_SECONDS)

    def test_migration_completion_verification_accepts_the_success_response(self):
        marker_path = WRAPPER._migration_marker_path(self.loaded)
        marker_path.parent.mkdir(parents=True, exist_ok=True)
        marker_path.write_text(json.dumps({
            "format": 2,
            "transaction_id": "migration-1",
            "business_authority": "rqlite",
            "source_sha256": "a" * 64,
            "source_size": 1,
            "state_epoch": "epoch-1",
            "target_sha256": "b" * 64,
            "retention_until": "2026-11-11T00:00:00Z",
        }), encoding="utf-8")
        marker_path.chmod(0o600)
        response = json.dumps({
            "ok": True,
            "value": {
                "transaction_id": "migration-1",
                "target_slot": "state",
                "phase": "finalize",
                "state": "finalized",
                "recovery_required": False,
            },
        }).encode()
        with mock.patch.object(WRAPPER, "_run_deployd_cli") as run_deployd:
            WRAPPER._verify_migration_completion(self.loaded, response)
        run_deployd.assert_not_called()

    def test_migration_preflight_failure_without_transaction_is_not_recovery(self):
        def run(command, *args, **kwargs):
            if command and command[0] == str(WRAPPER.GO_DEPLOY_PATH):
                if command[1] == "migrate":
                    return subprocess.CompletedProcess(command, 1, b"", b"preflight failed")
                if command[1] == "status":
                    return subprocess.CompletedProcess(command, 0, json.dumps({"value": {"transactions": []}}).encode(), b"")
            return subprocess.CompletedProcess(command, 0, b"", b"")

        with mock.patch.object(WRAPPER.subprocess, "run", side_effect=run):
            with self.assertRaises(WRAPPER.WrapperError) as failure:
                WRAPPER._run_initial_state_migration(self.loaded)
        self.assertEqual(failure.exception.code, "activation_failed")

    def test_finalized_cutover_upgrade_does_not_start_another_migration(self):
        marker_path = WRAPPER._migration_marker_path(self.loaded)
        marker_path.parent.mkdir(parents=True, exist_ok=True)
        marker_path.write_text(json.dumps({
            "format": 2, "transaction_id": "migration-existing",
            "business_authority": "rqlite", "source_sha256": "a" * 64,
            "source_size": 1, "state_epoch": "epoch-existing",
            "target_sha256": "b" * 64,
            "retention_until": "2026-11-11T00:00:00Z",
        }), encoding="utf-8")
        marker_path.chmod(0o600)
        response = json.dumps({"value": {"transaction": {
            "transaction_id": "migration-existing", "target_slot": "state",
            "phase": "finalize", "state": "finalized", "recovery_required": False,
        }}}).encode()
        with mock.patch.object(WRAPPER, "_run_deployd_cli", return_value=subprocess.CompletedProcess([], 0, response, b"")) as run:
            WRAPPER._run_initial_state_migration(self.loaded)
        run.assert_called_once_with("transaction", "--transaction", "migration-existing", timeout_seconds=30)

    def test_migration_recovery_failure_keeps_the_migration_fence_code(self):
        def run(command, *args, **kwargs):
            if command and command[0] == str(WRAPPER.GO_DEPLOY_PATH):
                if command[1] == "migrate":
                    return subprocess.CompletedProcess(command, 1, b"", b"migration failed")
                if command[1] == "status":
                    return subprocess.CompletedProcess(command, 0, json.dumps({"value": {"transactions": [{
                        "transaction_id": "migration-1",
                        "target_slot": "state",
                        "state": "needs_recovery",
                    }]}}).encode(), b"")
                if command[1] == "recover":
                    return subprocess.CompletedProcess(command, 1, b"", b"recovery failed")
            return subprocess.CompletedProcess(command, 0, b"", b"")

        with mock.patch.object(WRAPPER.subprocess, "run", side_effect=run):
            with self.assertRaises(WRAPPER.WrapperError) as failure:
                WRAPPER._run_initial_state_migration(self.loaded)
        self.assertEqual(failure.exception.code, "migration_recovery_required")
        self.assertEqual(failure.exception.detail["transaction_id"], "migration-1")

    def test_completed_migration_can_bootstrap_without_legacy_sqlite(self):
        runtime_root = WRAPPER._go_runtime_root(self.loaded)
        for slot in WRAPPER.GO_SLOT_NAMES:
            (runtime_root / "slots" / slot).mkdir(parents=True)
        marker_path = WRAPPER._migration_marker_path(self.loaded)
        marker_path.parent.mkdir(parents=True, exist_ok=True)
        marker_path.write_text(json.dumps({
            "format": 2,
            "transaction_id": "migration-1",
            "business_authority": "rqlite",
            "source_sha256": "a" * 64,
            "source_size": 1,
            "state_epoch": "epoch-1",
            "target_sha256": "b" * 64,
            "retention_until": "2026-11-11T00:00:00Z",
        }), encoding="utf-8")
        marker_path.chmod(0o600)
        with mock.patch.object(WRAPPER, "STATE_DB_PATH", runtime_root / "state.sqlite3"):
            self.assertTrue(WRAPPER._prepare_initial_go_runtime_layout(self.loaded))

    def test_migration_failure_stops_legacy_and_never_starts_slots(self):
        runtime_root = WRAPPER._go_runtime_root(self.loaded)
        for slot in WRAPPER.GO_SLOT_NAMES:
            (runtime_root / "slots" / slot).mkdir(parents=True)
        legacy_state = runtime_root / "state.sqlite3"
        legacy_state.write_bytes(b"legacy")
        with mock.patch.object(
            WRAPPER,
            "STATE_DB_PATH",
            legacy_state,
        ), mock.patch.object(
            WRAPPER,
            "_run_initial_state_migration",
            side_effect=WRAPPER.WrapperError("activation_failed", "migration failed"),
        ), mock.patch.object(
            WRAPPER.subprocess,
            "run",
            return_value=subprocess.CompletedProcess([], 0, b"", b""),
        ) as run:
            with self.assertRaises(WRAPPER.WrapperError) as failure:
                WRAPPER._activate_go_runtime(self.loaded, deployd_enabled=True)
        self.assertEqual(failure.exception.code, "activation_failed")
        commands = [call.args[0] for call in run.call_args_list]
        self.assertNotIn(["systemctl", "restart", "agent-vm-native@blue.service"], commands)
        self.assertNotIn(["systemctl", "restart", "agent-vm-native@green.service"], commands)
        self.assertIn(["systemctl", "stop", "agent-vm-native.service"], commands)

    def test_recovery_fence_survives_failure_and_can_be_claimed_by_next_activation(self):
        with WRAPPER._activation_fence(self.loaded, "tx-old", "a" * 40) as lease:
            lease.retain_for_recovery()
        fence = self.loaded["fence_path"]
        self.assertEqual(json.loads(fence.read_text(encoding="utf-8"))["state"], "recovery_required")
        with self.assertRaises(WRAPPER.WrapperError) as failure:
            with WRAPPER._activation_fence(self.loaded, "tx-new", "b" * 40):
                pass
        self.assertEqual(failure.exception.code, "release_activation_in_progress")
        with WRAPPER._activation_fence(self.loaded, "tx-old", "a" * 40):
            self.assertEqual(json.loads(fence.read_text(encoding="utf-8"))["transaction_id"], "tx-old")
        self.assertFalse(fence.exists())

    def test_product_runtime_retire_web_unit_and_keep_resident_control(self):
        runtime_root = WRAPPER._go_runtime_root(self.loaded)
        (runtime_root / "slots" / "blue").mkdir(parents=True)
        (runtime_root / "slots" / "green").mkdir(parents=True)
        (runtime_root / "active").symlink_to(runtime_root / "slots" / "blue")
        with mock.patch.object(WRAPPER, "_run_initial_state_migration"), mock.patch.object(
            WRAPPER.subprocess, "run",
            return_value=subprocess.CompletedProcess([], 0, b"", b""),
        ) as run:
            WRAPPER._activate_product_runtime(self.loaded, {"format": MANIFEST.PRODUCT_MANIFEST_FORMAT})
        commands = [call.args[0] for call in run.call_args_list]
        self.assertIn(["systemctl", "stop", "agent-vm-web.service"], commands)
        self.assertNotIn(["systemctl", "start", "agent-vm-web.service"], commands)
        self.assertIn(["systemctl", "stop", "agent-vm-native.service"], commands)
        self.assertIn(["systemctl", "disable", "agent-vm-native.service"], commands)
        self.assertIn(["systemctl", "restart", "agent-vm-native@blue.service"], commands)
        self.assertIn(["systemctl", "restart", "agent-vm-native@green.service"], commands)
        self.assertIn(["systemctl", "restart", "agent-vm-deployd.service"], commands)

    def test_product_runtime_starts_deployd_for_format4(self):
        runtime_root = WRAPPER._go_runtime_root(self.loaded)
        (runtime_root / "slots" / "blue").mkdir(parents=True)
        (runtime_root / "slots" / "green").mkdir(parents=True)
        (runtime_root / "active").symlink_to(runtime_root / "slots" / "blue")
        with mock.patch.object(WRAPPER, "_run_initial_state_migration"), mock.patch.object(
            WRAPPER.subprocess,
            "run",
            return_value=subprocess.CompletedProcess([], 0, b"", b""),
        ) as run:
            WRAPPER._activate_product_runtime(self.loaded, {"format": MANIFEST.PRODUCT_MANIFEST_FORMAT})
        commands = [call.args[0] for call in run.call_args_list]
        self.assertIn(["systemctl", "enable", "agent-vm-deployd.service"], commands)
        self.assertNotIn(["systemctl", "restart", "agent-vm-native.service"], commands)

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

    def _go_payload(self, release_id, *, product=False):
        extracted = self.root / "go-candidate"
        client_root = ROOT / "skills/agent-vm/scripts"
        ops_root = ROOT / "skills/agent-vm-ops"
        artifacts = []
        for name, relative, role, mode in sorted(MANIFEST.GO_FIXED_ARTIFACTS):
            path = extracted / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            if relative in {"bin/agent-vm-control", "bin/agent-vm-deployd", "bin/agent-vm-deploy"}:
                # Minimal little-endian ELF64 header for the amd64 target.
                path.write_bytes(b"\x7fELF\x02\x01\x01\x00" + b"\x00" * 10 + b"\x3e\x00" + b"\x00" * 42)
            elif relative == "systemd/agent-vm-native.service":
                path.write_bytes((ops_root / "assets/systemd/agent-vm-native.service").read_bytes())
            elif relative == "systemd/agent-vm-native@.service":
                path.write_bytes((ops_root / "assets/systemd/agent-vm-native@.service").read_bytes())
            elif relative == "systemd/agent-vm-deployd.service":
                path.write_bytes((ops_root / "assets/systemd/agent-vm-deployd.service").read_bytes())
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
        if product:
            actions = MANIFEST.default_operations()
            manifest = MANIFEST.validate_product_manifest({
                "format": MANIFEST.PRODUCT_MANIFEST_FORMAT,
                "version": "1.2.1",
                "release_id": release_id,
                "source_revision": release_id,
                "protocol": 4,
                "target": {"os": "linux", "arch": "amd64"},
                "toolchain": {"bun": MANIFEST.PRODUCT_BUN_TOOLCHAIN, "go": MANIFEST.PRODUCT_GO_TOOLCHAIN, "cgo_enabled": False},
                "artifacts": artifacts,
                "actions": actions,
                "client_view": {"version": "1.2.1", "source_revision": release_id, "protocol": 4, "transport": "go-request", "actions": actions},
                "state_compatibility": {"reads": list(MANIFEST.STATE_CAPABILITY_MARKERS), "writes": list(MANIFEST.STATE_CAPABILITY_MARKERS)},
            })
        else:
            manifest = MANIFEST.build_go_manifest(release_id, artifacts)
        (extracted / "release-manifest.json").write_text(json.dumps(manifest, sort_keys=True) + "\n", encoding="utf-8")
        client_artifact = next(item for item in artifacts if item["role"] == "client")
        sidecar = {
            "format": MANIFEST.PRODUCT_MANIFEST_FORMAT if product else 2,
            **({"version": "1.2.1"} if product else {"release_id": release_id}),
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

    def test_format_four_installer_populates_both_slot_runtimes(self):
        release_id = "d" * 40
        payload = self._go_payload(release_id, product=True)
        release_root = self.root / "product-release"
        WRAPPER.safe_extract_archive(payload, release_root)
        manifest = MANIFEST.load_release_manifest(release_root / "release-manifest.json")

        WRAPPER._install_product_release_files(self.loaded, release_root, manifest)

        runtime_root = WRAPPER._go_runtime_root(self.loaded)
        for slot in WRAPPER.GO_SLOT_NAMES:
            slot_root = runtime_root / "slots" / slot
            self.assertEqual(
                (slot_root / "bin" / "agent-vm-control").read_bytes(),
                (release_root / "bin" / "agent-vm-control").read_bytes(),
            )
            self.assertEqual(
                (slot_root / "release-manifest.json").read_bytes(),
                (release_root / "release-manifest.json").read_bytes(),
            )
            self.assertEqual(
                (slot_root / "client-release.json").read_bytes(),
                (release_root / "client-release.json").read_bytes(),
            )
            self.assertEqual(os.readlink(slot_root / "app.sock"), f"/run/agent-vm-native/{slot}/app.sock")
            for item in manifest["artifacts"]:
                relative = Path(*item["path"].split("/"))
                with self.subTest(slot=slot, artifact=item["path"]):
                    self.assertEqual((slot_root / relative).read_bytes(), (release_root / relative).read_bytes())
        bridge = (release_root / "agent_vm_control_bridge.ts").read_bytes()
        self.assertEqual(Path(self.loaded["control_path"]).read_bytes(), bridge)
        self.assertEqual((Path(self.loaded["control_path"]).parent / "agent_vm_control_bridge.ts").read_bytes(), bridge)

    def test_format_two_installs_go_binary_bridge_and_retires_old_units(self):
        release_id = "a" * 40
        payload = self._go_payload(release_id)
        unit_dir = Path(self.loaded["unit_dir"])
        unit_dir.mkdir(parents=True)
        runtime_root = Path(self.loaded["release_root"]).parent
        for slot in WRAPPER.GO_SLOT_NAMES:
            (runtime_root / "slots" / slot).mkdir(parents=True)
        (runtime_root / "active").symlink_to(Path("slots/blue"))
        for name in MANIFEST.GO_RETIRED_UNITS:
            old = unit_dir / name
            old.write_bytes(b"old unit\n")
            old.chmod(0o644)
        WRAPPER.stage(self.loaded, "tx-go", release_id, payload)
        verified = WRAPPER.verify_transaction(self.loaded, "tx-go", release_id)
        self.assertTrue(verified["verified"])
        with mock.patch.object(WRAPPER, "_run_initial_state_migration"), mock.patch.object(
            WRAPPER.subprocess,
            "run",
            return_value=subprocess.CompletedProcess([], 0, b"", b""),
        ) as run:
            activated = WRAPPER.activate(self.loaded, "tx-go", release_id)
        self.assertTrue(activated["activated"])
        commands = [call.args[0] for call in run.call_args_list]
        for slot in WRAPPER.GO_SLOT_NAMES:
            self.assertIn(["systemctl", "restart", f"agent-vm-native@{slot}.service"], commands)
            self.assertIn(["systemctl", "is-active", f"agent-vm-native@{slot}.service"], commands)
        self.assertIn(["systemctl", "restart", "agent-vm-deployd.service"], commands)
        self.assertIn(["systemctl", "is-active", "agent-vm-deployd.service"], commands)
        for slot in WRAPPER.GO_SLOT_NAMES:
            self.assertTrue((runtime_root / "slots" / slot / "bin" / "agent-vm-control").is_file())
            self.assertTrue((runtime_root / "slots" / slot / "release-manifest.json").is_file())
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

    def test_format_four_retained_release_accepts_historical_deployd_unit(self):
        root = self.root / "historical-product"
        unit = root / "systemd/agent-vm-deployd.service"
        unit.parent.mkdir(parents=True)
        current = (ROOT / "skills/agent-vm-ops/assets/systemd/agent-vm-deployd.service").read_bytes()
        historical = current.replace(
            b"After=local-fs.target\n",
            b"After=local-fs.target\nWants=agent-vm-native@blue.service agent-vm-native@green.service\n",
        )
        unit.write_bytes(historical)
        unit.chmod(0o644)
        manifest = {
            "format": MANIFEST.PRODUCT_MANIFEST_FORMAT,
            "artifacts": [{"path": "systemd/agent-vm-deployd.service", "role": "unit"}],
        }
        with self.assertRaises(WRAPPER.WrapperError):
            WRAPPER._verify_unit_contents(root, manifest, allow_historical_content=False)
        WRAPPER._verify_unit_contents(root, manifest, allow_historical_content=True)

    def test_unit_policy_bytes_are_pinned_even_when_manifest_checksum_matches(self):
        payload = self._tampered_unit_payload()
        WRAPPER.stage(self.loaded, "tx-tampered-unit", "b" * 40, payload)
        with self.assertRaises(WRAPPER.WrapperError) as failure:
            WRAPPER.verify_transaction(self.loaded, "tx-tampered-unit", "b" * 40)
        self.assertEqual(failure.exception.code, "unit_unverified")

    def test_worker_recovery_fencing_is_journaled_before_installation(self):
        WRAPPER.stage(self.loaded, "tx-worker-recovery", "b" * 40, self.payload)
        WRAPPER.verify_transaction(self.loaded, "tx-worker-recovery", "b" * 40)
        pending = WorkerRecoveryBootstrapTests.pending()
        record_path = Path(self.loaded["state_root"]) / "tx-worker-recovery.activation.json"
        def fence():
            record = json.loads(record_path.read_text())
            self.assertEqual(record["activation_phase"], "worker-recovery-fencing")
            self.assertEqual(record["worker_recovery_pending"], pending)
            self.assertFalse(Path(self.loaded["control_path"]).exists())
        with mock.patch.object(WRAPPER, "_ensure_drained", return_value=pending), mock.patch.object(WRAPPER, "_fence_destroy_recovery_runtime", side_effect=fence) as fenced, mock.patch.object(WRAPPER.subprocess, "run", return_value=subprocess.CompletedProcess([],0,b"",b"")):
            WRAPPER.activate(self.loaded,"tx-worker-recovery","b" * 40)
        fenced.assert_called_once()
        self.assertEqual(json.loads(record_path.read_text())["worker_recovery_pending"],pending)

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
        self.assertEqual(run.call_args.args[0], ["systemctl", "daemon-reload"])
        self.assertFalse(any("stop" in arg or "disable" in arg for arg in run.call_args.args[0]))

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

    def test_activation_failure_reloads_retained_manifest_with_historical_compatibility(self):
        first = self.root / "historical-first.tar"
        second = self.root / "historical-second.tar"
        RELEASE.build_release(ROOT, first, "c" * 40)
        RELEASE.build_release(ROOT, second, "d" * 40)
        with mock.patch.object(WRAPPER.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, b"", b"")):
            WRAPPER.stage(self.loaded, "tx-historical-first", "c" * 40, first.read_bytes())
            WRAPPER.activate(self.loaded, "tx-historical-first", "c" * 40)
            old_current = Path(self.loaded["current_link"]).resolve()
            WRAPPER.stage(self.loaded, "tx-historical-second", "d" * 40, second.read_bytes())
            WRAPPER.verify_transaction(self.loaded, "tx-historical-second", "d" * 40)
        with mock.patch.object(
            WRAPPER, "_daemon_reload", side_effect=[WRAPPER.WrapperError("activation_failed", "injected"), None],
        ), mock.patch.object(WRAPPER, "load_release_manifest", wraps=WRAPPER.load_release_manifest) as load_manifest:
            with self.assertRaises(WRAPPER.WrapperError):
                WRAPPER.activate(self.loaded, "tx-historical-second", "d" * 40)
        retained_calls = [
            call for call in load_manifest.call_args_list
            if call.args and Path(call.args[0]) == old_current / MANIFEST.MANIFEST_NAME
        ]
        self.assertGreaterEqual(len(retained_calls), 2)
        for call in retained_calls:
            self.assertTrue(call.kwargs.get("allow_historical_web_config_mode"))
            self.assertTrue(call.kwargs.get("allow_historical_state_compatibility"))

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

    def test_rollback_runtime_failure_restores_selected_release_runtime(self):
        first = self.root / "runtime-first.tar"
        second = self.root / "runtime-second.tar"
        RELEASE.build_release(ROOT, first, "a" * 40)
        RELEASE.build_release(ROOT, second, "b" * 40)
        with mock.patch.object(WRAPPER.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, b"", b"")):
            WRAPPER.stage(self.loaded, "tx-runtime-first", "a" * 40, first.read_bytes())
            WRAPPER.activate(self.loaded, "tx-runtime-first", "a" * 40)
            WRAPPER.stage(self.loaded, "tx-runtime-second", "b" * 40, second.read_bytes())
            WRAPPER.activate(self.loaded, "tx-runtime-second", "b" * 40)
            with mock.patch.object(
                WRAPPER,
                "_activate_legacy_runtime",
                side_effect=[WRAPPER.WrapperError("runtime_failed", "injected"), None],
            ) as activate_runtime:
                with self.assertRaises(WRAPPER.WrapperError) as failure:
                    WRAPPER.rollback(self.loaded, "tx-runtime-second", "b" * 40)
        self.assertEqual(failure.exception.code, "runtime_failed")
        self.assertEqual(activate_runtime.call_count, 2)
        self.assertEqual(Path(self.loaded["current_link"]).resolve().name, "b" * 40)
        record = json.loads((Path(self.loaded["state_root"]) / "tx-runtime-second.activation.json").read_text(encoding="utf-8"))
        self.assertEqual(record["rollback_state"], "failed")


class WorkerRecoveryBootstrapTests(unittest.TestCase):
    @staticmethod
    def pending():
        return [{"operation_id": "failed-destroy", "action": "destroy", "session_id": "session", "generation": 2, "worker": {"state": "starting", "start_attempted": True}}]

    def test_installation_exception_requires_recovery_capability_and_keeps_drain(self):
        pending = self.pending()
        responses = [
            {"ok": True, "result": {"draining": True}},
            {"ok": True, "result": {"pending_operations": pending, "in_flight_operations": 0}},
            {"unidentified_destroy_start": 1, "requires_release_drain": True},
        ]
        with mock.patch.object(WRAPPER, "_current_target", return_value=Path("/retained-release")), mock.patch.object(WRAPPER, "_read_activation_safe_pending_ids", side_effect=AssertionError("must not consult legacy state")), mock.patch.object(WRAPPER, "_systemctl") as systemctl, mock.patch.object(WRAPPER.subprocess, "run", side_effect=[subprocess.CompletedProcess([],0,json.dumps(value).encode(),b"") for value in responses]):
            result = WRAPPER._ensure_drained({"require_drained":True,"current_link":"/current","release_root":"/releases","control_path":"/client.ts"}, recovery_candidate_root=Path("/verified-candidate"))
        self.assertEqual(result,pending)
        systemctl.assert_not_called()

    def test_unknown_worker_requires_exact_bounded_destroy_barrier(self):
        metrics = {"in_flight_operations": 0}
        self.assertTrue(WRAPPER._unidentified_destroy_recovery_pending(self.pending(), metrics))
        for change in ("pid", "generation", "action", "attempted", "duplicate", "inflight"):
            with self.subTest(change=change):
                pending, current = self.pending(), dict(metrics)
                if change == "pid": pending[0]["worker"]["pid"] = 123
                elif change == "generation": pending[0]["generation"] = True
                elif change == "action": pending[0]["action"] = "acquire"
                elif change == "attempted": pending[0]["worker"]["start_attempted"] = False
                elif change == "duplicate": pending += self.pending()
                elif change == "inflight": current["in_flight_operations"] = 1
                self.assertFalse(WRAPPER._unidentified_destroy_recovery_pending(pending, current))

    def test_recovery_fences_all_worker_owners_before_checking_incus(self):
        calls = []
        def run(argv, **kwargs):
            calls.append(argv)
            if argv[0] == "incus":
                self.assertEqual(len(calls), 5)
                return subprocess.CompletedProcess(argv, 0, b"[]", b"")
            return subprocess.CompletedProcess(argv, 0, b"MainPID=0\nControlGroup=\n", b"")
        with mock.patch.object(WRAPPER, "_systemctl") as stop, mock.patch.object(WRAPPER, "_verify_stopped_recovery_cgroups") as groups, mock.patch.object(WRAPPER.subprocess, "run", side_effect=run):
            WRAPPER._fence_destroy_recovery_runtime()
            self.assertEqual(groups.call_count, 2)
            self.assertEqual([call.args for call in stop.call_args_list], [("stop", unit) for unit in ("agent-vm-deployd.service", "agent-vm-native@blue.service", "agent-vm-native@green.service", "agent-vm-native.service")])

    def test_recovery_rejects_live_process_or_background_effect(self):
        for mode in ("process", "native", "malformed"):
            with self.subTest(mode=mode):
                def run(argv, **kwargs):
                    if argv[0] == "incus":
                        return subprocess.CompletedProcess(argv, 0, b"[{}]" if mode == "native" else b"null", b"")
                    return subprocess.CompletedProcess(argv, 0, b"MainPID=42\nControlGroup=/still-live\n" if mode == "process" else b"MainPID=0\nControlGroup=\n", b"")
                with mock.patch.object(WRAPPER, "_systemctl"), mock.patch.object(WRAPPER.subprocess, "run", side_effect=run):
                    with self.assertRaises(WRAPPER.WrapperError): WRAPPER._fence_destroy_recovery_runtime()

    def test_stopped_cgroup_proof_rejects_surviving_processes_and_children(self):
        for mode in ("absent", "empty", "process", "child", "symlink", "missing-procs"):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                directory = root / "system.slice/agent-vm-native.service"
                if mode != "absent":
                    directory.mkdir(parents=True)
                    if mode != "missing-procs":
                        (directory / "cgroup.procs").write_text("42\n" if mode == "process" else "")
                    if mode == "child": (directory / "child").mkdir()
                    if mode == "symlink":
                        target = directory.parent / "other"
                        directory.rename(target)
                        directory.symlink_to(target, target_is_directory=True)
                if mode in ("absent", "empty"):
                    WRAPPER._verify_stopped_recovery_cgroups(root)
                else:
                    with self.assertRaises(WRAPPER.WrapperError): WRAPPER._verify_stopped_recovery_cgroups(root)

    def test_old_or_malformed_candidate_cannot_bypass_pending_fence(self):
        for output in (b"old binary", b"{}", b'{"unidentified_destroy_start":true,"requires_release_drain":true}', b'{"unidentified_destroy_start":1,"requires_release_drain":false}'):
            with mock.patch.object(WRAPPER.subprocess, "run", return_value=subprocess.CompletedProcess([],0,output,b"")):
                with self.assertRaises(WRAPPER.WrapperError): WRAPPER._verify_destroy_recovery_candidate(Path("/verified-candidate"))


if __name__ == "__main__":
    unittest.main()
