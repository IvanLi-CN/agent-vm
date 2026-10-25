"""Offline boundary tests. They do not constitute real Incus acceptance."""
from __future__ import annotations

import datetime as dt
from contextlib import redirect_stderr, redirect_stdout
import io
import importlib.util
import json
import os
from pathlib import Path
import shlex
import shutil
import subprocess
import tempfile
import threading
import time
import types
import unittest
from unittest import mock
from urllib.parse import urlparse, parse_qs

ROOT = Path(__file__).resolve().parents[3]
GIB = 1024**3
KEY = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest fixture"


def load(name, relative):
    spec = importlib.util.spec_from_file_location(name, ROOT / relative)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def wait_until(predicate, *, timeout=3.0, interval=0.02):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return True
        time.sleep(min(interval, max(0.0, deadline - time.monotonic())))
    return bool(predicate())


def process_is_zombie(pid):
    try:
        state = (Path("/proc") / str(pid) / "stat").read_text()
    except FileNotFoundError:
        return True
    close_paren = state.rfind(")")
    return close_paren >= 0 and state[close_paren + 2 :].split()[0] == "Z"


CONTROL = load("native_control", "skills/agent-vm-ops/scripts/agent_vm_control.py")
CLI = load("native_cli", "skills/agent-vm/scripts/agent_vm.py")
import release_manifest as MANIFEST  # noqa: E402


class ConfigurationTests(unittest.TestCase):
    def test_environment_fields_are_required(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "config.json"
            path.write_text(json.dumps({"initial_lease_seconds": 1}))
            with self.assertRaises(CONTROL.ControlError) as error:
                CONTROL.load_config(path)
        self.assertEqual(error.exception.code, "config_missing")
        self.assertIn("host_address", error.exception.detail["fields"])

    def test_invalid_environment_address_fails_closed(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "config.json"
            path.write_text(json.dumps({
                "project": "agents", "profile": "agent-vm", "image_alias": "image",
                "image_fingerprint": "a" * 64, "network": "agentsbr0", "network_acl": "agent-vm-isolation",
                "pool": "agents", "host_address": "not-an-ip", "port_start": 20000,
                "port_end": 20030, "vm_ip_start": "198.51.100.20", "vm_ip_end": "198.51.100.250",
                "gateway_ip": "198.51.100.1", "storage_path": directory,
                "state_db": str(Path(directory) / "state.sqlite3"), "timer_dir": directory,
            }))
            with self.assertRaises(CONTROL.ControlError) as error:
                CONTROL.load_config(path)
        self.assertEqual(error.exception.code, "config_invalid")

    def test_release_units_use_fixed_systemd_directory_not_dynamic_timer_dir(self):
        manifest = {"artifacts": [{"path": "systemd/agent-vm-native-age-gc.service", "role": "unit"}]}
        paths = CONTROL.runtime_artifact_paths(
            {"timer_dir": "/var/lib/agent-vm-native/generated-timers"},
            Path("/opt/agent-vm-native/release-manifest.json"),
            manifest,
        )
        self.assertEqual(paths["systemd/agent-vm-native-age-gc.service"], CONTROL.DEFAULT_TIMER_DIR / "agent-vm-native-age-gc.service")


class NativeCommands(CONTROL.CommandRunner):
    """Model the subprocess protocol, including ownership and native devices."""
    def __init__(self):
        self.instances = {}
        self.calls = []
        self.free = 80 * GIB
        self.proxy_inventory = []
        self.forwards = []
        self.failure = None
        self.on_start = None
        self.on_version = None
        self.on_list_instances = None
        self.image_available = True
        self.image_type = "virtual-machine"
        self.on_wait = None
        self.proxy_allowed = True
        self.project_limits = {}
        self.storage_source = None
        self.on_config_set = None
        self.guest_growth_results = []

    def run(self, argv, *, input_data=None, timeout=30, check=True):
        args = list(argv)
        self.calls.append((args, input_data, timeout))
        if self.failure and self.failure(args):
            raise CONTROL.ControlError("command_failed", "injected native failure")
        body = b""
        if args[0] in {"conntrack", "ss", "systemctl"}: pass
        elif args[:2] == ["incus", "version"]:
            if self.on_version: self.on_version()
            body = b"7.5.1"
        elif args[:3] == ["incus", "--project", "agents"] or args[:2] == ["incus", "query"]:
            command = args[1:] if args[1] == "query" else args[3:]
            verb = command[0]
            if verb == "query":
                if args[1] != "query":
                    raise CONTROL.ControlError("command_failed", "Incus query rejects global --project")
                path = urlparse(command[1]).path
                project = parse_qs(urlparse(command[1]).query).get("project", ["agents"])[0]
                if path == "/1.0/projects": value = [{"name": "agents"}, {"name": "default"}]
                elif path == "/1.0/projects/agents": value = {"config": {**self.project_limits, "restricted.devices.proxy": "allow" if self.proxy_allowed else "block"}}
                elif path == "/1.0/storage-pools/agents": value = {"name": "agents", "driver": "dir",
                                                                       "config": {"source": self.storage_source}}
                elif path == "/1.0/network-acls/agent-vm-isolation": value = json.loads((ROOT / "skills/agent-vm-ops/assets/network-acl.json").read_text())
                elif path.startswith("/1.0/images/"):
                    if not self.image_available:
                        raise CONTROL.ControlError("command_failed", "image absent")
                    value = {"fingerprint": path.rsplit("/", 1)[-1], "type": self.image_type}
                elif path == "/1.0/instances": value = list(self.instances.values()) if project == "agents" else self.proxy_inventory
                elif path == "/1.0/networks": value = [{"managed": True, "type": "bridge", "name": "agentsbr0"}]
                elif path.endswith("/forwards"): value = self.forwards
                elif path.endswith("/leases"): value = []
                elif path.startswith("/1.0/instances/"):
                    parts = path.split("/")
                    item = self.instances.get(parts[3])
                    if not item: raise CONTROL.ControlError("command_failed", "instance absent")
                    value = {"status": item["status"], "network": {"eth0": {"addresses": [{"family": "inet", "address": item["expanded_devices"]["eth0"].get("ipv4.address")} ]}}} if path.endswith("/state") else item
                else: raise AssertionError(path)
                body = json.dumps(value).encode()
            elif verb == "list":
                if self.on_list_instances: self.on_list_instances()
                body = json.dumps(list(self.instances.values())).encode()
            elif verb == "init":
                image, name = command[1:3]
                if name in self.instances: raise AssertionError("duplicate init")
                config = {item.split("=", 1)[0]: item.split("=", 1)[1] for index, item in enumerate(command) if index and command[index-1] == "--config"}
                self.instances[name] = {"name": name, "status": "Stopped", "config": config, "devices": {},
                                        "expanded_devices": {"eth0": {"type": "nic", "network": "agentsbr0"}, "root": {"type": "disk", "size": "20GiB"}}}
            elif verb in {"start", "stop", "delete"}:
                name = command[1]
                if verb == "delete": del self.instances[name]; self.free += 20*GIB
                else:
                    self.instances[name]["status"] = "Running" if verb == "start" else "Stopped"
                    self.instances[name]["agent_ready"] = False
                    if verb == "start":
                        self.instances[name]["guest_disk_bytes"] = int(self.instances[name]["expanded_devices"]["root"]["size"].removesuffix("GiB")) * GIB
                    if verb == "start" and self.on_start: self.on_start(name)
            elif verb == "wait":
                name, condition = command[1:3]
                if self.on_wait: self.on_wait(name, condition)
                if condition == "agent": self.instances[name]["agent_ready"] = True
            elif command[:2] == ["config", "set"]:
                name, setting = command[2:4]
                key, value = setting.split("=", 1)
                self.instances[name]["config"][key] = value
                if self.on_config_set: self.on_config_set(name, key, value)
            elif command[:2] == ["config", "device"]:
                verb, name, device = command[2:5]
                item = self.instances[name]
                if verb == "remove":
                    item["devices"].pop(device, None); item["expanded_devices"].pop(device, None)
                else:
                    fields = command[5:]
                    base = dict(item["expanded_devices"].get(device, {}))
                    if verb == "add": base["type"] = fields.pop(0)
                    base.update(dict(field.split("=", 1) for field in fields))
                    item["devices"][device] = base
                    item["expanded_devices"][device] = dict(base)
            elif verb == "exec":
                if not self.instances[command[1]].get("agent_ready"):
                    raise CONTROL.ControlError("command_failed", "VM agent isn't currently running")
                if "cat /etc/ssh/ssh_host_ed25519_key.pub" in command: body = KEY.encode()
                elif command[-2:] == ["cat", "/proc/sys/kernel/random/boot_id"]:
                    body = self.instances[command[1]].get("boot_id", "00000000-0000-4000-8000-000000000001").encode()
                elif command[3:5] == ["python3", "-c"]:
                    if self.guest_growth_results:
                        response = self.guest_growth_results.pop(0)
                        if isinstance(response, CONTROL.ControlError):
                            raise response
                        result = subprocess.CompletedProcess(args, response.returncode, response.stdout, response.stderr)
                        for attribute in ("stdout_truncated", "stderr_truncated"):
                            if hasattr(response, attribute):
                                setattr(result, attribute, getattr(response, attribute))
                        return result
                    size = self.instances[command[1]]["guest_disk_bytes"]
                    if size < int(command[-1]) * GIB:
                        raise CONTROL.ControlError("command_failed", "guest virtual disk size is stale")
                    filesystem = size - 1000000
                    threshold = int(command[-1]) * GIB * 95 // 100
                    filesystem_before = threshold - 1000000
                    body = json.dumps({
                        "status": "grown",
                        "virtual_bytes": size,
                        "partition_bytes": size,
                        "filesystem_before": filesystem_before,
                        "filesystem_bytes": filesystem,
                        "filesystem_threshold_bytes": threshold,
                        "filesystem": "ext4",
                        "source": "/dev/sda2",
                        "parent": "/dev/sda",
                        "partition": "2",
                        "attempts": 1,
                        "capacity": {
                            "status": "grown",
                            "requested_bytes": int(command[-1]) * GIB,
                            "disk_bytes": size,
                            "partition_bytes": size,
                            "filesystem_bytes": filesystem,
                            "filesystem_before": filesystem_before,
                            "filesystem_threshold_bytes": threshold,
                            "filesystem": "ext4",
                            "source": "/dev/sda2",
                            "parent": "/dev/sda",
                            "partition": "2",
                            "tools": {
                                "findmnt": "/usr/bin/findmnt",
                                "lsblk": "/usr/bin/lsblk",
                                "blockdev": "/usr/sbin/blockdev",
                                "growpart": "/usr/bin/growpart",
                                "resize2fs": "/usr/sbin/resize2fs",
                            },
                            "attempts": [{"attempt": 1}],
                            "verified": True,
                        },
                    }).encode()
            else: raise AssertionError(command)
        else: raise AssertionError(args)
        return subprocess.CompletedProcess(args, 0, body, b"")

    def run_bounded(self, argv, *, input_data=None, timeout=30, stdout_limit=0, stderr_limit=0, check=True):
        result = self.run(argv, input_data=input_data, timeout=timeout, check=check)
        result.stdout_truncated = getattr(result, "stdout_truncated", False)
        result.stderr_truncated = getattr(result, "stderr_truncated", False)
        return result


class NativeTimerTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.runner = mock.Mock(spec=CONTROL.CommandRunner)
        self.generated_timer_dir = self.root / "generated-timers"
        self.static_unit_dir = self.root / "static-units"
        self.generated_timer_dir.mkdir()
        self.static_unit_dir.mkdir()
        self.systemd = CONTROL.SystemdManager(self.runner, {"timer_dir": str(self.generated_timer_dir)})
        self.systemd.static_unit_dir = self.static_unit_dir
        self.task = "avm-0123456789abcdef"
        self.unit = self.systemd.timer_unit(self.task, 1)
        self.failure = CONTROL.ControlError("command_failed", "native unit does not exist")
        self.absent = b"LoadState=not-found\nActiveState=inactive\nFragmentPath=\nUnitFileState=\n"

    def test_uncreated_timer_is_idempotently_removed_after_native_absence_proof(self):
        self.runner.run.side_effect = [self.failure,
            subprocess.CompletedProcess([], 1, self.absent, b""),
            subprocess.CompletedProcess([], 0, b"", b"")]
        self.assertEqual(self.systemd.remove_expiry(self.task, 1), self.unit)
        calls = [call.args[0] for call in self.runner.run.call_args_list]
        self.assertEqual(calls[0], ["systemctl", "disable", "--now", self.unit])
        self.assertEqual(calls[1], ["systemctl", "show", self.unit, "-p", "LoadState", "-p", "ActiveState",
                                    "-p", "FragmentPath", "-p", "UnitFileState"])
        self.assertEqual(calls[2], ["systemctl", "daemon-reload"])
        self.assertFalse((self.generated_timer_dir / self.unit).exists())

    def test_existing_timer_file_is_not_discarded_after_disable_failure(self):
        path = self.generated_timer_dir / self.unit
        path.write_text("preserve this timer")
        self.runner.run.side_effect = [self.failure, subprocess.CompletedProcess([], 1, self.absent, b"")]
        with self.assertRaises(CONTROL.ControlError) as error:
            self.systemd.remove_expiry(self.task, 1)
        self.assertIs(error.exception, self.failure)
        self.assertEqual(path.read_text(), "preserve this timer")
        self.assertEqual(self.runner.run.call_count, 2)

    def test_unverified_or_active_native_unit_cannot_be_treated_as_absent(self):
        probes = [
            subprocess.CompletedProcess([], 0, self.absent.replace(b"not-found", b"loaded"), b""),
            subprocess.CompletedProcess([], 0, self.absent.replace(b"inactive", b"active"), b""),
            subprocess.CompletedProcess([], 1, b"", b"permission denied"),
            subprocess.CompletedProcess([], 2, self.absent, b"unavailable"),
            CONTROL.ControlError("command_timeout", "native inspection timed out"),
        ]
        for probe in probes:
            with self.subTest(probe=probe):
                self.runner.reset_mock()
                self.runner.run.side_effect = [self.failure, probe]
                with self.assertRaises(CONTROL.ControlError) as error:
                    self.systemd.remove_expiry(self.task, 1)
                self.assertIs(error.exception, self.failure)
                self.assertEqual(self.runner.run.call_count, 2)

    def test_expiry_active_requires_the_timer_to_target_its_service(self):
        expires_at = "2026-10-04T12:34:56+00:00"
        path = self.generated_timer_dir / self.unit
        content = (
            "[Timer]\n"
            "OnCalendar=2026-10-04 12:34:56 UTC\n"
            "Persistent=yes\n"
            f"Unit=agent-vm-native-expire@{self.systemd.instance_name(self.task, 1)}.service\n"
        )
        path.write_text(content)
        self.runner.run.return_value = subprocess.CompletedProcess(
            [], 0,
            f"ActiveState=active\nFragmentPath={path}\nUnitFileState=enabled\n".encode(),
            b"",
        )
        self.assertTrue(self.systemd.expiry_active(self.task, 1, expires_at))
        path.write_text(content.replace("Unit=agent-vm-native-expire@", "Unit=other-service@"))
        self.assertFalse(self.systemd.expiry_active(self.task, 1, expires_at))

    def test_service_asset_requires_loaded_fragment_and_exec_contract(self):
        unit = "agent-vm-native-reconcile.service"
        path = self.static_unit_dir / unit
        path.write_text(
            "[Service]\n"
            "ExecStart=/usr/bin/python3 /opt/agent-vm-native/agent_vm_control.py --timer-reconcile\n"
        )
        self.runner.run.return_value = subprocess.CompletedProcess(
            [], 0, f"LoadState=loaded\nFragmentPath={path}\n".encode(), b""
        )
        self.assertTrue(self.systemd.service_asset_active(
            unit,
            ("ExecStart=/usr/bin/python3 /opt/agent-vm-native/agent_vm_control.py --timer-reconcile",),
        ))
        path.write_text("[Service]\n")
        self.assertFalse(self.systemd.service_asset_active(
            unit,
            ("ExecStart=/usr/bin/python3 /opt/agent-vm-native/agent_vm_control.py --timer-reconcile",),
        ))

    def test_service_asset_validates_template_with_systemd_cat(self):
        unit = "agent-vm-native-expire@.service"
        path = self.static_unit_dir / unit
        required = "ExecStart=/usr/bin/python3 /opt/agent-vm-native/agent_vm_control.py --timer-expire %i"
        path.write_text(f"[Service]\n{required}\n")
        self.runner.run.return_value = subprocess.CompletedProcess(
            [], 0, f"# {path}\n[Service]\n{required}\n".encode(), b""
        )
        self.assertTrue(self.systemd.service_asset_active(unit, (required,)))

    def test_enabled_unit_active_requires_active_enabled_unit(self):
        self.runner.run.return_value = subprocess.CompletedProcess(
            [], 0, b"ActiveState=active\nUnitFileState=disabled\n", b""
        )
        self.assertFalse(self.systemd.enabled_unit_active("agent-vm-native-reconcile.service"))
        self.runner.run.return_value = subprocess.CompletedProcess(
            [], 0, b"ActiveState=active\nUnitFileState=enabled\n", b""
        )
        self.assertTrue(self.systemd.enabled_unit_active("agent-vm-native-reconcile.service"))

    def test_enabled_service_ready_accepts_completed_oneshot_service(self):
        self.runner.run.return_value = subprocess.CompletedProcess(
            [], 0, b"LoadState=loaded\nUnitFileState=enabled\n", b""
        )
        self.assertTrue(self.systemd.enabled_service_ready("agent-vm-native-reconcile.service"))


class Fixture(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.now = dt.datetime(2026, 10, 4, tzinfo=dt.timezone.utc)
        self.config = {"project": "agents", "profile": "agent-vm", "image_fingerprint": "a"*64,
                       "network": "agentsbr0", "network_acl": "agent-vm-isolation", "pool": "agents", "host_address": "198.51.100.19", "port_start": 20000,
                       "port_end": 20030, "vm_ip_start": "198.51.100.20", "vm_ip_end": "198.51.100.250", "gateway_ip": "198.51.100.1",
                       "initial_lease_seconds": 1800, "renew_seconds": 900, "max_lease_seconds": 7200,
                       "cache_seconds": 86400, "create_cost_bytes": 4*GIB, "resume_cost_bytes": GIB,
                       "resize_cost_bytes": GIB, "storage_path": str(self.root),
                       "cpu_budget": 4, "memory_budget_mib": 16384, "required_guest_tools": ["python3", "sshd", "rsync", "docker"],
                       "state_db": str(self.root / "state.sqlite3"), "timer_dir": str(self.root / "systemd")}
        self.runner = NativeCommands()
        self.runner.storage_source = self.config["storage_path"]
        self.store = CONTROL.StateStore(Path(self.config["state_db"]))
        self.addCleanup(self.store.close)
        self.systemd = CONTROL.SystemdManager(self.runner, self.config, enabled=False)
        self.resources = mock.Mock(spec=CONTROL.CTResources)
        self.resources.cpu.return_value = 16
        self.resources.memory.return_value = {"scope": "ct-memory", "sources": ["lxcfs"], "limit_bytes": 32*GIB,
                                              "available_bytes": 24*GIB, "threshold_bytes": (32*GIB+9)//10}
        self.resources.cpu_pressure.return_value = {
            "scope": "ct-cpu-pressure",
            "sources": ["cgroupfs"],
            "mountpoint": "/sys/fs/cgroup",
            "mount_root": "/",
            "filesystem": "cgroup2",
            "cgroup_path": "/",
            "metric": "/sys/fs/cgroup/cpu.pressure",
            "some": {"avg10": 0.0, "avg60": 0.0, "avg300": 0.0, "total": 0.0},
            "full": {"avg10": 0.0, "avg60": 0.0, "avg300": 0.0, "total": 0.0},
        }
        self.control = CONTROL.Control(self.config, runner=self.runner, store=self.store, systemd=self.systemd,
                                       resources=self.resources, clock=lambda: self.now)
        self.config = self.control.config
        original = Path.read_text
        def read(path, *args, **kwargs):
            values = {"/proc/sys/net/ipv4/ip_local_port_range": "32768 60999", "/sys/fs/cgroup/cpu.max": "max 100000", "/sys/fs/cgroup/memory.max": "max"}
            return values[str(path)] if str(path) in values else original(path, *args, **kwargs)
        for patch in (mock.patch.object(Path, "read_text", read),
                      mock.patch.object(CONTROL.shutil, "disk_usage", side_effect=lambda _: shutil._ntuple_diskusage(100*GIB, 100*GIB-self.runner.free, self.runner.free)),
                      mock.patch.object(CONTROL.shutil, "which", return_value="/usr/bin/conntrack"),
                      mock.patch.object(CONTROL.os, "cpu_count", return_value=16)):
            patch.start(); self.addCleanup(patch.stop)
        self.counter = 0

    def runtime_manifest(self, root: Path, revision: str, *, client: bytes = b"client\n", control: bytes = b"control\n"):
        artifacts = []
        for name, path, role, mode in sorted(MANIFEST.FIXED_ARTIFACTS):
            content = client if role == "client" else control if role == "control" else f"{role}:{path}\n".encode()
            target = root / path
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(content)
            target.chmod(mode)
            artifacts.append({
                "name": name,
                "path": path,
                "role": role,
                "sha256": CONTROL.hashlib.sha256(content).hexdigest(),
                "mode": mode,
            })
        manifest = MANIFEST.build_manifest(revision, artifacts)
        manifest_path = root / MANIFEST.MANIFEST_NAME
        manifest_path.write_text(json.dumps(manifest), encoding="utf-8")
        return manifest, manifest_path

    def request(self, action, session="thread-a", operation=None, **fields):
        self.counter += 1
        task = self.store.get_task(session_id=session)
        value = {"protocol": CONTROL.PROTOCOL_VERSION, "action": action}
        if action != "admin-metrics":
            value["session_id"] = session
        if action == "port-list": value["generation"] = task["generation"]
        if action in self.control.WRITE_ACTIONS:
            value.update(operation_id=operation or f"op-{self.counter}", generation=task["generation"] if task else 0)
        value.update(fields)
        return value

    def call(self, action, session="thread-a", **fields):
        return self.control.dispatch(self.request(action, session, **fields))["result"]

    def acquire(self, session="thread-a"):
        return self.call("acquire", session, public_key=KEY)

    @staticmethod
    def guest_growth_result(disk_gib=20, *, status="grown", attempts=1, returncode=0, stderr=""):
        size = disk_gib * GIB
        filesystem = size - 1000000
        threshold = size * 95 // 100
        filesystem_before = (
            filesystem if status == "already_grown" and attempts == 0 else threshold - 1000000
        )
        failure_capacity = {
            "requested_bytes": size,
            "disk_bytes": size,
            "partition_bytes": size,
            "filesystem_bytes": filesystem,
            "filesystem_before": filesystem_before,
            "filesystem_threshold_bytes": threshold,
            "filesystem": "ext4",
            "source": "/dev/sda2",
            "parent": "/dev/sda",
            "partition": "2",
            "tools": {
                "findmnt": "/usr/bin/findmnt",
                "lsblk": "/usr/bin/lsblk",
                "blockdev": "/usr/sbin/blockdev",
                "growpart": "/usr/bin/growpart",
                "resize2fs": "/usr/sbin/resize2fs",
            },
            "verified": True,
        }
        attempt_records = [{"attempt": index + 1} for index in range(attempts)]
        if status in {"grown_after_retry", "already_grown"} and attempts == 2:
            attempt_records[0]["error"] = {
                "phase": "filesystem-grow",
                "command": ["resize2fs", "/dev/sda2"],
                "returncode": 1,
                "stdout": "",
                "stderr": "resize2fs: Device or resource busy",
                "capacity": failure_capacity,
                "transient": True,
                "stdout_truncated": False,
                "stderr_truncated": False,
            }
        payload = {
            "status": status,
            "virtual_bytes": size,
            "partition_bytes": size,
            "filesystem_before": filesystem_before,
            "filesystem_bytes": filesystem,
            "filesystem_threshold_bytes": threshold,
            "filesystem": "ext4",
            "source": "/dev/sda2",
            "parent": "/dev/sda",
            "partition": "2",
            "attempts": attempts,
            "capacity": {
                "status": status,
                "requested_bytes": size,
                "disk_bytes": size,
                "partition_bytes": size,
                "filesystem_bytes": filesystem,
                "filesystem_before": filesystem_before,
                "filesystem_threshold_bytes": threshold,
                "filesystem": "ext4",
                "source": "/dev/sda2",
                "parent": "/dev/sda",
                "partition": "2",
                "tools": {
                    "findmnt": "/usr/bin/findmnt",
                    "lsblk": "/usr/bin/lsblk",
                    "blockdev": "/usr/sbin/blockdev",
                    "growpart": "/usr/bin/growpart",
                    "resize2fs": "/usr/sbin/resize2fs",
                },
                "attempts": attempt_records,
                "verified": True,
            },
        }
        return subprocess.CompletedProcess(
            [], returncode,
            json.dumps(payload).encode() if returncode == 0 else b"",
            stderr.encode() if isinstance(stderr, str) else stderr,
        )

    @staticmethod
    def guest_growth_failure(*, disk_gib=20, phase="filesystem-grow", command=None, returncode=1, stderr="resize2fs: transient guest failure"):
        size = disk_gib * GIB
        threshold = size * 95 // 100
        attempt_capacity = {
            "requested_bytes": size,
            "disk_bytes": size,
            "partition_bytes": size,
            "filesystem_bytes": 10 * GIB,
            "filesystem_before": 10 * GIB,
            "filesystem_threshold_bytes": threshold,
            "filesystem": "ext4",
            "source": "/dev/sda2",
            "parent": "/dev/sda",
            "partition": "2",
            "tools": {
                "findmnt": "/usr/bin/findmnt",
                "lsblk": "/usr/bin/lsblk",
                "blockdev": "/usr/sbin/blockdev",
                "growpart": "/usr/bin/growpart",
                "resize2fs": "/usr/sbin/resize2fs",
            },
            "verified": True,
        }
        error = {
            "phase": phase,
            "command": command or ["resize2fs", "/dev/sda2"],
            "returncode": returncode,
            "stdout": "",
            "stderr": stderr,
            "capacity": attempt_capacity,
            "transient": False,
            "stdout_truncated": False,
            "stderr_truncated": False,
        }
        report_capacity = dict(attempt_capacity, attempts=[])
        report_capacity["attempts"] = [{"attempt": 1, "error": error}]
        report_error = dict(error, capacity=report_capacity)
        return subprocess.CompletedProcess([], returncode, b"", (json.dumps({"ok": False, "error": report_error}) + "\n").encode())

    def error(self, code, function, *args, **kwargs):
        with self.assertRaises(CONTROL.ControlError) as context: function(*args, **kwargs)
        self.assertEqual(context.exception.code, code)


class LifecycleTests(Fixture):
    def test_legacy_compatibility_warning_is_persisted_and_replayed(self):
        request = self.request("acquire", operation="legacy-warning", public_key=KEY)
        response = self.control.dispatch(request)
        codes = {item["code"] for item in response["warnings"]}
        self.assertIn("legacy_compatibility", codes)
        replay = self.control.dispatch(request)
        self.assertEqual(replay["outcome"], "replayed")
        self.assertEqual({item["code"] for item in replay["warnings"]}, codes)

    def test_drain_exit_stays_closed_while_release_activation_is_fenced(self):
        self.store.set_meta("draining", "1")
        fence = self.root / "release-activation.fence"
        fence.write_text(json.dumps({"format": 1, "transaction_id": "tx", "release_id": "a" * 40}), encoding="utf-8")
        self.control.release_fence_path = fence
        healthy = {"ready": True, "resources": {"verified": True}, "instances": {"verified": True}, "capacity": {}}
        with mock.patch.object(self.control, "doctor", return_value=healthy):
            self.error(
                "release_activation_in_progress",
                self.control.drain_exit,
                {"operation_id": "drain-exit-fenced"},
                None,
            )

    def test_incompatible_request_is_rejected_before_journal_or_native_effect(self):
        request = self.request("acquire", operation="incompatible-request", public_key=KEY)
        request["compatibility"] = {
            "release_id": "f" * 40,
            "protocol": CONTROL.PROTOCOL_VERSION,
            "action": "acquire",
            "request": {"schema": 99, "semantic_revision": 1},
            "accepted_response": {"schema_min": 1, "schema_max": 1, "semantic_min": 1, "semantic_max": 1},
        }
        self.error("request_schema_incompatible", self.control.dispatch, request)
        self.assertIsNone(self.store.operation("incompatible-request"))
        self.assertEqual(self.store.list_tasks(), [])
        self.assertEqual(self.runner.instances, {})

    def test_changed_verified_artifact_is_rejected_before_write(self):
        release_root = self.root / "verified-release"
        release_root.mkdir()
        _, manifest_path = self.runtime_manifest(release_root, "f" * 40)
        control = release_root / "agent_vm_control.py"
        control.write_text("changed\n")
        config = dict(self.config, state_db=str(self.root / "artifact-state.sqlite3"), release_manifest_path=str(manifest_path))
        store = CONTROL.StateStore(Path(config["state_db"]))
        self.addCleanup(store.close)
        with mock.patch.object(CONTROL, "DEFAULT_TIMER_DIR", release_root / "systemd"):
            checked = CONTROL.Control(config, runner=self.runner, store=store, systemd=self.systemd, resources=self.resources, clock=lambda: self.now)
        request = {"protocol": CONTROL.PROTOCOL_VERSION, "action": "acquire", "session_id": "thread-a", "generation": 0, "operation_id": "artifact-mismatch", "public_key": KEY}
        self.error("artifact_checksum_mismatch", checked.dispatch, request)
        self.assertIsNone(store.operation("artifact-mismatch"))

    def test_invalid_runtime_artifact_is_rejected_before_state_store_creation(self):
        release_root = self.root / "preflight-release"
        release_root.mkdir()
        _, manifest_path = self.runtime_manifest(release_root, "e" * 40)
        control = release_root / "agent_vm_control.py"
        control.write_text("tampered\n")
        state_db = self.root / "preflight-state" / "state.sqlite3"
        config = dict(self.config, state_db=str(state_db), release_manifest_path=str(manifest_path))
        with mock.patch.object(CONTROL, "DEFAULT_TIMER_DIR", release_root / "systemd"):
            with self.assertRaises(CONTROL.ControlError) as failure:
                CONTROL.Control(config, runner=self.runner, systemd=self.systemd, resources=self.resources, clock=lambda: self.now)
        self.assertEqual(failure.exception.code, "artifact_checksum_mismatch")
        self.assertFalse(state_db.parent.exists())

    def test_replayed_response_refreshes_release_metadata(self):
        request = self.request("acquire", operation="release-refresh", public_key=KEY)
        self.control.dispatch(request)
        self.control.release_manifest = self.runtime_manifest(self.root / "replay-release", "f" * 40)[0]
        self.control.release_manifest_present = True
        self.control.release_artifacts_verified = True
        with mock.patch.object(CONTROL, "verify_runtime_bundle"):
            replay = self.control.dispatch(request)
        self.assertEqual(replay["outcome"], "replayed")
        self.assertEqual(replay["release"]["release_id"], "f" * 40)

    def test_readiness_uses_one_budget_and_emits_one_slow_start_diagnostic(self):
        self.acquire()
        elapsed = [0.0]
        waits = []

        def wait(_, condition, seconds):
            waits.append((condition, seconds))
            if condition == "agent" and len(waits) == 1:
                elapsed[0] = 120.0
                raise CONTROL.ControlError(
                    "command_failed",
                    "wait timed out",
                    {"returncode": 1, "stderr": "Error: Timed out waiting for condition"},
                )
            if condition == "agent":
                elapsed[0] = 150.0
                self.runner.instances[next(iter(self.runner.instances))]["agent_ready"] = True

        with mock.patch.object(CONTROL.time, "monotonic", side_effect=lambda: elapsed[0]), \
             mock.patch.object(self.control.incus, "wait", side_effect=wait), \
             mock.patch.object(self.control, "_emit_slow_start_diagnostic", return_value={"emitted": True}) as diagnostic:
            result = self.call("restart")

        self.assertEqual(waits, [("agent", 120), ("agent", 180), ("ipv4", 150)])
        diagnostic.assert_called_once()
        self.assertIn("readiness", result)
        self.assertEqual(result["readiness"]["budget_seconds"], 300)

    def test_readiness_timeout_reports_phase_and_cleanup_after_shared_budget(self):
        self.acquire()
        elapsed = [0.0]
        waits = []

        def wait(_, condition, seconds):
            waits.append((condition, seconds))
            if condition == "agent" and len(waits) == 1:
                elapsed[0] = 120.0
            else:
                elapsed[0] = 300.0
            raise CONTROL.ControlError(
                "command_failed",
                "wait timed out",
                {"returncode": 1, "stderr": "Error: Timed out waiting for condition"},
            )

        with mock.patch.object(CONTROL.time, "monotonic", side_effect=lambda: elapsed[0]), \
             mock.patch.object(self.control.incus, "wait", side_effect=wait), \
             mock.patch.object(self.control, "_emit_slow_start_diagnostic", return_value={"emitted": True}):
            with self.assertRaises(CONTROL.ControlError) as failure:
                self.call("restart")

        self.assertEqual(failure.exception.code, "guest_readiness_timeout")
        self.assertEqual(failure.exception.detail["phase"], "agent")
        self.assertEqual(failure.exception.detail["budget_seconds"], 300)
        self.assertEqual(waits, [("agent", 120), ("agent", 180)])
        task = self.store.get_task(session_id="thread-a")
        self.assertEqual(task["state"], "cached")
        self.assertEqual(self.store.get_ports(task["task_id"]), [])

    def test_slow_start_diagnostic_is_redacted_audited_and_observation_only(self):
        self.acquire()
        task = self.store.get_task(session_id="thread-a")
        starts_before = sum(args[3:4] == ["start"] for args, _, _ in self.runner.calls)
        stderr = io.StringIO()
        with mock.patch.object(self.control.incus, "instance_info", return_value={"status": "Running", "pid": 1}), \
             mock.patch.object(
                 self.control.incus,
                 "instance_log",
                 return_value=(
                     'password=supersecret token=tok bearer abc '
                     '"password": "json-secret" AWS_SECRET_ACCESS_KEY=aws-secret '
                     "sshpass -p hunter2 sshpass -p 'space secret' sshpass -p \"quoted\\\" secret\" "
                     "sshpass -pvery-inline-secret sshpass -p'joined secret'bar sshpass -p$'ansi space secret' "
                     "sshpass -p escaped\\ secret "
                     "sshpass -p$(printf shell-substitution-secret)\n"
                     "sshpass -p$(printf 'multi\nline-substitution-secret')\n"
                     "sshpass -p`printf backtick-secret`\n"
                     "sshpass -p<(printf process-substitution-secret)\n"
                     "sshpass -pcontinued\\" "\nline-continuation-secret\n"
                     'https://user:url-secret@example.test '
                     'ssh://user:ssh-secret@example.test postgres://user:db-secret@example.test\n'
                     '-----BEGIN PRIVATE KEY-----\nprivate-key-secret\n-----END PRIVATE KEY-----\n'
                     '-----BEGIN OPENSSH PRIVATE KEY-----\ntruncated-key-secret\n'
                     + CONTROL.DIAGNOSTIC_LOG_TRUNCATION_MARKER
                 ),
             ), \
             mock.patch.object(CONTROL.time, "monotonic", return_value=120.0), \
             mock.patch.object(CONTROL.sys, "stderr", stderr):
            diagnostic = self.control._emit_slow_start_diagnostic(
                task,
                phase="agent",
                started_monotonic=0.0,
                readiness={"stage_timings": {"native_start_seconds": 2.0}},
            )

        self.assertNotIn("supersecret", json.dumps(diagnostic))
        self.assertNotIn("=tok", json.dumps(diagnostic))
        self.assertNotIn("bearer abc", json.dumps(diagnostic).lower())
        for secret in (
            "json-secret", "aws-secret", "hunter2", "space secret", "quoted", "very-inline-secret", "joined secret",
            "ansi space secret", "escaped secret", "url-secret", "ssh-secret", "db-secret",
            "shell-substitution-secret", "line-substitution-secret", "backtick-secret", "process-substitution-secret",
            "line-continuation-secret",
            "private-key-secret", "truncated-key-secret",
        ):
            self.assertNotIn(secret, json.dumps(diagnostic))
        self.assertTrue(diagnostic["startup_log"]["truncated"])
        self.assertEqual(json.loads(stderr.getvalue())["event"], "agent-vm-slow-start")
        row = self.store.connection.execute(
            "SELECT outcome, detail_json FROM audit WHERE action = 'boot-diagnostic' ORDER BY id DESC LIMIT 1"
        ).fetchone()
        self.assertEqual(row["outcome"], "partial")
        self.assertIn("native_start_seconds", row["detail_json"])
        self.assertEqual(sum(args[3:4] == ["start"] for args, _, _ in self.runner.calls), starts_before)

    def test_slow_start_diagnostic_discards_native_error_details(self):
        self.acquire()
        task = self.store.get_task(session_id="thread-a")
        stderr = io.StringIO()
        error = CONTROL.ControlError(
            "command_failed",
            "diagnostic command failed",
            {"stderr": "password=secret ssh://user:url-secret@example.test"},
        )
        with mock.patch.object(self.control.incus, "instance_info", return_value={"status": "Running", "pid": 1}), \
             mock.patch.object(self.control.incus, "instance_log", side_effect=error), \
             mock.patch.object(CONTROL.sys, "stderr", stderr):
            diagnostic = self.control._emit_slow_start_diagnostic(
                task,
                phase="agent",
                started_monotonic=0.0,
                readiness={},
            )

        serialized = json.dumps(diagnostic)
        self.assertNotIn("secret", serialized)
        self.assertNotIn("url-secret", serialized)
        self.assertNotIn("secret", stderr.getvalue())

    def test_slow_start_diagnostic_reports_only_safe_timeout_cleanup_state(self):
        self.acquire()
        task = self.store.get_task(session_id="thread-a")
        error = CONTROL.ControlError(
            "command_timeout",
            "diagnostic command timed out",
            {
                "reaped": True,
                "process_group_killed": False,
                "process_group_cleanup_error": "PermissionError",
                "stderr": "password=secret",
            },
        )
        with mock.patch.object(self.control.incus, "instance_info", return_value={"status": "Running", "pid": 1}), \
             mock.patch.object(self.control.incus, "instance_log", side_effect=error):
            diagnostic = self.control._emit_slow_start_diagnostic(
                task,
                phase="agent",
                started_monotonic=0.0,
                readiness={},
            )

        startup_error = next(item for item in diagnostic["errors"] if item["source"] == "startup_log")
        self.assertTrue(startup_error["reaped"])
        self.assertFalse(startup_error["process_group_killed"])
        self.assertEqual(startup_error["process_group_cleanup_error"], "PermissionError")
        self.assertNotIn("secret", json.dumps(diagnostic))

    def test_proxy_project_restriction_is_checked_before_allocation(self):
        self.runner.proxy_allowed = False
        self.error("network_unverified", self.acquire)
        self.assertEqual(self.runner.instances, {})

    def test_native_readiness_wait_precedes_exec_and_preserves_deadline(self):
        first = self.acquire()
        calls = [args[3:] for args, _, _ in self.runner.calls if args[:3] == ["incus", "--project", "agents"]]
        wait_indexes = [index for index, args in enumerate(calls) if args[0] == "wait"]
        self.assertEqual([calls[index][2] for index in wait_indexes], ["agent", "ipv4"])
        self.assertLess(max(wait_indexes), next(index for index, args in enumerate(calls) if args[0] == "exec"))
        self.assertEqual(first["lease_expires_at"], CONTROL.iso_time(self.now + dt.timedelta(minutes=30)))
        self.call("restart")

    def test_native_readiness_failure_stops_without_exposing_port(self):
        self.runner.failure = lambda args: len(args) > 3 and args[3] == "wait"
        self.error("command_failed", self.acquire)
        task = self.store.get_task(session_id="thread-a")
        self.assertEqual(task["state"], "cached")
        self.assertEqual(self.store.get_ports(task["task_id"]), [])
        self.assertEqual(self.runner.instances[task["instance_name"]]["status"], "Stopped")

    def test_port_add_rechecks_active_generation_before_commit(self):
        self.acquire()
        original_add = self.control._add_proxy

        def expire_after_proxy(task, port):
            original_add(task, port)
            self.store.update_task(task["task_id"], state="closing", lease_expires_at=None)

        with mock.patch.object(self.control, "_add_proxy", side_effect=expire_after_proxy):
            with self.assertRaises(CONTROL.ControlError) as failure:
                self.call("port-add", name="web", vm_port=8080)

        self.assertEqual(failure.exception.code, "task_not_active")
        self.assertEqual(failure.exception.detail["operation_phase"], "port-add")
        self.assertTrue(failure.exception.detail["outcome_verified"])
        task = self.store.get_task(session_id="thread-a")
        self.assertIsNone(self.store.get_port(task["task_id"], "web", "tcp"))

    def test_port_remove_failure_keeps_closing_state_and_marks_outcome_uncertain(self):
        self.acquire()
        self.call("port-add", name="web", vm_port=8080)
        task = self.store.get_task(session_id="thread-a")
        failure = CONTROL.ControlError("conntrack_unverified", "matching flows remain")
        with mock.patch.object(self.control, "_conntrack_delete", side_effect=failure):
            with self.assertRaises(CONTROL.ControlError) as raised:
                self.call("port-remove", name="web")
        self.assertEqual(raised.exception.detail["operation_phase"], "port-remove")
        self.assertFalse(raised.exception.detail["outcome_verified"])
        self.assertEqual(self.store.get_port(task["task_id"], "web", "tcp")["state"], "closing")

    def test_existing_port_retry_cannot_reopen_mapping_after_expiry_preemption(self):
        self.acquire()
        self.call("port-add", name="web", vm_port=8080)
        task = self.store.get_task(session_id="thread-a")
        original_add = self.control._add_proxy

        def expire_after_retry(task, port):
            original_add(task, port)
            self.store.update_task(task["task_id"], state="closing", lease_expires_at=None)

        with mock.patch.object(self.control, "_add_proxy", side_effect=expire_after_retry):
            with self.assertRaises(CONTROL.ControlError) as failure:
                self.call("port-add", name="web", vm_port=8080)

        self.assertEqual(failure.exception.code, "task_not_active")
        self.assertTrue(failure.exception.detail["outcome_verified"])
        self.assertIsNone(self.store.get_port(task["task_id"], "web", "tcp"))
        self.assertNotIn("agent-vm-" + task["task_id"] + "-web-tcp", self.runner.instances[task["instance_name"]]["devices"])

    def test_existing_port_retry_with_deleted_row_withdraws_late_proxy(self):
        self.acquire()
        self.call("port-add", name="web", vm_port=8080)
        task = self.store.get_task(session_id="thread-a")
        instance = self.runner.instances[task["instance_name"]]
        device_name = "agent-vm-" + task["task_id"] + "-web-tcp"
        instance["devices"].pop(device_name)
        instance["expanded_devices"].pop(device_name)
        original_add = self.control.incus.device_add

        def cleanup_row_only(_task):
            self.store.remove_port(task["task_id"], "web", "tcp")
            return [], []

        def add_then_expire(name, device, kind, properties):
            original_add(name, device, kind, properties)
            current = self.store.get_task(task_id=task["task_id"])
            self.now = CONTROL.parse_time(current["lease_expires_at"])
            self.control._expire_preempt({"task_id": task["task_id"], "generation": task["generation"]})

        with mock.patch.object(self.control, "_cleanup_task", side_effect=cleanup_row_only), \
             mock.patch.object(self.control.incus, "device_add", side_effect=add_then_expire):
            with self.assertRaises(CONTROL.ControlError) as failure:
                self.call("port-add", name="web", vm_port=8080)

        self.assertEqual(failure.exception.code, "task_not_active")
        self.assertIsNone(self.store.get_port(task["task_id"], "web", "tcp"))
        self.assertNotIn(device_name, instance["devices"])

    def test_restart_rechecks_active_generation_before_success(self):
        self.acquire()
        original_verify = self.control._verify_guest_tools

        def expire_after_post_check(instance_name):
            original_verify(instance_name)
            task = self.store.get_task(session_id="thread-a")
            self.store.update_task(task["task_id"], state="closing", lease_expires_at=None)

        with mock.patch.object(self.control, "_verify_guest_tools", side_effect=expire_after_post_check):
            with self.assertRaises(CONTROL.ControlError) as failure:
                self.call("restart")

        self.assertEqual(failure.exception.code, "task_not_active")

    def test_restart_serializes_drain_with_native_stop_start(self):
        self.acquire()
        attempted = threading.Event()
        drained = threading.Event()
        events = []

        def enter_drain():
            attempted.set()
            self.control.dispatch({
                "protocol": CONTROL.PROTOCOL_VERSION,
                "action": "drain-enter",
                "generation": 0,
                "operation_id": "restart-drain-race",
            })
            events.append("drain")
            drained.set()

        original_stop = self.control._stop_instance
        original_start = self.control._start_instance

        def stop(task):
            events.append("stop")
            thread = threading.Thread(target=enter_drain)
            thread.start()
            self.assertTrue(attempted.wait(1))
            thread.join(1)
            self.assertFalse(thread.is_alive())
            self.assertTrue(drained.is_set())
            original_stop(task)
            self._drain_thread = thread

        def start(task):
            events.append("start")
            original_start(task)

        with mock.patch.object(self.control, "_stop_instance", side_effect=stop), \
             mock.patch.object(self.control, "_start_instance", side_effect=start):
            self.call("restart")

        self._drain_thread.join(1)
        self.assertTrue(drained.is_set())
        self.assertLess(events.index("drain"), events.index("start"))
        self.call("drain-exit")

    def test_readiness_wait_cannot_extend_or_outlive_lease(self):
        def expire(name, condition):
            self.now += dt.timedelta(minutes=31)
        self.runner.on_wait = expire
        self.error("lease_expired", self.acquire)
        task = self.store.get_task(session_id="thread-a")
        self.assertEqual(task["state"], "cached")
        self.assertEqual(self.store.get_ports(task["task_id"]), [])

    def test_missing_pinned_image_blocks_preflight_and_new_allocation(self):
        self.runner.image_available = False
        doctor = self.call("doctor")
        self.assertFalse(doctor["ready"])
        self.assertIn("image_unavailable", doctor["issues"])
        self.error("image_unavailable", self.acquire)
        self.assertEqual(self.runner.instances, {})
        self.assertEqual(self.store.list_tasks(), [])

    def test_container_image_is_refused_before_cache_reclaim(self):
        cached = self.acquire("old"); self.call("release", "old")
        self.runner.free = 10*GIB
        self.runner.image_type = "container"
        self.error("image_unverified", self.acquire, "new")
        self.assertEqual(self.store.get_task(task_id=cached["task_id"])["state"], "cached")
        self.assertEqual(len(self.runner.instances), 1)

    def test_api_queries_put_project_in_url_and_never_global_flags(self):
        self.acquire()
        queries = [args for args, _, _ in self.runner.calls if 'query' in args]
        self.assertTrue(queries)
        for args in queries:
            self.assertEqual(args[:2], ['incus', 'query'])
            self.assertNotIn('--project', args)
            self.assertIn('project=', args[-1])

    def test_isolation_policy_drift_is_rejected_before_creating_vm(self):
        query = self.control.incus.query
        def drift(path, **kwargs):
            value = query(path, **kwargs)
            if path.startswith('/1.0/network-acls/'):
                value['egress'][0]['action'] = 'allow'
            return value
        with mock.patch.object(self.control.incus, 'query', side_effect=drift):
            self.error('network_unverified', self.acquire)
        self.assertEqual(self.runner.instances, {})

    def test_malformed_isolation_acl_is_structured(self):
        query = self.control.incus.query

        def malformed(path, **kwargs):
            value = query(path, **kwargs)
            if path.startswith('/1.0/network-acls/'):
                value['egress'] = ["malformed"]
            return value

        with mock.patch.object(self.control.incus, 'query', side_effect=malformed):
            self.error('network_unverified', self.acquire)
        self.assertEqual(self.runner.instances, {})

    def test_malformed_native_device_inventory_is_structured(self):
        active = self.acquire()
        self.runner.instances[active["instance_name"]]["devices"]["eth0"] = "malformed"
        task = self.store.get_task(task_id=active["task_id"])
        with self.assertRaises(CONTROL.ControlError) as failure:
            self.control._ownership(task)
        self.assertEqual(failure.exception.code, "ownership_mismatch")

    def test_malformed_native_proxy_listen_is_structured(self):
        self.runner.proxy_inventory = [{
            "name": "foreign-proxy",
            "expanded_devices": {"proxy": {"type": "proxy", "listen": 20001}},
        }]
        with self.assertRaises(CONTROL.ControlError) as failure:
            self.control._native_ports()
        self.assertEqual(failure.exception.code, "port_inventory_unverified")

    def test_early_old_timer_rearms_recorded_deadline(self):
        first = self.acquire()
        renewed = self.call("renew")
        self.now = CONTROL.parse_time(first["lease_expires_at"])
        with mock.patch.object(self.systemd, "schedule_expiry") as schedule:
            result = self.control.dispatch({"protocol": CONTROL.PROTOCOL_VERSION, "action": "timer-expire", "task_id": first["task_id"], "generation": 1})
        self.assertEqual(result["result"]["action"], "rearmed")
        self.assertEqual(schedule.call_args.args[-1], renewed["lease_expires_at"])
        self.assertEqual(next(iter(self.runner.instances.values()))["status"], "Running")

    def test_failed_generation_switch_can_be_reconciled_without_foreign_deletion(self):
        self.acquire()
        self.call("release")
        self.runner.failure = lambda args: "user.agent-vm.generation=2" in args
        self.error("command_failed", self.acquire)
        self.assertEqual(self.call("status")["state"], "closing")
        self.runner.failure = None
        self.call("admin-reconcile", apply=True)
        self.assertEqual(self.call("status")["state"], "cached")
        self.assertEqual(next(iter(self.runner.instances.values()))["config"]["user.agent-vm.generation"], "2")

    def test_reconcile_stops_an_unexpectedly_running_cached_guest(self):
        first = self.acquire()
        self.call("release")
        instance = self.runner.instances[first["instance_name"]]
        instance["status"] = "Running"
        self.call("admin-reconcile", apply=True)
        self.assertEqual(instance["status"], "Stopped")
        self.assertEqual(self.call("status")["state"], "cached")

    def test_metadata_and_port_reservations_precede_native_exposure(self):
        original = self.control._add_proxy
        def inspect(task, port):
            durable = self.store.get_port(task["task_id"], port["port_name"], port["protocol"])
            self.assertEqual(durable["state"], "preparing")
            self.assertIsNotNone(self.store.get_task(task_id=task["task_id"])["lease_expires_at"])
            return original(task, port)
        with mock.patch.object(self.control, "_add_proxy", side_effect=inspect):
            self.acquire()
            self.call("port-add", name="web", vm_port=8080)

    def test_host_port_scan_failure_is_not_treated_as_empty_inventory(self):
        self.runner.failure = lambda args: args[0] == "ss"
        self.error("command_failed", self.acquire)
        self.assertEqual(self.runner.instances, {})

    def test_allocation_replay_and_reuse_never_renew(self):
        request = self.request("acquire", public_key=KEY)
        first = self.control.dispatch(request)
        self.now += dt.timedelta(seconds=20)
        replay = self.control.dispatch(request)
        reused = self.acquire()
        self.assertEqual(first["result"]["lease_expires_at"], replay["result"]["lease_expires_at"])
        self.assertEqual(first["result"]["lease_expires_at"], reused["lease_expires_at"])
        self.assertEqual(replay["outcome"], "replayed")
        self.assertNotEqual(first["server_time"], replay["server_time"])
        self.assertEqual(sum(args[3:4] == ["init"] for args, _, _ in self.runner.calls), 1)
        self.assertEqual(self.call("status")["generation"], 1)
        init = next(args for args, _, _ in self.runner.calls if args[3:4] == ["init"])
        self.assertEqual(init[4], "a"*64)
        instance = next(iter(self.runner.instances.values()))
        self.assertEqual(instance["config"]["boot.autostart"], "false")
        self.assertEqual(instance["expanded_devices"]["eth0"]["security.port_isolation"], "true")

    def test_renew_commit_survives_timer_failure_without_double_extension(self):
        first = self.acquire()
        request = self.request("renew")
        with mock.patch.object(self.systemd, "schedule_expiry", side_effect=CONTROL.ControlError("timer_failed", "injected")):
            self.error("timer_failed", self.control.dispatch, request)
        after = self.call("status")["lease_expires_at"]
        replay = self.control.dispatch(request)
        self.assertEqual(replay["outcome"], "replayed")
        self.assertEqual(after, self.call("status")["lease_expires_at"])
        self.assertEqual(CONTROL.parse_time(after)-CONTROL.parse_time(first["lease_expires_at"]), dt.timedelta(minutes=15))

    def test_renew_journal_persists_compatibility_warning_and_response_metadata(self):
        self.acquire()
        request = self.request("renew", operation="renew-warning")
        first = self.control.dispatch(request)
        replay = self.control.dispatch(request)
        self.assertEqual(replay["outcome"], "replayed")
        self.assertEqual(
            {item["code"] for item in first["warnings"]},
            {item["code"] for item in replay["warnings"]},
        )
        saved = self.store.operation("renew-warning")
        self.assertEqual(saved["outcome"], "success")
        self.assertEqual(saved["response"], {"schema": 1, "semantic_revision": 1})
        self.assertEqual(
            {item["code"] for item in saved["warnings"]},
            {item["code"] for item in first["warnings"]},
        )

    def test_operation_payload_conflict_and_crash_are_explicit(self):
        self.acquire()
        request = self.request("port-add", name="web", vm_port=8080)
        with mock.patch.object(self.control, "_add_proxy", side_effect=SystemExit(99)):
            with self.assertRaises(SystemExit): self.control.dispatch(request)
        self.error("operation_incomplete", self.control.dispatch, request)
        self.error("operation_conflict", self.control.dispatch, dict(request, vm_port=8081))
        self.assertEqual(len(self.store.get_ports(self.call("status")["task_id"])), 2)
        self.call("admin-reconcile", apply=True)
        self.assertEqual(len(self.store.get_ports(self.call("status")["task_id"])), 1)

    def test_release_is_lazy_and_age_is_not_reset(self):
        first = self.acquire()
        self.now += dt.timedelta(minutes=5)
        cached = self.call("release")
        self.assertEqual(cached["state"], "cached")
        self.assertEqual(cached["ports"], [])
        self.assertEqual(next(iter(self.runner.instances.values()))["status"], "Stopped")
        self.now += dt.timedelta(hours=1)
        again = self.call("release")
        self.assertEqual(again["inactive_since"], cached["inactive_since"])
        restored = self.acquire()
        self.assertEqual(restored["task_id"], first["task_id"])
        self.assertEqual(restored["generation"], 2)
        self.assertEqual([p["port_name"] for p in restored["ports"]], ["ssh"])

    def test_expiry_and_stale_generation_do_not_open_or_damage_new_lease(self):
        first = self.acquire()
        self.now = CONTROL.parse_time(first["lease_expires_at"])
        self.error("lease_expired", self.call, "renew")
        self.error("lease_expired", self.call, "ssh-info")
        self.control.dispatch({"protocol": CONTROL.PROTOCOL_VERSION, "action": "timer-expire", "task_id": first["task_id"], "generation": 1})
        cached = self.call("status")
        self.assertEqual(cached["inactive_since"], first["lease_expires_at"])
        self.assertEqual(cached["state"], "cached")
        self.acquire()
        stale = self.control.dispatch({"protocol": CONTROL.PROTOCOL_VERSION, "action": "timer-expire", "task_id": first["task_id"], "generation": 1})
        self.assertEqual(stale["result"]["reason"], "stale-generation")
        self.assertTrue(self.call("status")["effective_lease"])

    def test_expiry_preempts_slow_owner_before_final_object_lock(self):
        first = self.acquire()
        self.now = CONTROL.parse_time(first["lease_expires_at"])
        with self.control._object_lock("session:thread-a"):
            self.control._expire_preempt({"task_id": first["task_id"], "generation": 1})
            self.assertEqual(self.store.get_task(task_id=first["task_id"])["state"], "closing")
            self.assertEqual(next(iter(self.runner.instances.values()))["status"], "Stopped")
            self.assertEqual(len(self.store.get_ports(first["task_id"])), 1)
        self.control.dispatch({"protocol": CONTROL.PROTOCOL_VERSION, "action": "timer-expire", "task_id": first["task_id"], "generation": 1})
        self.assertEqual(self.call("status")["state"], "cached")

    def test_stale_expiry_error_cannot_overwrite_new_generation(self):
        first = self.acquire()
        task = self.store.get_task(task_id=first["task_id"])
        self.now = CONTROL.parse_time(first["lease_expires_at"])

        def switch_generation_then_fail(_task):
            self.store.update_task(task["task_id"], generation=2, state="active",
                                   lease_expires_at=CONTROL.iso_time(self.now + dt.timedelta(minutes=10)),
                                   last_error=None)
            raise CONTROL.ControlError("cleanup_failed", "stale cleanup failure")

        with mock.patch.object(self.control, "_cleanup_task", side_effect=switch_generation_then_fail):
            self.control._expire_preempt({"task_id": task["task_id"], "generation": task["generation"]})
        latest = self.store.get_task(task_id=task["task_id"])
        self.assertEqual(latest["generation"], 2)
        self.assertEqual(latest["state"], "active")
        self.assertIsNone(latest["last_error"])

    def test_cleanup_does_not_stop_after_durable_generation_changes(self):
        first = self.acquire()
        task = self.store.get_task(task_id=first["task_id"])
        closing = self.store.update_task(task["task_id"], state="closing", lease_expires_at=None)

        def switch_generation(_task, _port):
            self.store.update_task(
                closing["task_id"],
                generation=2,
                state="active",
                lease_expires_at=CONTROL.iso_time(self.now + dt.timedelta(minutes=10)),
            )

        with mock.patch.object(self.control, "_remove_proxy", side_effect=switch_generation):
            errors, _ = self.control._cleanup_task(closing)

        self.assertIn("stale-cleanup:generation", errors)
        self.assertEqual(self.runner.instances[closing["instance_name"]]["status"], "Running")

    def test_flow_failure_still_stops_and_retains_reservation(self):
        first = self.acquire()
        with mock.patch.object(CONTROL.shutil, "which", return_value=None):
            self.error("cleanup_incomplete", self.call, "release")
        self.assertEqual(next(iter(self.runner.instances.values()))["status"], "Stopped")
        self.assertEqual(self.call("status")["state"], "closing")
        self.assertEqual(len(self.store.get_ports(first["task_id"])), 1)
        self.call("admin-reconcile", apply=True)
        self.assertEqual(self.call("status")["state"], "cached")
        for args, _, _ in self.runner.calls:
            if args[0] == "conntrack":
                self.assertNotIn("-F", args)
                self.assertIn("--reply-port-src", args)
                self.assertIn(first["vm_ip"], args)

    def test_foreign_identity_is_never_stopped_or_deleted(self):
        self.acquire()
        next(iter(self.runner.instances.values()))["config"]["user.agent-vm.session"] = "foreign"
        self.error("cleanup_incomplete", self.call, "release")
        self.assertEqual(next(iter(self.runner.instances.values()))["status"], "Running")
        self.assertFalse(any(args[3:4] in (["stop"], ["delete"]) for args, _, _ in self.runner.calls))

    def test_draining_preserves_access_but_refuses_new_and_extended_leases(self):
        self.acquire()
        self.call("drain-enter")
        self.assertTrue(self.acquire()["effective_lease"])
        self.call("ssh-info")
        self.error("draining", self.call, "renew")
        self.error("draining", self.acquire, "thread-b")
        before = len(self.runner.calls)
        self.error("draining", self.call, "restart")
        self.assertFalse(any(args[3:4] == ["stop"] for args, _, _ in self.runner.calls[before:]))
        self.error("draining", self.call, "resize", disk_gib=40, allow_restart=True)
        self.call("release")
        self.error("draining", self.acquire)

    def test_reconcile_apply_requires_a_json_boolean(self):
        self.acquire()
        before = len(self.runner.calls)
        self.error("invalid_request", self.call, "admin-reconcile", apply="false")
        self.assertEqual(len(self.runner.calls), before)

    def test_reconcile_reuses_native_inventory_for_absent_tasks(self):
        for index in range(3):
            task = self.control._base_task(f"reconcile-{index}", index + 1)
            task.update(
                state="absent",
                resources_json=json.dumps({"cpu": 0, "memory_mib": 0, "disk_gib": 0}),
                resource_reservation_json="{}",
                reservation_bytes=0,
            )
            self.store.put_task(task)

        self.runner.calls.clear()
        result = self.call("admin-reconcile")
        self.assertTrue(result["dry_run"])
        self.assertEqual(len(result["actions"]), 3)
        list_calls = [
            args for args, _, _ in self.runner.calls
            if args[:4] == ["incus", "--project", "agents", "list"]
        ]
        self.assertEqual(len(list_calls), 1)

    def test_reconcile_refreshes_inventory_before_absent_repair(self):
        task = self.control._base_task("reconcile-repair", 1)
        task.update(
            state="absent",
            resources_json=json.dumps({"cpu": 1, "memory_mib": 1, "disk_gib": 1}),
            resource_reservation_json="{}",
            reservation_bytes=0,
        )
        self.store.put_task(task)
        listings = [0]

        def inject_native_instance():
            listings[0] += 1
            if listings[0] == 2:
                self.runner.instances[task["instance_name"]] = {"name": task["instance_name"]}

        self.runner.on_list_instances = inject_native_instance
        result = self.call("admin-reconcile", apply=True)
        self.assertEqual(result["actions"][0]["error"]["code"], "resource_unverified")
        self.assertEqual(listings[0], 2)
        current = self.store.get_task(task_id=task["task_id"])
        self.assertEqual(current["resources_json"], task["resources_json"])

    def test_port_range_inventory_and_exact_collision(self):
        self.acquire()
        self.runner.proxy_inventory = [{"name": "foreign-proxy", "expanded_devices": {"proxy": {"type": "proxy", "listen": "tcp:0.0.0.0:20001-20003"}}}]
        self.runner.forwards = [{"config": {}, "ports": [{"listen_port": "20004,20005-20006"}]}]
        mapping = self.call("port-add", name="web", vm_port=8080)
        self.assertEqual(mapping["host_port"], 20007)
        self.error("port_busy", self.call, "port-add", name="other", vm_port=9000, host_port=20002)
        self.call("port-remove", name="web")
        self.assertEqual(len(self.call("port-list")["ports"]), 1)

    def test_resize_plan_has_no_mutation_and_disk_growth_preserves_deadline(self):
        first = self.acquire()
        before = len(self.runner.calls)
        plan = self.call("resize", cpu=3, disk_gib=40)
        self.assertFalse(plan["applied"])
        self.assertFalse(any(args[3:4] in (["stop"], ["delete"]) for args, _, _ in self.runner.calls[before:]))
        disk_plan = self.call("resize", disk_gib=40)
        self.assertFalse(disk_plan["applied"])
        self.assertEqual(self.runner.instances[first["instance_name"]]["expanded_devices"]["root"]["size"], "20GiB")
        self.assertFalse(any(args[3:4] == ["stop"] for args, _, _ in self.runner.calls[before:]))
        grown = self.call("resize", disk_gib=40, allow_restart=True)
        self.assertEqual(grown["resources"]["disk_gib"], 40)
        self.assertEqual(grown["guest_filesystem"]["virtual_bytes"], 40*GIB)
        self.assertEqual(grown["lease_expires_at"], first["lease_expires_at"])
        self.error("resize_not_growth", self.call, "resize", disk_gib=30)

    def test_write_failure_is_partial_and_never_claims_success(self):
        self.acquire()
        self.runner.failure = lambda args: "size=40GiB" in args
        self.error("resize_partial", self.call, "resize", disk_gib=40, allow_restart=True)
        self.assertEqual(self.call("status")["last_error"]["code"], "resize_partial")
        self.assertEqual(self.call("status")["state"], "cached")

    def test_same_capacity_guest_growth_failure_closes_active_access(self):
        self.acquire()
        self.runner.guest_growth_results = [self.guest_growth_failure()]
        self.error("resize_partial", self.call, "resize", disk_gib=20, allow_restart=True)

        task = self.store.get_task(session_id="thread-a")
        self.assertEqual(task["state"], "cached")
        self.assertIsNone(task["lease_expires_at"])
        self.assertIsNone(task["ssh_host_port"])
        self.assertEqual(self.store.get_ports(task["task_id"]), [])
        failure = json.loads(task["last_error"])
        self.assertTrue(failure["cleanup"]["performed"])
        self.assertEqual(failure["error"]["detail"]["phase"], "filesystem-grow")

    def test_acquire_guest_growth_failure_is_structured_and_closed(self):
        self.runner.guest_growth_results = [self.guest_growth_failure()]
        self.error("guest_resize_unverified", self.acquire)

        task = self.store.get_task(session_id="thread-a")
        self.assertEqual(task["state"], "cached")
        self.assertIsNone(task["lease_expires_at"])
        self.assertIsNone(task["ssh_host_port"])
        self.assertEqual(self.store.get_ports(task["task_id"]), [])
        self.assertEqual(self.runner.instances[task["instance_name"]]["status"], "Stopped")
        failure = json.loads(task["last_error"])
        self.assertEqual(failure["code"], "acquire_partial")
        self.assertEqual(failure["phase"], "filesystem-grow")
        self.assertEqual(failure["command"], ["resize2fs", "/dev/sda2"])
        self.assertEqual(failure["returncode"], 1)
        self.assertEqual(failure["stderr"], "resize2fs: transient guest failure")
        self.assertEqual(failure["guest_capacity"]["disk_bytes"], 20 * GIB)

    def test_native_init_timeout_keeps_absence_pending_for_delayed_reconciliation(self):
        task = self.control._base_task("thread-a", 1)
        task.update(
            state="preparing",
            lease_expires_at=CONTROL.iso_time(self.now + dt.timedelta(minutes=10)),
            resources_json=json.dumps({"cpu": 2, "memory_mib": 4096, "disk_gib": 20}),
            resource_reservation_json=json.dumps({"cpu": 2, "memory_mib": 4096, "disk_gib": 20}),
            reservation_bytes=4 * GIB,
        )
        self.store.put_task(task)
        with mock.patch.object(self.control, "_cleanup_task", return_value=([], [])), \
             mock.patch.object(self.control.incus, "list_instances", return_value=[]):
            with self.assertRaises(CONTROL.ControlError) as failure:
                self.control._release_task(
                    task,
                    require_native_init_absence_retry=True,
                    native_init_cleanup_verified=True,
                )
        self.assertEqual(failure.exception.code, "cleanup_incomplete")
        pending = self.store.get_task(task_id=task["task_id"])
        self.assertEqual(pending["state"], "closing")
        self.assertEqual(pending["reservation_bytes"], 4 * GIB)
        self.assertTrue(json.loads(pending["last_error"])["native_init_absence_retry"])

        self.now += dt.timedelta(seconds=CONTROL.NATIVE_INIT_ABSENCE_RETRY_SECONDS)
        with mock.patch.object(self.control, "_cleanup_task", return_value=([], [])), \
             mock.patch.object(self.control.incus, "list_instances", return_value=[]):
            self.control._release_task(pending)
        self.assertEqual(self.store.get_task(task_id=task["task_id"])["state"], "absent")

    def test_native_init_timeout_reconciles_through_public_admin_action(self):
        timeout = CONTROL.ControlError(
            "command_timeout",
            "native init timed out",
            {
                "reaped": True,
                "pipes_drained": True,
                "process_group_killed": True,
                "process_group_cleanup_error": None,
            },
        )
        with mock.patch.object(self.control.incus, "init", side_effect=timeout):
            with self.assertRaises(CONTROL.ControlError) as failure:
                self.acquire()
        self.assertEqual(failure.exception.code, "command_timeout")
        task = self.store.get_task(session_id="thread-a")
        self.assertEqual(task["state"], "closing")
        self.assertGreater(task["reservation_bytes"], 0)
        record = json.loads(task["last_error"])
        self.assertTrue(record["native_init_absence_retry"])
        self.assertTrue(record["native_init_cleanup_verified"])

        self.now += dt.timedelta(seconds=CONTROL.NATIVE_INIT_ABSENCE_RETRY_SECONDS)
        result = self.call("admin-reconcile", apply=True)
        self.assertFalse(result["dry_run"])
        self.assertEqual(self.store.get_task(session_id="thread-a")["state"], "absent")
        self.assertEqual(self.store.operation("op-1")["outcome"], "error")

    def test_deterministic_native_init_failure_releases_reservation(self):
        failure = CONTROL.ControlError("command_failed", "image unpack failed")
        with mock.patch.object(self.control.incus, "init", side_effect=failure):
            with self.assertRaises(CONTROL.ControlError) as error:
                self.acquire()
        self.assertEqual(error.exception.code, "command_failed")
        task = self.store.get_task(session_id="thread-a")
        self.assertEqual(task["state"], "absent")
        self.assertEqual(task["reservation_bytes"], 0)
        self.assertFalse(json.loads(task["last_error"])["native_init_absence_retry"])
        self.assertEqual(self.store.operation("op-1")["outcome"], "error")

    def test_native_init_cleanup_failure_keeps_reservation_pending(self):
        failure = CONTROL.ControlError(
            "command_failed",
            "native init cleanup could not be verified",
            {
                "process_group_killed": False,
                "process_group_cleanup_error": "process_owner_boundary_unavailable",
            },
        )
        with mock.patch.object(self.control.incus, "init", side_effect=failure):
            with self.assertRaises(CONTROL.ControlError) as error:
                self.acquire()
        self.assertEqual(error.exception.code, "command_failed")
        task = self.store.get_task(session_id="thread-a")
        self.assertEqual(task["state"], "closing")
        self.assertGreater(task["reservation_bytes"], 0)
        self.assertTrue(json.loads(task["last_error"])["native_init_absence_retry"])
        self.assertEqual(self.store.operation("op-1")["outcome"], "pending")

    def test_native_init_failure_with_residual_instance_is_never_cached(self):
        failure = CONTROL.ControlError("command_failed", "image unpack failed")

        def init_with_residue(_image, name, metadata):
            self.runner.instances[name] = {
                "name": name,
                "status": "Stopped",
                "config": dict(metadata),
                "devices": {},
                "expanded_devices": {
                    "eth0": {"type": "nic", "network": "agentsbr0"},
                    "root": {"type": "disk", "size": "20GiB"},
                },
            }
            raise failure

        with mock.patch.object(self.control.incus, "init", side_effect=init_with_residue):
            with self.assertRaises(CONTROL.ControlError) as error:
                self.acquire()
        self.assertEqual(error.exception.code, "command_failed")
        task = self.store.get_task(session_id="thread-a")
        self.assertEqual(task["state"], "absent")
        self.assertEqual(task["reservation_bytes"], 0)
        self.assertNotIn(task["instance_name"], self.runner.instances)
        self.assertEqual(self.store.operation("op-1")["outcome"], "error")

    def test_native_init_cleanup_failure_never_caches_visible_instance(self):
        task = self.control._base_task("thread-a", 1)
        task.update(
            state="preparing",
            lease_expires_at=CONTROL.iso_time(self.now + dt.timedelta(minutes=10)),
            resources_json=json.dumps({"cpu": 2, "memory_mib": 4096, "disk_gib": 20}),
            resource_reservation_json=json.dumps({"cpu": 2, "memory_mib": 4096, "disk_gib": 20}),
            reservation_bytes=4 * GIB,
        )
        self.store.put_task(task)
        self.runner.instances[task["instance_name"]] = {
            "name": task["instance_name"],
            "status": "Stopped",
            "config": {
                "user.agent-vm.managed": "native-v1",
                "user.agent-vm.task": task["task_id"],
                "user.agent-vm.session": task["session_id"],
                "user.agent-vm.generation": str(task["generation"]),
            },
            "devices": {},
            "expanded_devices": {"root": {"type": "disk", "size": "20GiB"}},
        }
        with mock.patch.object(self.control, "_cleanup_task", return_value=([], [])), \
             mock.patch.object(self.systemd, "retry_cleanup") as retry_cleanup:
            with self.assertRaises(CONTROL.ControlError) as error:
                self.control._release_task(
                    task,
                    require_native_init_absence_retry=True,
                    native_init_cleanup_verified=False,
                )
        self.assertEqual(error.exception.code, "cleanup_incomplete")
        current = self.store.get_task(task_id=task["task_id"])
        self.assertEqual(current["state"], "closing")
        self.assertEqual(current["reservation_bytes"], 4 * GIB)
        self.assertEqual(self.runner.instances[task["instance_name"]]["status"], "Stopped")
        retry_cleanup.assert_called_once()

    def test_verified_native_init_cleanup_deletes_visible_instance_before_absence_retry(self):
        task = self.control._base_task("thread-a", 1)
        task.update(
            state="preparing",
            lease_expires_at=CONTROL.iso_time(self.now + dt.timedelta(minutes=10)),
            resources_json=json.dumps({"cpu": 2, "memory_mib": 4096, "disk_gib": 20}),
            resource_reservation_json=json.dumps({"cpu": 2, "memory_mib": 4096, "disk_gib": 20}),
            reservation_bytes=4 * GIB,
        )
        self.store.put_task(task)
        self.runner.instances[task["instance_name"]] = {
            "name": task["instance_name"],
            "status": "Stopped",
            "config": {
                "user.agent-vm.managed": "native-v1",
                "user.agent-vm.task": task["task_id"],
                "user.agent-vm.session": task["session_id"],
                "user.agent-vm.generation": str(task["generation"]),
            },
            "devices": {},
            "expanded_devices": {"root": {"type": "disk", "size": "20GiB"}},
        }
        with mock.patch.object(self.control, "_cleanup_task", return_value=([], [])):
            with self.assertRaises(CONTROL.ControlError) as error:
                self.control._release_task(
                    task,
                    require_native_init_absence_retry=True,
                    native_init_cleanup_verified=True,
                )
        self.assertEqual(error.exception.code, "cleanup_incomplete")
        self.assertNotIn(task["instance_name"], self.runner.instances)
        current = self.store.get_task(task_id=task["task_id"])
        self.assertEqual(current["state"], "closing")
        self.assertEqual(current["reservation_bytes"], 4 * GIB)

    def test_native_inventory_failure_keeps_expiry_retryable(self):
        task = self.control._base_task("thread-a", 1)
        task.update(
            state="preparing",
            lease_expires_at=CONTROL.iso_time(self.now + dt.timedelta(minutes=10)),
            resources_json=json.dumps({"cpu": 2, "memory_mib": 4096, "disk_gib": 20}),
            resource_reservation_json=json.dumps({"cpu": 2, "memory_mib": 4096, "disk_gib": 20}),
            reservation_bytes=4 * GIB,
        )
        self.store.put_task(task)
        with mock.patch.object(
            self.control.incus,
            "list_instances",
            side_effect=[[], CONTROL.ControlError("command_failed", "inventory unavailable")],
        ), mock.patch.object(self.systemd, "remove_expiry") as remove_expiry, mock.patch.object(
            self.systemd, "retry_cleanup"
        ) as retry_cleanup:
            with self.assertRaises(CONTROL.ControlError) as error:
                self.control._release_task(task)
        self.assertEqual(error.exception.code, "cleanup_incomplete")
        remove_expiry.assert_not_called()
        retry_cleanup.assert_called_once()
        self.assertEqual(self.store.get_task(task_id=task["task_id"])["state"], "closing")

    def test_pending_native_init_recovery_waits_for_stable_absence(self):
        task = self.control._base_task("thread-a", 1)
        task.update(
            state="preparing",
            lease_expires_at=CONTROL.iso_time(self.now + dt.timedelta(minutes=10)),
            resources_json=json.dumps({"cpu": 2, "memory_mib": 4096, "disk_gib": 20}),
            resource_reservation_json=json.dumps({"cpu": 2, "memory_mib": 4096, "disk_gib": 20}),
            reservation_bytes=4 * GIB,
        )
        self.store.put_task(task)
        self.store.record_operation(
            "pending-acquire", "acquire", "thread-a", 1,
            {"outcome": "pending", "_request_digest": "digest"},
        )
        with mock.patch.object(self.control, "_cleanup_task", return_value=([], [])), \
             mock.patch.object(self.control.incus, "list_instances", return_value=[]):
            with self.assertRaises(CONTROL.ControlError):
                self.control._release_task(task)
        pending = self.store.get_task(task_id=task["task_id"])
        self.assertEqual(pending["state"], "closing")
        self.assertEqual(json.loads(pending["last_error"])["native_init_absence_checks"], 1)

        self.now += dt.timedelta(seconds=CONTROL.NATIVE_INIT_ABSENCE_RETRY_SECONDS)
        with mock.patch.object(self.control, "_cleanup_task", return_value=([], [])), \
             mock.patch.object(self.control.incus, "list_instances", return_value=[]):
            self.control._release_task(pending)
        self.assertEqual(self.store.get_task(task_id=task["task_id"])["state"], "absent")
        self.assertEqual(self.store.operation("pending-acquire")["outcome"], "error")

    def test_malformed_native_init_retry_timestamp_is_repaired(self):
        task = self.control._base_task("thread-a", 1)
        task.update(
            state="closing",
            lease_expires_at=None,
            resources_json=json.dumps({"cpu": 2, "memory_mib": 4096, "disk_gib": 20}),
            resource_reservation_json=json.dumps({"cpu": 2, "memory_mib": 4096, "disk_gib": 20}),
            reservation_bytes=4 * GIB,
            last_error=json.dumps({
                "native_init_absence_retry": True,
                "native_init_absence_retry_after": None,
                "native_init_cleanup_verified": True,
            }),
        )
        self.store.put_task(task)
        with mock.patch.object(self.control, "_cleanup_task", return_value=([], [])), \
             mock.patch.object(self.control.incus, "list_instances", return_value=[]):
            with self.assertRaises(CONTROL.ControlError):
                self.control._release_task(task)
        repaired = json.loads(self.store.get_task(task_id=task["task_id"])["last_error"])
        self.assertIsInstance(repaired["native_init_absence_retry_after"], str)
        self.assertEqual(repaired["native_init_absence_checks"], 1)

    def test_resize_guest_growth_failure_is_structured_and_closed(self):
        self.acquire()
        self.runner.guest_growth_results = [self.guest_growth_failure(disk_gib=40)]
        self.error("resize_partial", self.call, "resize", disk_gib=40, allow_restart=True)

        task = self.store.get_task(session_id="thread-a")
        self.assertEqual(task["state"], "cached")
        self.assertIsNone(task["lease_expires_at"])
        self.assertIsNone(task["ssh_host_port"])
        self.assertEqual(self.store.get_ports(task["task_id"]), [])
        failure = json.loads(task["last_error"])
        self.assertEqual(failure["code"], "resize_partial")
        self.assertEqual(failure["phase"], "guest-post-check")
        self.assertEqual(failure["completed"], ["virtual_disk"])
        self.assertTrue(failure["cleanup"]["ok"])
        self.assertEqual(failure["error"]["detail"]["phase"], "filesystem-grow")

    def test_resize_guest_growth_cleanup_retries_proxy_withdrawal_after_stop(self):
        self.acquire()
        task = self.store.get_task(session_id="thread-a")
        device_name = "agent-vm-" + task["task_id"] + "-ssh-tcp"
        remove_attempts = 0

        def fail_first_proxy_removal(args):
            nonlocal remove_attempts
            if len(args) >= 6 and args[3:6] == ["config", "device", "remove"]:
                remove_attempts += 1
                return remove_attempts == 1
            return False

        self.runner.failure = fail_first_proxy_removal
        self.runner.guest_growth_results = [self.guest_growth_failure(disk_gib=40)]
        self.error("resize_partial", self.call, "resize", disk_gib=40, allow_restart=True)

        task = self.store.get_task(session_id="thread-a")
        self.assertEqual(remove_attempts, 2)
        self.assertEqual(task["state"], "cached")
        self.assertEqual(self.store.get_ports(task["task_id"]), [])
        self.assertNotIn(device_name, self.runner.instances[task["instance_name"]]["devices"])
        self.assertEqual(self.runner.instances[task["instance_name"]]["status"], "Stopped")

    def test_acquire_guest_growth_failure_can_retry_from_cached_state(self):
        self.runner.guest_growth_results = [self.guest_growth_failure()]
        self.error("guest_resize_unverified", self.acquire)
        self.runner.guest_growth_results = [self.guest_growth_result(status="grown_after_retry", attempts=2)]

        result = self.acquire()

        self.assertEqual(result["state"], "active")
        self.assertEqual(result["guest_filesystem"]["status"], "grown_after_retry")
        self.assertNotIn("last_error", result)
        task = self.store.get_task(session_id="thread-a")
        self.assertTrue(result["effective_lease"])
        self.assertEqual([port["port_name"] for port in self.store.get_ports(task["task_id"])], ["ssh"])

    def test_guest_growth_transport_failure_has_explicit_output_fields(self):
        self.runner.guest_growth_results = [CONTROL.ControlError(
            "command_timeout",
            "guest command timed out",
            {
                "stdout": "partial stdout",
                "stderr": "partial stderr",
                "stdout_truncated": True,
                "stderr_truncated": False,
            },
        )]
        self.error("guest_resize_unverified", self.acquire)

        failure = json.loads(self.store.get_task(session_id="thread-a")["last_error"])
        self.assertEqual(failure["phase"], "guest-exec")
        self.assertEqual(failure["stdout"], "partial stdout")
        self.assertEqual(failure["stderr"], "partial stderr")
        self.assertTrue(failure["stdout_truncated"])
        self.assertFalse(failure["stderr_truncated"])
        self.assertFalse(failure["guest_capacity"]["verified"])

    def test_guest_growth_report_requires_consistent_topology(self):
        response = self.guest_growth_result()
        payload = json.loads(response.stdout)
        payload["partition"] = "3"
        response.stdout = json.dumps(payload).encode()
        self.runner.guest_growth_results = [response]
        self.error("guest_resize_unverified", self.acquire)

        failure = json.loads(self.store.get_task(session_id="thread-a")["last_error"])
        self.assertEqual(failure["phase"], "guest-report")

    def test_guest_growth_report_requires_transient_retry_evidence(self):
        response = self.guest_growth_result(status="grown_after_retry", attempts=2)
        payload = json.loads(response.stdout)
        payload["capacity"]["attempts"] = [{"attempt": 1, "error": {"transient": True}}, {"attempt": 2}]
        response.stdout = json.dumps(payload).encode()
        self.runner.guest_growth_results = [response]
        self.error("guest_resize_unverified", self.acquire)

        failure = json.loads(self.store.get_task(session_id="thread-a")["last_error"])
        self.assertEqual(failure["phase"], "guest-report")

    def test_guest_growth_report_bounds_retry_evidence_depth(self):
        response = self.guest_growth_result(status="grown_after_retry", attempts=2)
        payload = json.loads(response.stdout)
        error = payload["capacity"]["attempts"][0]["error"]
        nested = dict(error)
        for _ in range(4):
            nested = {**error, "retry_of": nested}
        payload["capacity"]["attempts"][0]["error"] = nested
        response.stdout = json.dumps(payload).encode()
        self.runner.guest_growth_results = [response]

        self.error("guest_resize_unverified", self.acquire)

        failure = json.loads(self.store.get_task(session_id="thread-a")["last_error"])
        self.assertEqual(failure["phase"], "guest-report")

    def test_guest_growth_report_requires_growth_delta_for_mutating_status(self):
        response = self.guest_growth_result()
        payload = json.loads(response.stdout)
        payload["filesystem_before"] = payload["filesystem_bytes"]
        payload["capacity"]["filesystem_before"] = payload["capacity"]["filesystem_bytes"]
        response.stdout = json.dumps(payload).encode()
        self.runner.guest_growth_results = [response]

        self.error("guest_resize_unverified", self.acquire)

        failure = json.loads(self.store.get_task(session_id="thread-a")["last_error"])
        self.assertEqual(failure["phase"], "guest-report")

    def test_guest_growth_report_bounds_attempt_records(self):
        response = self.guest_growth_result()
        payload = json.loads(response.stdout)
        payload["attempts"] = 3
        payload["capacity"]["attempts"] = [{"attempt": 1}, {"attempt": 2}, {"attempt": 3}]
        response.stdout = json.dumps(payload).encode()
        self.runner.guest_growth_results = [response]

        self.error("guest_resize_unverified", self.acquire)

        failure = json.loads(self.store.get_task(session_id="thread-a")["last_error"])
        self.assertEqual(failure["phase"], "guest-report")

    def test_guest_growth_report_rejects_malformed_host_output(self):
        for body in (b"", b"[]", b"not-json\n"):
            with self.subTest(body=body):
                self.runner.guest_growth_results = [subprocess.CompletedProcess([], 0, body, b"")]
                self.error("guest_resize_unverified", self.acquire)

                failure = json.loads(self.store.get_task(session_id="thread-a")["last_error"])
                self.assertEqual(failure["phase"], "guest-report")

    def test_guest_growth_failure_report_validates_retry_linkage(self):
        response = self.guest_growth_failure()
        payload = json.loads(response.stderr)
        payload["error"]["retry_of"] = {"phase": "filesystem-grow"}
        response.stderr = (json.dumps(payload) + "\n").encode()
        self.runner.guest_growth_results = [response]

        self.error("guest_resize_unverified", self.acquire)

        failure = json.loads(self.store.get_task(session_id="thread-a")["last_error"])
        self.assertEqual(failure["phase"], "guest-report")
        self.assertEqual(failure["cause"], "invalid_failure_report")

    def test_guest_growth_failure_report_requires_attempts(self):
        response = self.guest_growth_failure()
        payload = json.loads(response.stderr)
        payload["error"]["capacity"].pop("attempts")
        response.stderr = (json.dumps(payload) + "\n").encode()
        self.runner.guest_growth_results = [response]

        self.error("guest_resize_unverified", self.acquire)

        failure = json.loads(self.store.get_task(session_id="thread-a")["last_error"])
        self.assertEqual(failure["phase"], "guest-report")
        self.assertEqual(failure["cause"], "invalid_failure_report")

    def test_guest_growth_failure_report_rejects_oversized_nested_output(self):
        for attribute, limit in (("stdout", 8192), ("stderr", 4000)):
            with self.subTest(attribute=attribute):
                response = self.guest_growth_failure()
                payload = json.loads(response.stderr)
                payload["error"]["capacity"]["attempts"][0]["error"][attribute] = "x" * (limit + 1)
                response.stderr = (json.dumps(payload) + "\n").encode()
                self.runner.guest_growth_results = [response]

                self.error("guest_resize_unverified", self.acquire)

                failure = json.loads(self.store.get_task(session_id="thread-a")["last_error"])
                self.assertEqual(failure["phase"], "guest-report")
                self.assertEqual(failure["cause"], "invalid_failure_report")

    def test_guest_growth_failure_report_persists_truncation_flags(self):
        response = self.guest_growth_failure()
        payload = json.loads(response.stderr)
        report_error = payload["error"]
        attempt_error = report_error["capacity"]["attempts"][0]["error"]
        for error in (report_error, attempt_error):
            error["stdout_truncated"] = True
            error["stderr_truncated"] = True
        response.stderr = (json.dumps(payload) + "\n").encode()
        self.runner.guest_growth_results = [response]

        self.error("guest_resize_unverified", self.acquire)

        failure = json.loads(self.store.get_task(session_id="thread-a")["last_error"])
        self.assertTrue(failure["stdout_truncated"])
        self.assertTrue(failure["stderr_truncated"])

    def test_guest_growth_failure_report_requires_timeout_cleanup_evidence(self):
        response = self.guest_growth_failure()
        payload = json.loads(response.stderr)
        report_error = payload["error"]
        report_error["cause"] = "command_timeout"
        report_error["capacity"]["attempts"][0]["error"]["cause"] = "command_timeout"
        response.stderr = (json.dumps(payload) + "\n").encode()
        self.runner.guest_growth_results = [response]

        self.error("guest_resize_unverified", self.acquire)

        failure = json.loads(self.store.get_task(session_id="thread-a")["last_error"])
        self.assertEqual(failure["phase"], "guest-report")

    def test_guest_growth_failure_report_accepts_stdout_transport(self):
        response = self.guest_growth_failure()
        response.stdout = response.stderr
        response.stderr = b""
        self.runner.guest_growth_results = [response]

        self.error("guest_resize_unverified", self.acquire)

        failure = json.loads(self.store.get_task(session_id="thread-a")["last_error"])
        self.assertEqual(failure["phase"], "filesystem-grow")

    def test_guest_growth_failure_report_rejects_verified_empty_attempts(self):
        response = self.guest_growth_failure()
        payload = json.loads(response.stderr)
        payload["error"]["capacity"]["attempts"] = []
        payload["error"]["capacity"]["verified"] = True
        response.stderr = (json.dumps(payload) + "\n").encode()
        self.runner.guest_growth_results = [response]

        self.error("guest_resize_unverified", self.acquire)

        failure = json.loads(self.store.get_task(session_id="thread-a")["last_error"])
        self.assertEqual(failure["phase"], "guest-report")
        self.assertEqual(failure["cause"], "invalid_failure_report")

    def test_guest_growth_failure_report_preserves_nontransient_refresh_error(self):
        response = self.guest_growth_failure()
        payload = json.loads(response.stderr)
        report_error = payload["error"]
        attempt = report_error["capacity"]["attempts"][0]
        previous = attempt["error"]
        refresh = dict(
            previous,
            phase="partition-settle",
            command=["udevadm", "settle", "--timeout=5"],
            returncode=1,
            stderr="udevadm: settle failed",
            transient=False,
            retry_of=previous,
        )
        attempt["refresh_error"] = refresh
        report_error.update(
            phase="partition-settle",
            command=["udevadm", "settle", "--timeout=5"],
            returncode=1,
            stderr="udevadm: settle failed",
            transient=False,
            retry_of=previous,
        )
        response.stderr = (json.dumps(payload) + "\n").encode()
        self.runner.guest_growth_results = [response]

        self.error("guest_resize_unverified", self.acquire)

        failure = json.loads(self.store.get_task(session_id="thread-a")["last_error"])
        self.assertEqual(failure["phase"], "partition-settle")
        self.assertEqual(
            failure["guest_capacity"]["attempts"][0]["refresh_error"]["phase"],
            "partition-settle",
        )

    def test_guest_growth_report_rejects_bounded_output_truncation(self):
        for attribute in ("stdout_truncated", "stderr_truncated"):
            with self.subTest(attribute=attribute):
                response = self.guest_growth_result()
                setattr(response, attribute, True)
                self.runner.guest_growth_results = [response]
                self.error("guest_resize_unverified", self.acquire)

                failure = json.loads(self.store.get_task(session_id="thread-a")["last_error"])
                self.assertEqual(failure["phase"], "guest-exec")
                self.assertEqual(failure[attribute], True)

    def test_guest_growth_report_rejects_oversized_nested_output(self):
        for attribute, limit in (("stdout", 8192), ("stderr", 4000)):
            with self.subTest(attribute=attribute):
                response = self.guest_growth_result(status="grown_after_retry", attempts=2)
                payload = json.loads(response.stdout)
                payload["capacity"]["attempts"][0]["error"][attribute] = "x" * (limit + 1)
                response.stdout = json.dumps(payload).encode()
                self.runner.guest_growth_results = [response]

                self.error("guest_resize_unverified", self.acquire)

                failure = json.loads(self.store.get_task(session_id="thread-a")["last_error"])
                self.assertEqual(failure["phase"], "guest-report")

    def test_already_grown_guest_root_is_an_idempotent_success(self):
        self.acquire()
        self.call("release")
        self.runner.guest_growth_results = [self.guest_growth_result(status="already_grown", attempts=0)]

        result = self.acquire()

        self.assertEqual(result["state"], "active")
        self.assertEqual(result["guest_filesystem"]["status"], "already_grown")
        self.assertEqual(result["guest_filesystem"]["attempts"], 0)
        self.assertEqual(len(result["ports"]), 1)

    def test_already_grown_guest_root_after_transient_retry_is_accepted(self):
        self.runner.guest_growth_results = [self.guest_growth_result(status="already_grown", attempts=2)]

        result = self.acquire()

        self.assertEqual(result["state"], "active")
        self.assertEqual(result["guest_filesystem"]["status"], "already_grown")
        self.assertEqual(result["guest_filesystem"]["attempts"], 2)

    def test_already_grown_after_retry_rejects_mismatched_attempt_topology(self):
        response = self.guest_growth_result(status="already_grown", attempts=2)
        payload = json.loads(response.stdout)
        payload["capacity"]["attempts"][0]["error"]["capacity"]["source"] = "/dev/sdb2"
        response.stdout = json.dumps(payload).encode()
        self.runner.guest_growth_results = [response]

        self.error("guest_resize_unverified", self.acquire)

        failure = json.loads(self.store.get_task(session_id="thread-a")["last_error"])
        self.assertEqual(failure["phase"], "guest-report")

    def test_guest_growth_threshold_uses_requested_capacity_when_disk_is_larger(self):
        response = self.guest_growth_result(status="already_grown", attempts=0)
        payload = json.loads(response.stdout)
        observed = 24 * GIB
        threshold = 20 * GIB * 95 // 100
        payload.update(
            virtual_bytes=observed,
            partition_bytes=observed,
            filesystem_before=threshold,
            filesystem_bytes=threshold,
            filesystem_threshold_bytes=threshold,
        )
        payload["capacity"].update(
            disk_bytes=observed,
            partition_bytes=observed,
            filesystem_before=threshold,
            filesystem_bytes=threshold,
            filesystem_threshold_bytes=threshold,
        )
        response.stdout = json.dumps(payload).encode()
        self.runner.guest_growth_results = [response]

        result = self.acquire()

        self.assertEqual(result["state"], "active")
        self.assertEqual(result["guest_filesystem"]["status"], "already_grown")
        self.assertEqual(result["guest_filesystem"]["filesystem_threshold_bytes"], threshold)

    def test_restart_and_resize_refuse_imminent_expiry(self):
        first = self.acquire()
        self.now = CONTROL.parse_time(first["lease_expires_at"]) - dt.timedelta(seconds=20)
        self.error("lease_window_short", self.call, "restart")
        self.error("lease_window_short", self.call, "resize", disk_gib=40, allow_restart=True)

    def test_restart_and_restart_resize_require_the_full_readiness_window(self):
        first = self.acquire()
        self.now = CONTROL.parse_time(first["lease_expires_at"]) - dt.timedelta(seconds=240)
        self.error("lease_window_short", self.call, "restart")
        self.error("lease_window_short", self.call, "resize", disk_gib=40, allow_restart=True)

    def test_partial_disk_growth_can_complete_after_cached_reacquire(self):
        self.acquire()
        self.runner.failure = lambda args: any("automatic growth supports" in arg for arg in args)
        self.error("resize_partial", self.call, "resize", disk_gib=40, allow_restart=True)
        cached = self.call("status")
        self.assertEqual(cached["state"], "cached")
        self.assertEqual(cached["resources"]["disk_gib"], 40)
        self.assertEqual(cached["last_error"]["completed"], ["virtual_disk"])
        self.runner.failure = None
        self.acquire()
        completed = self.call("resize", disk_gib=40)
        self.assertIn("guest_filesystem", completed["completed"])
        self.assertEqual(completed["guest_filesystem"]["virtual_bytes"], 40 * GIB)
        self.assertNotIn("last_error", completed)

    def test_slow_start_diagnostic_uses_verified_current_cgroup_pressure_path(self):
        self.acquire()
        task = self.store.get_task(session_id="thread-a")
        observation = dict(self.resources.cpu_pressure.return_value)
        observation.update({
            "cgroup_path": "/system.slice/agent-vm",
            "metric": "/sys/fs/cgroup/system.slice/agent-vm/cpu.pressure",
        })
        with mock.patch.object(self.resources, "cpu_pressure", return_value=observation), \
             mock.patch.object(self.control.incus, "instance_info", return_value={"status": "Running", "pid": 1}), \
             mock.patch.object(self.control.incus, "instance_log", return_value=""):
            diagnostic = self.control._emit_slow_start_diagnostic(
                task,
                phase="agent",
                started_monotonic=0.0,
                readiness={},
            )

        self.assertEqual(diagnostic["cpu_psi"]["cgroup_path"], "/system.slice/agent-vm")
        self.assertEqual(
            diagnostic["cpu_psi"]["metric"],
            "/sys/fs/cgroup/system.slice/agent-vm/cpu.pressure",
        )

    def test_cpu_pressure_observation_rejects_malformed_payload(self):
        base = self.resources.cpu_pressure.return_value
        mutations = (
            ("missing-full", lambda value: value.pop("full")),
            ("full-exceeds-some", lambda value: value["full"].update(avg10=1.0)),
            ("non-finite", lambda value: value["some"].update(avg10=float("nan"))),
        )
        for name, mutate in mutations:
            with self.subTest(name=name):
                observation = json.loads(json.dumps(base))
                mutate(observation)
                with mock.patch.object(self.resources, "cpu_pressure", return_value=observation):
                    result = self.control._cpu_pressure_observation()
                self.assertFalse(result["verified"])
                self.assertEqual(result["error"]["code"], "resource_unverified")


class GuestRootGrowthScriptTests(unittest.TestCase):
    def execute_script(
        self,
        *,
        initial_partition=None,
        initial_filesystem=None,
        resize_returncode=0,
        resize_returncodes=None,
        resize_failure_completes=False,
        growpart_returncode=0,
        growpart_returncodes=None,
        growpart_expands_partition=False,
        growpart_stdout="",
        growpart_stderr="",
        filesystem_type="ext4",
        timeout_command=None,
        command_timeout=None,
        killpg_error=False,
        escaped_child_pid_path=None,
        root_exits_before_timeout=False,
        source_output="/dev/sda2\n",
        partition_marker_exists=True,
        udevadm_available=False,
        udevadm_returncode=0,
    ):
        target = 20 * GIB
        threshold = target * 95 // 100
        state = {
            "partition": initial_partition if initial_partition is not None else target // 2,
            "filesystem": initial_filesystem if initial_filesystem is not None else target // 4,
        }
        marker = "/sys/class/block/sda2/partition"
        calls = []
        growpart_calls = 0
        resize_calls = 0

        real_popen = subprocess.Popen

        def popen(command, **kwargs):
            nonlocal growpart_calls, resize_calls
            command = list(command)
            calls.append(command)
            output = b""
            error = b""
            returncode = 0
            if timeout_command is not None and command == timeout_command:
                output = b"partial stdout"
                error = b"partial stderr"
                timeout = True
            elif command == ["findmnt", "-n", "-o", "SOURCE", "-T", "/"]:
                output = source_output.encode()
                timeout = False
            elif command == ["findmnt", "-n", "-o", "FSTYPE", "-T", "/"]:
                output = (filesystem_type + "\n").encode()
                timeout = False
            elif command[:4] == ["lsblk", "-n", "-o", "PKNAME"]:
                output = b"sda\n"
                timeout = False
            elif command[:4] == ["lsblk", "-n", "-o", "TYPE"]:
                output = ("part\n" if command[-1] == "/dev/sda2" else "disk\n").encode()
                timeout = False
            elif command[:2] == ["blockdev", "--getsize64"]:
                value = target if command[-1] == "/dev/sda" else state["partition"]
                output = f"{value}\n".encode()
                timeout = False
            elif command[:2] == ["blockdev", "--rereadpt"]:
                state["partition"] = target
                timeout = False
            elif command[:2] == ["udevadm", "settle"]:
                returncode = udevadm_returncode
                error = ("udevadm: settle failed\n" if udevadm_returncode else "").encode()
                timeout = False
            elif command[0] == "growpart":
                returncode = (
                    growpart_returncodes[growpart_calls]
                    if growpart_returncodes is not None and growpart_calls < len(growpart_returncodes)
                    else growpart_returncode
                )
                growpart_calls += 1
                if returncode == 0 and growpart_expands_partition:
                    state["partition"] = target
                output = growpart_stdout.encode()
                error = (growpart_stderr if returncode else "").encode()
                timeout = False
            elif command[0] == "resize2fs":
                selected_returncode = (
                    resize_returncodes[resize_calls]
                    if resize_returncodes is not None and resize_calls < len(resize_returncodes)
                    else resize_returncode
                )
                resize_calls += 1
                if selected_returncode:
                    returncode = selected_returncode
                    error = b"resize2fs: Device or resource busy\n"
                    if resize_failure_completes:
                        state["filesystem"] = threshold
                else:
                    state["filesystem"] = threshold
                timeout = False
            elif command[0] == "xfs_growfs":
                selected_returncode = (
                    resize_returncodes[resize_calls]
                    if resize_returncodes is not None and resize_calls < len(resize_returncodes)
                    else resize_returncode
                )
                resize_calls += 1
                if selected_returncode:
                    returncode = selected_returncode
                    error = b"xfs_growfs: Device or resource busy\n"
                    if resize_failure_completes:
                        state["filesystem"] = threshold
                else:
                    state["filesystem"] = threshold
                timeout = False
            else:
                raise AssertionError(command)

            helper = (
                "import sys; sys.stdout.buffer.write(" + repr(output) + "); "
                "sys.stderr.buffer.write(" + repr(error) + "); "
                "sys.stdout.flush(); sys.stderr.flush(); sys.exit(" + str(returncode) + ")"
            )
            if timeout:
                if escaped_child_pid_path is None:
                    helper = helper.replace("import sys;", "import sys, time;").replace(
                        "; sys.exit(" + str(returncode),
                        "; time.sleep(10); sys.exit(" + str(returncode),
                    )
                else:
                    helper = (
                        "import os, subprocess, sys, time; "
                        "child=subprocess.Popen([sys.executable, '-c', "
                        "\"import os,sys,time; os.setsid(); "
                        "open(sys.argv[1], 'w').write('ready'); time.sleep(10)\", "
                        "sys.argv[2]], close_fds=False); "
                        "open(sys.argv[1], 'w').write(str(child.pid))\n"
                        "while not os.path.exists(sys.argv[2]):\n"
                        "    time.sleep(0.01)\n"
                        "sys.stdout.buffer.write(" + repr(output) + "); "
                        "sys.stderr.buffer.write(" + repr(error) + "); "
                        "sys.stdout.flush(); sys.stderr.flush(); "
                        + ("time.sleep(0.15); sys.exit(" if root_exits_before_timeout else "time.sleep(10); sys.exit(")
                        + str(returncode) + ")"
                    )
            process_command = [os.sys.executable, "-c", helper]
            if escaped_child_pid_path is not None:
                process_command.extend([escaped_child_pid_path, escaped_child_pid_path + ".ready"])
            process = real_popen(
                process_command,
                stdout=kwargs.get("stdout"),
                stderr=kwargs.get("stderr"),
                start_new_session=True,
            )
            return process

        def which(name):
            if name == "udevadm":
                return "/usr/bin/udevadm" if udevadm_available else None
            return f"/usr/bin/{name}"

        def statvfs(path):
            self.assertEqual(path, "/")
            return types.SimpleNamespace(f_blocks=state["filesystem"], f_frsize=1)

        real_open = open

        def fake_open(path, *args, **kwargs):
            if path == marker:
                return io.StringIO("2\n")
            return real_open(path, *args, **kwargs)

        captured_stdout = io.StringIO()
        captured_stderr = io.StringIO()
        script = CONTROL.GUEST_ROOT_GROWTH_SCRIPT
        if command_timeout is not None:
            script = script.replace("COMMAND_TIMEOUT = 45", f"COMMAND_TIMEOUT = {command_timeout}")
        killpg_patch = mock.patch.object(
            CONTROL.os,
            "killpg",
            side_effect=PermissionError("denied"),
        ) if killpg_error else mock.patch.object(CONTROL.os, "killpg", wraps=CONTROL.os.killpg)
        with mock.patch.object(CONTROL.subprocess, "Popen", side_effect=popen), \
             mock.patch.object(CONTROL.os, "statvfs", side_effect=statvfs), \
             mock.patch.object(
                 CONTROL.os.path,
                 "isfile",
                 side_effect=lambda path: partition_marker_exists and path == marker,
             ), \
             mock.patch.object(CONTROL.shutil, "which", side_effect=which), \
             mock.patch.object(CONTROL.time, "sleep", return_value=None), \
             mock.patch.object(CONTROL.sys, "argv", ["guest-root-growth", "20"]), \
             mock.patch("builtins.open", side_effect=fake_open), \
             killpg_patch, \
             redirect_stdout(captured_stdout), redirect_stderr(captured_stderr):
            try:
                exec(script, {"__name__": "__main__"})
            except SystemExit as stopped:
                exit_code = stopped.code
            else:
                exit_code = 0
        return exit_code, captured_stdout.getvalue(), captured_stderr.getvalue(), calls

    def test_guest_root_growth_script_grows_on_first_attempt(self):
        target = 20 * GIB
        exit_code, stdout, stderr, calls = self.execute_script(
            initial_partition=target // 2,
            initial_filesystem=target // 4,
            growpart_expands_partition=True,
        )

        self.assertEqual(exit_code, 0)
        self.assertEqual(stderr, "")
        payload = json.loads(stdout)
        self.assertEqual(payload["status"], "grown")
        self.assertEqual(payload["attempts"], 1)
        self.assertGreaterEqual(payload["filesystem_bytes"], payload["filesystem_threshold_bytes"])
        self.assertEqual(len([command for command in calls if command[0] == "growpart"]), 1)
        self.assertNotIn(["blockdev", "--rereadpt", "/dev/sda"], calls)

    def test_guest_root_growth_script_retries_stale_partition_and_grows(self):
        exit_code, stdout, stderr, calls = self.execute_script()

        self.assertEqual(exit_code, 0)
        self.assertEqual(stderr, "")
        payload = json.loads(stdout)
        self.assertEqual(payload["status"], "grown_after_retry")
        self.assertEqual(payload["attempts"], 2)
        first_error = payload["capacity"]["attempts"][0]["error"]
        self.assertEqual(first_error["phase"], "partition-verify")
        self.assertEqual(first_error["cause"], "stale_partition_capacity_below_target")
        self.assertTrue(first_error["transient"])
        self.assertIn(["blockdev", "--rereadpt", "/dev/sda"], calls)

    def test_guest_root_growth_script_reports_already_grown_after_transient_resize(self):
        target = 20 * GIB
        exit_code, stdout, stderr, calls = self.execute_script(
            initial_partition=target,
            initial_filesystem=target // 4,
            resize_returncodes=[1],
            resize_failure_completes=True,
        )

        self.assertEqual(exit_code, 0)
        self.assertEqual(stderr, "")
        payload = json.loads(stdout)
        self.assertEqual(payload["status"], "already_grown")
        self.assertEqual(payload["attempts"], 2)
        self.assertEqual(len([command for command in calls if command[0] == "resize2fs"]), 1)

    def test_guest_root_growth_script_fails_closed_on_growpart_error(self):
        exit_code, stdout, stderr, calls = self.execute_script(
            growpart_returncode=2,
            growpart_stderr="growpart: invalid geometry\n",
        )

        self.assertEqual(exit_code, 1)
        self.assertEqual(stdout, "")
        error = json.loads(stderr)["error"]
        self.assertEqual(error["phase"], "partition-grow")
        self.assertFalse(error["transient"])
        self.assertEqual(len([command for command in calls if command[0] == "growpart"]), 1)
        self.assertNotIn(["blockdev", "--rereadpt", "/dev/sda"], calls)

    def test_guest_root_growth_script_retries_transient_growpart_error_once(self):
        exit_code, stdout, stderr, calls = self.execute_script(
            growpart_returncode=1,
            growpart_stderr="growpart: Device or resource busy\n",
        )

        self.assertEqual(exit_code, 1)
        self.assertEqual(stdout, "")
        error = json.loads(stderr)["error"]
        self.assertEqual(error["phase"], "partition-grow")
        self.assertTrue(error["transient"])
        self.assertEqual(error["retry_of"]["phase"], "partition-grow")
        self.assertEqual(len(error["capacity"]["attempts"]), 2)
        self.assertEqual(len([command for command in calls if command[0] == "growpart"]), 2)
        self.assertIn(["blockdev", "--rereadpt", "/dev/sda"], calls)

    def test_guest_root_growth_script_recovers_from_transient_growpart_error(self):
        exit_code, stdout, stderr, calls = self.execute_script(
            growpart_returncodes=[1, 0],
            growpart_stderr="growpart: Device or resource busy\n",
        )

        self.assertEqual(exit_code, 0)
        self.assertEqual(stderr, "")
        payload = json.loads(stdout)
        self.assertEqual(payload["status"], "grown_after_retry")
        self.assertEqual(payload["attempts"], 2)
        self.assertEqual(len([command for command in calls if command[0] == "growpart"]), 2)
        self.assertIn(["blockdev", "--rereadpt", "/dev/sda"], calls)

    def test_guest_root_growth_script_settles_partition_when_udevadm_is_available(self):
        exit_code, stdout, stderr, calls = self.execute_script(udevadm_available=True)

        self.assertEqual(exit_code, 0)
        self.assertEqual(stderr, "")
        self.assertIn(["udevadm", "settle", "--timeout=5"], calls)

    def test_guest_root_growth_script_reports_udevadm_settle_failure(self):
        exit_code, stdout, stderr, calls = self.execute_script(
            udevadm_available=True,
            udevadm_returncode=1,
        )

        self.assertEqual(exit_code, 1)
        self.assertEqual(stdout, "")
        error = json.loads(stderr)["error"]
        self.assertEqual(error["phase"], "partition-settle")
        self.assertEqual(error["retry_of"]["phase"], "partition-verify")
        self.assertEqual(error["capacity"]["attempts"][0]["refresh_error"]["phase"], "partition-settle")
        self.assertIn(["udevadm", "settle", "--timeout=5"], calls)

    def test_guest_root_growth_script_grows_xfs_root(self):
        exit_code, stdout, stderr, calls = self.execute_script(filesystem_type="xfs")

        self.assertEqual(exit_code, 0)
        self.assertEqual(stderr, "")
        payload = json.loads(stdout)
        self.assertEqual(payload["status"], "grown_after_retry")
        self.assertEqual(payload["filesystem"], "xfs")
        self.assertEqual(payload["capacity"]["filesystem"], "xfs")
        self.assertGreaterEqual(payload["capacity"]["filesystem_bytes"], payload["filesystem_threshold_bytes"])
        self.assertIn(["xfs_growfs", "/"], calls)
        self.assertFalse(any(command[0] == "resize2fs" for command in calls))

    def test_guest_root_growth_script_reports_xfs_failure(self):
        exit_code, stdout, stderr, calls = self.execute_script(filesystem_type="xfs", resize_returncode=1)

        self.assertEqual(exit_code, 1)
        self.assertEqual(stdout, "")
        error = json.loads(stderr)["error"]
        self.assertEqual(error["command"], ["xfs_growfs", "/"])
        self.assertEqual(error["retry_of"]["phase"], "partition-verify")
        self.assertIn(["xfs_growfs", "/"], calls)

    def test_guest_root_growth_script_emits_retry_link_for_final_failure(self):
        exit_code, stdout, stderr, _ = self.execute_script(resize_returncode=1)

        self.assertEqual(exit_code, 1)
        self.assertEqual(stdout, "")
        error = json.loads(stderr)["error"]
        self.assertEqual(error["phase"], "filesystem-grow")
        self.assertTrue(error["transient"])
        self.assertEqual(error["retry_of"]["phase"], "partition-verify")
        self.assertEqual(error["retry_of"]["cause"], "stale_partition_capacity_below_target")
        self.assertEqual(len(error["capacity"]["attempts"]), 2)

    def test_guest_root_growth_script_is_idempotent_when_complete(self):
        exit_code, stdout, stderr, calls = self.execute_script(
            initial_partition=20 * GIB,
            initial_filesystem=20 * GIB * 95 // 100,
        )

        self.assertEqual(exit_code, 0)
        self.assertEqual(stderr, "")
        payload = json.loads(stdout)
        self.assertEqual(payload["status"], "already_grown")
        self.assertEqual(payload["attempts"], 0)
        self.assertFalse(any(command[0] in {"growpart", "resize2fs"} for command in calls))

    def test_guest_root_growth_script_rejects_unsupported_filesystem(self):
        exit_code, stdout, stderr, _ = self.execute_script(filesystem_type="btrfs")

        self.assertEqual(exit_code, 1)
        self.assertEqual(stdout, "")
        error = json.loads(stderr)["error"]
        self.assertEqual(error["phase"], "capability-check")
        self.assertIn("direct ext4/xfs", error["cause"])
        self.assertEqual(error["capacity"]["source"], "/dev/sda2")
        self.assertEqual(error["capacity"]["filesystem"], "btrfs")

    def test_guest_root_growth_script_rejects_missing_partition_marker(self):
        exit_code, stdout, stderr, _ = self.execute_script(partition_marker_exists=False)

        self.assertEqual(exit_code, 1)
        self.assertEqual(stdout, "")
        error = json.loads(stderr)["error"]
        self.assertEqual(error["phase"], "topology-check")
        self.assertEqual(error["cause"], "root_is_not_partition")

    def test_guest_root_growth_script_preserves_timeout_evidence(self):
        command = ["findmnt", "-n", "-o", "SOURCE", "-T", "/"]
        exit_code, stdout, stderr, _ = self.execute_script(timeout_command=command, command_timeout=0.2)

        self.assertEqual(exit_code, 1)
        self.assertEqual(stdout, "")
        error = json.loads(stderr)["error"]
        self.assertEqual(error["phase"], "topology-source")
        self.assertEqual(error["cause"], "command_timeout")
        self.assertEqual(error["stdout"], "partial stdout")
        self.assertEqual(error["stderr"], "partial stderr")
        self.assertFalse(error["stdout_truncated"])
        self.assertFalse(error["stderr_truncated"])
        self.assertTrue(error["process_group_killed"])

    def test_guest_root_growth_script_falls_back_when_group_kill_is_denied(self):
        command = ["findmnt", "-n", "-o", "SOURCE", "-T", "/"]
        exit_code, stdout, stderr, _ = self.execute_script(
            timeout_command=command,
            command_timeout=1.0,
            killpg_error=True,
        )

        self.assertEqual(exit_code, 1)
        error = json.loads(stderr)["error"]
        self.assertEqual(error["cause"], "command_timeout")
        self.assertTrue(error["process_group_cleanup_error"].startswith("PermissionError"))
        self.assertEqual(error["process_group_killed"], Path("/proc").is_dir())

    @unittest.skipUnless(Path("/proc").is_dir(), "requires Linux /proc")
    def test_guest_root_growth_script_kills_setsid_descendant_outside_process_group(self):
        command = ["findmnt", "-n", "-o", "SOURCE", "-T", "/"]
        with tempfile.TemporaryDirectory() as directory:
            pid_path = Path(directory) / "child.pid"
            exit_code, stdout, stderr, _ = self.execute_script(
                timeout_command=command,
                command_timeout=1.0,
                escaped_child_pid_path=str(pid_path),
            )

            self.assertEqual(exit_code, 1)
            error = json.loads(stderr)["error"]
            self.assertEqual(error["cause"], "command_timeout")
            if not wait_until(pid_path.exists):
                self.fail("guest growth setsid child did not publish its pid")
            child_pid = int(pid_path.read_text())
            if not wait_until(lambda: process_is_zombie(child_pid)):
                self.fail("guest growth setsid descendant remained alive after timeout")

    @unittest.skipUnless(Path("/proc").is_dir(), "requires Linux /proc")
    def test_guest_root_growth_script_kills_setsid_descendant_after_leader_exit(self):
        command = ["findmnt", "-n", "-o", "SOURCE", "-T", "/"]
        with tempfile.TemporaryDirectory() as directory:
            pid_path = Path(directory) / "child.pid"
            exit_code, _, stderr, _ = self.execute_script(
                timeout_command=command,
                command_timeout=1.0,
                escaped_child_pid_path=str(pid_path),
                root_exits_before_timeout=True,
            )

            self.assertEqual(exit_code, 1)
            error = json.loads(stderr)["error"]
            self.assertEqual(error["cause"], "command_timeout")
            if not wait_until(pid_path.exists):
                self.fail("guest growth setsid child did not publish its pid")
            child_pid = int(pid_path.read_text())
            if not wait_until(lambda: process_is_zombie(child_pid)):
                self.fail("guest growth setsid descendant remained alive after leader exit")

    def test_guest_root_growth_script_bounds_command_output(self):
        exit_code, stdout, stderr, _ = self.execute_script(source_output="/dev/sda2\n" + "x" * 20000 + "\n")

        self.assertEqual(exit_code, 1)
        error = json.loads(stderr)["error"]
        self.assertEqual(error["phase"], "topology-source")
        self.assertTrue(error["stdout_truncated"])
        self.assertEqual(error["stdout"], ("/dev/sda2\n" + "x" * 20000 + "\n")[:1024])

    def test_guest_root_growth_script_bounds_file_backed_stderr(self):
        source = "growpart: " + "e" * 5000
        exit_code, stdout, stderr, _ = self.execute_script(
            growpart_returncode=2,
            growpart_stderr=source,
        )

        self.assertEqual(exit_code, 1)
        error = json.loads(stderr or stdout)["error"]
        self.assertTrue(error["stderr_truncated"])
        self.assertEqual(error["stderr"], source[-4000:])

    def test_guest_root_growth_script_preserves_stdout_below_capture_limit(self):
        source_output = "/dev/sda2\n" + "x" * 5000 + "\n"
        exit_code, stdout, stderr, _ = self.execute_script(source_output=source_output)

        self.assertEqual(exit_code, 1)
        error = json.loads(stderr or stdout)["error"]
        self.assertFalse(error["stdout_truncated"])
        self.assertEqual(len(error["stdout"]), len(source_output))

    def test_guest_root_growth_script_rejects_ambiguous_topology_output(self):
        exit_code, stdout, stderr, _ = self.execute_script(source_output="/dev/sda2\n/dev/sda3\n")

        self.assertEqual(exit_code, 1)
        self.assertEqual(stdout, "")
        error = json.loads(stderr)["error"]
        self.assertEqual(error["phase"], "topology-source")
        self.assertEqual(error["cause"], "ambiguous_output")


class CommandRunnerTests(unittest.TestCase):
    def test_guest_root_growth_script_compiles_with_bounded_attempts(self):
        compile(CONTROL.GUEST_ROOT_GROWTH_SCRIPT, "<guest-root-growth>", "exec")
        self.assertIn("for attempt in (1, 2)", CONTROL.GUEST_ROOT_GROWTH_SCRIPT)
        self.assertNotIn('"try again"', CONTROL.GUEST_ROOT_GROWTH_SCRIPT)

    def test_command_timeout_preserves_bounded_output(self):
        timeout = subprocess.TimeoutExpired(["incus"], 1, output=b"partial stdout", stderr=b"partial stderr")
        with mock.patch.object(CONTROL.subprocess, "run", side_effect=timeout), self.assertRaises(CONTROL.ControlError) as failure:
            CONTROL.CommandRunner().run(["incus", "exec", "guest"])
        self.assertEqual(failure.exception.code, "command_timeout")
        self.assertEqual(failure.exception.detail, {"stdout": "partial stdout", "stderr": "partial stderr"})

    def test_bounded_runner_discards_stdout_after_limit(self):
        code = "import sys; sys.stdout.write('x' * 1000000)"
        result = CONTROL.CommandRunner().run_bounded(
            [os.sys.executable, "-c", code], timeout=5, stdout_limit=128
        )
        self.assertEqual(len(result.stdout), 128)
        self.assertTrue(result.stdout_truncated)

    def test_bounded_runner_nonzero_preserves_output_truncation(self):
        code = (
            "import sys; sys.stdout.write('o' * 1000000); "
            "sys.stderr.write('e' * 1000000); sys.exit(3)"
        )
        with self.assertRaises(CONTROL.ControlError) as failure:
            CONTROL.CommandRunner().run_bounded(
                [os.sys.executable, "-c", code], timeout=5, stdout_limit=128, stderr_limit=128
            )
        self.assertEqual(failure.exception.code, "command_failed")
        self.assertTrue(failure.exception.detail["stdout_truncated"])
        self.assertTrue(failure.exception.detail["stderr_truncated"])

    def test_bounded_runner_limits_stderr_and_kills_on_timeout(self):
        flood = "import sys; sys.stderr.write('e' * 1000000)"
        result = CONTROL.CommandRunner().run_bounded(
            [os.sys.executable, "-c", flood], timeout=5, stderr_limit=128
        )
        self.assertEqual(len(result.stderr), 128)
        sleeper = "import time; time.sleep(10)"
        with mock.patch.object(CONTROL.os, "killpg", wraps=CONTROL.os.killpg) as killpg, \
             mock.patch.object(CONTROL.CommandRunner, "_kill_process_group_members", return_value=(True, None)) as members:
            with self.assertRaises(CONTROL.ControlError) as failure:
                CONTROL.CommandRunner().run_bounded([os.sys.executable, "-c", sleeper], timeout=0.05)
        killpg.assert_called_once()
        if Path("/proc").is_dir():
            members.assert_called_once()
        else:
            members.assert_not_called()
        self.assertEqual(failure.exception.code, "command_timeout")
        self.assertTrue(failure.exception.detail["reaped"])
        self.assertTrue(failure.exception.detail["process_group_killed"])

    def test_bounded_runner_timeout_preserves_output(self):
        code = (
            "import sys,time; "
            "sys.stdout.write('partial-out'); sys.stderr.write('partial-err'); "
            "sys.stdout.flush(); sys.stderr.flush(); time.sleep(10)"
        )
        with self.assertRaises(CONTROL.ControlError) as failure:
            CONTROL.CommandRunner().run_bounded(
                [os.sys.executable, "-c", code], timeout=1, stdout_limit=128, stderr_limit=128
            )
        self.assertEqual(failure.exception.code, "command_timeout")
        self.assertEqual(failure.exception.detail["stdout"], "partial-out")
        self.assertEqual(failure.exception.detail["stderr"], "partial-err")
        self.assertFalse(failure.exception.detail["stdout_truncated"])
        self.assertFalse(failure.exception.detail["stderr_truncated"])

    def test_bounded_runner_reports_process_group_cleanup_failure(self):
        sleeper = "import time; time.sleep(10)"
        with mock.patch.object(CONTROL.os, "killpg", side_effect=PermissionError("denied")), \
             mock.patch.object(
                 CONTROL.CommandRunner,
                 "_kill_process_group_members",
                 return_value=(False, "process_group_inspection_failed"),
             ):
            with self.assertRaises(CONTROL.ControlError) as failure:
                CONTROL.CommandRunner().run_bounded([os.sys.executable, "-c", sleeper], timeout=0.05)
        self.assertTrue(failure.exception.detail["reaped"])
        self.assertFalse(failure.exception.detail["process_group_killed"])
        cleanup_error = failure.exception.detail["process_group_cleanup_error"]
        if Path("/proc").is_dir():
            self.assertTrue(cleanup_error.startswith("PermissionError:process_group_inspection_failed"))
        else:
            self.assertEqual(cleanup_error, "PermissionError")

    @unittest.skipUnless(Path("/proc").is_dir(), "requires Linux /proc")
    def test_bounded_runner_refuses_tree_cleanup_without_leader_identity(self):
        sleeper = "import time; time.sleep(10)"
        with mock.patch.object(CONTROL.CommandRunner, "_process_identity", return_value=None), \
             mock.patch.object(
                 CONTROL.CommandRunner,
                 "_kill_process_group_members",
                 side_effect=AssertionError("unverified tree must not be signalled"),
             ) as group_cleanup, \
             mock.patch.object(
                 CONTROL.CommandRunner,
                 "_kill_verified_process_group_members",
                 side_effect=AssertionError("unverified group must not be signalled"),
             ) as verified_group_cleanup:
            with self.assertRaises(CONTROL.ControlError) as failure:
                CONTROL.CommandRunner().run_bounded([os.sys.executable, "-c", sleeper], timeout=0.05)
        self.assertEqual(failure.exception.code, "command_timeout")
        self.assertIn("process_identity_unverified", failure.exception.detail["process_group_cleanup_error"])
        group_cleanup.assert_not_called()
        verified_group_cleanup.assert_not_called()

    @unittest.skipUnless(Path("/proc").is_dir(), "requires Linux /proc")
    def test_bounded_runner_kills_descendants_holding_pipes(self):
        with tempfile.TemporaryDirectory() as directory:
            pid_path = Path(directory) / "child.pid"
            code = (
                "import subprocess,sys,time; "
                "child=subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(10)'], close_fds=False); "
                "open(sys.argv[1], 'w').write(str(child.pid))\n"
                "time.sleep(10)"
            )
            with self.assertRaises(CONTROL.ControlError):
                CONTROL.CommandRunner().run_bounded(
                    [os.sys.executable, "-c", code, str(pid_path)], timeout=0.2
                )
            if not wait_until(pid_path.exists):
                self.fail("diagnostic child did not publish its pid")
            child_pid = int(pid_path.read_text())
            if not wait_until(lambda: process_is_zombie(child_pid)):
                self.fail("diagnostic descendant remained alive after timeout")

    @unittest.skipUnless(Path("/proc").is_dir(), "requires Linux /proc")
    def test_bounded_runner_kills_setsid_descendant_outside_process_group(self):
        with tempfile.TemporaryDirectory() as directory:
            pid_path = Path(directory) / "child.pid"
            ready_path = Path(directory) / "child.ready"
            code = (
                "import os,subprocess,sys,time; "
                "child=subprocess.Popen([sys.executable, '-c', "
                "\"import os,sys,time; os.setsid(); open(sys.argv[1], 'w').write('ready'); time.sleep(10)\", "
                "sys.argv[2]], close_fds=False); "
                "open(sys.argv[1], 'w').write(str(child.pid))\n"
                "while not os.path.exists(sys.argv[2]):\n"
                "    time.sleep(0.01)\n"
                "time.sleep(10)"
            )
            with self.assertRaises(CONTROL.ControlError):
                CONTROL.CommandRunner().run_bounded(
                    [os.sys.executable, "-c", code, str(pid_path), str(ready_path)], timeout=1.0
                )
            if not wait_until(pid_path.exists):
                self.fail("setsid diagnostic child did not publish its pid")
            child_pid = int(pid_path.read_text())
            if not wait_until(lambda: process_is_zombie(child_pid)):
                self.fail("setsid diagnostic descendant remained alive after timeout")

    @unittest.skipUnless(Path("/proc").is_dir(), "requires Linux /proc")
    def test_bounded_runner_kills_setsid_descendant_after_leader_exit(self):
        with tempfile.TemporaryDirectory() as directory:
            pid_path = Path(directory) / "child.pid"
            ready_path = Path(directory) / "child.ready"
            code = (
                "import os,subprocess,sys,time; "
                "child=subprocess.Popen([sys.executable, '-c', "
                "\"import os,sys,time; os.setsid(); open(sys.argv[1], 'w').write('ready'); time.sleep(10)\", "
                "sys.argv[2]], close_fds=False); "
                "open(sys.argv[1], 'w').write(str(child.pid))\n"
                "while not os.path.exists(sys.argv[2]):\n"
                "    time.sleep(0.01)\n"
                "sys.stdout.flush(); sys.stderr.flush(); time.sleep(0.15); sys.exit(0)"
            )
            with self.assertRaises(CONTROL.ControlError) as failure:
                CONTROL.CommandRunner().run_bounded(
                    [os.sys.executable, "-c", code, str(pid_path), str(ready_path)], timeout=1.0
                )
            self.assertEqual(failure.exception.code, "command_timeout")
            if not wait_until(pid_path.exists):
                self.fail("setsid diagnostic child did not publish its pid")
            child_pid = int(pid_path.read_text())
            if not wait_until(lambda: process_is_zombie(child_pid)):
                self.fail("setsid diagnostic descendant remained alive after leader exit")

    @unittest.skipUnless(Path("/proc").is_dir(), "requires Linux /proc")
    def test_bounded_runner_fallback_kills_descendants_when_group_signal_fails(self):
        with tempfile.TemporaryDirectory() as directory:
            pid_path = Path(directory) / "child.pid"
            code = (
                "import subprocess,sys,time; "
                "child=subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(10)'], close_fds=False); "
                "open(sys.argv[1], 'w').write(str(child.pid)); "
                "time.sleep(10)"
            )
            with mock.patch.object(CONTROL.os, "killpg", side_effect=PermissionError("denied")):
                with self.assertRaises(CONTROL.ControlError) as failure:
                    CONTROL.CommandRunner().run_bounded(
                        [os.sys.executable, "-c", code, str(pid_path)], timeout=1.0
                    )
            self.assertTrue(failure.exception.detail["process_group_killed"])
            # A constrained CI runner may deny signalling a transient process
            # discovered during the fallback scan; the diagnostic remains
            # actionable while descendant cleanup is verified below.
            self.assertIn(
                failure.exception.detail["process_group_cleanup_error"],
                (None, "PermissionError"),
            )
            if not wait_until(pid_path.exists):
                self.fail("diagnostic fallback child did not publish its pid")
            child_pid = int(pid_path.read_text())
            if not wait_until(lambda: process_is_zombie(child_pid)):
                self.fail("diagnostic fallback descendant remained alive after timeout")


class IncusWaitTests(unittest.TestCase):
    def setUp(self):
        self.runner = mock.Mock(spec=CONTROL.CommandRunner)
        self.config = {"project": "agents", "profile": "agent-vm"}
        self.client = CONTROL.IncusClient(self.runner, self.config)

    def test_native_timeout_is_distinguished_from_explicit_failure(self):
        self.runner.run.return_value = subprocess.CompletedProcess([], 1, b"", b"Error: Timed out waiting for condition")
        with self.assertRaises(CONTROL.GuestWaitTimeout):
            self.client.wait("vm", "agent", 120)
        self.runner.run.assert_called_once_with(
            ["incus", "--project", "agents", "wait", "vm", "agent", "--timeout", "120", "--interval", "1"],
            input_data=None,
            timeout=120,
            check=False,
        )
        self.runner.run.return_value = subprocess.CompletedProcess([], 1, b"", b"Error: Instance is stopped")
        with self.assertRaises(CONTROL.ControlError) as failure:
            self.client.wait("vm", "agent", 120)
        self.assertEqual(failure.exception.code, "command_failed")

    def test_transport_timeout_at_native_wait_boundary_is_a_guest_timeout(self):
        self.runner.run.side_effect = CONTROL.ControlError("command_timeout", "wait transport timed out")
        with self.assertRaises(CONTROL.GuestWaitTimeout):
            self.client.wait("vm", "agent", 1)

    def test_native_init_uses_a_long_bounded_timeout_for_image_unpack(self):
        self.runner.run_bounded.return_value = subprocess.CompletedProcess([], 0, b"", b"")

        self.client.init("a" * 64, "vm", {"user.agent-vm.managed": "native-v1"})

        self.runner.run_bounded.assert_called_once_with(
            [
                "incus",
                "--project",
                "agents",
                "init",
                "a" * 64,
                "vm",
                "--vm",
                "--profile",
                "agent-vm",
                "--config",
                "boot.autostart=false",
                "--config",
                "user.agent-vm.managed=native-v1",
            ],
            timeout=CONTROL.NATIVE_INIT_TIMEOUT_SECONDS,
            stdout_limit=CONTROL.DIAGNOSTIC_LOG_LIMIT,
            stderr_limit=4000,
            check=True,
        )

    def test_bounded_exec_uses_process_group_safe_runner(self):
        self.runner.run_bounded.return_value = subprocess.CompletedProcess([], 0, b"ok", b"")

        result = self.client.exec("vm", ["python3", "-c", "print('ok')"], timeout=120, check=False, bounded=True)

        self.assertEqual(result.stdout, b"ok")
        self.runner.run_bounded.assert_called_once_with(
            ["incus", "--project", "agents", "exec", "vm", "--", "python3", "-c", "print('ok')"],
            timeout=120,
            stdout_limit=CONTROL.DIAGNOSTIC_LOG_LIMIT,
            stderr_limit=4000,
            check=False,
        )
        self.runner.run.assert_not_called()

    def test_instance_log_uses_bounded_runner_and_marks_truncation(self):
        result = subprocess.CompletedProcess([], 0, b"x" * CONTROL.DIAGNOSTIC_LOG_LIMIT, b"")
        result.stdout_truncated = True
        self.runner.run_bounded.return_value = result
        output = self.client.instance_log("vm")
        self.assertTrue(output.endswith(CONTROL.DIAGNOSTIC_LOG_TRUNCATION_MARKER))
        self.runner.run_bounded.assert_called_once_with(
            ["incus", "--project", "agents", "info", "vm", "--show-log"],
            timeout=CONTROL.DIAGNOSTIC_COMMAND_TIMEOUT_SECONDS,
            stdout_limit=CONTROL.DIAGNOSTIC_LOG_LIMIT,
        )

    def test_instance_info_can_use_bounded_runner_for_diagnostics(self):
        self.runner.run_bounded.return_value = subprocess.CompletedProcess(
            [], 0, b'{"status":"Running"}', b""
        )
        self.assertEqual(
            self.client.instance_info("vm", timeout=5, bounded=True),
            {"status": "Running"},
        )
        self.runner.run_bounded.assert_called_once_with(
            ["incus", "query", "/1.0/instances/vm/state?project=agents"],
            timeout=5,
            stdout_limit=CONTROL.DIAGNOSTIC_LOG_LIMIT,
            stderr_limit=4000,
        )
        self.runner.run.assert_not_called()

    def test_instance_list_rejects_malformed_entries(self):
        for body in (b"[null]", b"[{}]", b'[{"name":""}]'):
            with self.subTest(body=body):
                self.runner.run.return_value = subprocess.CompletedProcess([], 0, body, b"")
                with self.assertRaises(CONTROL.ControlError) as failure:
                    self.client.list_instances()
                self.assertEqual(failure.exception.code, "incus_invalid_output")

    def test_instance_info_rejects_malformed_status(self):
        for body in (b"{}", b'{"status":null}', b'{"status":7}'):
            with self.subTest(body=body):
                self.runner.run.return_value = subprocess.CompletedProcess([], 0, body, b"")
                with self.assertRaises(CONTROL.ControlError) as failure:
                    self.client.instance_info("vm")
                self.assertEqual(failure.exception.code, "incus_invalid_output")


class ReclaimTests(Fixture):
    def test_failed_native_reclaim_delete_keeps_acquire_operation_pending(self):
        cached = self.acquire("cached")
        self.call("release", "cached")
        self.runner.free = 10 * GIB
        self.runner.failure = lambda args: args[3:4] == ["delete"]
        request = self.request("acquire", "new", operation="reclaim-delete-uncertain", public_key=KEY)

        with self.assertRaises(CONTROL.ControlError) as failure:
            self.control.dispatch(request)

        self.assertEqual(failure.exception.code, "reclaim_failed")
        self.assertEqual(failure.exception.detail["detail"]["operation_phase"], "cache-delete")
        self.assertFalse(failure.exception.detail["outcome_verified"])
        self.assertEqual(self.store.operation(request["operation_id"])["outcome"], "pending")
        self.assertEqual(self.store.get_task(task_id=cached["task_id"])["state"], "deleting")

    def test_manual_gc_previews_and_deletes_all_eligible_caches(self):
        old = self.acquire("old")
        self.call("release", "old")
        self.now += dt.timedelta(hours=25)
        self.acquire("young")
        self.call("release", "young")
        self.acquire("active")
        dry = self.call("admin-gc")
        self.assertTrue(dry["dry_run"])
        self.assertEqual({item["task_id"] for item in dry["candidates"]},
                         {old["task_id"], self.store.get_task(session_id="young")["task_id"]})
        self.assertEqual(len(self.runner.instances), 3)
        self.call("admin-gc", apply=True)
        self.assertEqual(len(self.runner.instances), 1)

    def test_pressure_reclaims_other_session_oldest_first_and_stops_when_enough(self):
        old = self.acquire("old"); self.call("release", "old")
        self.now += dt.timedelta(minutes=1)
        young = self.acquire("young"); self.call("release", "young")
        self.runner.free = 10*GIB
        result = self.acquire("new")
        self.assertEqual(result["reclaimed"], [old["task_id"]])
        self.assertIsNotNone(self.store.get_task(task_id=young["task_id"]))
        self.assertEqual(self.store.get_task(task_id=young["task_id"])["state"], "cached")

    def test_enough_space_does_not_delete_cache(self):
        old = self.acquire("old"); self.call("release", "old")
        self.assertEqual(self.acquire("new")["reclaimed"], [])
        self.assertEqual(self.store.get_task(task_id=old["task_id"])["state"], "cached")

    def test_capacity_metrics_and_ram_limits_fail_closed(self):
        with mock.patch.object(CONTROL.shutil, "disk_usage", side_effect=OSError("unavailable CT path")):
            self.error("resource_unverified", self.acquire)
        self.assertEqual(self.runner.instances, {})
        self.config["cpu_overcommit_ratio"] = 1
        self.acquire("one"); self.acquire("two")
        self.error("resource_insufficient", self.acquire, "three")
        self.assertEqual(len(self.runner.instances), 2)

    def test_invalid_ct_filesystem_metrics_block_admission(self):
        for total, free in ((0, 0), (100*GIB, -1), (100*GIB, 101*GIB)):
            with self.subTest(total=total, free=free), mock.patch.object(
                CONTROL.shutil, "disk_usage", return_value=shutil._ntuple_diskusage(total, total-free, free)
            ):
                self.error("resource_unverified", self.acquire)
        self.assertEqual(self.runner.instances, {})

    def test_capacity_measures_only_ct_pool_and_invokes_no_commands(self):
        with mock.patch.object(CONTROL.shutil, "disk_usage", return_value=shutil._ntuple_diskusage(100*GIB, 20*GIB, 80*GIB)) as measure:
            facts = self.control._ensure_capacity_verified()
        measure.assert_called_once_with(self.config["storage_path"])
        self.assertTrue(any(args[:2] == ["incus", "query"] and "/1.0/storage-pools/agents" in args[2]
                            for args, _, _ in self.runner.calls))
        self.assertEqual(facts["scope"], "ct-filesystem")
        self.assertEqual(len(facts["layers"]), 1)
        self.assertEqual(facts["layers"][0]["free"], 80*GIB)
        self.assertEqual(self.acquire()["state"], "active")

    def test_capacity_rejects_incus_pool_source_drift(self):
        self.runner.storage_source = str(self.root / "different-pool")
        with mock.patch.object(CONTROL.shutil, "disk_usage") as measure:
            self.error("resource_unverified", self.control._ensure_capacity_verified)
        measure.assert_not_called()

    def test_capacity_rejects_non_directory_incus_pool(self):
        original = self.runner.run

        def non_dir(args, **kwargs):
            result = original(args, **kwargs)
            if args[:2] == ["incus", "query"] and "/1.0/storage-pools/agents" in args[2]:
                result.stdout = json.dumps({"name": "agents", "driver": "lvm", "config": {"source": ""}}).encode()
            return result

        with mock.patch.object(self.runner, "run", side_effect=non_dir):
            self.error("resource_unverified", self.control._ensure_capacity_verified)

    def test_config_rejects_removed_external_probe_field(self):
        path = self.root / "legacy-config.json"
        path.write_text(json.dumps({"backing_probes": [{"argv": ["unauthorized-probe"]}]}))
        self.error("config_invalid", CONTROL.load_config, path)
        self.assertEqual(self.runner.calls, [])

    def test_object_lock_is_visible_to_another_process(self):
        import hashlib
        identity = "session:thread-a"
        lock = self.store.path.parent / "locks" / hashlib.sha256(identity.encode()).hexdigest()
        code = "import fcntl,os,sys; fd=os.open(sys.argv[1],os.O_RDWR); fcntl.flock(fd,fcntl.LOCK_EX|fcntl.LOCK_NB)"
        with self.control._object_lock(identity):
            result = subprocess.run([os.sys.executable, "-c", code, str(lock)], capture_output=True)
            self.assertNotEqual(result.returncode, 0)
        result = subprocess.run([os.sys.executable, "-c", code, str(lock)], capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_running_cache_or_unknown_owner_is_not_reclaimed(self):
        owned = self.acquire("owned"); self.call("release", "owned")
        self.runner.instances[owned["instance_name"]]["status"] = "Running"
        self.runner.free = 10*GIB
        self.error("resource_unverified", self.acquire, "new")
        self.assertIn(owned["instance_name"], self.runner.instances)

    def test_long_native_operation_does_not_hold_global_lock(self):
        import fcntl
        def check(name):
            fd = os.open(self.control.lock_path, os.O_RDWR)
            try: fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            finally: os.close(fd)
        self.runner.on_start = check
        self.acquire()


class NativeCliTests(unittest.TestCase):
    def test_admin_metrics_is_read_only_and_does_not_require_a_session(self):
        args = CLI.build_parser().parse_args(["admin", "metrics"])
        for environment in ({}, {"AGENT_VM_SESSION_ID": "inherited-session"}):
            with self.subTest(environment=environment), \
                 mock.patch.dict(os.environ, environment, clear=False), \
                 mock.patch.object(CLI, "control_request", return_value={}) as request, \
                 mock.patch.object(CLI, "json_output"):
                self.assertEqual(CLI.command_admin(args), 0)
            request.assert_called_once_with("admin-metrics", session=None)

    def test_admin_metrics_rejects_explicit_session(self):
        args = CLI.build_parser().parse_args(["admin", "metrics", "--session-id", "explicit"])
        with self.assertRaises(CLI.CliError) as failure:
            CLI.command_admin(args)
        self.assertEqual(failure.exception.code, "invalid_request")

    def test_management_timeout_is_action_specific_and_stderr_is_streamed(self):
        for action, fields, expected in (
            ("acquire", {}, 600),
            ("restart", {}, 600),
            ("resize", {"allow_restart": True}, 600),
            ("resize", {"allow_restart": False}, 240),
            ("status", {}, 240),
        ):
            response = {"ok": True, "protocol": CLI.PROTOCOL_VERSION, "action": action,
                        "session_id": "thread", "outcome": "success",
                        "server_time": "2026-10-04T00:00:00+00:00", "result": {}}
            process = subprocess.CompletedProcess([], 0, json.dumps(response).encode(), b"")
            with self.subTest(action=action, fields=fields), mock.patch.object(CLI.subprocess, "run", return_value=process) as run:
                CLI.control_request(action, session="thread", fields=fields)
                self.assertEqual(run.call_args.kwargs["timeout"], expected)
                self.assertNotEqual(run.call_args.kwargs["stdout"], subprocess.PIPE)
                self.assertNotEqual(run.call_args.kwargs["stderr"], subprocess.PIPE)
                self.assertTrue(hasattr(run.call_args.kwargs["stdout"], "fileno"))
                self.assertTrue(hasattr(run.call_args.kwargs["stderr"], "fileno"))

    def test_exec_argv_and_online_tar_are_safely_quoted(self):
        parser = CLI.build_parser()
        arguments = ["printf", "%s", "a b", "$(touch /tmp/forbidden)", "semi;colon", "quote'and\""]
        with mock.patch.object(CLI, "session_id", return_value="thread"), mock.patch.object(CLI, "fetch_ssh_info", return_value=({}, {}, Path("/tmp/config"))), mock.patch.object(CLI, "run_command", return_value=23) as run:
            self.assertEqual(CLI.command_exec(parser.parse_args(["exec", "--", *arguments])), 23)
            self.assertEqual(shlex.split(run.call_args.args[0][-1]), arguments)
            self.assertEqual(CLI.command_export(parser.parse_args(["export", "/workspace/a b;name"])), 23)
            self.assertEqual(shlex.split(run.call_args.args[0][-1]), ["tar", "-C", "/", "-cf", "-", "--", "workspace/a b;name"])

    def test_protocol_mismatch_and_ssh_management_stdin(self):
        process = subprocess.CompletedProcess([], 0, json.dumps({"ok": True, "protocol": 999}).encode(), b"")
        with mock.patch.object(CLI.subprocess, "run", return_value=process) as run:
            with self.assertRaises(CLI.CliError) as context: CLI.control_request("status", session="thread")
            self.assertEqual(context.exception.code, "protocol_mismatch")
            args = run.call_args.args[0]
            self.assertEqual(args[args.index("--") + 1], "agent-vm-host")
            self.assertEqual(json.loads(run.call_args.kwargs["input"])["session_id"], "thread")
            self.assertNotIn("thread", args)

    def test_ssh_paths_with_spaces_are_private_and_parseable(self):
        with tempfile.TemporaryDirectory(prefix="ssh space ") as directory:
            root = Path(directory)
            private = root / "my key"; private.write_text("placeholder")
            info = {"task_id": "avm-test", "generation": 3, "host": "198.51.100.19", "port": 20000,
                    "host_key_alias": "agent-vm-avm-test-g3", "host_key": "ssh-ed25519 AAAATEST"}
            with mock.patch.object(CLI, "LOCAL_CONFIG_ROOT", root): path = CLI.write_ssh_config("thread", info, private)
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            self.assertIn('StrictHostKeyChecking yes', path.read_text())
            self.assertNotIn('ProxyJump', path.read_text())
            result = subprocess.run(["ssh", "-G", "-F", str(path), "agent-vm"], capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("user root", result.stdout)
            self.assertIn(str(private), result.stdout)

    def test_missing_session_never_calls_remote(self):
        with mock.patch.dict(os.environ, {}, clear=True), mock.patch.object(CLI.subprocess, "run") as run:
            self.assertEqual(CLI.main(["status"]), 2)
            run.assert_not_called()

    def test_contract_commands_are_available_and_resize_requires_resources(self):
        parser = CLI.build_parser()
        for value in (["sync", "--name", "Task_1"], ["resize", "--disk", "40GiB"], ["ssh-info", "--format", "ssh-config"],
                      ["admin", "drain", "enter"], ["admin", "session", "list"], ["admin", "metrics"], ["admin", "gc"], ["version"], ["help"]):
            parser.parse_args(value)


if __name__ == "__main__": unittest.main()
