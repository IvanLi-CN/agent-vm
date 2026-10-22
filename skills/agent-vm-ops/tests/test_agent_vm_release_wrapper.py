"""Permission and failure-state tests for the fixed release wrapper."""
from __future__ import annotations

import importlib.util
import io
import json
import os
from pathlib import Path
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
            "control_path": str(self.root / "installed" / "agent_vm_control.py"),
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
            member = tarfile.TarInfo("agent_vm.py")
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
        self.assertTrue(Path(self.loaded["control_path"]).with_name("agent_vm.py").is_file())
        self.assertTrue(Path(self.loaded["control_path"]).with_name("release_manifest.py").is_file())
        self.assertTrue(Path(self.loaded["control_path"]).with_name("release-manifest.json").is_file())
        self.assertTrue(Path(self.loaded["control_path"]).with_name("client-release.json").is_file())
        self.assertEqual(run.call_args.args[0], ["systemctl", "daemon-reload"])
        self.assertFalse(any("stop" in arg or "disable" in arg for arg in run.call_args.args[0]))

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
        self.assertFalse(Path(self.loaded["control_path"]).with_name("agent_vm.py").exists())
        self.assertFalse(Path(self.loaded["control_path"]).with_name("client-release.json").exists())

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
        with mock.patch.object(WRAPPER.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, b"", b"")):
            WRAPPER.stage(self.loaded, "tx-first", "c" * 40, first.read_bytes())
            WRAPPER.activate(self.loaded, "tx-first", "c" * 40)
            WRAPPER.stage(self.loaded, "tx-second", "d" * 40, second.read_bytes())
            WRAPPER.activate(self.loaded, "tx-second", "d" * 40)
            result = WRAPPER.rollback(self.loaded, "tx-second", "d" * 40)
        self.assertTrue(result["rolled_back"])
        self.assertEqual(result["release_id"], "c" * 40)
        self.assertEqual(Path(self.loaded["current_link"]).resolve().name, "c" * 40)
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
