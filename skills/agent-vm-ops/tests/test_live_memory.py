"""Explicit native live-memory adjustment: accounting, fencing and failures."""
import datetime as dt
import io
import json
import unittest
from unittest import mock

from test_native_agent_vm import CONTROL, CLI, Fixture, GIB


class LiveMemoryTests(Fixture):
    def setUp(self):
        super().setUp()
        patcher = mock.patch.object(CONTROL.time, "sleep")
        self.settle = patcher.start()
        self.addCleanup(patcher.stop)

    @staticmethod
    def balloon_timeout(target):
        return CONTROL.ControlError("command_failed", "command failed (1): incus",
            {"returncode": 1, "stderr": f"Error: Failed updating memory limit: Failed setting memory to {target}MiB (currently 3482MiB) as it was taking too long"})

    def changes(self, since):
        return [args[3:] for args, _, _ in self.runner.calls[since:]
                if args[:3] == ["incus", "--project", "agents"] and args[3] in {"config", "start", "stop", "delete"}]

    def test_live_reduction_under_pressure_preserves_lease_ports_and_disk(self):
        first = self.acquire()
        self.resources.memory.return_value["available_bytes"] = 0
        before = len(self.runner.calls)
        with mock.patch.object(self.control, "_reclaim_capacity") as reclaim:
            result = self.call("resize", memory_mib=2048, live_memory=True)
        reclaim.assert_not_called()
        self.assertEqual(self.changes(before), [["config", "set", first["instance_name"], "limits.memory=2048MiB"]])
        self.assertEqual(result["resources"], {"cpu": 2, "memory_mib": 2048, "disk_gib": 20})
        self.assertEqual(result["lease_expires_at"], first["lease_expires_at"])
        self.assertEqual(result["ports"], first["ports"])
        self.assertEqual(result["state"], "active")
        self.assertEqual(self.control._allocation_totals()["memory_mib"], 2048)

    def test_settlement_rechecks_all_guards_before_the_only_completion(self):
        for boundary in ("boot", "configuration", "reservation", "expiry", "drain", "window"):
            with self.subTest(boundary=boundary):
                first = self.acquire(boundary)
                attempts = []
                def fail(name, *_):
                    attempts.append(name)
                    self.runner.instances[name]["config"]["limits.memory"] = "4096MiB"
                    raise self.balloon_timeout(2048)
                def settle(seconds):
                    self.assertEqual(seconds, 45)
                    if boundary == "boot": self.runner.instances[first["instance_name"]]["boot_id"] = "00000000-0000-4000-8000-000000000002"
                    if boundary == "configuration": self.runner.instances[first["instance_name"]]["config"]["limits.memory"] = "2048MiB"
                    if boundary == "reservation": self.store.update_task(first["task_id"], resource_reservation_json='{}')
                    if boundary == "expiry":
                        self.now = CONTROL.parse_time(first["lease_expires_at"])
                        self.control._expire_preempt({"task_id": first["task_id"], "generation": first["generation"]})
                    if boundary == "drain": self.store.set_meta("draining", "1")
                    if boundary == "window": self.now = CONTROL.parse_time(first["lease_expires_at"]) - dt.timedelta(seconds=59)
                self.runner.on_config_set = fail
                self.settle.side_effect = settle
                self.error("resize_partial", self.call, "resize", boundary, memory_mib=2048, live_memory=True)
                self.assertEqual(len(attempts), 1)
                self.runner.on_config_set = None
                self.settle.side_effect = None
                self.store.set_meta("draining", "0")
                self.call("release", boundary)

    def test_three_minute_start_window_covers_bounded_settlement(self):
        first = self.acquire()
        expiry = CONTROL.iso_time(self.now + dt.timedelta(seconds=180))
        self.store.update_task(first["task_id"], lease_expires_at=expiry)
        attempts = []
        def fail_once(name, *_):
            attempts.append(name)
            if len(attempts) == 1:
                self.runner.instances[name]["config"]["limits.memory"] = "4096MiB"
                raise self.balloon_timeout(2048)
        def settle(seconds):
            self.now += dt.timedelta(seconds=seconds)
        self.runner.on_config_set = fail_once
        self.settle.side_effect = settle
        result = self.call("resize", memory_mib=2048, live_memory=True)
        self.assertTrue(result["applied"])
        self.assertEqual(result["lease_expires_at"], expiry)
        self.assertEqual(len(attempts), 2)

    def test_reduction_does_not_depend_on_unavailable_admission_metrics(self):
        self.acquire()
        self.resources.memory.side_effect = CONTROL.ControlError("resource_unverified", "unavailable")
        result = self.call("resize", memory_mib=2048, live_memory=True)
        self.assertTrue(result["applied"])
        self.error("resource_unverified", self.call, "resize", memory_mib=3072, live_memory=True)

    def test_reduction_can_return_capacity_despite_a_new_admission_policy_conflict(self):
        self.acquire()
        self.runner.project_limits = {"limits.instances": "8"}
        self.assertTrue(self.call("resize", memory_mib=2048, live_memory=True)["applied"])
        self.error("project_instance_limits_conflict", self.call, "resize", memory_mib=4096, live_memory=True)

    def test_growth_uses_logical_allocation_guard_even_under_pressure(self):
        first = self.acquire()
        before = len(self.runner.calls)
        self.resources.memory.return_value["available_bytes"] = 0
        result = self.call("resize", memory_mib=8192, live_memory=True)
        self.assertEqual(result["resources"]["memory_mib"], 8192)
        self.assertTrue(self.call("status")["admission"]["memory_pressure"])
        self.resources.memory.return_value["available_bytes"] = 24*GIB
        self.config.update(memory_budget_mib=4096, memory_overcommit_ratio=1)
        self.error("resource_insufficient", self.call, "resize", memory_mib=12288, live_memory=True)
        self.assertGreaterEqual(len(self.changes(before)), 1)
        self.assertEqual(self.call("status")["resources"]["memory_mib"], 8192)

    def test_restart_growth_uses_logical_allocation_guard_even_under_pressure(self):
        self.acquire()
        self.resources.memory.return_value["available_bytes"] = 0
        result = self.call("resize", memory_mib=8192, allow_restart=True)
        self.assertTrue(result["applied"])
        self.assertTrue(self.call("status")["admission"]["memory_pressure"])
        self.config.update(memory_budget_mib=4096, memory_overcommit_ratio=1)
        self.error("resource_insufficient", self.call, "resize", memory_mib=12288, allow_restart=True)

    def test_growth_reserves_maximum_then_commits_only_after_native_ack(self):
        self.acquire()
        observed = []
        self.runner.on_config_set = lambda *_: observed.append(self.control._allocation_totals()["memory_mib"])
        result = self.call("resize", memory_mib=8192, live_memory=True)
        self.assertEqual(observed, [8192])
        self.assertEqual(result["verification"]["configured_memory_mib"], 8192)
        self.assertEqual(result["resource_reservation"], {})

    def test_other_session_cannot_spend_unconfirmed_reduction(self):
        self.acquire()
        self.config.update(memory_budget_mib=4096, memory_overcommit_ratio=1)
        def compete(*_):
            self.error("resource_insufficient", self.acquire, "other")
        self.runner.on_config_set = compete
        self.call("resize", memory_mib=2048, live_memory=True)
        self.runner.on_config_set = None
        self.assertIsNone(self.store.get_task(session_id="other"))
        result = self.call("acquire", "other", public_key="ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest fixture", memory_mib=2048)
        self.assertEqual(result["state"], "active")
        self.assertEqual(self.control._allocation_totals()["memory_mib"], 4096)

    def test_native_partial_failure_keeps_guest_running_and_conservative_accounting(self):
        first = self.acquire()
        def fail_after_effect(*_):
            raise CONTROL.ControlError("command_timeout", "balloon update was partially applied")
        self.runner.on_config_set = fail_after_effect
        before = len(self.runner.calls)
        request = self.request("resize", memory_mib=2048, live_memory=True)
        self.error("resize_partial", self.control.dispatch, request)
        status = self.call("status")
        self.assertEqual(status["native_status"], "Running")
        self.assertTrue(status["effective_lease"])
        self.assertEqual(status["resources"]["memory_mib"], 4096)
        self.assertEqual(self.control._allocation_totals()["memory_mib"], 4096)
        self.assertEqual(status["ports"], first["ports"])
        self.assertEqual(status["last_error"]["error"]["code"], "command_timeout")
        self.assertFalse(any(change[0] in {"start", "stop", "delete"} for change in self.changes(before)))
        calls = len(self.runner.calls)
        self.error("operation_incomplete", self.control.dispatch, request)
        self.assertEqual(len(self.runner.calls), calls)  # failed ID replay has no native effects
        self.error("resize_incomplete", self.call, "resize", memory_mib=2048, live_memory=True)
        self.runner.on_config_set = None
        self.call("release")
        self.assertEqual(self.call("status")["state"], "cached")

    def test_guest_restart_or_stale_native_config_cannot_reduce_accounting(self):
        first = self.acquire()
        instance = self.runner.instances[first["instance_name"]]
        self.runner.on_config_set = lambda *_: instance.update(boot_id="00000000-0000-4000-8000-000000000002")
        self.error("resize_partial", self.call, "resize", memory_mib=2048, live_memory=True)
        self.assertEqual(self.control._allocation_totals()["memory_mib"], 4096)
        self.assertEqual(instance["status"], "Running")
        self.call("release"); self.acquire()
        instance["config"]["limits.memory"] = "3072MiB"
        before = len(self.runner.calls)
        self.error("resize_unverified", self.call, "resize", memory_mib=2048, live_memory=True)
        self.assertEqual(self.changes(before), [])

    def test_explicit_restart_recovers_partial_live_update_even_at_recorded_size(self):
        first = self.acquire()
        def fail_after_effect(*_):
            raise CONTROL.ControlError("command_timeout", "uncertain balloon")
        self.runner.on_config_set = fail_after_effect
        self.error("resize_partial", self.call, "resize", memory_mib=8192, live_memory=True)
        self.runner.on_config_set = None
        plan = self.call("resize", memory_mib=4096)
        self.assertFalse(plan["applied"])
        observed = []
        self.runner.on_config_set = lambda *_: observed.append(self.control._allocation_totals()["memory_mib"])
        result = self.call("resize", memory_mib=4096, allow_restart=True)
        self.assertTrue(result["applied"])
        self.assertEqual(observed, [8192, 8192])
        self.assertEqual(result["resources"]["memory_mib"], 4096)
        self.assertEqual(result["resource_reservation"], {})
        self.assertEqual(result["lease_expires_at"], first["lease_expires_at"])
        self.assertEqual(self.runner.instances[first["instance_name"]]["config"]["limits.memory"], "4096MiB")

    def test_one_guarded_completion_attempt_after_native_balloon_timeout(self):
        first = self.acquire()
        observations = []
        def timeout_then_ack(name, *_):
            observations.append(self.control._allocation_totals()["memory_mib"])
            if len(observations) == 1:
                self.runner.instances[name]["config"]["limits.memory"] = "4096MiB"
                raise self.balloon_timeout(2048)
        self.runner.on_config_set = timeout_then_ack
        result = self.call("resize", memory_mib=2048, live_memory=True)
        self.assertEqual(observations, [4096, 4096])
        self.settle.assert_called_once_with(45)
        self.assertTrue(result["applied"])
        self.assertEqual([x["ok"] for x in result["native_attempts"]], [False, True])
        self.assertIn("taking too long", result["native_attempts"][0]["error"]["detail"]["stderr"])
        self.assertEqual(result["resources"]["memory_mib"], 2048)
        self.assertEqual(result["lease_expires_at"], first["lease_expires_at"])

    def test_native_balloon_failure_has_exactly_two_attempts_and_no_fallback(self):
        first = self.acquire()
        attempts = []
        def timeout(name, *_):
            attempts.append(name)
            self.runner.instances[name]["config"]["limits.memory"] = "4096MiB"
            raise self.balloon_timeout(2048)
        self.runner.on_config_set = timeout
        self.error("resize_partial", self.call, "resize", memory_mib=2048, live_memory=True)
        self.assertEqual(len(attempts), 2)
        self.assertEqual(self.control._allocation_totals()["memory_mib"], 4096)
        self.assertEqual(self.runner.instances[first["instance_name"]]["status"], "Running")
        self.assertEqual(len(self.call("status")["last_error"]["native_attempts"]), 2)

    def test_completion_attempt_rechecks_boot_configuration_expiry_and_drain(self):
        for boundary in ("boot", "configuration", "expiry", "drain"):
            with self.subTest(boundary=boundary):
                first = self.acquire(boundary)
                attempts = []
                def fail(name, *_):
                    attempts.append(name)
                    item = self.runner.instances[name]
                    if boundary != "configuration": item["config"]["limits.memory"] = "4096MiB"
                    if boundary == "boot": item["boot_id"] = "00000000-0000-4000-8000-000000000002"
                    if boundary == "expiry": self.now = CONTROL.parse_time(first["lease_expires_at"])
                    if boundary == "drain": self.store.set_meta("draining", "1")
                    raise self.balloon_timeout(2048)
                self.runner.on_config_set = fail
                self.error("resize_partial", self.call, "resize", boundary, memory_mib=2048, live_memory=True)
                self.assertEqual(len(attempts), 1)
                self.runner.on_config_set = None
                self.resources.memory.return_value["available_bytes"] = 24*GIB
                self.store.set_meta("draining", "0")
                self.call("release", boundary)

    def test_growth_wrong_target_and_completed_balloon_are_not_retried(self):
        for boundary in ("growth", "wrong-target", "already-reached"):
            with self.subTest(boundary=boundary):
                first = self.acquire(boundary)
                target = 8192 if boundary == "growth" else 2048
                attempts = []
                def fail(name, *_):
                    attempts.append(name)
                    self.runner.instances[name]["config"]["limits.memory"] = "4096MiB"
                    error = self.balloon_timeout(1024 if boundary == "wrong-target" else target)
                    if boundary == "already-reached":
                        error.detail["stderr"] = error.detail["stderr"].replace("3482MiB", "2048MiB")
                    raise error
                self.runner.on_config_set = fail
                self.error("resize_partial", self.call, "resize", boundary, memory_mib=target, live_memory=True)
                self.assertEqual(len(attempts), 1)
                self.assertEqual(self.control._allocation_totals()["memory_mib"], max(4096, target))
                self.runner.on_config_set = None
                self.call("release", boundary)

    def test_expiry_during_native_update_does_not_reopen_or_commit_active_state(self):
        first = self.acquire()
        def expire(*_):
            self.now = CONTROL.parse_time(first["lease_expires_at"])
            self.control._expire_preempt({"task_id": first["task_id"], "generation": first["generation"]})
        self.runner.on_config_set = expire
        self.error("resize_partial", self.call, "resize", memory_mib=8192, live_memory=True)
        task = self.store.get_task(session_id="thread-a")
        self.assertEqual(task["state"], "closing")
        self.assertFalse(self.call("status")["effective_lease"])
        self.assertEqual(self.runner.instances[first["instance_name"]]["status"], "Stopped")
        self.assertEqual(self.control._allocation_totals()["memory_mib"], 8192)

    def test_invalid_modes_drain_and_short_window_do_not_modify_guest(self):
        first = self.acquire()
        for fields in ({"cpu": 2}, {"disk_gib": 20}, {"memory_mib": 2048, "cpu": 2},
                       {"memory_mib": 2048, "allow_restart": True}, {"memory_mib": 2048, "live_memory": "yes"}):
            self.error("invalid_request", self.call, "resize", **{"live_memory": True, **fields})
        self.call("drain-enter")
        self.error("draining", self.call, "resize", memory_mib=2048, live_memory=True)
        self.call("drain-exit")
        self.now = CONTROL.parse_time(first["lease_expires_at"]) - dt.timedelta(seconds=10)
        self.error("lease_window_short", self.call, "resize", memory_mib=2048, live_memory=True)
        self.assertEqual(self.runner.instances[first["instance_name"]]["config"]["limits.memory"], "4096MiB")

    def test_protocol_three_live_request_cannot_mutate_or_rewrite_pending_journal(self):
        self.acquire()
        request = self.request("resize", operation="old-live", memory_mib=2048, live_memory=True)
        request["protocol"] = 3
        record = {"outcome": "pending", "_request_digest": "old-digest"}
        self.store.record_operation("old-live", "resize", "thread-a", 1, record)
        before = len(self.runner.calls)
        self.error("protocol_mismatch", self.control.dispatch, request)
        self.assertEqual(self.store.operation("old-live"), record)
        self.assertEqual(self.changes(before), [])


class LiveCliTests(unittest.TestCase):
    def test_wire_flags_units_and_local_mode_rejection(self):
        with mock.patch.object(CLI, "mutation", return_value={"ok": True}) as mutation, \
             mock.patch("sys.stdout", new_callable=io.StringIO), mock.patch("sys.stderr", new_callable=io.StringIO):
            self.assertEqual(CLI.main(["resize", "--memory", "2GiB", "--live"]), 0)
            self.assertEqual(mutation.call_args.kwargs["fields"],
                             {"allow_restart": False, "live_memory": True, "memory_mib": 2048})
            mutation.reset_mock()
            for args in (["resize", "--live"], ["resize", "--cpu", "2", "--live"],
                         ["resize", "--memory", "2GiB", "--disk", "20GiB", "--live"]):
                self.assertEqual(CLI.main(args), 2)
            mutation.assert_not_called()
            with self.assertRaises(SystemExit):
                CLI.main(["resize", "--memory", "2GiB", "--live", "--allow-restart"])


if __name__ == "__main__": unittest.main()
