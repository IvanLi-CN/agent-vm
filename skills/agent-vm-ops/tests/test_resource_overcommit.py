"""Resource admission and protocol upgrade boundaries, without host probes."""
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import threading
import unittest
from unittest import mock

from test_native_agent_vm import CONTROL, CLI, Fixture, GIB, KEY


class CTObservationTests(unittest.TestCase):
    mount = "12 1 0:9 /meminfo /proc/meminfo rw - fuse.lxcfs lxcfs rw\n"

    def observe(self, values):
        reads = []
        def read(path):
            reads.append(path)
            if path not in values:
                raise CONTROL.ControlError("resource_unverified", "fixture metric missing")
            return values[path]
        with mock.patch.object(CONTROL.CTResources, "_read", side_effect=read):
            result = CONTROL.CTResources().memory()
        return result, reads

    def test_finite_cgroup_reclaims_inactive_file_and_bounds_lxcfs(self):
        values = {"/sys/fs/cgroup/memory.max": str(16*GIB), "/sys/fs/cgroup/memory.current": str(14*GIB),
                  "/sys/fs/cgroup/memory.stat": f"anon 1\ninactive_file {4*GIB}\n",
                  "/proc/self/mountinfo": self.mount,
                  "/proc/meminfo": f"MemTotal: {12*GIB//1024} kB\nMemAvailable: {3*GIB//1024} kB\n"}
        value, reads = self.observe(values)
        self.assertEqual(value["limit_bytes"], 12*GIB)
        self.assertEqual(value["available_bytes"], 3*GIB)
        self.assertEqual(value["sources"], ["cgroup-v2", "lxcfs"])
        self.assertEqual(set(reads), set(values))
        values["/proc/self/mountinfo"] = ""
        value, reads = self.observe(values)
        self.assertEqual(value["available_bytes"], 6*GIB)
        self.assertNotIn("/proc/meminfo", reads)

    def test_unlimited_cgroup_requires_verified_meminfo_before_reading_it(self):
        values = {"/sys/fs/cgroup/memory.max": "max", "/proc/self/mountinfo": self.mount,
                  "/proc/meminfo": "MemTotal: 25165824 kB\nMemAvailable: 16777216 kB\n"}
        value, reads = self.observe(values)
        self.assertEqual(value["limit_bytes"], 24*GIB)
        self.assertEqual(value["available_bytes"], 16*GIB)
        self.assertLess(reads.index("/proc/self/mountinfo"), reads.index("/proc/meminfo"))
        for mount in ("", self.mount.replace("fuse.lxcfs", "proc"), self.mount + self.mount):
            read = mock.Mock(side_effect=lambda path: {"/sys/fs/cgroup/memory.max": "max", "/proc/self/mountinfo": mount}[path])
            with self.subTest(mount=mount), mock.patch.object(CONTROL.CTResources, "_read", read):
                with self.assertRaises(CONTROL.ControlError): CONTROL.CTResources().memory()
                self.assertNotIn(mock.call("/proc/meminfo"), read.call_args_list)

    def test_invalid_memory_metrics_fail_closed(self):
        base = {"/sys/fs/cgroup/memory.max": str(4*GIB), "/sys/fs/cgroup/memory.current": str(3*GIB),
                "/sys/fs/cgroup/memory.stat": "inactive_file 0\n", "/proc/self/mountinfo": ""}
        changes = [{"/sys/fs/cgroup/memory.max": "0"}, {"/sys/fs/cgroup/memory.current": "-1"},
                   {"/sys/fs/cgroup/memory.stat": ""}, {"/sys/fs/cgroup/memory.stat": "inactive_file 1\ninactive_file 2\n"},
                   {"/sys/fs/cgroup/memory.stat": f"inactive_file {4*GIB}"},
                   {"/proc/self/mountinfo": self.mount, "/proc/meminfo": "MemTotal: 1 kB\nMemAvailable: 2 kB"}]
        for change in changes:
            with self.subTest(change=change), self.assertRaises(CONTROL.ControlError): self.observe({**base, **change})
        base["/sys/fs/cgroup/memory.current"] = str(6*GIB)
        self.assertEqual(self.observe(base)[0]["available_bytes"], 0)
        base["/sys/fs/cgroup/memory.current"] = "0"
        self.assertEqual(self.observe(base)[0]["available_bytes"], 4*GIB)

    def test_cpu_capacity_is_own_quota_and_visible_affinity(self):
        with mock.patch.object(CONTROL.os, "sched_getaffinity", return_value=set(range(16)), create=True), \
             mock.patch.object(CONTROL.CTResources, "_read", return_value="750000 100000") as read:
            self.assertEqual(CONTROL.CTResources().cpu(), 7)
            read.assert_called_once_with("/sys/fs/cgroup/cpu.max")


class AdmissionTests(Fixture):
    def test_default_ceilings_and_fractional_floor(self):
        self.config.update(cpu_budget=15, memory_budget_mib=12288)
        policy = self.control._resource_policy()
        self.assertEqual(policy["limits"], {"cpu": 60, "memory_mib": 49152})
        self.config.update(cpu_overcommit_ratio=1.1, memory_overcommit_ratio=1.01)
        self.assertEqual(self.control._resource_policy()["limits"], {"cpu": 16, "memory_mib": 12410})
        for value in (True, "4", 0.9, float("nan"), float("inf")):
            self.config["cpu_overcommit_ratio"] = value
            self.error("config_invalid", self.control._resource_policy)

    def test_base_budget_not_multiplied_ceiling_must_fit_ct(self):
        self.config.update(cpu_budget=15, memory_budget_mib=12288)
        self.resources.memory.return_value.update(limit_bytes=24*GIB)
        self.assertEqual(self.control._resource_policy()["limits"]["cpu"], 60)
        self.config["cpu_budget"] = 17
        self.error("resource_budget_invalid", self.acquire)
        self.assertEqual(self.runner.instances, {})

    def test_native_aggregate_quota_conflict_does_not_mutate_project(self):
        self.runner.project_limits = {"limits.cpu": "16", "limits.memory": "32GiB"}
        self.error("project_resource_limits_conflict", self.acquire)
        result = self.call("doctor")
        self.assertTrue(result["ready"])
        self.assertEqual(result["resources"]["error"]["detail"]["conflicting_limits"], self.runner.project_limits)
        self.assertFalse(any(args[:3] == ["incus", "project", "set"] for args, _, _ in self.runner.calls))

    def test_quotas_and_pressure_reject_before_reclaim_or_native_stop(self):
        active = self.acquire("active")
        cache = self.acquire("cache"); self.call("release", "cache")
        self.runner.free = 10*GIB
        self.config.update(cpu_overcommit_ratio=1, memory_overcommit_ratio=1)
        with mock.patch.object(self.control, "_reclaim_capacity") as reclaim:
            self.error("resource_insufficient", self.call, "resize", "active", cpu=8, allow_restart=True)
            self.error("resource_insufficient", self.call, "acquire", "new", public_key=KEY, memory_mib=32768)
            self.resources.memory.return_value["available_bytes"] = 0
            self.error("memory_pressure", self.call, "resize", "active", cpu=3, allow_restart=True)
            self.error("memory_pressure", self.acquire, "cache")
            reclaim.assert_not_called()
        self.assertEqual(self.runner.instances[active["instance_name"]]["status"], "Running")
        self.assertEqual(self.store.get_task(task_id=cache["task_id"])["state"], "cached")

    def test_pressure_and_failed_observation_do_not_block_existing_lease_operations(self):
        active = self.acquire()
        self.resources.memory.return_value["available_bytes"] = 0
        self.assertTrue(self.call("status")["admission"]["memory_pressure"])
        doctor = self.call("doctor")
        self.assertTrue(doctor["ready"])
        self.assertFalse(doctor["admission_ready"])
        self.assertEqual(self.acquire()["lease_expires_at"], active["lease_expires_at"])
        self.resources.memory.side_effect = CONTROL.ControlError("resource_unverified", "missing metric")
        self.assertFalse(self.call("status")["admission"]["verified"])
        self.call("renew")
        self.assertEqual(self.call("ssh-info")["generation"], active["generation"])
        self.assertEqual(self.call("release")["state"], "cached")
        self.error("resource_unverified", self.acquire)

    def test_stopped_cache_does_not_count_and_resize_is_not_double_counted(self):
        self.config.update(cpu_budget=4, cpu_overcommit_ratio=1, memory_overcommit_ratio=1)
        self.acquire("cache"); self.call("release", "cache")
        self.acquire("active")
        self.call("resize", "active", cpu=4, allow_restart=True)
        self.assertEqual(self.control._resource_diagnostics()["allocated_and_reserved"], {"cpu": 4, "memory_mib": 4096})
        self.error("resource_insufficient", self.acquire, "cache")

    def test_pre_native_resize_failure_releases_only_its_reservation(self):
        active = self.acquire()
        before = self.store.get_task(task_id=active["task_id"])
        with mock.patch.object(self.control, "_reclaim_capacity", side_effect=CONTROL.ControlError("capacity_insufficient", "fixture disk shortage")):
            self.error("capacity_insufficient", self.call, "resize", cpu=8, allow_restart=True)
        after = self.store.get_task(task_id=active["task_id"])
        self.assertEqual(after["resource_reservation_json"], before["resource_reservation_json"])
        self.assertEqual(after["lease_expires_at"], before["lease_expires_at"])
        self.assertEqual(self.runner.instances[active["instance_name"]]["status"], "Running")

    def test_concurrent_reservations_obey_atomic_total_and_conservative_max(self):
        self.config.update(cpu_budget=4, cpu_overcommit_ratio=1, memory_overcommit_ratio=1)
        barrier = threading.Barrier(2)
        results = []
        def reserve(session):
            store = CONTROL.StateStore(Path(self.config["state_db"]))
            try:
                control = CONTROL.Control(self.config, runner=self.runner, store=store, systemd=self.systemd,
                                          resources=self.resources, clock=lambda: self.now)
                task = control._base_task(session, 1)
                task["lease_expires_at"] = CONTROL.iso_time(self.now + dt.timedelta(minutes=30))
                barrier.wait(timeout=5)
                try: control._reserve_resources(task, {"cpu": 3, "memory_mib": 4096, "disk_gib": 20}); results.append("admitted")
                except CONTROL.ControlError as exc: results.append(exc.code)
            finally: store.close()
        threads = [threading.Thread(target=reserve, args=(name,)) for name in ("one", "two")]
        for thread in threads: thread.start()
        for thread in threads: thread.join(timeout=10); self.assertFalse(thread.is_alive())
        self.assertCountEqual(results, ["admitted", "resource_insufficient"])
        record = self.store.list_tasks()[0]
        self.store.update_task(record["task_id"], state="closing", resources_json=json.dumps({"cpu": 2, "memory_mib": 4096}))
        self.assertEqual(self.control._allocation_totals()["cpu"], 3)

    def test_expiry_during_resize_cannot_commit_success_or_reopen_guest(self):
        self.acquire()
        def expire(name):
            task = self.store.get_task(session_id="thread-a")
            self.now += dt.timedelta(minutes=31)
            self.control._expire_preempt({"task_id": task["task_id"], "generation": task["generation"]})
        self.runner.on_start = expire
        self.error("resize_partial", self.call, "resize", cpu=4, allow_restart=True)
        self.assertFalse(self.call("status")["effective_lease"])
        self.assertEqual(self.runner.instances[self.store.list_tasks()[0]["instance_name"]]["status"], "Stopped")


class CreationTests(Fixture):
    def test_custom_creation_active_reuse_and_mismatch_without_side_effects(self):
        first = self.call("acquire", public_key=KEY, cpu=8, memory_mib=12288, disk_gib=40)
        self.assertEqual(first["resources"], {"cpu": 8, "memory_mib": 12288, "disk_gib": 40})
        self.assertEqual(first["guest_filesystem"]["virtual_bytes"], 40*GIB)
        with mock.patch.object(self.control.incus, "stop") as stop:
            same = self.call("acquire", public_key=KEY, cpu=8, memory_mib=12288, disk_gib=40)
            self.assertEqual(same["lease_expires_at"], first["lease_expires_at"])
            self.error("resource_config_mismatch", self.call, "acquire", public_key=KEY, cpu=4)
            self.error("resource_config_mismatch", self.call, "acquire", public_key=KEY, disk_gib=20)
            stop.assert_not_called()

    def test_cache_omitted_retains_and_explicit_fields_adjust_before_start(self):
        self.call("acquire", public_key=KEY, cpu=8, memory_mib=12288, disk_gib=40)
        self.call("release")
        resumed = self.acquire()
        self.assertEqual(resumed["resources"], {"cpu": 8, "memory_mib": 12288, "disk_gib": 40})
        self.call("release")
        self.error("resize_not_growth", self.call, "acquire", public_key=KEY, disk_gib=20)
        adjusted = self.call("acquire", public_key=KEY, cpu=4, memory_mib=8192, disk_gib=60)
        self.assertEqual(adjusted["resources"], {"cpu": 4, "memory_mib": 8192, "disk_gib": 60})
        self.assertEqual(adjusted["guest_filesystem"]["virtual_bytes"], 60*GIB)
        self.assertIsNone(adjusted["inactive_since"])

    def test_failed_resume_does_not_refresh_cache_age(self):
        first = self.acquire(); released = self.call("release")
        self.now += dt.timedelta(hours=1)
        self.runner.failure = lambda args: len(args) > 3 and args[3] == "start"
        self.error("command_failed", self.acquire)
        self.assertEqual(self.call("status")["inactive_since"], released["inactive_since"])
        self.runner.failure = None
        resumed = self.acquire()
        self.now += dt.timedelta(minutes=1)
        self.assertNotEqual(self.call("release")["inactive_since"], released["inactive_since"])
        self.assertEqual(resumed["task_id"], first["task_id"])

    def test_status_reports_unexpected_stop_without_oom_guess_or_restart(self):
        first = self.acquire()
        self.runner.instances[first["instance_name"]]["status"] = "Stopped"
        with mock.patch.object(self.control.incus, "start") as start:
            status = self.call("status")
            self.assertTrue(status["effective_lease"])
            self.assertFalse(status["guest_available"])
            self.assertEqual(status["guest_error"]["cause"], "unknown")
            self.assertFalse(self.acquire()["guest_available"])
            start.assert_not_called()

    def test_old_protocol_and_operation_digest_cannot_be_rewritten(self):
        request = self.request("acquire", public_key=KEY, cpu=8)
        request["protocol"] = 1
        self.error("protocol_mismatch", self.control.dispatch, request)
        digest = hashlib.sha256(json.dumps(request, sort_keys=True).encode()).hexdigest()
        self.store.record_operation(request["operation_id"], "acquire", "thread-a", 0,
                                    {"outcome": "pending", "_request_digest": digest})
        request["protocol"] = CONTROL.PROTOCOL_VERSION
        self.error("operation_conflict", self.control.dispatch, request)
        self.assertEqual(self.store.operation(request["operation_id"])["_request_digest"], digest)
        self.assertEqual(self.runner.instances, {})


class AcquireCliTests(unittest.TestCase):
    def test_acquire_uses_same_unit_parser_and_sends_explicit_fields(self):
        args = CLI.build_parser().parse_args(["acquire", "--cpu", "8", "--memory", "12GiB", "--disk", "40GiB"])
        with mock.patch.object(CLI, "session_id", return_value="thread"), \
             mock.patch.object(CLI, "make_ssh_key", return_value=(Path("/tmp/key"), KEY)), \
             mock.patch.object(CLI, "control_request", return_value={"result": {"generation": 0}}), \
             mock.patch.object(CLI, "write_session_state"), mock.patch.object(CLI, "json_output"), \
             mock.patch.object(CLI, "mutation", return_value={"result": {"effective_lease": True, "task_id": "avm-test", "generation": 1}}) as mutation:
            CLI.command_acquire(args)
            self.assertEqual(mutation.call_args.kwargs["fields"], {"public_key": KEY, "cpu": 8, "memory_mib": 12288, "disk_gib": 40})

    def test_protocol_one_local_journal_not_replayed_after_upgrade(self):
        with tempfile.TemporaryDirectory() as directory, mock.patch.object(CLI, "LOCAL_STATE_ROOT", Path(directory)):
            op = "original-operation"
            path = CLI.local_session_dir("thread") / "operations" / (hashlib.sha256(op.encode()).hexdigest() + ".json")
            path.parent.mkdir(parents=True)
            original = {"request": {"protocol": 1, "action": "acquire"}, "outcome": "pending"}
            path.write_text(json.dumps(original))
            with mock.patch.object(CLI.subprocess, "run") as run, self.assertRaises(CLI.CliError) as error:
                CLI.control_request("acquire", session="thread", generation=0, write=True, operation_id=op)
            self.assertEqual(error.exception.code, "operation_conflict")
            self.assertEqual(json.loads(path.read_text()), original)
            run.assert_not_called()


if __name__ == "__main__": unittest.main()
