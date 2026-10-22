"""Offline tests for the generic agent-vm session contract."""
from __future__ import annotations

import importlib.util
import hashlib
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock


ROOT = Path(__file__).resolve().parents[3]
SPEC = importlib.util.spec_from_file_location("agent_vm_client", ROOT / "skills/agent-vm/scripts/agent_vm.py")
assert SPEC and SPEC.loader
CLI = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CLI)


class SessionContractTests(unittest.TestCase):
    def test_session_option_is_accepted_before_or_after_command(self):
        before = CLI.build_parser().parse_args(["--session-id", "before", "status"])
        after = CLI.build_parser().parse_args(["status", "--session-id", "after"])
        nested = CLI.build_parser().parse_args(["admin", "status", "--session-id", "nested"])
        self.assertEqual(before.session_id, "before")
        self.assertEqual(after.session_id, "after")
        self.assertEqual(nested.session_id, "nested")

    def test_explicit_session_overrides_environment(self):
        with mock.patch.dict(CLI.os.environ, {"AGENT_VM_SESSION_ID": "environment"}):
            self.assertEqual(CLI.session_id("explicit"), "explicit")

    def test_environment_is_fallback(self):
        with mock.patch.dict(CLI.os.environ, {"AGENT_VM_SESSION_ID": "environment"}):
            self.assertEqual(CLI.session_id(), "environment")

    def test_missing_session_fails_before_remote_write(self):
        with mock.patch.dict(CLI.os.environ, {}, clear=True), mock.patch.object(CLI.subprocess, "run") as run:
            with self.assertRaises(CLI.CliError) as error:
                CLI.control_request("acquire", session=None, write=True)
        self.assertEqual(error.exception.code, "missing_session")
        run.assert_not_called()

    def test_session_state_namespace_is_derived_from_selected_identity(self):
        self.assertNotEqual(CLI.local_session_dir("first"), CLI.local_session_dir("second"))
        self.assertIn("agent-vm", str(CLI.local_session_dir("first")))

    def test_sync_passes_selected_identity_to_helper(self):
        args = CLI.build_parser().parse_args(["sync", "--name", "repo", "--session-id", "explicit"])
        with mock.patch.object(CLI, "run_command", return_value=0) as run_command:
            self.assertEqual(CLI.command_sync(args), 0)
        command = run_command.call_args.args[0]
        self.assertEqual(command[-2:], ["--session-id", "explicit"])

    def test_missing_sidecar_sends_legacy_capability_and_preserves_warning(self):
        response = {
            "ok": True,
            "protocol": CLI.PROTOCOL_VERSION,
            "action": "status",
            "session_id": "thread",
            "server_time": "2026-10-22T00:00:00+00:00",
            "result": {},
        }
        with tempfile.TemporaryDirectory() as directory, \
             mock.patch.object(CLI, "DEFAULT_RELEASE_MANIFEST", Path(directory) / "missing.json"), \
             mock.patch.object(CLI.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, json.dumps(response).encode(), b"")) as run:
            result = CLI.control_request("status", session="thread")
        request = json.loads(run.call_args.kwargs["input"])
        self.assertEqual(request["compatibility"]["release_id"], CLI.LEGACY_RELEASE_ID)
        self.assertTrue(request["compatibility"]["legacy"])
        self.assertTrue(any(item["code"] == "legacy_compatibility" for item in result["warnings"]))
        self.assertTrue(any(item["code"] == "legacy_control_release" for item in result["warnings"]))

    def test_replay_reuses_journaled_capability_after_sidecar_change(self):
        operation_id = "replay-capability"
        session = "thread"
        capability = {
            "release_id": "a" * 40,
            "protocol": CLI.PROTOCOL_VERSION,
            "action": "drain-enter",
            "request": {"schema": 1, "semantic_revision": 1},
            "accepted_response": {"schema_min": 1, "schema_max": 1, "semantic_min": 1, "semantic_max": 1},
        }
        request = {
            "protocol": CLI.PROTOCOL_VERSION,
            "action": "drain-enter",
            "session_id": session,
            "operation_id": operation_id,
            "compatibility": capability,
        }
        response = {
            "ok": True,
            "protocol": CLI.PROTOCOL_VERSION,
            "action": "drain-enter",
            "session_id": session,
            "operation_id": operation_id,
            "server_time": "2026-10-22T00:00:00+00:00",
            "result": {"draining": True},
            "release": {"release_id": "b" * 40, "source_revision": "b" * 40, "protocol": CLI.PROTOCOL_VERSION},
        }
        metadata = {
            "release_id": "b" * 40,
            "source_revision": "b" * 40,
            "protocol": CLI.PROTOCOL_VERSION,
            "manifest_present": True,
            "artifacts_verified": True,
            "actions": {
                "drain-enter": {
                    "request": {"schema": 1, "semantic_revision": 1},
                    "response": {"schema": 1, "semantic_revision": 1},
                    "client_to_control": {"schema_min": 1, "schema_max": 1, "semantic_min": 1, "semantic_max": 1},
                    "control_to_client": {"schema_min": 1, "schema_max": 1, "semantic_min": 1, "semantic_max": 1},
                },
            },
        }
        with tempfile.TemporaryDirectory() as directory, \
             mock.patch.object(CLI, "local_session_dir", return_value=Path(directory)), \
             mock.patch.object(CLI, "load_client_release_metadata", return_value=(metadata, [])), \
             mock.patch.object(CLI.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, json.dumps(response).encode(), b"")) as run:
            operations = Path(directory) / "operations"
            operations.mkdir()
            journal = operations / (hashlib.sha256(operation_id.encode()).hexdigest() + ".json")
            journal.write_text(json.dumps({"request": request, "outcome": "pending"}), encoding="utf-8")
            result = CLI.control_request("drain-enter", session=session, write=True, operation_id=operation_id)
            saved = json.loads(journal.read_text(encoding="utf-8"))
        sent = json.loads(run.call_args.kwargs["input"])
        self.assertEqual(sent["compatibility"]["release_id"], "a" * 40)
        self.assertEqual(result["operation_id"], operation_id)
        self.assertEqual(saved["outcome"], "success")
        self.assertEqual(saved["response"]["operation_id"], operation_id)

    def test_present_invalid_sidecar_fails_before_ssh(self):
        with tempfile.TemporaryDirectory() as directory:
            sidecar = Path(directory) / "release.json"
            sidecar.write_text(json.dumps({"format": 999}), encoding="utf-8")
            with mock.patch.object(CLI, "DEFAULT_RELEASE_MANIFEST", sidecar), mock.patch.object(CLI.subprocess, "run") as run:
                with self.assertRaises(CLI.CliError) as failure:
                    CLI.control_request("status", session="thread")
            self.assertEqual(failure.exception.code, "release_metadata_invalid")
            run.assert_not_called()

    def test_client_sidecar_checks_client_path_mode_and_checksum(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            client = root / "agent_vm.py"
            content = b"client\n"
            client.write_bytes(content)
            client.chmod(0o755)
            action = {
                "request": {"schema": 1, "semantic_revision": 1},
                "response": {"schema": 1, "semantic_revision": 1},
                "client_to_control": {"schema_min": 1, "schema_max": 1, "semantic_min": 1, "semantic_max": 1},
                "control_to_client": {"schema_min": 1, "schema_max": 1, "semantic_min": 1, "semantic_max": 1},
            }
            sidecar = {
                "format": 1,
                "release_id": "a" * 40,
                "source_revision": "a" * 40,
                "protocol": 4,
                "client_artifact": {
                    "name": "agent_vm.py",
                    "path": "agent_vm.py",
                    "role": "client",
                    "sha256": hashlib.sha256(content).hexdigest(),
                    "mode": 0o755,
                },
                "client_view": {
                    "release_id": "a" * 40,
                    "source_revision": "a" * 40,
                    "protocol": 4,
                    "actions": {name: action for name in CLI.CLIENT_ACTION_NAMES},
                },
            }
            path = root / CLI.CLIENT_SIDECAR_NAME
            path.write_text(json.dumps(sidecar), encoding="utf-8")
            metadata, warnings = CLI.load_client_release_metadata(path)
            self.assertEqual(metadata["release_id"], "a" * 40)
            self.assertEqual(warnings, [])
            for mutation, code in (
                (lambda value: value["client_artifact"].update(path="../agent_vm.py"), "release_metadata_invalid"),
                (lambda value: value["client_artifact"].update(mode=0o644), "release_metadata_invalid"),
                (lambda value: value["client_artifact"].update(sha256="0" * 64), "artifact_checksum_mismatch"),
            ):
                with self.subTest(code=code):
                    mutated = json.loads(json.dumps(sidecar))
                    mutation(mutated)
                    path.write_text(json.dumps(mutated), encoding="utf-8")
                    with self.assertRaises(CLI.CliError) as failure:
                        CLI.load_client_release_metadata(path)
                    self.assertEqual(failure.exception.code, code)

    def test_present_sidecar_requires_the_complete_client_view(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            client = root / "agent_vm.py"
            client.write_bytes(b"client\n")
            client.chmod(0o755)
            action = {
                "request": {"schema": 1, "semantic_revision": 1},
                "response": {"schema": 1, "semantic_revision": 1},
                "client_to_control": {"schema_min": 1, "schema_max": 1, "semantic_min": 1, "semantic_max": 1},
                "control_to_client": {"schema_min": 1, "schema_max": 1, "semantic_min": 1, "semantic_max": 1},
            }
            sidecar = {
                "format": 1,
                "release_id": "a" * 40,
                "source_revision": "a" * 40,
                "protocol": 4,
                "client_artifact": {
                    "name": "agent_vm.py",
                    "path": "agent_vm.py",
                    "role": "client",
                    "sha256": hashlib.sha256(b"client\n").hexdigest(),
                    "mode": 0o755,
                },
                "client_view": {
                    "release_id": "a" * 40,
                    "source_revision": "a" * 40,
                    "protocol": 4,
                    "actions": {name: action for name in CLI.CLIENT_ACTION_NAMES},
                },
            }
            path = root / CLI.CLIENT_SIDECAR_NAME
            for mutation in (
                lambda value: value.pop("client_view"),
                lambda value: value["client_view"]["actions"].pop("status"),
            ):
                mutated = json.loads(json.dumps(sidecar))
                mutation(mutated)
                path.write_text(json.dumps(mutated), encoding="utf-8")
                with self.assertRaises(CLI.CliError) as failure:
                    CLI.load_client_release_metadata(path)
                self.assertEqual(failure.exception.code, "release_metadata_invalid")

    def test_malformed_remote_warning_is_rejected(self):
        response = {
            "ok": True,
            "protocol": CLI.PROTOCOL_VERSION,
            "action": "status",
            "session_id": "thread",
            "server_time": "2026-10-22T00:00:00+00:00",
            "result": {},
            "warnings": "not-an-array",
        }
        with mock.patch.object(CLI.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, json.dumps(response).encode(), b"")):
            with self.assertRaises(CLI.CliError) as failure:
                CLI.control_request("status", session="thread")
        self.assertEqual(failure.exception.code, "invalid_remote_response")

    def test_incompatible_remote_response_is_rejected_before_return(self):
        response = {
            "ok": True,
            "protocol": CLI.PROTOCOL_VERSION,
            "action": "status",
            "session_id": "thread",
            "server_time": "2026-10-22T00:00:00+00:00",
            "result": {},
            "response": {"schema": 2, "semantic_revision": 1},
        }
        with tempfile.TemporaryDirectory() as directory, \
             mock.patch.object(CLI, "DEFAULT_RELEASE_MANIFEST", Path(directory) / "missing.json"), \
             mock.patch.object(CLI.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, json.dumps(response).encode(), b"")):
            with self.assertRaises(CLI.CliError) as failure:
                CLI.control_request("status", session="thread")
        self.assertEqual(failure.exception.code, "response_schema_incompatible")

    def test_public_parser_has_no_update_command(self):
        with self.assertRaises(SystemExit):
            CLI.build_parser().parse_args(["update"])


if __name__ == "__main__":
    unittest.main()
