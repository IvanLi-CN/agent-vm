"""Current Python client transport contract for the direct Bun controller."""
from __future__ import annotations

import importlib.util
import json
from pathlib import Path
import shlex
import subprocess
import tempfile
import unittest
from unittest import mock


ROOT = Path(__file__).resolve().parents[3]
SPEC = importlib.util.spec_from_file_location("transport_client", ROOT / "skills/agent-vm/scripts/agent_vm.py")
CLI = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CLI)


class ControlTransportTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.patch = mock.patch.object(CLI, "DEFAULT_RELEASE_MANIFEST", Path(self.temp.name) / "absent.json")
        self.patch.start()
        self.addCleanup(self.patch.stop)

    @staticmethod
    def response(action="status"):
        return {"ok": True, "protocol": 4, "action": action, "session_id": "transport-test",
                "server_time": "2026-10-25T00:00:00+00:00", "outcome": "success", "result": {}}

    def test_canonical_controller_runs_directly_with_bun(self):
        with mock.patch.dict(CLI.os.environ, {}, clear=True), mock.patch.object(CLI.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, json.dumps(self.response()).encode(), b"")) as run:
            result = CLI.control_request("status", session="transport-test")
        argv = run.call_args.args[0]
        self.assertEqual(shlex.split(argv[-1]), ["bun", "/opt/agent-vm-native/agent_vm_control.ts"])
        self.assertEqual(argv[:-1], ["ssh", "-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "StrictHostKeyChecking=yes", "--", "agent-vm-host"])
        self.assertEqual(json.loads(run.call_args.kwargs["input"])["session_id"], "transport-test")
        self.assertEqual(result["outcome"], "success")

    def test_configured_controller_path_remains_one_quoted_argument(self):
        target = "/operator path/control;$(false).ts"
        with mock.patch.dict(CLI.os.environ, {CLI.REMOTE_CONTROL_ENV: target}), mock.patch.object(CLI.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, json.dumps(self.response()).encode(), b"")) as run:
            CLI.control_request("status", session="transport-test")
        self.assertEqual(shlex.split(run.call_args.args[0][-1]), ["bun", target])

    def test_action_timeouts_and_stream_limits_are_preserved(self):
        for action, fields, timeout in (("acquire", {}, 600), ("restart", {}, 600), ("resize", {"allow_restart": True}, 600), ("resize", {"allow_restart": False}, 240), ("status", {}, 240)):
            with self.subTest(action=action, fields=fields), mock.patch.object(CLI.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, json.dumps(self.response(action)).encode(), b"")) as run:
                CLI.control_request(action, session="transport-test", fields=fields)
            self.assertEqual(run.call_args.kwargs["timeout"], timeout)
            self.assertTrue(hasattr(run.call_args.kwargs["stdout"], "fileno"))
            self.assertTrue(hasattr(run.call_args.kwargs["stderr"], "fileno"))
        self.assertEqual(CLI.REMOTE_STDOUT_LIMIT_BYTES, 2 * 1024 * 1024)
        self.assertEqual(CLI.REMOTE_STDERR_LIMIT_BYTES, 64 * 1024)

    def test_transport_timeout_keeps_uncertain_outcome(self):
        with mock.patch.object(CLI.subprocess, "run", side_effect=subprocess.TimeoutExpired("ssh", 240)), self.assertRaises(CLI.CliError) as failure:
            CLI.control_request("status", session="transport-test")
        self.assertEqual(failure.exception.code, "remote_timeout")
        self.assertIn("stream_cleanup_verified", failure.exception.detail)


if __name__ == "__main__":
    unittest.main()
