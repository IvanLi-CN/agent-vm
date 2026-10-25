"""Resource admission and protocol upgrade boundaries, without host probes."""
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sqlite3
import tempfile
import threading
import unittest
from unittest import mock

from test_native_agent_vm import CONTROL, CLI, Fixture, GIB, KEY


class SQLiteStateTests(unittest.TestCase):
    def test_corrupt_database_initialization_is_structured(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "state.sqlite3"
            path.write_bytes(b"not a sqlite database")
            with self.assertRaises(CONTROL.ControlError) as failure:
                CONTROL.StateStore(path)
        self.assertEqual(failure.exception.code, "sqlite_unavailable")
        self.assertEqual(failure.exception.detail["phase"], "wal-init")
        self.assertIn("sqlite_name", failure.exception.detail)
        self.assertIs(failure.exception.detail["state_committed"], False)

    def test_commit_failure_reports_unknown_commit_state(self):
        class FailingCommit:
            def execute(self, *_args):
                return None

            def commit(self):
                raise sqlite3.OperationalError("database or disk is full")

            def rollback(self):
                return None

        store = object.__new__(CONTROL.StateStore)
        store._lock = threading.RLock()
        store.connection = FailingCommit()
        store.depth = 0
        with self.assertRaises(CONTROL.ControlError) as failure:
            with store.transaction():
                pass
        self.assertEqual(failure.exception.code, "sqlite_unavailable")
        self.assertEqual(failure.exception.detail["phase"], "commit")
        self.assertIsNone(failure.exception.detail["state_committed"])

    def test_rollback_failure_reports_unknown_state_and_unusable_connection(self):
        class FailingRollback:
            def __init__(self):
                self.calls = 0

            def execute(self, *_args):
                self.calls += 1
                if self.calls == 2:
                    raise sqlite3.OperationalError("database or disk is full")
                return None

            def rollback(self):
                raise sqlite3.OperationalError("rollback failed")

        store = object.__new__(CONTROL.StateStore)
        store._lock = threading.RLock()
        store.connection = FailingRollback()
        store.depth = 0
        store._unusable = False
        with self.assertRaises(CONTROL.ControlError) as failure:
            with store.transaction():
                store.connection.execute("INSERT")
        self.assertIsNone(failure.exception.detail["state_committed"])
        self.assertIn("rollback_error", failure.exception.detail)
        self.assertTrue(store._unusable)

    def test_non_sqlite_failure_with_rollback_error_is_structured(self):
        class FailingRollback:
            def execute(self, *_args):
                return None

            def rollback(self):
                raise sqlite3.OperationalError("rollback failed")

        store = object.__new__(CONTROL.StateStore)
        store._lock = threading.RLock()
        store.connection = FailingRollback()
        store.depth = 0
        store._unusable = False
        with self.assertRaises(CONTROL.ControlError) as failure:
            with store.transaction():
                raise CONTROL.ControlError("native_failed", "native operation failed")
        self.assertEqual(failure.exception.code, "sqlite_unavailable")
        self.assertIsNone(failure.exception.detail["state_committed"])
        self.assertEqual(failure.exception.detail["original_error"]["type"], "ControlError")
        self.assertTrue(store._unusable)


class CTObservationTests(unittest.TestCase):
    mount = "12 1 0:9 /meminfo /proc/meminfo rw - fuse.lxcfs lxcfs rw\n"
    cgroup_mount = "42 35 0:28 / /sys/fs/cgroup rw - cgroup2 cgroup rw\n"
    cgroup_path = "0::/\n"

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
        values = {"/sys/fs/cgroup/memory.max": str(16*GIB), "/sys/fs/cgroup/memory.current": str(10*GIB),
                  "/sys/fs/cgroup/memory.stat": f"anon 1\ninactive_file {4*GIB}\n",
                  "/proc/self/mountinfo": self.mount,
                  "/proc/meminfo": f"MemTotal: {12*GIB//1024} kB\nMemAvailable: {3*GIB//1024} kB\n"}
        value, reads = self.observe(values)
        self.assertEqual(value["limit_bytes"], 12*GIB)
        self.assertEqual(value["available_bytes"], 3*GIB)
        self.assertEqual(value["sources"], ["cgroup-v2", "lxcfs"])
        self.assertTrue(set(values).issubset(reads))
        values["/proc/self/mountinfo"] = ""
        value, reads = self.observe(values)
        self.assertEqual(value["available_bytes"], 10*GIB)
        self.assertNotIn("/proc/meminfo", reads)

    def test_finite_cgroup_rejects_current_above_lxcfs_capacity(self):
        values = {"/sys/fs/cgroup/memory.max": str(16*GIB), "/sys/fs/cgroup/memory.current": str(14*GIB),
                  "/sys/fs/cgroup/memory.stat": f"inactive_file {1*GIB}\n",
                  "/proc/self/mountinfo": self.mount,
                  "/proc/meminfo": f"MemTotal: {12*GIB//1024} kB\nMemAvailable: {3*GIB//1024} kB\n"}
        with self.assertRaises(CONTROL.ControlError): self.observe(values)

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

    def test_unlimited_cgroup_rejects_current_above_lxcfs_capacity(self):
        values = {"/sys/fs/cgroup/memory.max": "max", "/sys/fs/cgroup/memory.current": str(25*GIB),
                  "/proc/self/mountinfo": self.mount,
                  "/proc/meminfo": f"MemTotal: {24*GIB//1024} kB\nMemAvailable: {16*GIB//1024} kB\n"}
        with self.assertRaises(CONTROL.ControlError): self.observe(values)

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
        with self.assertRaises(CONTROL.ControlError): self.observe(base)
        base["/sys/fs/cgroup/memory.current"] = "0"
        self.assertEqual(self.observe(base)[0]["available_bytes"], 4*GIB)

    def test_memory_observation_includes_current_and_oom_events_when_available(self):
        values = {"/sys/fs/cgroup/memory.max": str(4*GIB), "/sys/fs/cgroup/memory.current": str(3*GIB),
                  "/sys/fs/cgroup/memory.stat": "inactive_file 0\n", "/sys/fs/cgroup/memory.events": "oom 2\noom_kill 1\n" ,
                  "/proc/self/mountinfo": ""}
        value, _ = self.observe(values)
        self.assertEqual(value["current_bytes"], 3*GIB)
        self.assertEqual(value["oom_events"], {"oom": 2, "oom_kill": 1})

    def test_malformed_optional_oom_events_do_not_block_verified_capacity(self):
        values = {"/sys/fs/cgroup/memory.max": str(4*GIB), "/sys/fs/cgroup/memory.current": str(3*GIB),
                  "/sys/fs/cgroup/memory.stat": "inactive_file 0\n", "/sys/fs/cgroup/memory.events": "oom invalid\n",
                  "/proc/self/mountinfo": ""}
        value, _ = self.observe(values)
        self.assertEqual(value["limit_bytes"], 4*GIB)
        self.assertIsNone(value["oom_events"])

    def test_duplicate_optional_oom_events_are_unavailable(self):
        values = {"/sys/fs/cgroup/memory.max": str(4*GIB), "/sys/fs/cgroup/memory.current": str(3*GIB),
                  "/sys/fs/cgroup/memory.stat": "inactive_file 0\n", "/sys/fs/cgroup/memory.events": "oom 1\noom 2\n",
                  "/proc/self/mountinfo": ""}
        self.assertIsNone(self.observe(values)[0]["oom_events"])

    def test_oversized_ct_metric_is_structured_as_unverified(self):
        with self.assertRaises(CONTROL.ControlError) as failure:
            CONTROL.CTResources._unsigned("9" * 5000, "memory.current")
        self.assertEqual(failure.exception.code, "resource_unverified")

    def test_cpu_capacity_is_own_quota_and_visible_affinity(self):
        def read(path):
            if path == "/proc/self/mountinfo":
                return self.cgroup_mount
            if path == "/proc/self/cgroup":
                return self.cgroup_path
            return "750000 100000"

        with mock.patch.object(CONTROL.os, "sched_getaffinity", return_value=set(range(16)), create=True), \
             mock.patch.object(CONTROL.CTResources, "_read", side_effect=read) as read:
            self.assertEqual(CONTROL.CTResources().cpu(), 7)
            self.assertEqual(
                read.call_args_list,
                [mock.call("/proc/self/mountinfo"), mock.call("/proc/self/cgroup"),
                 mock.call("/sys/fs/cgroup/cpu.max")],
            )

    def test_cpu_capacity_reads_current_nested_cgroup_quota(self):
        nested = "0::/system.slice/agent-vm\n"

        def read(path):
            if path == "/proc/self/mountinfo":
                return self.cgroup_mount
            if path == "/proc/self/cgroup":
                return nested
            if path == "/sys/fs/cgroup/system.slice/agent-vm/cpu.max":
                return "500000 100000"
            if path == "/sys/fs/cgroup/system.slice/cpu.max":
                return "max 100000"
            if path == "/sys/fs/cgroup/cpu.max":
                return "max 100000"
            raise AssertionError(path)

        with mock.patch.object(CONTROL.os, "sched_getaffinity", return_value=set(range(16)), create=True), \
             mock.patch.object(CONTROL.CTResources, "_read", side_effect=read):
            self.assertEqual(CONTROL.CTResources().cpu(), 5)

    def test_cpu_capacity_includes_finite_ancestor_when_child_is_unlimited(self):
        nested = "0::/system.slice/agent-vm\n"

        def read(path):
            if path == "/proc/self/mountinfo":
                return self.cgroup_mount
            if path == "/proc/self/cgroup":
                return nested
            if path == "/sys/fs/cgroup/system.slice/agent-vm/cpu.max":
                return "max 100000"
            if path == "/sys/fs/cgroup/system.slice/cpu.max":
                return "1200000 100000"
            if path == "/sys/fs/cgroup/cpu.max":
                return "max 100000"
            raise AssertionError(path)

        with mock.patch.object(CONTROL.os, "sched_getaffinity", return_value=set(range(16)), create=True), \
             mock.patch.object(CONTROL.CTResources, "_read", side_effect=read):
            self.assertEqual(CONTROL.CTResources().cpu(), 12)

    def test_cpu_pressure_requires_verified_some_and_full_psi(self):
        raw = "some avg10=12.34 avg60=5.00 avg300=1.00 total=100\nfull avg10=7.89 avg60=3.00 avg300=0.50 total=50\n"
        def read(path):
            if path == "/proc/self/mountinfo":
                return self.cgroup_mount
            if path == "/proc/self/cgroup":
                return self.cgroup_path
            return raw
        with mock.patch.object(CONTROL.CTResources, "_read", side_effect=read) as read:
            result = CONTROL.CTResources().cpu_pressure()
        self.assertEqual(result["scope"], "ct-cpu-pressure")
        self.assertEqual(result["sources"], ["cgroupfs"])
        self.assertEqual(result["mountpoint"], "/sys/fs/cgroup")
        self.assertEqual(result["mount_root"], "/")
        self.assertEqual(result["filesystem"], "cgroup2")
        self.assertEqual(result["cgroup_path"], "/")
        self.assertEqual(result["metric"], "/sys/fs/cgroup/cpu.pressure")
        self.assertEqual(result["full"]["avg10"], 7.89)
        self.assertEqual(result["some"]["avg10"], 12.34)
        self.assertEqual(read.call_args_list, [mock.call("/proc/self/mountinfo"), mock.call("/proc/self/cgroup"), mock.call("/sys/fs/cgroup/cpu.pressure")])

        with mock.patch.object(CONTROL.CTResources, "_read", side_effect=lambda path: self.cgroup_mount if path == "/proc/self/mountinfo" else self.cgroup_path if path == "/proc/self/cgroup" else "some avg10=1\n"):
            with self.assertRaises(CONTROL.ControlError) as failure:
                CONTROL.CTResources().cpu_pressure()
        self.assertEqual(failure.exception.code, "resource_unverified")

        contradictory = "some avg10=1 avg60=1 avg300=1 total=1\nfull avg10=2 avg60=1 avg300=1 total=1\n"
        with mock.patch.object(CONTROL.CTResources, "_read", side_effect=lambda path: self.cgroup_mount if path == "/proc/self/mountinfo" else self.cgroup_path if path == "/proc/self/cgroup" else contradictory):
            with self.assertRaises(CONTROL.ControlError) as failure:
                CONTROL.CTResources().cpu_pressure()
        self.assertEqual(failure.exception.code, "resource_unverified")

        contaminated = raw + "unexpected avg10=0 avg60=0 avg300=0 total=0\n"
        with mock.patch.object(CONTROL.CTResources, "_read", side_effect=lambda path: self.cgroup_mount if path == "/proc/self/mountinfo" else self.cgroup_path if path == "/proc/self/cgroup" else contaminated):
            with self.assertRaises(CONTROL.ControlError) as failure:
                CONTROL.CTResources().cpu_pressure()
        self.assertEqual(failure.exception.code, "resource_unverified")

        malformed_total = raw.replace("total=100", "total=1.5")
        with mock.patch.object(CONTROL.CTResources, "_read", side_effect=lambda path: self.cgroup_mount if path == "/proc/self/mountinfo" else self.cgroup_path if path == "/proc/self/cgroup" else malformed_total):
            with self.assertRaises(CONTROL.ControlError) as failure:
                CONTROL.CTResources().cpu_pressure()
        self.assertEqual(failure.exception.code, "resource_unverified")

        with mock.patch.object(CONTROL.CTResources, "_read", side_effect=lambda path: self.cgroup_mount.replace("/ /sys", "/nested /sys") if path == "/proc/self/mountinfo" else self.cgroup_path):
            with self.assertRaises(CONTROL.ControlError) as failure:
                CONTROL.CTResources().cpu_pressure()
        self.assertEqual(failure.exception.code, "resource_unverified")

        with mock.patch.object(CONTROL.CTResources, "_read", side_effect=lambda path: self.cgroup_mount if path == "/proc/self/mountinfo" else "0::/slice\n1:name=/legacy\n" if path == "/proc/self/cgroup" else raw):
            with self.assertRaises(CONTROL.ControlError) as failure:
                CONTROL.CTResources().cpu_pressure()
        self.assertEqual(failure.exception.code, "resource_unverified")

        with mock.patch.object(CONTROL.CTResources, "_read", return_value=""):
            with self.assertRaises(CONTROL.ControlError) as failure:
                CONTROL.CTResources().cpu_pressure()
        self.assertEqual(failure.exception.code, "resource_unverified")


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

    def test_logical_quota_rejects_but_pressure_does_not_block_new_demand(self):
        active = self.acquire("active")
        cache = self.acquire("cache"); self.call("release", "cache")
        self.runner.free = 10*GIB
        self.config.update(cpu_overcommit_ratio=1, memory_overcommit_ratio=1)
        with mock.patch.object(self.control, "_reclaim_capacity") as reclaim:
            reclaim.return_value = []
            self.error("resource_insufficient", self.call, "resize", "active", cpu=8, allow_restart=True)
            self.error("resource_insufficient", self.call, "acquire", "new", public_key=KEY, memory_mib=32768)
            reclaim.assert_not_called()
            self.runner.free = 80*GIB
            self.resources.memory.return_value["available_bytes"] = 0
            self.assertEqual(self.call("resize", "active", cpu=3, allow_restart=True)["resources"]["cpu"], 3)
        self.assertEqual(self.runner.instances[active["instance_name"]]["status"], "Running")
        self.assertEqual(self.store.get_task(task_id=cache["task_id"])["state"], "cached")

    def test_low_memory_pressure_does_not_block_new_logical_reservation(self):
        self.config.update(memory_budget_mib=32768, memory_overcommit_ratio=2)
        self.resources.memory.return_value["available_bytes"] = 0
        self.assertTrue(self.control._resource_diagnostics()["memory_pressure"])
        result = self.acquire("new")
        self.assertEqual(result["state"], "active")
        self.assertEqual(self.control._resource_diagnostics()["remaining"]["memory_mib"], 61440)

    def test_cpu_startup_pressure_blocks_only_new_boot_when_physically_overcommitted(self):
        self.config.update(cpu_budget=15, cpu_overcommit_ratio=4)
        self.resources.cpu_pressure.return_value = {
            "scope": "ct-cpu-pressure",
            "sources": ["cgroupfs"],
            "mountpoint": "/sys/fs/cgroup",
            "mount_root": "/",
            "filesystem": "cgroup2",
            "cgroup_path": "/",
            "metric": "/sys/fs/cgroup/cpu.pressure",
            "some": {"avg10": 55.12, "avg60": 49.12, "avg300": 49.51, "total": 100.0},
            "full": {"avg10": 45.67, "avg60": 39.88, "avg300": 37.85, "total": 50.0},
        }
        base = self.call("acquire", "base", public_key=KEY, cpu=16)
        with self.assertRaises(CONTROL.ControlError) as failure:
            self.call("acquire", "blocked", public_key=KEY, cpu=2)
        self.assertEqual(failure.exception.code, "resource_pressure")
        self.assertEqual(failure.exception.detail["projected_cpu"], 18)
        self.assertTrue(failure.exception.detail["physically_oversubscribed"])
        self.assertEqual(self.runner.instances[base["instance_name"]]["status"], "Running")
        self.assertEqual(len(self.runner.instances), 1)

        self.resources.cpu_pressure.return_value["full"]["avg10"] = 0.0
        admitted = self.call("acquire", "after", public_key=KEY, cpu=2)
        self.assertEqual(admitted["resources"]["cpu"], 2)

    def test_cpu_startup_pressure_threshold_is_inclusive(self):
        self.config.update(cpu_budget=15, cpu_overcommit_ratio=4, cpu_startup_pressure_full_avg10_percent=45.67)
        self.resources.cpu_pressure.return_value["some"]["avg10"] = 45.67
        self.resources.cpu_pressure.return_value["full"]["avg10"] = 45.67
        self.call("acquire", "base", public_key=KEY, cpu=16)
        with self.assertRaises(CONTROL.ControlError) as failure:
            self.call("acquire", "equal", public_key=KEY, cpu=2)
        self.assertEqual(failure.exception.code, "resource_pressure")

    def test_cached_resume_pressure_projection_uses_target_cpu(self):
        self.config.update(cpu_overcommit_ratio=8)
        self.call("acquire", "active", public_key=KEY, cpu=11)
        self.call("acquire", "cache", public_key=KEY, cpu=8)
        self.call("release", "cache")
        self.resources.cpu_pressure.return_value["some"]["avg10"] = 55.12
        self.resources.cpu_pressure.return_value["full"]["avg10"] = 45.67

        resumed = self.call("acquire", "cache", public_key=KEY, cpu=4)

        self.assertEqual(resumed["resources"]["cpu"], 4)
        self.assertEqual(self.runner.instances[resumed["instance_name"]]["config"]["limits.cpu"], "4")

    def test_unverified_cpu_pressure_blocks_new_startup(self):
        self.resources.cpu_pressure.return_value = {
            "scope": "host-cpu-pressure",
            "sources": ["procfs"],
            "mount_root": "/",
            "cgroup_path": "/",
            "metric": "/sys/fs/cgroup/cpu.pressure",
            "some": {"avg10": 0.0, "avg60": 0.0, "avg300": 0.0, "total": 0.0},
            "full": {"avg10": 0.0, "avg60": 0.0, "avg300": 0.0, "total": 0.0},
        }
        self.error("resource_unverified", self.acquire, "unverified")
        self.assertEqual(self.runner.instances, {})

    def test_restart_resize_uses_target_cpu_for_startup_projection(self):
        self.config.update(cpu_budget=15, cpu_overcommit_ratio=2)
        base = self.call("acquire", "base", public_key=KEY, cpu=16)
        self.call("acquire", "other", public_key=KEY, cpu=14)
        self.resources.cpu_pressure.return_value["some"]["avg10"] = 55.12
        self.resources.cpu_pressure.return_value["full"]["avg10"] = 45.67

        result = self.call("resize", "base", cpu=2, allow_restart=True)

        self.assertEqual(result["resources"]["cpu"], 2)
        self.assertEqual(self.runner.instances[base["instance_name"]]["config"]["limits.cpu"], "2")

    def test_cpu_startup_pressure_checks_restart_before_stopping_guest(self):
        self.config.update(cpu_budget=15, cpu_overcommit_ratio=4)
        base = self.call("acquire", "base", public_key=KEY, cpu=16)
        self.call("acquire", "other", public_key=KEY, cpu=2)
        self.resources.cpu_pressure.return_value["some"]["avg10"] = 55.12
        self.resources.cpu_pressure.return_value["full"]["avg10"] = 45.67
        stop_calls = sum(args[3:4] == ["stop"] for args, _, _ in self.runner.calls)
        with self.assertRaises(CONTROL.ControlError) as failure:
            self.call("restart", "base")
        self.assertEqual(failure.exception.code, "resource_pressure")
        self.assertEqual(sum(args[3:4] == ["stop"] for args, _, _ in self.runner.calls), stop_calls)
        self.assertEqual(self.runner.instances[base["instance_name"]]["status"], "Running")

    def test_pressure_and_failed_observation_do_not_block_existing_lease_operations(self):
        active = self.acquire()
        self.resources.memory.return_value["available_bytes"] = 0
        self.assertTrue(self.call("status")["admission"]["memory_pressure"])
        doctor = self.call("doctor")
        self.assertTrue(doctor["ready"])
        self.assertTrue(doctor["admission_ready"])
        self.assertEqual(self.acquire()["lease_expires_at"], active["lease_expires_at"])
        self.resources.memory.side_effect = CONTROL.ControlError("resource_unverified", "missing metric")
        self.assertFalse(self.call("status")["admission"]["verified"])
        metrics = self.call("admin-metrics")
        self.assertIsNone(metrics["ct_memory"]["pressure"])
        self.assertFalse(metrics["resources"]["verified"])
        self.assertEqual(metrics["resources"]["error"]["code"], "resource_unverified")
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
        diagnostics = self.control._resource_diagnostics()
        self.assertEqual(diagnostics["allocated"], {"cpu": 4, "memory_mib": 4096})
        self.assertEqual(diagnostics["reserved"], {"cpu": 0, "memory_mib": 0})
        self.error("resource_insufficient", self.acquire, "cache")

    def test_deployment_memory_policy_is_32_gib_visible_and_64_gib_logical(self):
        self.config.update(memory_budget_mib=32768, memory_overcommit_ratio=2)
        policy = self.control._resource_policy()
        self.assertEqual(policy["ct_capacity"]["memory_mib"], 32768)
        self.assertEqual(policy["limits"]["memory_mib"], 65536)
        self.assertEqual(self.control._resource_diagnostics()["remaining"]["memory_mib"], 65536)

    def test_admin_metrics_is_read_only_and_separates_pressure_from_admission(self):
        self.config.update(memory_budget_mib=32768, memory_overcommit_ratio=2)
        self.acquire()
        self.resources.memory.return_value["available_bytes"] = 0
        before = self.store.list_tasks()
        result = self.call("admin-metrics")
        self.assertEqual(CONTROL.parse_time(result["observed_at"]), self.now)
        self.assertTrue({
            "observed_at", "up", "draining", "admission_ready", "reclaim_required", "resources",
            "capacity", "disk_admission", "ct_cpu", "ct_memory", "instances", "task_states", "operations",
            "pending_operations", "image_unpack_failures", "warnings", "warning_counts", "doctor",
        }.issubset(result))
        self.assertTrue(result["up"])
        self.assertTrue(result["admission_ready"])
        self.assertTrue(result["ct_memory"]["pressure"])
        self.assertEqual(result["ct_memory"]["capacity_bytes"], 32 * GIB)
        self.assertIn("current_bytes", result["ct_memory"])
        self.assertIn("available_bytes", result["ct_memory"])
        self.assertIn("pressure_threshold_bytes", result["ct_memory"])
        self.assertIn("oom_events", result["ct_memory"])
        self.assertEqual(result["ct_cpu"]["capacity_vcpu"], 16)
        self.assertEqual(result["ct_cpu"]["pressure"]["full"]["avg10"], 0.0)
        self.assertEqual(result["resources"]["allocated"]["memory_mib"], 4096)
        self.assertEqual(result["resources"]["reserved"]["memory_mib"], 0)
        self.assertEqual(result["resources"]["allocated_and_reserved"]["memory_mib"], 4096)
        self.assertEqual(result["resources"]["limits"]["memory_mib"], 65536)
        self.assertEqual(result["resources"]["remaining"]["memory_mib"], 61440)
        self.assertIn("layers", result["capacity"])
        self.assertTrue(result["disk_admission"]["verified"])
        self.assertIn("physical_admission", result["disk_admission"])
        self.assertIn("admission", result["instances"])
        self.assertEqual(result["task_states"]["active"], 1)
        self.assertEqual(self.store.list_tasks(), before)

    def test_admin_metrics_reports_pending_operation_journals(self):
        request = self.request("acquire", operation="metrics-pending", public_key=KEY)
        digest = hashlib.sha256(json.dumps(request, sort_keys=True).encode()).hexdigest()
        self.store.record_operation(
            request["operation_id"], "acquire", request["session_id"], request["generation"],
            {"outcome": "pending", "_request_digest": digest},
        )
        pending = self.call("admin-metrics")["pending_operations"]
        self.assertEqual(pending[0]["operation_id"], request["operation_id"])
        self.assertEqual(pending[0]["action"], "acquire")

    def test_unknown_task_state_blocks_status_release_and_destroy(self):
        active = self.acquire("corrupt")
        self.store.update_task(active["task_id"], state="mystery")
        for action in ("status", "release", "destroy"):
            with self.subTest(action=action):
                self.error("resource_unverified", self.call, action, "corrupt")
                self.assertEqual(self.store.get_task(task_id=active["task_id"])["state"], "mystery")

    def test_resource_failure_preserves_verified_memory_pressure(self):
        self.resources.memory.return_value["available_bytes"] = 0
        with mock.patch.object(self.control, "_disk_admission_diagnostics", return_value={
            "verified": False,
            "error": {"code": "resource_unverified", "message": "disk observation failed"},
        }):
            result = self.control._resource_diagnostics()
        self.assertFalse(result["verified"])
        self.assertTrue(result["memory_pressure"])
        self.assertEqual(result["memory"]["available_bytes"], 0)
        self.assertIsNone(result["remaining"])

    def test_admin_metrics_keeps_reclaim_unknown_when_disk_observation_is_unverified(self):
        with mock.patch.object(self.control, "_disk_admission_diagnostics", return_value={
            "verified": False,
            "error": {"code": "resource_unverified", "message": "disk observation failed"},
        }):
            result = self.call("admin-metrics")
        self.assertIsNone(result["reclaim_required"])

    def test_native_resource_drift_fails_closed_for_new_admission(self):
        active = self.acquire()
        instance = self.runner.instances[active["instance_name"]]
        for field, value in (("limits.cpu", "8"), ("limits.memory", "8192MiB")):
            with self.subTest(field=field):
                instance["config"][field] = value
                doctor = self.call("doctor")
                self.assertFalse(doctor["resources"]["verified"])
                self.assertEqual(doctor["resources"]["error"]["code"], "resource_unverified")
                before = len(self.runner.calls)
                names = set(self.runner.instances)
                self.error("resource_unverified", self.acquire, "new")
                self.assertEqual(set(self.runner.instances), names)
                self.assertFalse(any(args[3:4] in (["init"], ["start"], ["config"], ["device"])
                                     for args, _, _ in self.runner.calls[before:]))
                instance["config"][field] = {"limits.cpu": "2", "limits.memory": "4096MiB"}[field]
        instance["expanded_devices"]["root"]["size"] = "40GiB"
        doctor = self.call("doctor")
        self.assertFalse(doctor["resources"]["verified"])
        self.assertEqual(doctor["resources"]["error"]["code"], "resource_unverified")
        before = len(self.runner.calls)
        names = set(self.runner.instances)
        self.error("resource_unverified", self.acquire, "new")
        self.assertEqual(set(self.runner.instances), names)
        self.assertFalse(any(args[3:4] in (["init"], ["start"], ["config"], ["device"])
                             for args, _, _ in self.runner.calls[before:]))

    def test_cached_resume_rejects_network_isolation_drift(self):
        cached = self.acquire("cache")
        self.call("release", "cache")
        instance = self.runner.instances[cached["instance_name"]]
        instance["expanded_devices"]["eth0"]["security.acls"] = "unmanaged-acl"
        self.error("ownership_mismatch", self.acquire, "cache")
        self.assertEqual(instance["status"], "Stopped")

    def test_restart_rejects_stopped_native_resource_drift(self):
        active = self.acquire()
        instance = self.runner.instances[active["instance_name"]]
        instance["status"] = "Stopped"
        for field, value in (("limits.cpu", "8"), ("limits.memory", "8192MiB")):
            with self.subTest(field=field):
                instance["config"][field] = value
                before = len(self.runner.calls)
                self.error("resource_unverified", self.call, "restart")
                self.assertFalse(any(args[3:4] in (["stop"], ["start"]) for args, _, _ in self.runner.calls[before:]))
                instance["config"][field] = {"limits.cpu": "2", "limits.memory": "4096MiB"}[field]
        instance["expanded_devices"]["root"]["size"] = "40GiB"
        before = len(self.runner.calls)
        self.error("resource_unverified", self.call, "restart")
        self.assertFalse(any(args[3:4] in (["stop"], ["start"]) for args, _, _ in self.runner.calls[before:]))

    def test_restart_rejects_network_isolation_drift_before_native_effect(self):
        active = self.acquire()
        instance = self.runner.instances[active["instance_name"]]
        instance["status"] = "Stopped"
        instance["expanded_devices"]["eth0"]["security.acls"] = "unmanaged-acl"
        before = len(self.runner.calls)
        self.error("ownership_mismatch", self.call, "restart")
        self.assertFalse(any(args[3:4] in (["stop"], ["start"]) for args, _, _ in self.runner.calls[before:]))

    def test_drain_exit_rejects_active_network_isolation_drift(self):
        active = self.acquire()
        self.store.set_meta("draining", "1")
        self.runner.instances[active["instance_name"]]["expanded_devices"]["eth0"]["security.acls"] = "unmanaged-acl"
        self.error("ownership_mismatch", self.control.drain_exit, {}, None)
        self.assertEqual(self.store.get_meta("draining"), "1")

    def test_corrupt_disk_allocation_fails_closed_for_new_admission(self):
        active = self.acquire()
        self.store.update_task(active["task_id"], resources_json=json.dumps({"cpu": 2, "memory_mib": 4096, "disk_gib": "invalid"}))
        doctor = self.call("doctor")
        self.assertFalse(doctor["resources"]["verified"])
        self.error("resource_unverified", self.acquire, "new")

    def test_corrupt_durable_resources_fail_closed_for_resize(self):
        active = self.acquire()
        self.store.update_task(active["task_id"], resources_json="not-json")
        self.error("resource_unverified", self.call, "resize", cpu=3, allow_restart=True)

    def test_corrupt_disk_reservation_keeps_doctor_structured(self):
        active = self.acquire()
        for value in ("invalid", 1.5):
            with self.subTest(value=value):
                self.store.update_task(active["task_id"], reservation_bytes=value)
                doctor = self.call("doctor")
                self.assertFalse(doctor["resources"]["verified"])
                self.assertEqual(doctor["resources"]["error"]["code"], "resource_unverified")
                metrics = self.call("admin-metrics")
                self.assertFalse(metrics["disk_admission"]["verified"])
        self.store.update_task(active["task_id"], reservation_bytes=0)

    def test_corrupt_cached_disk_reservation_fails_closed_for_new_admission(self):
        cached = self.acquire("cache")
        self.call("release", "cache")
        self.store.update_task(cached["task_id"], reservation_bytes=1.5)
        self.error("resource_unverified", self.acquire, "new")

    def test_non_object_resource_reservation_keeps_diagnostics_structured(self):
        active = self.acquire()
        self.store.update_task(active["task_id"], resource_reservation_json="null")
        doctor = self.call("doctor")
        self.assertFalse(doctor["resources"]["verified"])
        self.assertEqual(doctor["resources"]["error"]["code"], "resource_unverified")
        metrics = self.call("admin-metrics")
        self.assertFalse(metrics["disk_admission"]["verified"])

    def test_status_corrupt_resource_reservation_is_structured(self):
        active = self.acquire()
        self.store.update_task(active["task_id"], resource_reservation_json="{")
        self.error("resource_unverified", self.call, "status")

    def test_falsy_non_string_resource_reservation_is_not_empty(self):
        active = self.acquire()
        for value in (0, False):
            with self.subTest(value=value):
                self.store.update_task(active["task_id"], resource_reservation_json=value)
                doctor = self.call("doctor")
                self.assertFalse(doctor["resources"]["verified"])
                self.assertEqual(doctor["resources"]["error"]["code"], "resource_unverified")

    def test_absent_partial_task_status_preserves_unknown_resource_state(self):
        active = self.acquire()
        self.call("release")
        self.store.update_task(active["task_id"], state="absent", resources_json="{}", resource_reservation_json="{}")
        result = self.call("status")
        self.assertIsNone(result["resources"])
        self.assertFalse(result["resource_state"]["verified"])
        self.assertEqual(result["resource_state"]["error"]["code"], "resource_unverified")

    def test_absent_partial_task_blocks_admission_until_reconciled(self):
        active = self.acquire()
        self.call("release")
        self.runner.instances.pop(active["instance_name"])
        self.store.update_task(active["task_id"], state="absent", resources_json="{}", resource_reservation_json="{}")

        doctor = self.call("doctor")
        self.assertFalse(doctor["resources"]["verified"])
        self.assertEqual(doctor["resources"]["error"]["code"], "resource_unverified")

        preview = self.call("admin-reconcile", apply=False)
        self.assertEqual(preview["actions"], [{
            "task_id": active["task_id"],
            "generation": active["generation"],
            "action": "verify-absent-resources",
        }])

        applied = self.call("admin-reconcile", apply=True)
        self.assertEqual(applied["actions"], [{
            "task_id": active["task_id"],
            "generation": active["generation"],
            "action": "verify-absent-resources",
        }])
        repaired = self.store.get_task(task_id=active["task_id"])
        self.assertEqual(json.loads(repaired["resources_json"]), {"cpu": 0, "memory_mib": 0, "disk_gib": 0})
        self.assertTrue(self.call("doctor")["resources"]["verified"])

    def test_absent_task_with_native_instance_blocks_admission_and_reconcile(self):
        active = self.acquire()
        self.store.update_task(active["task_id"], state="absent")

        doctor = self.call("doctor")
        self.assertFalse(doctor["resources"]["verified"])
        self.assertEqual(doctor["resources"]["error"]["code"], "resource_unverified")
        result = self.call("admin-reconcile", apply=True)
        self.assertEqual(result["actions"][0]["error"]["code"], "resource_unverified")

    def test_absent_task_with_port_mapping_blocks_admission_and_reconcile(self):
        active = self.acquire()
        self.call("port-add", name="app", vm_port=8080)
        self.store.update_task(active["task_id"], state="absent")
        self.runner.instances.pop(active["instance_name"])

        doctor = self.call("doctor")
        self.assertFalse(doctor["resources"]["verified"])
        self.assertEqual(doctor["resources"]["error"]["code"], "cleanup_incomplete")
        result = self.call("admin-reconcile", apply=True)
        self.assertEqual(result["actions"][0]["error"]["code"], "cleanup_incomplete")

    def test_unknown_task_state_fails_closed_and_reconcile_reports_error(self):
        active = self.acquire()
        self.store.update_task(active["task_id"], state="mystery")

        doctor = self.call("doctor")
        self.assertFalse(doctor["resources"]["verified"])
        self.assertEqual(doctor["resources"]["error"]["code"], "resource_unverified")
        result = self.call("admin-reconcile", apply=True)
        self.assertEqual(result["actions"][0]["error"]["code"], "resource_unverified")

    def test_doctor_structures_systemd_probe_failure(self):
        self.control.systemd.enabled = True
        with mock.patch.object(self.control, "_image_guard"), \
             mock.patch.object(self.control, "_ensure_capacity_verified", return_value={"layers": []}), \
             mock.patch.object(self.control, "_network_guard"), \
             mock.patch.object(self.control, "_resource_diagnostics", return_value={"verified": True, "memory_pressure": False}), \
             mock.patch.object(self.control, "_instance_diagnostics", return_value={"verified": True, "policy_compliant": True}), \
             mock.patch.object(self.control, "_default_admission_diagnostics", return_value={"ready": True}), \
             mock.patch.object(self.runner, "run", side_effect=CONTROL.ControlError("command_timeout", "systemd unavailable")):
            doctor = self.control.doctor(self.request("doctor"), None)
        self.assertIn("unit_unverified:incus.service", doctor["issues"])

    def test_doctor_does_not_hold_global_lock_during_native_probe(self):
        import fcntl
        Path(self.control.lock_path).touch()

        def probe_lock():
            fd = os.open(self.control.lock_path, os.O_RDWR)
            try:
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            finally:
                os.close(fd)

        self.runner.on_version = probe_lock
        self.runner.on_list_instances = probe_lock
        self.control.doctor(self.request("doctor"), None)

    def test_status_does_not_hold_global_lock_during_capacity_probe(self):
        import fcntl
        Path(self.control.lock_path).touch()

        def probe_lock(*_args, **_kwargs):
            fd = os.open(self.control.lock_path, os.O_RDWR)
            try:
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            finally:
                os.close(fd)
            return {"scope": "ct-filesystem", "layers": [{
                "path": str(self.root), "total": 100 * GIB, "free": self.runner.free,
                "target": 20 * GIB, "low": 10 * GIB,
            }]}

        with mock.patch.object(self.control, "_storage_headroom", side_effect=probe_lock):
            self.control.status(self.request("status"), "thread-a")

    def test_admin_status_does_not_hold_global_lock_during_doctor_probe(self):
        import fcntl
        Path(self.control.lock_path).touch()

        def probe_lock():
            fd = os.open(self.control.lock_path, os.O_RDWR)
            try:
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            finally:
                os.close(fd)

        self.runner.on_version = probe_lock
        self.control.admin_status(self.request("admin-status"), None)

    def test_drain_exit_keeps_admission_closed_with_pending_operation(self):
        self.store.set_meta("draining", "1")
        self.store.record_operation(
            "pending-drain",
            "acquire",
            "thread-a",
            0,
            {"outcome": "pending"},
        )
        self.error("reconciliation_required", self.control.drain_exit, {}, None)
        self.assertEqual(self.store.get_meta("draining"), "1")

    def test_status_corrupt_last_error_is_structured(self):
        active = self.acquire()
        self.store.update_task(active["task_id"], last_error="{")
        self.error("resource_unverified", self.call, "status")

    def test_status_invalid_resource_value_is_structured(self):
        active = self.acquire()
        self.store.update_task(
            active["task_id"],
            resources_json=json.dumps({"cpu": "bogus", "memory_mib": 4096, "disk_gib": 20}),
        )
        self.error("resource_unverified", self.call, "status")

    def test_partial_resource_reservation_is_structured(self):
        active = self.acquire()
        self.store.update_task(active["task_id"], resource_reservation_json=json.dumps({"cpu": 2}))
        doctor = self.call("doctor")
        self.assertFalse(doctor["resources"]["verified"])
        self.assertEqual(doctor["resources"]["error"]["code"], "resource_unverified")

    def test_admin_metrics_rejects_session_and_operation_without_audit(self):
        before = len(self.store._rows("SELECT action FROM audit"))
        request = self.request("admin-metrics", session="thread-a")
        request["session_id"] = "thread-a"
        request["operation_id"] = "should-not-be-used"
        with self.assertRaises(CONTROL.ControlError) as failure:
            self.control.dispatch(request)
        self.assertEqual(failure.exception.code, "invalid_request")
        self.assertEqual(len(self.store._rows("SELECT action FROM audit")), before)

    def test_admin_metrics_rejects_explicit_null_session_and_operation(self):
        for field in ("session_id", "operation_id"):
            with self.subTest(field=field):
                request = self.request("admin-metrics")
                request[field] = None
                self.error("invalid_request", self.control.dispatch, request)

    def test_audit_failure_reports_committed_operation_state(self):
        failure = CONTROL.ControlError(
            "sqlite_unavailable",
            "state database cannot be verified",
            {"phase": "commit", "state_committed": False},
        )
        request = self.request("drain-enter", operation="audit-commit-failure")
        with mock.patch.object(self.store, "audit", side_effect=failure):
            with self.assertRaises(CONTROL.ControlError) as error:
                self.control.dispatch(request)
        self.assertEqual(error.exception.code, "sqlite_unavailable")
        self.assertIs(error.exception.detail["state_committed"], True)
        self.assertEqual(self.store.operation(request["operation_id"])["outcome"], "success")

    def test_journal_lookup_failure_keeps_commit_state_unknown(self):
        failure = CONTROL.ControlError(
            "sqlite_unavailable",
            "state database cannot be verified",
            {"phase": "read", "state_committed": False},
        )
        with mock.patch.object(self.store, "operation", side_effect=failure):
            annotated = self.control._annotate_state_error(
                failure, action="acquire", operation_id="journal-read-failure"
            )
        self.assertIsNone(annotated.detail["state_committed"])

    def test_pending_operation_is_not_rewritten_as_error_after_action_failure(self):
        request = self.request("acquire", operation="pending-preserved", public_key=KEY)
        failure = CONTROL.ControlError("command_failed", "native action failed", {"phase": "native-config"})
        with mock.patch.object(self.control, "_dispatch_action", side_effect=failure):
            self.error("command_failed", self.control.dispatch, request)
        self.assertEqual(self.store.operation(request["operation_id"])["outcome"], "pending")

    def test_error_envelope_marks_native_outcome_as_unverified(self):
        request = self.request("acquire", operation="native-outcome", public_key=KEY)
        failure = CONTROL.ControlError("command_failed", "native action failed", {"phase": "native-config"})
        with mock.patch.object(self.control, "_dispatch_action", side_effect=failure):
            with self.assertRaises(CONTROL.ControlError) as raised:
                self.control.dispatch(request)
        record = self.store.operation(request["operation_id"])
        self.assertEqual(record["outcome"], "pending")
        self.assertFalse(raised.exception.detail["outcome_verified"])

    def test_release_finalize_failure_keeps_pending_journal_while_task_is_closing(self):
        self.acquire()
        task = self.store.get_task(session_id="thread-a")
        self.store.update_task(task["task_id"], state="closing")
        request = self.request("release", operation="release-finalize-failure")
        failure = CONTROL.ControlError("command_failed", "expiry finalization failed")
        with mock.patch.object(self.control, "_dispatch_action", side_effect=failure):
            with self.assertRaises(CONTROL.ControlError) as raised:
                self.control.dispatch(request)
        self.assertFalse(raised.exception.detail["outcome_verified"])
        self.assertEqual(self.store.operation(request["operation_id"])["outcome"], "pending")

    def test_verified_outcome_overrides_native_phase_for_terminal_journal(self):
        request = self.request("acquire", operation="verified-native-cleanup", public_key=KEY)
        failure = CONTROL.ControlError(
            "command_failed",
            "native action failed after confirmed cleanup",
            {"phase": "guest-readiness", "outcome_verified": True},
        )
        with mock.patch.object(self.control, "_dispatch_action", side_effect=failure):
            with self.assertRaises(CONTROL.ControlError):
                self.control.dispatch(request)
        record = self.store.operation(request["operation_id"])
        self.assertEqual(record["outcome"], "error")
        self.assertTrue(record["error"]["detail"]["outcome_verified"])

    def test_unknown_state_commit_preserves_pending_operation(self):
        request = {
            "protocol": CONTROL.PROTOCOL_VERSION,
            "action": "drain-enter",
            "operation_id": "unknown-drain-commit",
            "generation": 0,
        }
        failure = CONTROL.ControlError(
            "sqlite_unavailable", "state commit outcome is unknown",
            {"phase": "commit", "state_committed": None},
        )
        with mock.patch.object(self.store, "set_meta", side_effect=failure):
            self.error("sqlite_unavailable", self.control.dispatch, request)
        self.assertEqual(self.store.operation(request["operation_id"])["outcome"], "pending")

    def test_sessionless_admin_write_is_visible_in_metrics_operations(self):
        request = {
            "protocol": CONTROL.PROTOCOL_VERSION,
            "action": "drain-enter",
            "operation_id": "admin-audit-visible",
            "generation": 0,
        }
        self.control.dispatch(request)
        metrics = self.call("admin-metrics")
        self.assertTrue(any(key.startswith("drain-enter|ok|") for key in metrics["operations"]))
        self.control.dispatch({
            "protocol": CONTROL.PROTOCOL_VERSION,
            "action": "drain-exit",
            "operation_id": "admin-audit-visible-exit",
            "generation": 0,
        })

    def test_pre_native_action_failure_is_terminal_and_replayable(self):
        request = self.request("acquire", operation="pre-native-failure", public_key=KEY)
        failure = CONTROL.ControlError("resource_insufficient", "logical reservation is full")
        with mock.patch.object(self.control, "_dispatch_action", side_effect=failure):
            self.error("resource_insufficient", self.control.dispatch, request)
        record = self.store.operation(request["operation_id"])
        self.assertEqual(record["outcome"], "error")
        self.assertTrue(record["error"]["detail"]["outcome_verified"])
        with mock.patch.object(self.control, "_dispatch_action") as dispatch:
            self.error("resource_insufficient", self.control.dispatch, request)
        dispatch.assert_not_called()

    def test_corrupt_operation_journal_is_structured(self):
        operation_id = "corrupt-journal"
        self.store.record_operation(operation_id, "drain-enter", None, 0, {"outcome": "success"})
        with self.store.transaction():
            self.store.connection.execute(
                "UPDATE operations SET response_json = ? WHERE operation_id = ?", ("{", operation_id)
            )
        self.error("operation_unverified", self.control.dispatch,
                   self.request("drain-enter", operation=operation_id))

    def test_success_operation_journal_must_match_durable_drain_state(self):
        operation_id = "stale-drain-success"
        request = self.request("drain-enter", operation=operation_id)
        digest = hashlib.sha256(json.dumps(request, sort_keys=True).encode()).hexdigest()
        record = {
            "ok": True,
            "protocol": CONTROL.PROTOCOL_VERSION,
            "action": "drain-enter",
            "session_id": request["session_id"],
            "generation": request["generation"],
            "operation_id": operation_id,
            "outcome": "success",
            "result": {"draining": True},
            "_request_digest": digest,
        }
        self.store.record_operation(operation_id, "drain-enter", request["session_id"], request["generation"], record)
        self.error("operation_unverified", self.control.dispatch, request)

    def test_success_and_error_operation_journals_require_complete_shapes(self):
        request = self.request("drain-enter", operation="incomplete-success")
        digest = hashlib.sha256(json.dumps(request, sort_keys=True).encode()).hexdigest()
        self.store.record_operation(
            "incomplete-success", "drain-enter", request["session_id"], request["generation"],
            {"ok": True, "protocol": CONTROL.PROTOCOL_VERSION, "outcome": "success",
             "_request_digest": digest},
        )
        self.error("operation_unverified", self.control.dispatch, request)
        request = self.request("drain-enter", operation="incomplete-error")
        digest = hashlib.sha256(json.dumps(request, sort_keys=True).encode()).hexdigest()
        self.store.record_operation(
            "incomplete-error", "drain-enter", request["session_id"], request["generation"],
            {"outcome": "error", "error": {"code": "draining"}, "_request_digest": digest},
        )
        self.error("operation_unverified", self.control.dispatch, request)

    def test_success_replay_rejects_stale_resize_state(self):
        self.acquire()
        first = self.request("resize", operation="resize-old", cpu=3, allow_restart=True)
        self.control.dispatch(first)
        current = self.request("resize", operation="resize-new", cpu=2, allow_restart=True)
        self.control.dispatch(current)
        self.error("operation_unverified", self.control.dispatch, first)

    def test_port_remove_replay_requires_current_lease_generation(self):
        self.acquire()
        add = self.request("port-add", operation="port-add-once", name="app", vm_port=8080)
        self.control.dispatch(add)
        remove = self.request("port-remove", operation="port-remove-once", name="app")
        self.control.dispatch(remove)
        self.call("release")
        self.acquire()
        self.error("operation_unverified", self.control.dispatch, remove)

    def test_gc_replay_requires_candidate_shape_and_postcondition(self):
        request = self.request("admin-gc", operation="gc-corrupt", apply=True)
        digest = hashlib.sha256(json.dumps(request, sort_keys=True).encode()).hexdigest()
        record = {
            "ok": True,
            "protocol": CONTROL.PROTOCOL_VERSION,
            "action": "admin-gc",
            "session_id": request["session_id"],
            "generation": request["generation"],
            "operation_id": request["operation_id"],
            "outcome": "success",
            "result": {"dry_run": False, "selection": "eligible", "minimum_idle_seconds": 0, "candidates": [{}]},
            "_request_digest": digest,
        }
        self.store.record_operation(request["operation_id"], "admin-gc", request["session_id"], request["generation"], record)
        self.error("operation_unverified", self.control.dispatch, request)

    def test_reconcile_replay_requires_current_task_state(self):
        self.acquire()
        request = self.request("admin-reconcile", operation="reconcile-once", apply=True)
        response = self.control.dispatch(request)
        self.assertEqual(response["result"]["actions"][0]["action"], "rearm-current-expiry")
        self.call("release")
        self.error("operation_unverified", self.control.dispatch, request)

    def test_absent_resource_reconcile_replay_is_verified(self):
        active = self.acquire()
        self.call("release")
        self.runner.instances.pop(active["instance_name"])
        self.store.update_task(active["task_id"], state="absent", resources_json="{}", resource_reservation_json="{}")
        request = self.request("admin-reconcile", operation="reconcile-absent", apply=True)
        response = self.control.dispatch(request)
        self.assertEqual(response["result"]["actions"][0]["action"], "verify-absent-resources")
        replay = self.control.dispatch(request)
        self.assertEqual(replay["outcome"], "replayed")
        self.assertEqual(replay["result"], response["result"])
        with self.control._object_lock("session:" + active["session_id"]):
            self.error("object_busy", self.control.dispatch, request)
        self.store.put_port({
            "task_id": active["task_id"], "generation": active["generation"],
            "port_name": "app", "protocol": "tcp", "host_port": 20000,
            "vm_port": 8080, "device_name": "proxy-app", "state": "active",
            "created_at": self.now.isoformat(),
        })
        self.error("operation_unverified", self.control.dispatch, request)
        self.store.remove_port(active["task_id"], "app", "tcp")
        self.store.update_task(
            active["task_id"],
            resources_json=json.dumps({"cpu": 1, "memory_mib": 0, "disk_gib": 0}),
        )
        self.error("operation_unverified", self.control.dispatch, request)
        self.store.update_task(
            active["task_id"],
            resources_json=json.dumps({"cpu": 0, "memory_mib": 0, "disk_gib": 0}),
            reservation_bytes=1,
        )
        self.error("operation_unverified", self.control.dispatch, request)

        self.store.update_task(active["task_id"], reservation_bytes=0)
        self.store.update_task(
            active["task_id"],
            resource_reservation_json=json.dumps({"memory_mib": 1}),
        )
        self.assertFalse(self.control._resource_diagnostics()["verified"])
        self.store.update_task(active["task_id"], resource_reservation_json="{}")
        self.assertTrue(self.control._resource_diagnostics()["verified"])

    def test_absent_resource_dry_run_replay_and_reservation_repair(self):
        active = self.acquire("dry-run-absent")
        self.call("release", "dry-run-absent")
        self.runner.instances.pop(active["instance_name"])
        self.store.update_task(
            active["task_id"],
            state="absent",
            resources_json=json.dumps({"cpu": 0, "memory_mib": 0, "disk_gib": 0}),
            resource_reservation_json="{}",
        )
        request = self.request(
            "admin-reconcile", session="dry-run-absent", operation="reconcile-absent-dry", apply=False
        )
        response = self.control.dispatch(request)
        replay = self.control.dispatch(request)
        self.assertEqual(replay["outcome"], "replayed")
        self.assertEqual(replay["result"], response["result"])

        self.store.update_task(active["task_id"], reservation_bytes=1)
        apply_request = self.request(
            "admin-reconcile", session="dry-run-absent", operation="reconcile-absent-reservation", apply=True
        )
        applied = self.control.dispatch(apply_request)
        self.assertEqual(self.store.get_task(task_id=active["task_id"])["reservation_bytes"], 0)
        replayed = self.control.dispatch(apply_request)
        self.assertEqual(replayed["outcome"], "replayed")
        self.assertEqual(replayed["result"], applied["result"])

        self.store.update_task(
            active["task_id"],
            resources_json=json.dumps({"cpu": 1, "memory_mib": 0, "disk_gib": 0}),
            resource_reservation_json=json.dumps({"disk_gib": 1}),
            reservation_bytes=1,
        )
        resource_request = self.request(
            "admin-reconcile", session="dry-run-absent", operation="reconcile-absent-resources", apply=True
        )
        resource_applied = self.control.dispatch(resource_request)
        self.assertEqual(
            json.loads(self.store.get_task(task_id=active["task_id"])["resources_json"]),
            {"cpu": 0, "memory_mib": 0, "disk_gib": 0},
        )
        self.assertEqual(
            json.loads(self.store.get_task(task_id=active["task_id"])["resource_reservation_json"]),
            {},
        )
        resource_replay = self.control.dispatch(resource_request)
        self.assertEqual(resource_replay["outcome"], "replayed")
        self.assertEqual(resource_replay["result"], resource_applied["result"])

    def test_reconcile_action_error_replays_as_the_committed_result(self):
        active = self.acquire()
        request = self.request("admin-reconcile", operation="reconcile-error", apply=False)
        digest = hashlib.sha256(json.dumps(request, sort_keys=True).encode()).hexdigest()
        record = {
            "ok": True,
            "protocol": CONTROL.PROTOCOL_VERSION,
            "action": "admin-reconcile",
            "session_id": request["session_id"],
            "generation": request["generation"],
            "operation_id": request["operation_id"],
            "outcome": "success",
            "result": {
                "dry_run": True,
                "actions": [{
                    "task_id": active["task_id"],
                    "generation": active["generation"],
                    "action": "inspect",
                    "error": {"code": "object_busy", "message": "resource is being changed"},
                }],
            },
            "_request_digest": digest,
        }
        self.store.record_operation(
            request["operation_id"], "admin-reconcile", request["session_id"], request["generation"], record
        )
        replay = self.control.dispatch(request)
        self.assertEqual(replay["outcome"], "replayed")
        self.assertEqual(replay["result"]["actions"][0]["error"]["code"], "object_busy")

    def test_gc_replay_is_fenced_after_the_task_slot_is_reused(self):
        first = self.acquire()
        self.call("release")
        request = self.request("admin-gc", operation="gc-slot-reuse", apply=True)
        self.control.dispatch(request)
        second = self.acquire()
        self.assertGreater(second["generation"], first["generation"])
        self.error("operation_unverified", self.control.dispatch, request)

    def test_port_replay_is_fenced_after_lease_expiry(self):
        self.acquire()
        request = self.request("port-add", operation="port-expired-replay", name="app", vm_port=8080)
        self.control.dispatch(request)
        task = self.store.get_task(session_id="thread-a")
        self.now = CONTROL.parse_time(task["lease_expires_at"])
        self.error("operation_unverified", self.control.dispatch, request)

    def test_recursive_inventory_scalar_corruption_is_structured(self):
        def query(path, **_):
            if path == "/1.0/projects?recursion=1":
                return [{"name": "agents"}]
            if path == "/1.0/instances?recursion=1":
                return [{"name": "foreign", "expanded_devices": {}}]
            if path.endswith("/leases"):
                return [{"address": "198.51.100.77", "hwaddr": 7}]
            raise AssertionError(path)

        with mock.patch.object(self.control.incus, "query", side_effect=query):
            with self.assertRaises(CONTROL.ControlError) as failure:
                self.control._native_ips("agent-vm-new")
        self.assertEqual(failure.exception.code, "vm_ip_unverified")

    def test_drain_exit_keeps_admission_closed_for_unreconciled_tasks(self):
        active = self.acquire()
        self.store.set_meta("draining", "1")
        self.store.update_task(active["task_id"], state="closing", lease_expires_at=None)
        request = self.request("drain-exit", operation="drain-exit-blocked")
        self.error("reconciliation_required", self.control.dispatch, request)
        self.assertEqual(self.store.get_meta("draining"), "1")

    def test_drain_exit_invalidates_acquire_that_passed_early_gate(self):
        started = threading.Event()
        proceed = threading.Event()
        result = []
        original = self.control._reserve_resources

        def paused(*args, **kwargs):
            started.set()
            self.assertTrue(proceed.wait(timeout=5))
            return original(*args, **kwargs)

        request = self.request("acquire", "race", operation="race-acquire", public_key=KEY)

        def acquire():
            try:
                self.control.dispatch(request)
            except CONTROL.ControlError as error:
                result.append(error.code)

        with mock.patch.object(self.control, "_reserve_resources", side_effect=paused):
            thread = threading.Thread(target=acquire)
            thread.start()
            self.assertTrue(started.wait(timeout=5))
            self.control.dispatch(self.request("drain-enter", operation="race-drain-enter"))
            self.error(
                "reconciliation_required",
                self.control.dispatch,
                self.request("drain-exit", operation="race-drain-exit"),
            )
            proceed.set()
            thread.join(timeout=5)
        self.assertFalse(thread.is_alive())
        self.assertEqual(result, ["draining"])
        self.assertIsNone(self.store.get_task(session_id="race"))

    def test_release_of_missing_native_instance_keeps_absent_state_readable(self):
        active = self.acquire()
        self.runner.instances.pop(active["instance_name"])
        released = self.call("release")
        self.assertEqual(released["state"], "absent")
        self.assertEqual(released["resources"], {"cpu": 2, "memory_mib": 4096, "disk_gib": 20})
        self.assertEqual(self.call("status")["state"], "absent")

    def test_acquire_rechecks_drain_before_active_commit(self):
        def enter_drain(*_):
            self.store.set_meta("draining", "1")

        with mock.patch.object(self.control, "_install_public_key", side_effect=enter_drain):
            self.error("draining", self.acquire)
        task = self.store.get_task(session_id="thread-a")
        self.assertNotEqual(task["state"], "active")
        self.store.set_meta("draining", "0")

    def test_preparing_native_instance_counts_once_in_slot_admission(self):
        observed = {}
        original = self.control._configure_network

        def configure_and_observe(*args):
            original(*args)
            observed.update(self.control._instance_diagnostics()["admission"])

        with mock.patch.object(self.control, "_configure_network", side_effect=configure_and_observe):
            self.acquire()
        self.assertEqual(observed, {"allocated": 1, "reserved": 0, "allocated_and_reserved": 1,
                                    "remaining": None, "limit": None, "capacity": None})

    def test_audit_summary_extracts_nested_image_failures_and_warnings(self):
        self.store.audit("acquire", "error", detail={
            "code": "command_failed",
            "message": "image preparation failed",
            "detail": {
                "phase": "native-init",
                "error": {"code": "command_failed", "message": "Unable to unpack image"},
            },
        })
        self.store.audit("status", "ok", detail={
            "default_admission": {"warnings": [{"code": "memory_pressure"}]},
        })
        self.store.audit("status", "ok", detail={
            "phase": "native-init", "message": "image unpack completed",
        })
        summary = self.store.audit_summary()
        self.assertEqual(summary["image_unpack_failures"], {"native-init|command_failed": 1})
        self.assertEqual(summary["warnings"], {"memory_pressure": 1})

    def test_audit_summary_prefers_nested_error_code(self):
        self.store.audit("acquire", "error", detail={
            "code": "acquire_partial",
            "detail": {"error": {"code": "command_failed", "message": "Unable to unpack image"}},
        })
        summary = self.store.audit_summary()
        self.assertEqual(summary["image_unpack_failures"], {"unknown|command_failed": 1})

    def test_audit_summary_counts_rootfs_unpack_failures(self):
        self.store.audit("acquire", "error", detail={
            "code": "command_failed", "message": "failed to unpack rootfs: no space left on device",
        })
        self.assertEqual(self.store.audit_summary()["image_unpack_failures"], {"unknown|command_failed": 1})

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
        self.store.update_task(record["task_id"], state="closing", resources_json=json.dumps({"cpu": 2, "memory_mib": 4096, "disk_gib": 20}))
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
    def test_acquire_waits_for_unrelated_native_cleanup_transition(self):
        original_guard = self.control._running_inventory_guard
        calls = []

        def guard():
            calls.append(True)
            if len(calls) == 1:
                raise CONTROL.ControlError(
                    "resource_unverified",
                    CONTROL.TRANSITIONAL_ADMISSION_ERROR_MESSAGE,
                )
            return original_guard()

        request = self.request("acquire", "transition", operation="transition-acquire", public_key=KEY)
        with mock.patch.object(self.control, "_running_inventory_guard", side_effect=guard), \
             mock.patch.object(CONTROL.time, "sleep") as sleep:
            response = self.control.dispatch(request)

        result = response["result"]
        self.assertEqual(result["state"], "active")
        self.assertEqual(len(calls), 2)
        self.assertEqual(response["operation_id"], request["operation_id"])
        self.assertEqual(response["generation"], request["generation"])
        journal = self.store.operation(request["operation_id"])
        self.assertEqual(journal["operation_id"], request["operation_id"])
        self.assertEqual(journal["generation"], request["generation"])
        self.assertEqual(journal["result"]["generation"], result["generation"])
        sleep.assert_called_once_with(CONTROL.TRANSITIONAL_ADMISSION_POLL_SECONDS)

    def test_other_resource_unverified_errors_fail_closed_without_wait(self):
        task = self.control._base_task("unverified", 1)
        resources = {"cpu": 2, "memory_mib": 4096, "disk_gib": 20}
        failure = CONTROL.ControlError("resource_unverified", "disk admission state is not verified")
        with mock.patch.object(self.control, "_reserve_resources", side_effect=failure) as reserve, \
             mock.patch.object(CONTROL.time, "sleep") as sleep:
            with self.assertRaises(CONTROL.ControlError) as raised:
                self.control._reserve_resources_with_transition_wait(
                    task, resources, admission_epoch="admission-epoch"
                )

        self.assertIs(raised.exception, failure)
        reserve.assert_called_once_with(task, resources, startup_cpu=None, admission_epoch="admission-epoch")
        sleep.assert_not_called()

    def test_transitional_admission_wait_is_bounded(self):
        task = self.control._base_task("timeout", 1)
        resources = {"cpu": 2, "memory_mib": 4096, "disk_gib": 20}
        failure = CONTROL.ControlError(
            "resource_unverified",
            CONTROL.TRANSITIONAL_ADMISSION_ERROR_MESSAGE,
            {"task_id": task["task_id"]},
        )
        with mock.patch.object(self.control, "_reserve_resources", side_effect=failure) as reserve, \
             mock.patch.object(CONTROL.time, "monotonic", side_effect=[0.0, 0.1, 30.0]), \
             mock.patch.object(CONTROL.time, "sleep") as sleep:
            with self.assertRaises(CONTROL.ControlError) as raised:
                self.control._reserve_resources_with_transition_wait(task, resources)

        self.assertEqual(raised.exception.code, "resource_unverified")
        self.assertEqual(raised.exception.message, CONTROL.TRANSITIONAL_ADMISSION_ERROR_MESSAGE)
        self.assertEqual(raised.exception.detail["task_id"], task["task_id"])
        self.assertEqual(raised.exception.detail["admission_retry_count"], 1)
        self.assertEqual(raised.exception.detail["admission_wait_seconds"], 30.0)
        self.assertEqual(reserve.call_count, 2)
        sleep.assert_called_once_with(CONTROL.TRANSITIONAL_ADMISSION_POLL_SECONDS)

    def test_memory_pressure_does_not_block_cached_resume_with_logical_headroom(self):
        first = self.acquire("cache")
        self.call("release", "cache")
        self.resources.memory.return_value["available_bytes"] = 0
        resumed = self.acquire("cache")
        self.assertEqual(resumed["task_id"], first["task_id"])
        self.assertEqual(resumed["state"], "active")

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
