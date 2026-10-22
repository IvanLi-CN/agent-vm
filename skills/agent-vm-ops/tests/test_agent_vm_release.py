"""Publisher transaction tests with a deterministic fake SSH control surface."""
from __future__ import annotations

import importlib.util
import hashlib
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock


ROOT = Path(__file__).resolve().parents[3]
SCRIPTS = ROOT / "skills/agent-vm-ops/scripts"
if str(SCRIPTS) not in os.sys.path:
    os.sys.path.insert(0, str(SCRIPTS))
import agent_vm_release as RELEASE  # noqa: E402


class FakeTransport:
    def __init__(self, *, fail_health_once=False, pending=False, drain_enter=True, unknown_state=False,
                 malformed_health=False, malformed_reconcile=False, malformed_rollback=False):
        self.calls = []
        self.fail_health_once = fail_health_once
        self.pending = pending
        self.drain_enter = drain_enter
        self.unknown_state = unknown_state
        self.malformed_health = malformed_health
        self.malformed_reconcile = malformed_reconcile
        self.malformed_rollback = malformed_rollback
        self.health_checks = 0

    def wrapper(self, host, wrapper_path, action, transaction_id, release_id, payload=None):
        self.calls.append(("wrapper", action, transaction_id, release_id, payload is not None))
        if action == "activate":
            return {"ok": True, "result": {"activated": True, "release_id": release_id}}
        if action == "rollback":
            if self.malformed_rollback:
                return {"ok": True, "result": {"rolled_back": True, "release_id": release_id,
                                                  "previous_release_id": release_id}}
            return {"ok": True, "result": {"rolled_back": True, "release_id": "c" * 40,
                                              "previous_release_id": release_id}}
        if action == "status":
            return {"ok": True, "result": {"current_release_id": "c" * 40,
                                              "previous_release_id": release_id}}
        if action == "stage":
            return {"ok": True, "result": {"staged": True}}
        if action == "verify":
            return {"ok": True, "result": {"verified": True}}
        return {"ok": True, "result": {action: True}}

    def control(self, host, remote_control, request):
        self.calls.append(("control", request["action"], request.get("operation_id")))
        action = request["action"]
        if action == "admin-metrics":
            result = {
                "up": True,
                "draining": True,
                "pending_operations": [{"operation_id": "pending"}] if self.pending else [],
                "task_states": {"unknown": 1} if self.unknown_state else {"active": 1},
            }
            if self.malformed_health:
                result["up"] = "true"
            return {"ok": True, "result": result}
        if action == "doctor":
            self.health_checks += 1
            ready = not self.fail_health_once or self.health_checks > 1
            result = {"ready": ready, "issues": [] if ready else ["injected"]}
            if self.malformed_health:
                result["ready"] = "true"
            return {"ok": True, "result": result}
        if action == "admin-reconcile":
            if self.malformed_reconcile:
                return {"ok": True, "result": {"dry_run": True, "actions": [{"action": "inspect"}]}}
            return {"ok": True, "result": {"dry_run": True, "actions": []}}
        return {"ok": True, "result": {"draining": self.drain_enter if action == "drain-enter" else action != "drain-exit"}}


class ReleasePublisherTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.bundle = self.root / "bundle.tar"
        RELEASE.build_release(ROOT, self.bundle, "e" * 40)

    _DEFAULT_CLIENT_TARGET = object()

    def publisher(self, transport, *, client_target=_DEFAULT_CLIENT_TARGET, clock=None):
        elapsed = [0.0]
        if client_target is self._DEFAULT_CLIENT_TARGET:
            client_target = self.root / "client" / "agent_vm.py"
        return RELEASE.ReleasePublisher(
            bundle=self.bundle,
            control_host="staging-control",
            wrapper_path="/usr/local/sbin/agent-vm-release-wrapper",
            client_target=client_target,
            transport=transport,
            sleep=lambda seconds: elapsed.__setitem__(0, elapsed[0] + seconds),
            monotonic=clock or (lambda: elapsed[0]),
        )

    def test_publish_is_remote_first_and_exits_drain_after_local_client(self):
        transport = FakeTransport()
        target = self.root / "client" / "agent_vm.py"
        result = self.publisher(transport, client_target=target).publish(timeout=5, poll=1, transaction_id="tx-publish")
        self.assertTrue(result["published"])
        self.assertTrue(target.is_file())
        self.assertTrue((target.parent / "client-release.json").is_file())
        actions = [item[1] for item in transport.calls]
        self.assertEqual(actions[:3], ["stage", "verify", "drain-enter"])
        self.assertLess(actions.index("drain-enter"), actions.index("activate"))
        self.assertEqual(actions[-1], "drain-exit")

    def test_publish_requires_local_client_target_before_remote_mutation(self):
        transport = FakeTransport()
        with self.assertRaises(RELEASE.ReleaseError) as failure:
            self.publisher(transport, client_target=None).publish(timeout=5, poll=1, transaction_id="tx-no-client")
        self.assertEqual(failure.exception.code, "local_client_target_missing")
        self.assertEqual(transport.calls, [])

    def test_remote_control_target_is_fixed(self):
        with self.assertRaises(RELEASE.ReleaseError) as failure:
            RELEASE.ReleasePublisher(
                bundle=self.bundle,
                control_host="staging-control",
                wrapper_path="/usr/local/sbin/agent-vm-release-wrapper",
                remote_control="/tmp/other-control.py",
                client_target=self.root / "client" / "agent_vm.py",
            )
        self.assertEqual(failure.exception.code, "remote_control_invalid")
        with self.assertRaises(RELEASE.ReleaseError) as wrapper_failure:
            RELEASE.ReleasePublisher(
                bundle=self.bundle,
                control_host="staging-control",
                wrapper_path="/tmp/other-wrapper",
                client_target=self.root / "client" / "agent_vm.py",
            )
        self.assertEqual(wrapper_failure.exception.code, "wrapper_path_invalid")

    def test_cli_defaults_to_canonical_wrapper_and_keeps_legacy_option(self):
        common = [
            "publish",
            "--bundle", str(self.bundle),
            "--control-host", "staging-control",
            "--client-target", str(self.root / "client" / "agent_vm.py"),
        ]
        self.assertEqual(
            RELEASE.build_parser().parse_args(common).wrapper,
            RELEASE.CANONICAL_REMOTE_WRAPPER,
        )
        self.assertEqual(
            RELEASE.build_parser().parse_args(common + ["--wrapper", RELEASE.CANONICAL_REMOTE_WRAPPER]).wrapper,
            RELEASE.CANONICAL_REMOTE_WRAPPER,
        )

    def test_ssh_status_wrapper_uses_status_command_without_action_arguments(self):
        transport = RELEASE.SSHTransport()
        with mock.patch.object(transport, "_run", return_value={}) as run:
            transport.wrapper("staging-control", RELEASE.CANONICAL_REMOTE_WRAPPER, "status", "tx", "e" * 40)
        run.assert_called_once_with("staging-control", [RELEASE.CANONICAL_REMOTE_WRAPPER, "status"], None)

    def test_health_rejects_malformed_boolean_values(self):
        transport = FakeTransport(malformed_health=True)
        publisher = self.publisher(transport)
        temporary, _, manifest = RELEASE._open_bundle(self.bundle)
        temporary.cleanup()
        with self.assertRaises(RELEASE.ReleaseError) as failure:
            publisher._health(manifest)
        self.assertEqual(failure.exception.code, "remote_health_failed")

    def test_health_rejects_malformed_reconcile_result(self):
        transport = FakeTransport(malformed_reconcile=True)
        publisher = self.publisher(transport)
        temporary, _, manifest = RELEASE._open_bundle(self.bundle)
        temporary.cleanup()
        with self.assertRaises(RELEASE.ReleaseError) as failure:
            publisher._health(manifest)
        self.assertEqual(failure.exception.code, "remote_reconcile_failed")

    def test_convergence_timeout_does_not_activate_or_exit_drain(self):
        transport = FakeTransport(pending=True)
        publisher = self.publisher(transport)
        with self.assertRaises(RELEASE.ReleaseError) as failure:
            publisher.publish(timeout=2, poll=1, transaction_id="tx-timeout")
        self.assertEqual(failure.exception.code, "convergence_timeout")
        actions = [item[1] for item in transport.calls]
        self.assertNotIn("activate", actions)
        self.assertNotIn("drain-exit", actions)

    def test_activation_requires_confirmed_drain(self):
        transport = FakeTransport(drain_enter=False)
        with self.assertRaises(RELEASE.ReleaseError) as failure:
            self.publisher(transport).publish(timeout=5, poll=1, transaction_id="tx-no-drain")
        self.assertEqual(failure.exception.code, "drain_unverified")
        self.assertNotIn("activate", [item[1] for item in transport.calls])

    def test_convergence_rejects_unknown_task_state(self):
        transport = FakeTransport(unknown_state=True)
        with self.assertRaises(RELEASE.ReleaseError) as failure:
            self.publisher(transport).publish(timeout=5, poll=1, transaction_id="tx-unknown-state")
        self.assertEqual(failure.exception.code, "state_unverified")
        self.assertNotIn("activate", [item[1] for item in transport.calls])

    def test_post_activation_health_failure_attempts_one_rollback(self):
        transport = FakeTransport(fail_health_once=True)
        target = self.root / "client" / "agent_vm.py"
        result = self.publisher(transport, client_target=target).publish(timeout=5, poll=1, transaction_id="tx-rollback")
        self.assertTrue(result["rolled_back"])
        actions = [item[1] for item in transport.calls]
        self.assertIn("rollback", actions)
        self.assertIn("status", actions)
        self.assertEqual(actions[-1], "drain-exit")

    def test_post_activation_health_failure_rejects_unverified_rollback(self):
        transport = FakeTransport(fail_health_once=True, malformed_rollback=True)
        with self.assertRaises(RELEASE.ReleaseError) as failure:
            self.publisher(transport).publish(timeout=5, poll=1, transaction_id="tx-bad-rollback")
        self.assertEqual(failure.exception.code, "rollback_failed")
        self.assertNotIn("drain-exit", [item[1] for item in transport.calls])

    def test_local_client_failure_keeps_remote_release_drained_without_rollback(self):
        transport = FakeTransport()
        publisher = self.publisher(transport, client_target=self.root / "client" / "agent_vm.py")
        publisher._install_local_client = lambda manifest: (_ for _ in ()).throw(RELEASE.ReleaseError("local_client_selfcheck_failed", "injected"))
        result = publisher.publish(timeout=5, poll=1, transaction_id="tx-local-failure")
        self.assertFalse(result["published"])
        self.assertTrue(result["draining"])
        actions = [item[1] for item in transport.calls]
        self.assertNotIn("rollback", actions)
        self.assertNotIn("drain-exit", actions)

    def test_manual_rollback_installs_the_supplied_previous_client_before_drain_exit(self):
        previous_bundle = self.root / "previous.tar"
        RELEASE.build_release(ROOT, previous_bundle, "c" * 40)
        target = self.root / "client" / "agent_vm.py"
        self.publisher(FakeTransport(), client_target=target).publish(timeout=5, poll=1, transaction_id="tx-current")
        transport = FakeTransport()
        result = RELEASE.ReleasePublisher(
            bundle=previous_bundle,
            control_host="staging-control",
            wrapper_path="/usr/local/sbin/agent-vm-release-wrapper",
            client_target=target,
            transport=transport,
        ).rollback(transaction_id="tx-current", release_id="e" * 40)
        self.assertTrue(result["rolled_back"])
        self.assertTrue(result["local_client_updated"])
        sidecar = json.loads((target.parent / "client-release.json").read_text(encoding="utf-8"))
        self.assertEqual(sidecar["release_id"], "c" * 40)
        self.assertEqual([item[1] for item in transport.calls][-1], "drain-exit")

    def test_local_client_recovery_marker_restores_the_previous_pair(self):
        target = self.root / "recover" / "agent_vm.py"
        target.parent.mkdir(parents=True)
        sidecar = target.with_name("client-release.json")
        backup_client = RELEASE._local_backup_path(target)
        backup_sidecar = RELEASE._local_backup_path(sidecar)
        target.write_bytes(b"old-client")
        target.chmod(0o755)
        sidecar.write_bytes(b"old-sidecar")
        sidecar.chmod(0o644)
        old_client_digest = RELEASE.sha256_file(target)
        old_sidecar_digest = RELEASE.sha256_file(sidecar)
        os.replace(target, backup_client)
        os.replace(sidecar, backup_sidecar)
        target.write_bytes(b"new-client")
        target.chmod(0o755)
        staged_dir = target.parent / "agent-vm-client-crash"
        staged_dir.mkdir()
        record = {
            "format": 1,
            "phase": "installing",
            "client_path": str(target),
            "sidecar_path": str(sidecar),
            "backup_client": str(backup_client),
            "backup_sidecar": str(backup_sidecar),
            "staged_dir": str(staged_dir),
            "previous": {
                "client": {"exists": True, "sha256": old_client_digest, "mode": 0o755},
                "sidecar": {"exists": True, "sha256": old_sidecar_digest, "mode": 0o644},
            },
            "target": {
                "client": {"exists": True, "sha256": hashlib.sha256(b"new-client").hexdigest(), "mode": 0o755},
                "sidecar": {"exists": True, "sha256": hashlib.sha256(b"new-sidecar").hexdigest(), "mode": 0o644},
            },
            "installed": {
                "client": {"exists": True, "sha256": hashlib.sha256(b"new-client").hexdigest(), "mode": 0o755},
                "sidecar": {"exists": False},
            },
        }
        RELEASE._write_json(target.parent / RELEASE.LOCAL_INSTALL_RECORD_NAME, record, 0o600)
        RELEASE._recover_local_client_install(target, sidecar)
        self.assertEqual(target.read_bytes(), b"old-client")
        self.assertEqual(sidecar.read_bytes(), b"old-sidecar")
        self.assertFalse((target.parent / RELEASE.LOCAL_INSTALL_RECORD_NAME).exists())
        self.assertFalse(staged_dir.exists())

    def test_preexisting_local_backup_is_rejected_and_preserved(self):
        target = self.root / "preexisting-backup" / "agent_vm.py"
        target.parent.mkdir(parents=True)
        sidecar = target.with_name("client-release.json")
        target.write_bytes(b"old-client")
        target.chmod(0o755)
        backup = RELEASE._local_backup_path(target)
        backup.write_bytes(b"operator-backup")
        backup.chmod(0o755)
        staged_dir = target.parent / "agent-vm-client-preexisting"
        staged_dir.mkdir()
        record = {
            "format": 1,
            "phase": "installing",
            "client_path": str(target),
            "sidecar_path": str(sidecar),
            "backup_client": str(backup),
            "backup_sidecar": str(RELEASE._local_backup_path(sidecar)),
            "staged_dir": str(staged_dir),
            "previous": {
                "client": {"exists": True, "sha256": RELEASE.sha256_file(target), "mode": 0o755},
                "sidecar": {"exists": False},
            },
            "target": {
                "client": {"exists": True, "sha256": "1" * 64, "mode": 0o755},
                "sidecar": {"exists": True, "sha256": "2" * 64, "mode": 0o644},
            },
            "installed": {"client": {"exists": False}, "sidecar": {"exists": False}},
        }
        RELEASE._write_json(target.parent / RELEASE.LOCAL_INSTALL_RECORD_NAME, record, 0o600)
        with self.assertRaises(RELEASE.ReleaseError) as failure:
            RELEASE._recover_local_client_install(target, sidecar)
        self.assertEqual(failure.exception.code, "local_client_recovery_failed")
        self.assertEqual(backup.read_bytes(), b"operator-backup")
        self.assertTrue((target.parent / RELEASE.LOCAL_INSTALL_RECORD_NAME).exists())

    def test_external_local_target_replacement_is_preserved_during_recovery(self):
        target = self.root / "external-replacement" / "agent_vm.py"
        target.parent.mkdir(parents=True)
        sidecar = target.with_name("client-release.json")
        target.write_bytes(b"old-client")
        target.chmod(0o755)
        old_digest = RELEASE.sha256_file(target)
        backup = RELEASE._local_backup_path(target)
        os.replace(target, backup)
        target.write_bytes(b"external-replacement")
        target.chmod(0o755)
        staged_dir = target.parent / "agent-vm-client-external"
        staged_dir.mkdir()
        record = {
            "format": 1,
            "phase": "installing",
            "client_path": str(target),
            "sidecar_path": str(sidecar),
            "backup_client": str(backup),
            "backup_sidecar": str(RELEASE._local_backup_path(sidecar)),
            "staged_dir": str(staged_dir),
            "previous": {
                "client": {"exists": True, "sha256": old_digest, "mode": 0o755},
                "sidecar": {"exists": False},
            },
            "target": {
                "client": {"exists": True, "sha256": "1" * 64, "mode": 0o755},
                "sidecar": {"exists": True, "sha256": "2" * 64, "mode": 0o644},
            },
            "installed": {
                "client": {"exists": True, "sha256": "1" * 64, "mode": 0o755},
                "sidecar": {"exists": False},
            },
        }
        RELEASE._write_json(target.parent / RELEASE.LOCAL_INSTALL_RECORD_NAME, record, 0o600)
        with self.assertRaises(RELEASE.ReleaseError) as failure:
            RELEASE._recover_local_client_install(target, sidecar)
        self.assertEqual(failure.exception.code, "local_client_recovery_failed")
        self.assertEqual(target.read_bytes(), b"external-replacement")
        self.assertEqual(backup.read_bytes(), b"old-client")

    def test_completed_local_client_marker_is_verified_before_cleanup(self):
        target = self.root / "complete" / "agent_vm.py"
        target.parent.mkdir(parents=True)
        sidecar = target.with_name("client-release.json")
        target.write_bytes(b"installed-client")
        target.chmod(0o755)
        sidecar.write_bytes(b"installed-sidecar")
        sidecar.chmod(0o644)
        staged_dir = target.parent / "agent-vm-client-complete"
        staged_dir.mkdir()
        record = {
            "format": 1,
            "phase": "complete",
            "client_path": str(target),
            "sidecar_path": str(sidecar),
            "backup_client": str(RELEASE._local_backup_path(target)),
            "backup_sidecar": str(RELEASE._local_backup_path(sidecar)),
            "staged_dir": str(staged_dir),
        }
        RELEASE._write_json(target.parent / RELEASE.LOCAL_INSTALL_RECORD_NAME, record, 0o600)
        with self.assertRaises(RELEASE.ReleaseError) as failure:
            RELEASE._recover_local_client_install(target, sidecar)
        self.assertEqual(failure.exception.code, "local_client_recovery_failed")
        self.assertTrue((target.parent / RELEASE.LOCAL_INSTALL_RECORD_NAME).exists())
        self.assertTrue(staged_dir.exists())

    def test_corrupt_local_backup_is_rejected_and_retained(self):
        target = self.root / "corrupt" / "agent_vm.py"
        target.parent.mkdir(parents=True)
        sidecar = target.with_name("client-release.json")
        target.write_bytes(b"old-client")
        target.chmod(0o755)
        sidecar.write_bytes(b"old-sidecar")
        sidecar.chmod(0o644)
        backup_client = RELEASE._local_backup_path(target)
        backup_sidecar = RELEASE._local_backup_path(sidecar)
        os.replace(target, backup_client)
        os.replace(sidecar, backup_sidecar)
        backup_client.write_bytes(b"tampered")
        staged_dir = target.parent / "agent-vm-client-corrupt"
        staged_dir.mkdir()
        record = {
            "format": 1,
            "phase": "installing",
            "client_path": str(target),
            "sidecar_path": str(sidecar),
            "backup_client": str(backup_client),
            "backup_sidecar": str(backup_sidecar),
            "staged_dir": str(staged_dir),
            "previous": {
                "client": {"exists": True, "sha256": RELEASE.sha256_file(target) if target.exists() else "0" * 64, "mode": 0o755},
                "sidecar": {"exists": True, "sha256": RELEASE.sha256_file(backup_sidecar), "mode": 0o644},
            },
            "target": {
                "client": {"exists": True, "sha256": "1" * 64, "mode": 0o755},
                "sidecar": {"exists": True, "sha256": "2" * 64, "mode": 0o644},
            },
            "installed": {"client": {"exists": False}, "sidecar": {"exists": False}},
        }
        RELEASE._write_json(target.parent / RELEASE.LOCAL_INSTALL_RECORD_NAME, record, 0o600)
        with self.assertRaises(RELEASE.ReleaseError) as failure:
            RELEASE._recover_local_client_install(target, sidecar)
        self.assertEqual(failure.exception.code, "local_client_recovery_failed")
        self.assertTrue(backup_client.exists())
        self.assertTrue((target.parent / RELEASE.LOCAL_INSTALL_RECORD_NAME).exists())

    def test_unexpected_local_staging_entries_are_not_recursively_deleted(self):
        target = self.root / "staging-boundary" / "agent_vm.py"
        target.parent.mkdir(parents=True)
        sidecar = target.with_name("client-release.json")
        staged_dir = target.parent / "agent-vm-client-unrelated"
        staged_dir.mkdir()
        protected = staged_dir / "unrelated.txt"
        protected.write_text("keep", encoding="utf-8")
        record = {
            "format": 1,
            "phase": "installing",
            "client_path": str(target),
            "sidecar_path": str(sidecar),
            "backup_client": str(RELEASE._local_backup_path(target)),
            "backup_sidecar": str(RELEASE._local_backup_path(sidecar)),
            "staged_dir": str(staged_dir),
            "previous": {"client": {"exists": False}, "sidecar": {"exists": False}},
            "target": {
                "client": {"exists": True, "sha256": "1" * 64, "mode": 0o755},
                "sidecar": {"exists": True, "sha256": "2" * 64, "mode": 0o644},
            },
            "installed": {"client": {"exists": False}, "sidecar": {"exists": False}},
        }
        RELEASE._write_json(target.parent / RELEASE.LOCAL_INSTALL_RECORD_NAME, record, 0o600)
        with self.assertRaises(RELEASE.ReleaseError) as failure:
            RELEASE._recover_local_client_install(target, sidecar)
        self.assertEqual(failure.exception.code, "local_client_recovery_failed")
        self.assertTrue(protected.exists())
        self.assertTrue(staged_dir.exists())


if __name__ == "__main__":
    unittest.main()
