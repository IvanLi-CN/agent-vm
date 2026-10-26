"""Publisher transaction tests with a deterministic fake SSH control surface."""
from __future__ import annotations

import importlib.util
import hashlib
import json
import os
from pathlib import Path
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


class FakeTransport:
    def __init__(self, *, fail_health_once=False, pending=False, drain_enter=True, unknown_state=False,
                 observation_consistent=True, legacy_metrics=False, malformed_health=False, malformed_reconcile=False, malformed_rollback=False,
                 fail_activation=False, require_legacy_drain_exit=False):
        self.calls = []
        self.fail_health_once = fail_health_once
        self.pending = pending
        self.drain_enter = drain_enter
        self.unknown_state = unknown_state
        self.observation_consistent = observation_consistent
        self.legacy_metrics = legacy_metrics
        self.malformed_health = malformed_health
        self.malformed_reconcile = malformed_reconcile
        self.malformed_rollback = malformed_rollback
        self.fail_activation = fail_activation
        self.require_legacy_drain_exit = require_legacy_drain_exit
        self.control_release_ids = []
        self.health_checks = 0

    def wrapper(self, host, wrapper_path, action, transaction_id, release_id, payload=None):
        self.calls.append(("wrapper", action, transaction_id, release_id, payload is not None))
        if action == "activate":
            if self.fail_activation:
                return {"ok": False, "error": {"code": "activation_failed", "message": "injected"}}
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
        compatibility = request.get("compatibility", {})
        self.control_release_ids.append((action, compatibility.get("release_id")))
        if action == "drain-exit" and self.require_legacy_drain_exit and compatibility.get("release_id") != "0" * 40:
            return {"ok": False, "error": {"code": "compatibility_rejected", "message": "legacy manifest required"}}
        if action == "admin-metrics":
            result = {
                "up": True,
                "draining": True,
                "pending_operations": [{"operation_id": "pending"}] if self.pending else [],
                "task_states": {"unknown": 1} if self.unknown_state else {"active": 1},
            }
            if not self.legacy_metrics:
                result["observation_consistent"] = self.observation_consistent
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
        # Transaction tests isolate the syntax subprocess; dedicated manifest
        # tests exercise its command, failure handling, and real transpilation.
        syntax_patch = mock.patch.object(RELEASE, "verify_typescript_syntax")
        self.typescript_check = syntax_patch.start()
        self.addCleanup(syntax_patch.stop)

    _DEFAULT_CLIENT_TARGET = object()

    def publisher(self, transport, *, client_target=_DEFAULT_CLIENT_TARGET, clock=None):
        elapsed = [0.0]
        if client_target is self._DEFAULT_CLIENT_TARGET:
            client_target = self.root / "client" / "agent_vm.ts"
        return RELEASE.ReleasePublisher(
            bundle=self.bundle,
            control_host="staging-control",
            wrapper_path="/usr/local/sbin/agent-vm-release-wrapper",
            client_target=client_target,
            transport=transport,
            sleep=lambda seconds: elapsed.__setitem__(0, elapsed[0] + seconds),
            monotonic=clock or (lambda: elapsed[0]),
        )

    def go_bundle(self, release_id: str) -> Path:
        extracted = self.root / "go-candidate"
        client_root = ROOT / "skills/agent-vm/scripts"
        ops_root = ROOT / "skills/agent-vm-ops"
        artifacts = []
        for name, relative, role, mode in sorted(MANIFEST.GO_FIXED_ARTIFACTS):
            path = extracted / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            if relative == "bin/agent-vm-control":
                path.write_bytes(b"\x7fELF\x02\x01\x01\x00" + b"\x00" * 10 + b"\x3e\x00" + b"\x00" * 42)
            elif relative == "systemd/agent-vm-native.service":
                path.write_bytes((ops_root / "assets/systemd/agent-vm-native.service").read_bytes())
            elif relative == "agent_vm_control_bridge.ts":
                path.write_bytes((ops_root / "scripts/agent_vm_control_bridge.ts").read_bytes())
            else:
                path.write_bytes((client_root / relative).read_bytes())
            path.chmod(mode)
            artifacts.append({"name": name, "path": relative, "role": role, "sha256": MANIFEST.sha256_file(path), "mode": mode})
        manifest = MANIFEST.build_go_manifest(release_id, artifacts)
        (extracted / "release-manifest.json").write_text(json.dumps(manifest, sort_keys=True) + "\n", encoding="utf-8")
        client_artifact = next(item for item in artifacts if item["role"] == "client")
        (extracted / "client-release.json").write_text(json.dumps({
            "format": 2,
            "release_id": release_id,
            "source_revision": release_id,
            "protocol": 4,
            "client_artifact": client_artifact,
            "client_dependencies": [item for item in artifacts if item["path"] in MANIFEST.CLIENT_DEPENDENCY_MODES],
            "client_view": manifest["client_view"],
        }, sort_keys=True) + "\n", encoding="utf-8")
        bundle = self.root / "go-bundle.tar"
        with tarfile.open(bundle, mode="w") as archive:
            for path in sorted(extracted.rglob("*")):
                if path.is_file():
                    archive.add(path, arcname=path.relative_to(extracted).as_posix(), recursive=False)
        return bundle

    def test_open_bundle_verifies_format_two_go_release(self):
        bundle = self.go_bundle("f" * 40)
        temporary, root, manifest = RELEASE._open_bundle(bundle)
        try:
            self.assertEqual(manifest["format"], 2)
            self.assertEqual(manifest["client_view"]["transport"], "go-request")
            self.assertTrue((root / "bin/agent-vm-control").is_file())
            self.assertTrue(RELEASE.verify_release(bundle)["verified"])
        finally:
            temporary.cleanup()

    def test_publish_is_remote_first_and_exits_drain_after_local_client(self):
        transport = FakeTransport()
        target = self.root / "client" / "agent_vm.ts"
        result = self.publisher(transport, client_target=target).publish(timeout=5, poll=1, transaction_id="tx-publish")
        self.assertTrue(result["published"])
        self.assertTrue(target.is_file())
        self.assertTrue((target.parent / "client-release.json").is_file())
        actions = [item[1] for item in transport.calls]
        self.assertEqual(actions[:3], ["stage", "verify", "drain-enter"])
        self.assertLess(actions.index("drain-enter"), actions.index("activate"))
        self.assertEqual(actions[-1], "drain-exit")

    def test_build_and_install_use_all_bun_files_and_never_python_client_paths(self):
        from release_manifest import verify_typescript_syntax
        temporary, root, manifest = RELEASE._open_bundle(self.bundle)
        try:
            verify_typescript_syntax(root, manifest)
            self.assertFalse((root / "agent_vm.py").exists())
            self.assertFalse((root / "sync_worktree.py").exists())
            sidecar = json.loads((root / "client-release.json").read_text())
            self.assertEqual({item["path"] for item in sidecar["client_dependencies"]}, set(RELEASE.CLIENT_DEPENDENCY_MODES))
            target = self.root / "complete" / "agent_vm.ts"
            publisher = self.publisher(FakeTransport(), client_target=target)
            publisher._install_local_client(manifest)
            for name, mode in RELEASE.LOCAL_PACKAGE_MODES.items():
                self.assertEqual((target.parent / name).read_bytes(), (root / name).read_bytes())
                self.assertEqual((target.parent / name).stat().st_mode & 0o7777, mode)
            RELEASE._client_selfcheck(target.parent, manifest["release_id"])
            self.assertFalse((target.parent / "agent_vm.py").exists())
            self.assertFalse((target.parent / RELEASE.LOCAL_INSTALL_RECORD_NAME).exists())
            marker = target.parent / RELEASE.LOCAL_INSTALL_RECORD_NAME
            marker.write_text("{}")
            for arguments in ([str(target), "version"], [str(target.parent / "sync_worktree.ts"), "--name", "demo"]):
                process = RELEASE.subprocess.run(
                    [RELEASE.shutil.which("bun"), "--no-macros", *arguments],
                    stdout=RELEASE.subprocess.PIPE, stderr=RELEASE.subprocess.PIPE,
                    timeout=30, cwd="/", env={"PATH": os.environ["PATH"], "AGENT_VM_RELEASE_MANIFEST": str(self.root / "absent.json")},
                )
                self.assertEqual(process.returncode, 2, process.stderr.decode())
                self.assertIn(b"recovery is pending", process.stderr)
            marker.unlink()
            # Both public entrypoints reject the former Python-only sidecar,
            # even when direct sync would otherwise select a separate CLI.
            del sidecar["client_dependencies"]
            sidecar["client_artifact"].update(name="agent_vm.py", path="agent_vm.py")
            (target.parent / "client-release.json").write_text(json.dumps(sidecar))
            for arguments in ([str(target), "version"], [str(target.parent / "sync_worktree.ts"), "--name", "demo"]):
                process = RELEASE.subprocess.run(
                    [RELEASE.shutil.which("bun"), "--no-macros", *arguments],
                    stdout=RELEASE.subprocess.PIPE, stderr=RELEASE.subprocess.PIPE,
                    timeout=30, cwd="/", env={"PATH": os.environ["PATH"]},
                )
                self.assertEqual(process.returncode, 2, process.stderr.decode())
                self.assertIn(b"sidecar", process.stderr)
        finally:
            temporary.cleanup()

    def test_retired_python_target_and_python_only_install_are_rejected_without_fallback(self):
        manifest = RELEASE.build_release(ROOT, self.bundle, "e" * 40)
        directory = self.root / "legacy"
        directory.mkdir()
        old = directory / "agent_vm.py"
        old.write_bytes(b"raise RuntimeError('must never execute')\n")
        old.chmod(0o755)
        for target in (old, directory / "agent_vm.ts"):
            with self.subTest(target=target.name):
                with self.assertRaises(RELEASE.ReleaseError) as failure:
                    self.publisher(FakeTransport(), client_target=target)._install_local_client(manifest)
                self.assertEqual(failure.exception.code, "local_client_target_invalid")
                self.assertFalse((directory / "agent_vm.ts").exists())
                self.assertTrue(old.read_bytes().startswith(b"raise RuntimeError"))

    def test_atomic_package_write_does_not_follow_or_remove_unowned_temporary_symlinks(self):
        adjacent = self.root / "operator-owned"
        adjacent.write_bytes(b"keep")
        target = self.root / "client.ts"
        old_temporary = self.root / ".client.ts.tmp"
        old_temporary.symlink_to(adjacent)
        RELEASE._write_atomic(target, b"verified", 0o644)
        self.assertEqual(target.read_bytes(), b"verified")
        self.assertEqual(adjacent.read_bytes(), b"keep")
        self.assertTrue(old_temporary.is_symlink())

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
                client_target=self.root / "client" / "agent_vm.ts",
            )
        self.assertEqual(failure.exception.code, "remote_control_invalid")
        with self.assertRaises(RELEASE.ReleaseError) as wrapper_failure:
            RELEASE.ReleasePublisher(
                bundle=self.bundle,
                control_host="staging-control",
                wrapper_path="/tmp/other-wrapper",
                client_target=self.root / "client" / "agent_vm.ts",
            )
        self.assertEqual(wrapper_failure.exception.code, "wrapper_path_invalid")

    def test_cli_defaults_to_canonical_wrapper_and_keeps_legacy_option(self):
        common = [
            "publish",
            "--bundle", str(self.bundle),
            "--control-host", "staging-control",
            "--client-target", str(self.root / "client" / "agent_vm.ts"),
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

    def test_ssh_control_uses_bun_and_the_typescript_controller(self):
        transport = RELEASE.SSHTransport()
        request = {"protocol": 4, "action": "drain-status"}
        response = {"ok": True, "result": {"draining": True}}
        with mock.patch.object(transport, "_run", return_value=response) as run:
            self.assertEqual(
                transport.control("staging-control", RELEASE.CANONICAL_REMOTE_CONTROL, request),
                response,
            )
        run.assert_called_once_with(
            "staging-control",
            ["bun", "/opt/agent-vm-native/agent_vm_control.ts"],
            json.dumps(request, sort_keys=True).encode(),
        )

    def test_bundle_verification_checks_typescript_and_fails_closed(self):
        self.assertTrue(RELEASE.verify_release(self.bundle)["verified"])
        self.typescript_check.assert_called_once()
        manifest = self.typescript_check.call_args.args[1]
        self.assertIn(
            ("control", "agent_vm_control.ts"),
            {(item["role"], item["path"]) for item in manifest["artifacts"]},
        )
        self.typescript_check.side_effect = RELEASE.ManifestError(
            "typescript_syntax_invalid", "injected syntax failure",
        )
        transport = FakeTransport()
        with self.assertRaises(RELEASE.ReleaseError) as failure:
            self.publisher(transport).publish(timeout=5, poll=1, transaction_id="tx-invalid-ts")
        self.assertEqual(failure.exception.code, "typescript_syntax_invalid")
        self.assertEqual(transport.calls, [])

    def test_python_syntax_check_only_compiles_python_files(self):
        python = self.root / "candidate.py"
        typescript = self.root / "candidate.ts"
        python.write_text("raise RuntimeError('must not execute')\n", encoding="utf-8")
        typescript.write_text("export const value: number = 1;\n", encoding="utf-8")
        manifest = {"artifacts": [
            {"role": "client", "path": python.name},
            {"role": "control", "path": typescript.name},
        ]}
        RELEASE._verify_python_syntax(self.root, manifest)
        python.write_text("def broken(:\n", encoding="utf-8")
        with self.assertRaises(RELEASE.ReleaseError) as failure:
            RELEASE._verify_python_syntax(self.root, manifest)
        self.assertEqual(failure.exception.code, "python_syntax_invalid")

    def test_health_rejects_malformed_boolean_values(self):
        transport = FakeTransport(malformed_health=True)
        publisher = self.publisher(transport)
        temporary, _, manifest = RELEASE._open_bundle(self.bundle)
        temporary.cleanup()
        with self.assertRaises(RELEASE.ReleaseError) as failure:
            publisher._health(manifest)
        self.assertEqual(failure.exception.code, "remote_health_failed")

    def test_health_rejects_inconsistent_metrics_snapshot(self):
        transport = FakeTransport(observation_consistent=False)
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

    def test_activation_failure_keeps_current_release_drained(self):
        transport = FakeTransport(fail_activation=True)
        with self.assertRaises(RELEASE.ReleaseError) as failure:
            self.publisher(transport).publish(timeout=5, poll=1, transaction_id="tx-activation-failure")
        self.assertEqual(failure.exception.code, "activation_failed")
        actions = [item[1] for item in transport.calls]
        self.assertNotIn("rollback", actions)
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

    def test_convergence_rejects_inconsistent_metrics_snapshot(self):
        transport = FakeTransport(observation_consistent=False)
        with self.assertRaises(RELEASE.ReleaseError) as failure:
            self.publisher(transport).publish(timeout=5, poll=1, transaction_id="tx-inconsistent")
        self.assertEqual(failure.exception.code, "observation_unverified")
        self.assertNotIn("activate", [item[1] for item in transport.calls])

    def test_convergence_accepts_legacy_metrics_before_activation(self):
        transport = FakeTransport(legacy_metrics=True)
        publisher = self.publisher(transport)
        temporary, _, manifest = RELEASE._open_bundle(self.bundle)
        try:
            result = publisher._wait_for_convergence(manifest, timeout=5, poll=1)
        finally:
            temporary.cleanup()
        self.assertEqual(result["pending_operations"], [])
        self.assertNotIn("activate", [item[1] for item in transport.calls])

    def test_health_requires_observation_for_candidates_but_allows_legacy_rollback(self):
        transport = FakeTransport(legacy_metrics=True)
        publisher = self.publisher(transport)
        temporary, _, manifest = RELEASE._open_bundle(self.bundle)
        try:
            with self.assertRaises(RELEASE.ReleaseError) as failure:
                publisher._health(manifest)
            self.assertEqual(failure.exception.code, "remote_health_failed")
            publisher._health(
                RELEASE.legacy_manifest(),
                allow_legacy_observation=True,
            )
        finally:
            temporary.cleanup()

    def test_post_activation_health_failure_attempts_one_rollback(self):
        transport = FakeTransport(fail_health_once=True, require_legacy_drain_exit=True)
        target = self.root / "client" / "agent_vm.ts"
        result = self.publisher(transport, client_target=target).publish(timeout=5, poll=1, transaction_id="tx-rollback")
        self.assertTrue(result["rolled_back"])
        actions = [item[1] for item in transport.calls]
        self.assertIn("rollback", actions)
        self.assertIn("status", actions)
        self.assertEqual(actions[-1], "drain-exit")
        self.assertEqual(transport.control_release_ids[-1], ("drain-exit", "0" * 40))

    def test_post_activation_health_failure_rejects_unverified_rollback(self):
        transport = FakeTransport(fail_health_once=True, malformed_rollback=True)
        with self.assertRaises(RELEASE.ReleaseError) as failure:
            self.publisher(transport).publish(timeout=5, poll=1, transaction_id="tx-bad-rollback")
        self.assertEqual(failure.exception.code, "rollback_failed")
        self.assertNotIn("drain-exit", [item[1] for item in transport.calls])

    def test_local_client_failure_keeps_remote_release_drained_without_rollback(self):
        transport = FakeTransport()
        publisher = self.publisher(transport, client_target=self.root / "client" / "agent_vm.ts")
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
        target = self.root / "client" / "agent_vm.ts"
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

    def package_record(self, *, phase="installing", absent=False):
        directory = self.root / "recovery"
        directory.mkdir(exist_ok=True)
        staging = directory / "agent-vm-client-crash"
        staging.mkdir()
        record = {"format": 2, "phase": phase, "client_path": str(directory / "agent_vm.ts"),
                  "staged_dir": str(staging), "previous": {}, "target": {}}
        for name, mode in RELEASE.LOCAL_PACKAGE_MODES.items():
            old, new = ("old:" + name).encode(), ("new:" + name).encode()
            staged = staging / name
            staged.write_bytes(new)
            staged.chmod(mode)
            record["target"][name] = {"exists": True, "sha256": hashlib.sha256(new).hexdigest(), "mode": mode}
            record["previous"][name] = {"exists": False}
            if not absent:
                destination = directory / name
                destination.write_bytes(old)
                destination.chmod(mode)
                record["previous"][name] = {"exists": True, "sha256": hashlib.sha256(old).hexdigest(), "mode": mode}
        RELEASE._write_json(directory / RELEASE.LOCAL_INSTALL_RECORD_NAME, record, 0o600)
        return directory, staging, record

    def move_package_file(self, directory, staging, name):
        destination = directory / name
        if destination.exists():
            os.replace(destination, RELEASE._local_backup_path(destination))
        os.replace(staging / name, destination)

    def recover(self, directory):
        RELEASE._recover_local_client_install(directory / "agent_vm.ts", directory / "client-release.json")

    def test_recovery_restores_complete_package_at_every_install_boundary(self):
        # Include power loss after backup rename but before candidate rename.
        for boundary in range(len(RELEASE.LOCAL_PACKAGE_MODES) * 2 + 1):
            with self.subTest(boundary=boundary):
                directory, staging, record = self.package_record()
                mutations = 0
                for name in RELEASE.LOCAL_PACKAGE_MODES:
                    if mutations == boundary:
                        break
                    os.replace(directory / name, RELEASE._local_backup_path(directory / name))
                    mutations += 1
                    if mutations == boundary:
                        break
                    os.replace(staging / name, directory / name)
                    mutations += 1
                (directory / "operator-results").write_bytes(b"keep")
                self.recover(directory)
                self.recover(directory)  # repeated recovery is harmless
                for name, mode in RELEASE.LOCAL_PACKAGE_MODES.items():
                    self.assertEqual((directory / name).read_bytes(), ("old:" + name).encode())
                    self.assertEqual((directory / name).stat().st_mode & 0o777, mode)
                self.assertEqual((directory / "operator-results").read_bytes(), b"keep")
                self.assertFalse(staging.exists())
                self.assertFalse((directory / RELEASE.LOCAL_INSTALL_RECORD_NAME).exists())
                import shutil
                shutil.rmtree(directory)

    def test_recovery_removes_only_first_install_package_files(self):
        directory, staging, _ = self.package_record(absent=True)
        for name in RELEASE.LOCAL_PACKAGE_MODES:
            self.move_package_file(directory, staging, name)
        (directory / "unowned").write_bytes(b"keep")
        self.recover(directory)
        self.assertEqual({p.name for p in directory.iterdir()}, {"unowned"})

    def test_completed_package_recovery_verifies_and_cleans_backups(self):
        directory, staging, record = self.package_record(phase="complete")
        for name in RELEASE.LOCAL_PACKAGE_MODES:
            self.move_package_file(directory, staging, name)
        # A crash during backup cleanup must not roll back a committed package.
        RELEASE._local_backup_path(directory / "agent_vm.ts").unlink()
        self.recover(directory)
        for name in RELEASE.LOCAL_PACKAGE_MODES:
            self.assertEqual((directory / name).read_bytes(), ("new:" + name).encode())
            self.assertFalse(RELEASE._local_backup_path(directory / name).exists())
        self.assertFalse((directory / RELEASE.LOCAL_INSTALL_RECORD_NAME).exists())

    def test_foreign_replacement_or_corrupt_backup_blocks_all_recovery_writes(self):
        for corrupt in ("backup", "destination", "staging"):
            with self.subTest(corrupt=corrupt):
                directory, staging, _ = self.package_record()
                for name in RELEASE.LOCAL_PACKAGE_MODES:
                    self.move_package_file(directory, staging, name)
                selected = list(RELEASE.LOCAL_PACKAGE_MODES)[-1]
                path = (RELEASE._local_backup_path(directory / selected) if corrupt == "backup"
                        else staging / selected if corrupt == "staging" else directory / selected)
                path.write_bytes(b"foreign")
                path.chmod(RELEASE.LOCAL_PACKAGE_MODES[selected])
                before = {p.name: p.read_bytes() for p in directory.iterdir() if p.is_file()}
                with self.assertRaises(RELEASE.ReleaseError) as failure:
                    self.recover(directory)
                self.assertEqual(failure.exception.code, "local_client_recovery_failed")
                self.assertEqual({p.name: p.read_bytes() for p in directory.iterdir() if p.is_file()}, before)
                self.assertTrue(path.exists())
                import shutil
                shutil.rmtree(directory)

    def test_unknown_staging_content_and_partial_or_legacy_markers_fail_closed(self):
        for fault in ("foreign-stage", "partial-package", "legacy-marker", "redirect-stage", "symlink"):
            with self.subTest(fault=fault):
                directory, staging, record = self.package_record()
                if fault == "foreign-stage":
                    (staging / "operator-file").write_bytes(b"keep")
                elif fault == "partial-package":
                    del record["target"]["guest-source.ts"]
                elif fault == "legacy-marker":
                    record["format"] = 1
                elif fault == "redirect-stage":
                    record["staged_dir"] = str(self.root / "operator-stage")
                else:
                    (staging / "client.ts").unlink()
                    (staging / "client.ts").symlink_to(directory / "client.ts")
                RELEASE._write_json(directory / RELEASE.LOCAL_INSTALL_RECORD_NAME, record, 0o600)
                before = {name: (directory / name).read_bytes() for name in RELEASE.LOCAL_PACKAGE_MODES}
                with self.assertRaises(RELEASE.ReleaseError) as failure:
                    self.recover(directory)
                self.assertEqual(failure.exception.code, "local_client_recovery_failed")
                self.assertEqual(before, {name: (directory / name).read_bytes() for name in RELEASE.LOCAL_PACKAGE_MODES})
                self.assertTrue(staging.exists())
                import shutil
                shutil.rmtree(directory)

    def test_actual_install_failure_restores_all_previous_modules(self):
        target = self.root / "installed" / "agent_vm.ts"
        publisher = self.publisher(FakeTransport(), client_target=target)
        temporary, root, manifest = RELEASE._open_bundle(self.bundle)
        try:
            publisher._install_local_client(manifest)
            before = {name: (target.parent / name).read_bytes() for name in RELEASE.LOCAL_PACKAGE_MODES}
            (target.parent / "results").write_bytes(b"keep")
            original = os.replace
            failed = False
            def interrupted(source, destination):
                nonlocal failed
                result = original(source, destination)
                if Path(destination) == target.parent / "guest-source.ts" and not failed:
                    failed = True
                    raise OSError("injected failure after module rename")
                return result
            with mock.patch.object(RELEASE.os, "replace", side_effect=interrupted):
                with self.assertRaises(RELEASE.ReleaseError):
                    publisher._install_local_client(manifest)
            self.assertTrue(failed)
            self.assertEqual(before, {name: (target.parent / name).read_bytes() for name in RELEASE.LOCAL_PACKAGE_MODES})
            self.assertEqual((target.parent / "results").read_bytes(), b"keep")
            self.assertFalse((target.parent / RELEASE.LOCAL_INSTALL_RECORD_NAME).exists())
        finally:
            temporary.cleanup()

    def test_preexisting_backup_is_preserved_before_install_mutation(self):
        target = self.root / "preexisting" / "agent_vm.ts"
        target.parent.mkdir()
        backup = RELEASE._local_backup_path(target.parent / "sync_worktree.ts")
        backup.write_bytes(b"operator backup")
        with self.assertRaises(RELEASE.ReleaseError) as failure:
            self.publisher(FakeTransport(), client_target=target)._install_local_client(RELEASE.build_release(ROOT, self.bundle, "e" * 40))
        self.assertEqual(failure.exception.code, "local_client_target_invalid")
        self.assertEqual(backup.read_bytes(), b"operator backup")
        self.assertFalse(target.exists())

    def test_failure_after_exclusive_backup_creation_restores_the_package(self):
        target = self.root / "backup-boundary" / "agent_vm.ts"
        publisher = self.publisher(FakeTransport(), client_target=target)
        manifest = RELEASE.build_release(ROOT, self.bundle, "e" * 40)
        publisher._install_local_client(manifest)
        before = {name: (target.parent / name).read_bytes() for name in RELEASE.LOCAL_PACKAGE_MODES}
        original = os.link
        def interrupted(source, destination, **options):
            original(source, destination, **options)
            raise OSError("injected failure after exclusive backup creation")
        with mock.patch.object(RELEASE.os, "link", side_effect=interrupted):
            with self.assertRaises(RELEASE.ReleaseError):
                publisher._install_local_client(manifest)
        self.assertEqual(before, {name: (target.parent / name).read_bytes() for name in RELEASE.LOCAL_PACKAGE_MODES})
        self.assertFalse((target.parent / RELEASE.LOCAL_INSTALL_RECORD_NAME).exists())

    def test_backup_created_after_preflight_is_preserved_without_overwriting_it(self):
        target = self.root / "backup-race" / "agent_vm.ts"
        publisher = self.publisher(FakeTransport(), client_target=target)
        manifest = RELEASE.build_release(ROOT, self.bundle, "e" * 40)
        publisher._install_local_client(manifest)
        before = {name: (target.parent / name).read_bytes() for name in RELEASE.LOCAL_PACKAGE_MODES}
        original = os.link
        def foreign(source, destination, **options):
            Path(destination).write_bytes(b"operator backup")
            original(source, destination, **options)
        with mock.patch.object(RELEASE.os, "link", side_effect=foreign):
            with self.assertRaises(RELEASE.ReleaseError) as failure:
                publisher._install_local_client(manifest)
        self.assertEqual(failure.exception.code, "local_client_recovery_failed")
        self.assertEqual(RELEASE._local_backup_path(target).read_bytes(), b"operator backup")
        self.assertEqual(before, {name: (target.parent / name).read_bytes() for name in RELEASE.LOCAL_PACKAGE_MODES})
        self.assertTrue((target.parent / RELEASE.LOCAL_INSTALL_RECORD_NAME).exists())

    def test_installed_integrity_failure_restores_the_entire_previous_package(self):
        target = self.root / "integrity" / "agent_vm.ts"
        publisher = self.publisher(FakeTransport(), client_target=target)
        temporary, root, manifest = RELEASE._open_bundle(self.bundle)
        try:
            publisher._install_local_client(manifest)
            before = {name: (target.parent / name).read_bytes() for name in RELEASE.LOCAL_PACKAGE_MODES}
            with mock.patch.object(RELEASE, "verify_client_sidecar", side_effect=RELEASE.ManifestError("artifact_checksum_mismatch", "injected")):
                with self.assertRaises(RELEASE.ReleaseError) as failure:
                    RELEASE._install_client_package(root, target, manifest["release_id"])
            self.assertEqual(failure.exception.code, "local_client_install_failed")
            self.assertEqual(before, {name: (target.parent / name).read_bytes() for name in RELEASE.LOCAL_PACKAGE_MODES})
            self.assertFalse((target.parent / RELEASE.LOCAL_INSTALL_RECORD_NAME).exists())
        finally:
            temporary.cleanup()

    def test_selfcheck_requires_bun_and_does_not_execute_python(self):
        temporary, root, manifest = RELEASE._open_bundle(self.bundle)
        try:
            publisher = self.publisher(FakeTransport())
            with mock.patch.object(RELEASE.shutil, "which", return_value=None):
                with self.assertRaises(RELEASE.ReleaseError) as failure:
                    publisher._install_local_client(manifest)
            self.assertEqual(failure.exception.code, "local_client_selfcheck_failed")
            self.assertFalse(publisher.client_target.exists())
            self.assertFalse((publisher.client_target.parent / RELEASE.LOCAL_INSTALL_RECORD_NAME).exists())
        finally:
            temporary.cleanup()

    def test_local_install_lock_refuses_a_second_publisher(self):
        import fcntl
        target = self.root / "locked" / "agent_vm.ts"
        target.parent.mkdir()
        descriptor = os.open(target.parent / ".agent-vm-client-install.lock", os.O_RDWR | os.O_CREAT, 0o600)
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
            manifest = RELEASE.build_release(ROOT, self.bundle, "e" * 40)
            with self.assertRaises(RELEASE.ReleaseError) as failure:
                self.publisher(FakeTransport(), client_target=target)._install_local_client(manifest)
            self.assertEqual(failure.exception.code, "local_client_install_busy")
            self.assertFalse(target.exists())
        finally:
            os.close(descriptor)


if __name__ == "__main__":
    unittest.main()