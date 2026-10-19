"""Reclamation authorization, compatibility and read-only admission regressions."""
from contextlib import contextmanager
import datetime as dt
import json
from unittest import mock

from test_native_agent_vm import CONTROL, CLI, Fixture, GIB, KEY


class ReclamationTests(Fixture):
    def cache(self, session="cache"):
        task = self.acquire(session)
        self.call("release", session)
        return self.store.get_task(task_id=task["task_id"])

    def test_manual_gc_has_no_age_floor_and_timer_has_exact_threshold(self):
        old = self.cache("old")
        self.now += dt.timedelta(seconds=86400)
        young = self.cache("young")
        preview = self.call("admin-gc")
        self.assertEqual(preview["selection"], "eligible")
        self.assertEqual(preview["minimum_idle_seconds"], 0)
        self.assertEqual(len(preview["candidates"]), 2)
        self.assertEqual(len(self.runner.instances), 2)
        timer = self.call("timer-age-gc")
        self.assertEqual(timer["selection"], "age")
        self.assertEqual(timer["minimum_idle_seconds"], 86400)
        self.assertEqual([item["task_id"] for item in timer["candidates"]], [old["task_id"]])
        self.assertIn(young["instance_name"], self.runner.instances)
        applied = self.call("admin-gc", apply=True)
        self.assertEqual([item["task_id"] for item in applied["candidates"]], [young["task_id"]])
        self.assertEqual(self.store.get_task(task_id=young["task_id"])["state"], "absent")

    def test_preview_then_resume_protects_new_generation(self):
        cached = self.cache()
        self.assertEqual(len(self.call("admin-gc")["candidates"]), 1)
        restored = self.acquire("cache")
        self.assertGreater(restored["generation"], cached["generation"])
        self.assertEqual(self.call("admin-gc", apply=True)["candidates"], [])
        self.assertEqual(self.runner.instances[cached["instance_name"]]["status"], "Running")

    def test_gc_skips_resume_that_changes_its_inventory_snapshot(self):
        cached = self.cache()
        original = self.control._object_lock
        resumed = False
        @contextmanager
        def lock(identity, **kwargs):
            nonlocal resumed
            if identity == "session:cache" and not resumed:
                resumed = True
                self.acquire("cache")
                self.call("release", "cache")
            with original(identity, **kwargs):
                yield
        with mock.patch.object(self.control, "_object_lock", lock):
            self.assertEqual(self.call("admin-gc", apply=True)["candidates"], [])
        self.assertIn(cached["instance_name"], self.runner.instances)

    def test_stopped_effective_lease_transitions_and_unknown_objects_survive(self):
        active = self.acquire("active")
        self.runner.instances[active["instance_name"]]["status"] = "Stopped"
        closing = self.cache("closing")
        self.store.update_task(closing["task_id"], state="closing")
        self.runner.instances["foreign"] = {"name": "foreign", "status": "Stopped", "config": {}}
        self.assertEqual(self.call("admin-gc", apply=True)["candidates"], [])
        self.assertEqual(len(self.runner.instances), 3)
        self.assertTrue(self.call("status", "active")["effective_lease"])

    def test_cached_state_with_effective_deadline_is_not_eligible(self):
        cached = self.cache()
        self.store.update_task(cached["task_id"], lease_expires_at=CONTROL.iso_time(self.now + dt.timedelta(minutes=10)))
        self.assertEqual(self.call("admin-gc")["candidates"], [])
        self.assertEqual(self.call("admin-gc", apply=True)["candidates"], [])
        self.assertIn(cached["instance_name"], self.runner.instances)

    def test_native_proxy_without_database_mapping_blocks_cache_deletion(self):
        cached = self.cache()
        self.runner.instances[cached["instance_name"]]["expanded_devices"]["unaccounted-proxy"] = {"type": "proxy"}
        self.assertEqual(self.call("admin-gc", apply=True)["candidates"], [])
        self.assertIn(cached["instance_name"], self.runner.instances)

    def test_foreign_generation_is_reported_without_deletion(self):
        cached = self.cache()
        self.runner.instances[cached["instance_name"]]["config"]["user.agent-vm.generation"] = "999"
        errors = self.call("admin-gc", apply=True)["candidates"]
        self.assertEqual(errors[0]["error"]["code"], "ownership_mismatch")
        self.assertEqual(self.store.get_task(task_id=cached["task_id"])["state"], "cached")

    def test_delete_failure_preserves_transition_and_error_until_reconciliation(self):
        cached = self.cache()
        self.runner.failure = lambda args: args[3:4] == ["delete"]
        errors = self.call("admin-gc", apply=True)["candidates"]
        self.assertEqual(errors[0]["error"]["code"], "command_failed")
        record = self.store.get_task(task_id=cached["task_id"])
        self.assertEqual(record["state"], "deleting")
        self.assertEqual(json.loads(record["last_error"])["code"], "command_failed")
        self.assertIn(cached["instance_name"], self.runner.instances)
        self.runner.failure = None
        self.call("admin-reconcile", apply=True)
        record = self.store.get_task(task_id=cached["task_id"])
        self.assertEqual(record["state"], "absent")
        self.assertIsNone(record["last_error"])
        self.assertIsNone(record["inactive_since"])

    def test_pending_deletion_rechecks_native_entrypoints_and_deadline(self):
        cached = self.cache()
        self.store.update_task(cached["task_id"], state="deleting")
        devices = self.runner.instances[cached["instance_name"]]["expanded_devices"]
        devices["unaccounted"] = {"type": "proxy"}
        result = self.call("admin-reconcile", apply=True)
        self.assertEqual(result["actions"][0]["error"]["code"], "cache_ineligible")
        del devices["unaccounted"]
        self.store.update_task(cached["task_id"], lease_expires_at=CONTROL.iso_time(self.now + dt.timedelta(minutes=1)))
        result = self.call("admin-reconcile", apply=True)
        self.assertEqual(result["actions"][0]["error"]["code"], "cache_ineligible")
        self.assertIn(cached["instance_name"], self.runner.instances)

    def test_unconfirmed_native_deletion_cannot_free_ownership(self):
        cached = self.cache()
        with mock.patch.object(self.control.incus, "delete"):
            errors = self.call("admin-gc", apply=True)["candidates"]
        self.assertEqual(errors[0]["error"]["code"], "cleanup_incomplete")
        self.assertEqual(self.store.get_task(task_id=cached["task_id"])["state"], "deleting")

    def test_native_delete_does_not_hold_global_lock(self):
        cached = self.cache()
        original = self.control.incus.delete
        def delete(name):
            # Another process must be able to obtain the lock while Incus waits.
            import subprocess
            script = 'import fcntl,sys; f=open(sys.argv[1],"a"); fcntl.flock(f,fcntl.LOCK_EX|fcntl.LOCK_NB)'
            subprocess.run(["python3", "-c", script, str(self.control.lock_path)], check=True, timeout=5)
            original(name)
        with mock.patch.object(self.control.incus, "delete", side_effect=delete):
            self.call("admin-gc", apply=True)
        self.assertNotIn(cached["instance_name"], self.runner.instances)

    def test_gc_rejects_ambiguous_apply_values_without_deletion(self):
        cached = self.cache()
        for value in ("false", "true", 1, None):
            self.error("invalid_request", self.call, "admin-gc", apply=value)
        self.assertIn(cached["instance_name"], self.runner.instances)


class InstanceAdmissionTests(Fixture):
    def test_count_limits_fail_before_reclamation_or_reservation(self):
        for key in ("limits.instances", "limits.virtual-machines"):
            with self.subTest(key=key):
                self.runner.project_limits = {key: "8"}
                with mock.patch.object(self.control, "_reclaim_capacity") as reclaim:
                    self.error("project_instance_limits_conflict", self.acquire)
                    reclaim.assert_not_called()
                self.assertEqual(self.store.list_tasks(), [])
                self.assertEqual(self.runner.instances, {})
                doctor = self.call("doctor")
                self.assertTrue(doctor["ready"])
                self.assertFalse(doctor["admission_ready"])
                self.assertEqual(doctor["instances"]["limits"], {key: "8"})
                self.assertEqual(doctor["default_admission"]["reasons"][0]["code"], "project_instance_limits_conflict")

    def test_count_conflict_does_not_block_existing_operations(self):
        active = self.acquire("active")
        cached = self.acquire("cache"); self.call("release", "cache")
        self.runner.project_limits = {"limits.instances": "8"}
        self.assertEqual(self.acquire("active")["generation"], active["generation"])
        self.call("renew", "active")
        self.call("ssh-info", "active")
        with mock.patch.object(self.control, "_reclaim_capacity") as reclaim:
            self.error("project_instance_limits_conflict", self.acquire, "cache")
            self.error("project_instance_limits_conflict", self.call, "resize", "active", cpu=3, allow_restart=True)
            reclaim.assert_not_called()
        self.assertEqual(self.store.get_task(task_id=cached["task_id"])["state"], "cached")
        self.call("release", "active")

    def test_count_ceiling_removal_leaves_other_native_limits_untouched(self):
        self.runner.project_limits = {"limits.containers": "1", "limits.processes": "4096"}
        self.acquire()
        doctor = self.call("doctor")
        self.assertTrue(doctor["instances"]["policy_compliant"])
        self.assertEqual(self.runner.project_limits, {"limits.containers": "1", "limits.processes": "4096"})
        self.assertFalse(any(args[:2] == ["incus", "project"] for args, _, _ in self.runner.calls))

    def test_more_than_eight_stopped_caches_do_not_consume_allocation(self):
        for index in range(9):
            session = f"cache-{index}"
            self.acquire(session); self.call("release", session)
        self.assertEqual(len(self.runner.instances), 9)
        doctor = self.call("doctor")
        self.assertEqual(doctor["instances"]["cached"], 9)
        self.assertEqual(doctor["resources"]["allocated_and_reserved"], {"cpu": 0, "memory_mib": 0})
        self.assertTrue(doctor["admission_ready"])
        self.acquire("tenth")
        self.assertEqual(len(self.runner.instances), 10)

    def test_component_health_is_separate_from_default_allocation_denial(self):
        self.config.update(memory_budget_mib=4096, memory_overcommit_ratio=1)
        self.acquire()
        doctor = self.call("doctor")
        self.assertTrue(doctor["ready"])
        self.assertFalse(doctor["admission_ready"])
        reason = doctor["default_admission"]["reasons"][0]
        self.assertEqual(reason["code"], "resource_insufficient")
        self.assertEqual(reason["dimensions"], ["memory_mib"])
        self.assertEqual(self.acquire()["state"], "active")

    def test_drain_and_disk_reservations_are_read_only_admission_reasons(self):
        cached = self.acquire(); self.call("release")
        self.store.update_task(cached["task_id"], reservation_bytes=4*GIB)
        self.runner.free = 20*GIB
        with mock.patch.object(self.control, "_reclaim_capacity") as reclaim:
            doctor = self.call("doctor")
            reclaim.assert_not_called()
        self.assertTrue(doctor["ready"])
        self.assertFalse(doctor["admission_ready"])
        self.assertTrue(doctor["default_admission"]["reclaim_required"])
        self.assertIn(cached["instance_name"], self.runner.instances)
        self.runner.free = 80*GIB
        self.call("drain-enter")
        doctor = self.call("doctor")
        self.assertTrue(doctor["ready"])
        self.assertFalse(doctor["admission_ready"])
        self.assertEqual(doctor["default_admission"]["reasons"][0]["code"], "draining")

    def test_invalid_project_observation_is_structured_and_denies_default(self):
        original = self.control.incus.query
        def query(path, **kwargs):
            return {"config": []} if path.startswith("/1.0/projects/agents") else original(path, **kwargs)
        # Doctor's independent NAT policy also rejects an invalid config; inspect
        # count diagnostics directly without pretending component health passes.
        with mock.patch.object(self.control.incus, "query", side_effect=query):
            self.assertFalse(self.control._instance_diagnostics()["verified"])

    def test_protocol_two_gc_cannot_widen_or_rewrite_old_journal(self):
        self.acquire("cache"); self.call("release", "cache")
        old = {"protocol": 2, "action": "admin-gc", "session_id": "owner", "generation": 0,
               "operation_id": "old-gc", "apply": True}
        record = {"outcome": "pending", "_request_digest": "old-digest"}
        self.store.record_operation("old-gc", "admin-gc", "owner", 0, record)
        self.error("protocol_mismatch", self.control.dispatch, old)
        self.assertEqual(self.store.operation("old-gc"), record)
        self.assertEqual(len(self.runner.instances), 1)
        self.assertEqual(CLI.PROTOCOL_VERSION, 4)
        self.assertEqual(CONTROL.PROTOCOL_VERSION, 4)

    def test_gc_retry_is_idempotent_and_cannot_delete_later_new_cache(self):
        self.acquire("cache"); self.call("release", "cache")
        request = self.request("admin-gc", operation="gc-once", apply=True)
        self.control.dispatch(request)
        self.acquire("cache"); self.call("release", "cache")
        with self.assertRaises(CONTROL.ControlError) as failure:
            self.control.dispatch(request)
        self.assertEqual(failure.exception.code, "operation_unverified")
        self.assertEqual(len(self.runner.instances), 1)
