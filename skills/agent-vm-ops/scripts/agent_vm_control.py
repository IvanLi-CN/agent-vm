#!/usr/bin/env python3
"""One-shot control plane for native Incus agent VMs.

The process handles one JSON request from stdin and emits one JSON response.
It intentionally has no listener, worker thread, or background loop.
"""

from __future__ import annotations

import argparse
from contextlib import contextmanager
import datetime as dt
from decimal import Decimal
import fcntl
import hashlib
import json
import os
import ipaddress
from pathlib import Path
import re
import selectors
import shlex
import secrets
import signal
import shutil
import sqlite3
import stat
import subprocess
import sys
import tempfile
import threading
import time
from typing import Any, Callable, Iterable, Mapping, Sequence
import uuid
from urllib.parse import quote, urlencode


PROTOCOL_VERSION = 4
MEMORY_SETTLE_SECONDS = 45
GUEST_READINESS_BUDGET_SECONDS = 300
GUEST_READINESS_DIAGNOSTIC_SECONDS = 120
DIAGNOSTIC_COMMAND_TIMEOUT_SECONDS = 5
DIAGNOSTIC_LOG_LIMIT = 8192
DIAGNOSTIC_LOG_TRUNCATION_MARKER = "\n[TRUNCATED BY DIAGNOSTIC LIMIT]"
DEFAULT_CONFIG_PATH = Path("/etc/agent-vm-native/config.json")
CONFIG_ENVIRONMENT_VARIABLE = "AGENT_VM_CONFIG"
DEFAULT_TIMER_DIR = Path("/etc/systemd/system")
REQUIRED_ENVIRONMENT_FIELDS = (
    "project",
    "profile",
    "image_alias",
    "image_fingerprint",
    "network",
    "network_acl",
    "pool",
    "host_address",
    "port_start",
    "port_end",
    "vm_ip_start",
    "vm_ip_end",
    "gateway_ip",
    "storage_path",
    "state_db",
    "timer_dir",
)
TASK_ID_RE = re.compile(r"^[a-z0-9][a-z0-9-]{0,47}$")
SESSION_ID_RE = re.compile(r"^[A-Za-z0-9_.:@/-]{1,160}$")
PORT_NAME_RE = re.compile(r"^[a-z][a-z0-9-]{0,30}$")
TASK_TIMER_RE = re.compile(r"^[a-z0-9][a-z0-9-]{0,47}-g[0-9]+$")


class ControlError(Exception):
    def __init__(self, code: str, message: str, detail: Any = None):
        super().__init__(message)
        self.code = code
        self.message = message
        self.detail = detail

    def as_dict(self) -> dict[str, Any]:
        value: dict[str, Any] = {"code": self.code, "message": self.message}
        if self.detail is not None:
            value["detail"] = self.detail
        return value


class GuestWaitTimeout(ControlError):
    """Incus' bounded wait expired without proving the condition."""

    def __init__(self, detail: Any = None):
        super().__init__("guest_wait_timeout", "Incus readiness wait expired", detail)


def overcommit_ratio(value: Any, name: str) -> Decimal:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise ControlError("config_invalid", f"{name} must be a finite number at least one")
    try:
        ratio = Decimal(str(value))
        if not ratio.is_finite() or ratio < 1:
            raise ValueError()
    except (ValueError, ArithmeticError) as exc:
        raise ControlError("config_invalid", f"{name} must be a finite number at least one") from exc
    return ratio


class CTResources:
    """Read only the container's visible boundary, never ancestor host data."""

    @staticmethod
    def _read(path: str) -> str:
        try:
            return Path(path).read_text()
        except OSError as exc:
            raise ControlError("resource_unverified", "CT resource metric is unavailable", {"metric": path}) from exc

    @staticmethod
    def _unsigned(value: str, metric: str, *, positive: bool = False) -> int:
        if not re.fullmatch(r"[0-9]+", value.strip()):
            raise ControlError("resource_unverified", "CT resource metric is invalid", {"metric": metric})
        number = int(value)
        if positive and number == 0:
            raise ControlError("resource_unverified", "CT capacity must be positive", {"metric": metric})
        return number

    def cpu(self) -> int:
        capacity = len(os.sched_getaffinity(0)) if hasattr(os, "sched_getaffinity") else (os.cpu_count() or 0)
        fields = self._read("/sys/fs/cgroup/cpu.max").split()
        if len(fields) != 2:
            raise ControlError("resource_unverified", "invalid CT CPU quota")
        period = self._unsigned(fields[1], "cpu.max period", positive=True)
        if fields[0] != "max":
            capacity = min(capacity, self._unsigned(fields[0], "cpu.max quota", positive=True) // period)
        return capacity

    def _lxcfs_meminfo(self) -> dict[str, int] | None:
        # Verify the mount before reading meminfo: an unvirtualized proc view
        # could disclose the outer host's capacity and must not be consumed.
        mounts = []
        for line in self._read("/proc/self/mountinfo").splitlines():
            before, separator, after = line.partition(" - ")
            left, right = before.split(), after.split()
            if separator and len(left) >= 6 and len(right) >= 2 and left[4] == "/proc/meminfo":
                mounts.append(right[:2])
        if mounts != [["fuse.lxcfs", "lxcfs"]]:
            return None
        values = {}
        for line in self._read("/proc/meminfo").splitlines():
            name, separator, value = line.partition(":")
            if separator and name in {"MemTotal", "MemAvailable"}:
                fields = value.split()
                if name in values or len(fields) != 2 or fields[1] != "kB":
                    raise ControlError("resource_unverified", "invalid LXCfs memory metric", {"metric": name})
                values[name] = self._unsigned(fields[0], name) * 1024
        if set(values) != {"MemTotal", "MemAvailable"} or not 0 < values["MemTotal"] or not 0 <= values["MemAvailable"] <= values["MemTotal"]:
            raise ControlError("resource_unverified", "LXCfs memory metrics are missing or inconsistent")
        return values

    def memory(self) -> dict[str, Any]:
        raw_limit = self._read("/sys/fs/cgroup/memory.max").strip()
        lxcfs = self._lxcfs_meminfo()
        sources = []
        if raw_limit == "max":
            if lxcfs is None:
                raise ControlError("resource_unverified", "unlimited CT cgroup requires a verified LXCfs memory view")
            limit, available = lxcfs["MemTotal"], lxcfs["MemAvailable"]
        else:
            limit = self._unsigned(raw_limit, "memory.max", positive=True)
            current = self._unsigned(self._read("/sys/fs/cgroup/memory.current"), "memory.current")
            entries = [line.split() for line in self._read("/sys/fs/cgroup/memory.stat").splitlines() if line.split()[:1] == ["inactive_file"]]
            if len(entries) != 1 or len(entries[0]) != 2:
                raise ControlError("resource_unverified", "inactive_file memory metric is missing or duplicated")
            inactive = self._unsigned(entries[0][1], "inactive_file")
            if inactive > current:
                raise ControlError("resource_unverified", "CT memory accounting is inconsistent")
            available = min(limit, max(0, limit - current + inactive))
            sources.append("cgroup-v2")
            if lxcfs is not None:
                limit = min(limit, lxcfs["MemTotal"])
                available = min(available, lxcfs["MemAvailable"], limit)
        if lxcfs is not None:
            sources.append("lxcfs")
        return {"scope": "ct-memory", "sources": sources, "limit_bytes": limit,
                "available_bytes": available, "threshold_bytes": max(1024**3, (limit + 9) // 10)}


def utc_now() -> dt.datetime:
    return dt.datetime.now(dt.timezone.utc)


def iso_time(value: dt.datetime | None = None) -> str:
    value = value or utc_now()
    return value.astimezone(dt.timezone.utc).replace(microsecond=0).isoformat()


def parse_time(value: str | None) -> dt.datetime | None:
    if not value:
        return None
    try:
        return dt.datetime.fromisoformat(value.replace("Z", "+00:00")).astimezone(
            dt.timezone.utc
        )
    except ValueError as exc:
        raise ControlError("invalid_time", f"invalid timestamp: {value}") from exc


def require_nonempty_string(value: Any, field: str, pattern: re.Pattern[str] | None = None) -> str:
    if not isinstance(value, str) or not value:
        raise ControlError("invalid_request", f"{field} must be a non-empty string")
    if pattern is not None and pattern.fullmatch(value) is None:
        raise ControlError("invalid_request", f"{field} contains unsupported characters")
    return value


def require_int(value: Any, field: str, minimum: int | None = None, maximum: int | None = None) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        raise ControlError("invalid_request", f"{field} must be an integer")
    if minimum is not None and value < minimum:
        raise ControlError("invalid_request", f"{field} must be >= {minimum}")
    if maximum is not None and value > maximum:
        raise ControlError("invalid_request", f"{field} must be <= {maximum}")
    return value


def read_json_file(path: Path) -> dict[str, Any]:
    try:
        with path.open("r", encoding="utf-8") as handle:
            value = json.load(handle)
    except FileNotFoundError as exc:
        raise ControlError("config_missing", f"configuration not found: {path}") from exc
    except (OSError, json.JSONDecodeError) as exc:
        raise ControlError("config_invalid", f"cannot read configuration: {path}") from exc
    if not isinstance(value, dict):
        raise ControlError("config_invalid", "configuration root must be an object")
    return value


def load_config(path: Path = DEFAULT_CONFIG_PATH) -> dict[str, Any]:
    raw = read_json_file(path)
    if "backing_probes" in raw:
        raise ControlError("config_invalid", "external storage probes are unsupported; remove backing_probes")
    config: dict[str, Any] = {
        "initial_lease_seconds": 1800,
        "renew_seconds": 900,
        "max_lease_seconds": 7200,
        "cache_seconds": 86400,
        "cpu_budget": 0,
        "memory_budget_mib": 0,
        "cpu_overcommit_ratio": 4,
        "memory_overcommit_ratio": 4,
        "create_cost_bytes": 4 * 1024**3,
        "resume_cost_bytes": 1024**3,
        "resize_cost_bytes": 1024**3,
        "required_guest_tools": ["python3", "sshd", "rsync", "docker", "growpart"],
        "timer_dir": str(DEFAULT_TIMER_DIR),
    }
    config.update(raw)
    missing = [field for field in REQUIRED_ENVIRONMENT_FIELDS if field not in raw or raw[field] in (None, "")]
    if missing:
        raise ControlError("config_missing", "required environment configuration is missing", {"fields": missing})
    for field in ("project", "profile", "image_alias", "image_fingerprint", "network", "network_acl", "pool"):
        require_nonempty_string(config.get(field), field)
    if len(config["image_fingerprint"]) != 64 or not re.fullmatch(
        r"[0-9a-f]{64}", config["image_fingerprint"]
    ):
        raise ControlError("config_invalid", "image_fingerprint must be a 64-character lowercase hex fingerprint")
    config["port_start"] = require_int(config["port_start"], "port_start", 1024, 65535)
    config["port_end"] = require_int(config["port_end"], "port_end", config["port_start"], 65535)
    for field in ("initial_lease_seconds", "renew_seconds", "max_lease_seconds", "cache_seconds"):
        config[field] = require_int(config[field], field, 1)
    if config["renew_seconds"] > config["max_lease_seconds"]:
        raise ControlError("config_invalid", "renew_seconds exceeds max_lease_seconds")
    for field in ("cpu_budget", "memory_budget_mib", "create_cost_bytes", "resume_cost_bytes", "resize_cost_bytes"):
        config[field] = require_int(config[field], field, 0)
    for field in ("cpu_overcommit_ratio", "memory_overcommit_ratio"):
        overcommit_ratio(config[field], field)
    config["host_address"] = require_nonempty_string(config["host_address"], "host_address")
    config["vm_ip_start"] = require_nonempty_string(config["vm_ip_start"], "vm_ip_start")
    config["vm_ip_end"] = require_nonempty_string(config["vm_ip_end"], "vm_ip_end")
    config["gateway_ip"] = require_nonempty_string(config["gateway_ip"], "gateway_ip")
    try:
        host_address = ipaddress.IPv4Address(config["host_address"])
        vm_ip_start = ipaddress.IPv4Address(config["vm_ip_start"])
        vm_ip_end = ipaddress.IPv4Address(config["vm_ip_end"])
        gateway_ip = ipaddress.IPv4Address(config["gateway_ip"])
    except ipaddress.AddressValueError as exc:
        raise ControlError("config_invalid", "host_address and VM IP bounds must be IPv4 addresses") from exc
    if int(vm_ip_start) > int(vm_ip_end):
        raise ControlError("config_invalid", "vm_ip_start must not exceed vm_ip_end")
    if not isinstance(config.get("required_guest_tools"), list) or not all(
        isinstance(item, str) and item for item in config["required_guest_tools"]
    ):
        raise ControlError("config_invalid", "required_guest_tools must be a list of strings")
    config["vm_ip_start"] = str(vm_ip_start)
    config["vm_ip_end"] = str(vm_ip_end)
    config["gateway_ip"] = str(gateway_ip)
    config["host_address"] = str(host_address)
    config["network_acl"] = require_nonempty_string(config["network_acl"], "network_acl", PORT_NAME_RE)
    config["storage_path"] = require_nonempty_string(config["storage_path"], "storage_path")
    config["state_db"] = require_nonempty_string(config["state_db"], "state_db")
    config["timer_dir"] = require_nonempty_string(config["timer_dir"], "timer_dir")
    return config


class CommandRunner:
    """Small subprocess boundary that never invokes a shell implicitly."""

    def run(
        self,
        argv: Sequence[str],
        *,
        input_data: bytes | str | None = None,
        timeout: float = 30,
        check: bool = True,
    ) -> subprocess.CompletedProcess[bytes]:
        try:
            result = subprocess.run(
                list(argv),
                input=input_data.encode() if isinstance(input_data, str) else input_data,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                timeout=timeout,
                check=False,
            )
        except FileNotFoundError as exc:
            raise ControlError("command_missing", f"command not found: {argv[0]}") from exc
        except subprocess.TimeoutExpired as exc:
            raise ControlError("command_timeout", f"command timed out: {argv[0]}") from exc
        if check and result.returncode != 0:
            stderr = result.stderr.decode("utf-8", "replace").strip()
            raise ControlError(
                "command_failed",
                f"command failed ({result.returncode}): {argv[0]}",
                {"returncode": result.returncode, "stderr": stderr[-4000:]},
            )
        return result

    @staticmethod
    def _process_group_members(pgid: int) -> tuple[list[int], bool]:
        members: list[int] = []
        try:
            entries = list(Path("/proc").iterdir())
        except OSError:
            return [], False
        complete = True
        for entry in entries:
            if not entry.name.isdigit():
                continue
            try:
                raw_stat = (entry / "stat").read_text()
                close_paren = raw_stat.rfind(")")
                fields = raw_stat[close_paren + 2 :].split()
                if close_paren < 0 or len(fields) < 3:
                    raise ValueError("invalid process stat")
                if fields[0] != "Z" and int(fields[2]) == pgid:
                    members.append(int(entry.name))
            except FileNotFoundError:
                continue
            except (OSError, ValueError):
                complete = False
        return members, complete

    @classmethod
    def _kill_process_group_members(cls, pgid: int) -> tuple[bool, str | None]:
        cleanup_error = None
        for _ in range(3):
            members, complete = cls._process_group_members(pgid)
            if not complete:
                return False, "process_group_inspection_failed"
            if not members:
                return True, cleanup_error
            for pid in members:
                try:
                    os.kill(pid, signal.SIGKILL)
                except ProcessLookupError:
                    continue
                except OSError as error:
                    cleanup_error = type(error).__name__
        members, complete = cls._process_group_members(pgid)
        if complete and not members:
            return True, cleanup_error
        return False, cleanup_error or "process_group_members_remain"

    def run_bounded(
        self,
        argv: Sequence[str],
        *,
        timeout: float = 30,
        stdout_limit: int = DIAGNOSTIC_LOG_LIMIT,
        stderr_limit: int = 4000,
        check: bool = True,
    ) -> subprocess.CompletedProcess[bytes]:
        """Run a command while draining pipes but retaining bounded output."""
        try:
            process = subprocess.Popen(
                list(argv),
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                start_new_session=True,
            )
        except FileNotFoundError as exc:
            raise ControlError("command_missing", f"command not found: {argv[0]}") from exc

        stdout = bytearray()
        stderr = bytearray()
        stdout_truncated = False
        selector = selectors.DefaultSelector()
        streams = ((process.stdout, "stdout"), (process.stderr, "stderr"))
        for stream, name in streams:
            if stream is not None:
                selector.register(stream, selectors.EVENT_READ, name)
        started = time.monotonic()
        try:
            while selector.get_map():
                remaining = timeout - (time.monotonic() - started)
                if remaining <= 0:
                    raise subprocess.TimeoutExpired(argv[0], timeout)
                events = selector.select(remaining)
                if not events:
                    continue
                for key, _ in events:
                    chunk = os.read(key.fileobj.fileno(), 65536)
                    if not chunk:
                        selector.unregister(key.fileobj)
                        key.fileobj.close()
                        continue
                    if key.data == "stdout":
                        available = max(0, stdout_limit - len(stdout))
                        if available:
                            stdout.extend(chunk[:available])
                        if len(chunk) > available:
                            stdout_truncated = True
                    else:
                        stderr.extend(chunk)
                        if len(stderr) > stderr_limit:
                            del stderr[:-stderr_limit]
            remaining = timeout - (time.monotonic() - started)
            if remaining <= 0:
                raise subprocess.TimeoutExpired(argv[0], timeout)
            returncode = process.wait(timeout=remaining)
        except subprocess.TimeoutExpired as exc:
            reaped = False
            process_group_killed = False
            process_group_cleanup_error = None
            try:
                os.killpg(process.pid, signal.SIGKILL)
                process_group_killed, fallback_error = self._kill_process_group_members(process.pid)
                if fallback_error:
                    process_group_cleanup_error = fallback_error
            except ProcessLookupError:
                # The leader and its process group are already gone.
                process_group_killed = True
            except OSError as group_error:
                process_group_cleanup_error = type(group_error).__name__
                process_group_killed, fallback_error = self._kill_process_group_members(process.pid)
                if fallback_error:
                    process_group_cleanup_error += ":" + fallback_error
                try:
                    process.kill()
                except ProcessLookupError:
                    pass
            try:
                process.wait(timeout=1)
                reaped = True
            except subprocess.TimeoutExpired:
                pass
            raise ControlError(
                "command_timeout",
                f"command timed out: {argv[0]}",
                {
                    "reaped": reaped,
                    "process_group_killed": process_group_killed,
                    "process_group_cleanup_error": process_group_cleanup_error,
                },
            ) from exc
        finally:
            selector.close()
            for stream, _ in streams:
                if stream is not None and not stream.closed:
                    stream.close()

        result = subprocess.CompletedProcess(
            list(argv), returncode, bytes(stdout), bytes(stderr)
        )
        result.stdout_truncated = stdout_truncated
        if check and returncode != 0:
            detail = {
                "returncode": returncode,
                "stderr": result.stderr.decode("utf-8", "replace").strip()[-4000:],
            }
            raise ControlError(
                "command_failed",
                f"command failed ({returncode}): {argv[0]}",
                detail,
            )
        return result


class IncusClient:
    def __init__(self, runner: CommandRunner, config: Mapping[str, Any]):
        self.runner = runner
        self.config = config

    @property
    def prefix(self) -> list[str]:
        return ["incus", "--project", str(self.config["project"])]

    def call(
        self,
        args: Sequence[str],
        *,
        input_data: bytes | str | None = None,
        timeout: float = 60,
        check: bool = True,
    ) -> subprocess.CompletedProcess[bytes]:
        return self.runner.run(
            self.prefix + list(args), input_data=input_data, timeout=timeout, check=check
        )

    def json(self, args: Sequence[str], *, timeout: float = 60) -> Any:
        result = self.call([*args, "--format", "json"], timeout=timeout)
        try:
            return json.loads(result.stdout.decode("utf-8"))
        except json.JSONDecodeError as exc:
            raise ControlError("incus_invalid_output", "Incus returned invalid JSON") from exc

    def list_instances(self) -> list[dict[str, Any]]:
        value = self.json(["list"])
        if not isinstance(value, list):
            raise ControlError("incus_invalid_output", "Incus list output is not an array")
        return [item for item in value if isinstance(item, dict)]

    def instance_info(
        self, name: str, *, timeout: float = 30, bounded: bool = False
    ) -> dict[str, Any]:
        value = self.query(
            f"/1.0/instances/{quote(name, safe='')}/state",
            timeout=timeout,
            bounded=bounded,
        )
        if not isinstance(value, dict):
            raise ControlError("incus_invalid_output", "Incus info output is not an object")
        return value

    def init(self, image: str, name: str, metadata: Mapping[str, str] | None = None) -> None:
        self.call(
            [
                "init",
                image,
                name,
                "--vm",
                "--profile",
                str(self.config["profile"]),
                "--config",
                "boot.autostart=false",
                *[part for key, value in (metadata or {}).items() for part in ("--config", f"{key}={value}")],
            ]
        )

    def start(self, name: str) -> None:
        self.call(["start", name], timeout=120)

    def wait(self, name: str, condition: str, seconds: int) -> None:
        try:
            result = self.call(
                ["wait", name, condition, "--timeout", str(seconds), "--interval", "1"],
                timeout=seconds,
                check=False,
            )
        except ControlError as error:
            if error.code == "command_timeout":
                raise GuestWaitTimeout({"transport_timeout": True}) from error
            raise
        if result.returncode == 0:
            return
        stderr = result.stderr.decode("utf-8", "replace").strip()
        detail = {"returncode": result.returncode, "stderr": stderr[-4000:]}
        if re.search(r"(?:timed?\s*out|timeout)", stderr, re.IGNORECASE):
            raise GuestWaitTimeout(detail)
        raise ControlError("command_failed", f"command failed ({result.returncode}): incus", detail)

    def stop(self, name: str, force: bool = True) -> None:
        args = ["stop", name]
        if force:
            args.append("--force")
        self.call(args, timeout=8)

    def delete(self, name: str) -> None:
        self.call(["delete", name, "--force"], timeout=120)

    def config_set(self, name: str, key: str, value: str) -> None:
        self.call(["config", "set", name, f"{key}={value}"])

    def config_show(self, name: str) -> dict[str, Any]:
        value = self.query(f"/1.0/instances/{quote(name, safe='')}")
        if not isinstance(value, dict):
            raise ControlError("incus_invalid_output", "Incus config output is not an object")
        return value

    def network_forwards(self) -> list[dict[str, Any]]:
        value = self.json(["network", "forward", "list", str(self.config["network"])])
        if not isinstance(value, list):
            raise ControlError("incus_invalid_output", "Incus network forwards output is not an array")
        return [item for item in value if isinstance(item, dict)]

    def query(
        self,
        path: str,
        *,
        project: str | None = None,
        timeout: float = 30,
        bounded: bool = False,
    ) -> Any:
        path += ("&" if "?" in path else "?") + urlencode({"project": project or self.config["project"]})
        # query uses raw API URLs and rejects Incus' global --project flag.
        if bounded:
            result = self.runner.run_bounded(
                ["incus", "query", path],
                timeout=timeout,
                stdout_limit=DIAGNOSTIC_LOG_LIMIT,
                stderr_limit=4000,
            )
            if getattr(result, "stdout_truncated", False):
                raise ControlError("incus_invalid_output", "query output was truncated")
        else:
            result = self.runner.run(["incus", "query", path], timeout=timeout)
        try:
            return json.loads(result.stdout)
        except ValueError as exc:
            raise ControlError("incus_invalid_output", "query did not return JSON") from exc

    def instance_log(self, name: str, *, timeout: float = DIAGNOSTIC_COMMAND_TIMEOUT_SECONDS) -> str:
        result = self.runner.run_bounded(
            self.prefix + ["info", name, "--show-log"],
            timeout=timeout,
            stdout_limit=DIAGNOSTIC_LOG_LIMIT,
        )
        output = result.stdout.decode("utf-8", "replace")
        if getattr(result, "stdout_truncated", False):
            output += DIAGNOSTIC_LOG_TRUNCATION_MARKER
        return output

    def device_set(self, name: str, device: str, key: str, value: str) -> None:
        info = self.config_show(name)
        if device not in info.get("devices", {}):
            self.call(["config", "device", "override", name, device, f"{key}={value}"])
        else:
            self.call(["config", "device", "set", name, device, f"{key}={value}"])

    def device_add(
        self,
        name: str,
        device: str,
        device_type: str,
        properties: Mapping[str, str],
    ) -> None:
        args = ["config", "device", "add", name, device, device_type]
        for key, value in properties.items():
            args.append(f"{key}={value}")
        self.call(args)

    def device_remove(self, name: str, device: str) -> None:
        self.call(["config", "device", "remove", name, device])

    def exec(
        self,
        name: str,
        argv: Sequence[str],
        *,
        input_data: bytes | str | None = None,
        timeout: float = 60,
        check: bool = True,
    ) -> subprocess.CompletedProcess[bytes]:
        return self.call(["exec", name, "--", *argv], input_data=input_data, timeout=timeout, check=check)


class StateStore:
    def __init__(self, path: Path):
        self.path = path
        self._lock = threading.RLock()
        self.path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        parent = self.path.parent.lstat()
        if not stat.S_ISDIR(parent.st_mode) or parent.st_uid != os.geteuid() or parent.st_mode & 0o077:
            raise ControlError("state_path_unsafe", "state parent must be a private owned directory")
        if self.path.is_symlink(): raise ControlError("state_path_unsafe", "state database cannot be a symlink")
        self.connection = sqlite3.connect(str(path), timeout=10, check_same_thread=False)
        self.path.chmod(0o600)
        self.depth = 0
        self.connection.row_factory = sqlite3.Row
        self.connection.execute("PRAGMA journal_mode=WAL")
        self.connection.execute("PRAGMA synchronous=FULL")
        self.connection.execute("PRAGMA foreign_keys=ON")
        self._schema()

    def _schema(self) -> None:
        with self.connection:
            self.connection.executescript(
                """
                CREATE TABLE IF NOT EXISTS meta (
                    key TEXT PRIMARY KEY,
                    value TEXT NOT NULL
                );
                CREATE TABLE IF NOT EXISTS tasks (
                    task_id TEXT PRIMARY KEY,
                    session_id TEXT NOT NULL UNIQUE,
                    generation INTEGER NOT NULL,
                    state TEXT NOT NULL,
                    instance_name TEXT NOT NULL UNIQUE,
                    vm_ip TEXT,
                    ssh_host_port INTEGER,
                    lease_expires_at TEXT,
                    inactive_since TEXT,
                    cleanup_after TEXT,
                    created_at TEXT NOT NULL,
                    updated_at TEXT NOT NULL,
                    last_error TEXT,
                    image_fingerprint TEXT NOT NULL,
                    resources_json TEXT NOT NULL DEFAULT '{}',
                    reservation_bytes INTEGER NOT NULL DEFAULT 0,
                    previous_generation INTEGER,
                    resource_reservation_json TEXT NOT NULL DEFAULT '{}'
                );
                CREATE TABLE IF NOT EXISTS ports (
                    task_id TEXT NOT NULL,
                    generation INTEGER NOT NULL,
                    port_name TEXT NOT NULL,
                    protocol TEXT NOT NULL,
                    host_port INTEGER NOT NULL,
                    vm_port INTEGER NOT NULL,
                    device_name TEXT NOT NULL,
                    state TEXT NOT NULL,
                    created_at TEXT NOT NULL,
                    PRIMARY KEY (task_id, port_name, protocol),
                    UNIQUE (host_port, protocol),
                    FOREIGN KEY (task_id) REFERENCES tasks(task_id)
                );
                CREATE TABLE IF NOT EXISTS operations (
                    operation_id TEXT PRIMARY KEY,
                    action TEXT NOT NULL,
                    session_id TEXT,
                    generation INTEGER,
                    response_json TEXT NOT NULL,
                    created_at TEXT NOT NULL
                );
                CREATE TABLE IF NOT EXISTS audit (
                    id INTEGER PRIMARY KEY AUTOINCREMENT,
                    action TEXT NOT NULL,
                    task_id TEXT,
                    session_id TEXT,
                    generation INTEGER,
                    outcome TEXT NOT NULL,
                    detail_json TEXT,
                    created_at TEXT NOT NULL
                );
                """
            )

    def close(self) -> None:
        with self._lock:
            self.connection.close()

    @contextmanager
    def transaction(self):
        """Nestable short transactions; never wrap native operations/waits."""
        with self._lock:
            outer = self.depth == 0
            if outer: self.connection.execute("BEGIN IMMEDIATE")
            self.depth += 1
            try:
                yield
            except BaseException:
                if outer: self.connection.rollback()
                raise
            else:
                if outer: self.connection.commit()
            finally:
                self.depth -= 1

    def _row(self, query: str, args: Sequence[Any] = ()) -> sqlite3.Row | None:
        with self._lock:
            return self.connection.execute(query, args).fetchone()

    def _rows(self, query: str, args: Sequence[Any] = ()) -> list[sqlite3.Row]:
        with self._lock:
            return list(self.connection.execute(query, args).fetchall())

    def get_task(self, *, session_id: str | None = None, task_id: str | None = None) -> dict[str, Any] | None:
        if (session_id is None) == (task_id is None):
            raise ValueError("provide exactly one task lookup key")
        if session_id is not None:
            row = self._row("SELECT * FROM tasks WHERE session_id = ?", (session_id,))
        else:
            row = self._row("SELECT * FROM tasks WHERE task_id = ?", (task_id,))
        return dict(row) if row is not None else None

    def list_tasks(self) -> list[dict[str, Any]]:
        return [dict(row) for row in self._rows("SELECT * FROM tasks ORDER BY created_at")]

    def put_task(self, task: Mapping[str, Any]) -> None:
        fields = (
            "task_id", "session_id", "generation", "state", "instance_name", "vm_ip",
            "ssh_host_port", "lease_expires_at", "inactive_since", "cleanup_after",
            "created_at", "updated_at", "last_error", "image_fingerprint", "resources_json", "reservation_bytes", "previous_generation", "resource_reservation_json",
        )
        values = [task.get(field, '{}' if field in {'resources_json', 'resource_reservation_json'} else 0 if field == 'reservation_bytes' else None) for field in fields]
        with self.transaction():
            self.connection.execute(
                f"""
                INSERT INTO tasks ({", ".join(fields)}) VALUES ({", ".join("?" for _ in fields)})
                ON CONFLICT(task_id) DO UPDATE SET
                    session_id=excluded.session_id,
                    generation=excluded.generation,
                    state=excluded.state,
                    instance_name=excluded.instance_name,
                    vm_ip=excluded.vm_ip,
                    ssh_host_port=excluded.ssh_host_port,
                    lease_expires_at=excluded.lease_expires_at,
                    inactive_since=excluded.inactive_since,
                    cleanup_after=excluded.cleanup_after,
                    updated_at=excluded.updated_at,
                    last_error=excluded.last_error,
                    image_fingerprint=excluded.image_fingerprint,
                    resources_json=excluded.resources_json,
                    reservation_bytes=excluded.reservation_bytes,
                    previous_generation=excluded.previous_generation,
                    resource_reservation_json=excluded.resource_reservation_json
                """,
                values,
            )

    def update_task(self, task_id: str, **changes: Any) -> dict[str, Any]:
        if not changes:
            task = self.get_task(task_id=task_id)
            if task is None:
                raise ControlError("task_missing", f"task not found: {task_id}")
            return task
        changes["updated_at"] = iso_time()
        assignments = ", ".join(f"{key} = ?" for key in changes)
        with self.transaction():
            self.connection.execute(
                f"UPDATE tasks SET {assignments} WHERE task_id = ?",
                [*changes.values(), task_id],
            )
        task = self.get_task(task_id=task_id)
        if task is None:
            raise ControlError("task_missing", f"task not found: {task_id}")
        return task

    def get_ports(self, task_id: str, state: str | None = None) -> list[dict[str, Any]]:
        if state is None:
            rows = self._rows("SELECT * FROM ports WHERE task_id = ? ORDER BY port_name, protocol", (task_id,))
        else:
            rows = self._rows(
                "SELECT * FROM ports WHERE task_id = ? AND state = ? ORDER BY port_name, protocol",
                (task_id, state),
            )
        return [dict(row) for row in rows]

    def get_port(self, task_id: str, port_name: str, protocol: str) -> dict[str, Any] | None:
        row = self._row(
            "SELECT * FROM ports WHERE task_id = ? AND port_name = ? AND protocol = ?",
            (task_id, port_name, protocol),
        )
        return dict(row) if row is not None else None

    def put_port(self, port: Mapping[str, Any]) -> None:
        fields = (
            "task_id", "generation", "port_name", "protocol", "host_port",
            "vm_port", "device_name", "state", "created_at",
        )
        with self.transaction():
            self.connection.execute(
                f"""
                INSERT INTO ports ({", ".join(fields)}) VALUES ({", ".join("?" for _ in fields)})
                ON CONFLICT(task_id, port_name, protocol) DO UPDATE SET
                    generation=excluded.generation,
                    host_port=excluded.host_port,
                    vm_port=excluded.vm_port,
                    device_name=excluded.device_name,
                    state=excluded.state
                """,
                [port.get(field) for field in fields],
            )

    def update_port(self, task_id: str, port_name: str, protocol: str, **changes: Any) -> None:
        if not changes:
            return
        assignments = ", ".join(f"{key} = ?" for key in changes)
        with self.transaction():
            self.connection.execute(
                f"UPDATE ports SET {assignments} WHERE task_id = ? AND port_name = ? AND protocol = ?",
                [*changes.values(), task_id, port_name, protocol],
            )

    def remove_port(self, task_id: str, port_name: str, protocol: str) -> None:
        with self.transaction():
            self.connection.execute(
                "DELETE FROM ports WHERE task_id = ? AND port_name = ? AND protocol = ?",
                (task_id, port_name, protocol),
            )

    def occupied_ports(self, protocol: str | None = None) -> set[int]:
        if protocol is None:
            rows = self._rows("SELECT host_port FROM ports WHERE state != 'removed'")
        else:
            rows = self._rows(
                "SELECT host_port FROM ports WHERE protocol = ? AND state != 'removed'",
                (protocol,),
            )
        return {int(row["host_port"]) for row in rows}

    def operation(self, operation_id: str) -> dict[str, Any] | None:
        row = self._row("SELECT response_json FROM operations WHERE operation_id = ?", (operation_id,))
        if row is None:
            return None
        return json.loads(row["response_json"])

    def record_operation(
        self,
        operation_id: str,
        action: str,
        session_id: str | None,
        generation: int | None,
        response: Mapping[str, Any],
    ) -> None:
        with self.transaction():
            self.connection.execute(
                """
                INSERT OR REPLACE INTO operations
                (operation_id, action, session_id, generation, response_json, created_at)
                VALUES (?, ?, ?, ?, ?, ?)
                """,
                (
                    operation_id,
                    action,
                    session_id,
                    generation,
                    json.dumps(response, sort_keys=True),
                    iso_time(),
                ),
            )

    def audit(
        self,
        action: str,
        outcome: str,
        *,
        task_id: str | None = None,
        session_id: str | None = None,
        generation: int | None = None,
        detail: Any = None,
    ) -> None:
        with self.transaction():
            self.connection.execute(
                """
                INSERT INTO audit (action, task_id, session_id, generation, outcome, detail_json, created_at)
                VALUES (?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    action,
                    task_id,
                    session_id,
                    generation,
                    outcome,
                    json.dumps(detail, sort_keys=True) if detail is not None else None,
                    iso_time(),
                ),
            )

    def get_meta(self, key: str) -> str | None:
        row = self._row("SELECT value FROM meta WHERE key = ?", (key,))
        return str(row["value"]) if row is not None else None

    def set_meta(self, key: str, value: str) -> None:
        with self.transaction():
            self.connection.execute(
                "INSERT INTO meta(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value",
                (key, value),
            )


class SystemdManager:
    def __init__(
        self,
        runner: CommandRunner,
        config: Mapping[str, Any],
        *,
        enabled: bool = True,
    ):
        self.runner = runner
        self.config = config
        self.enabled = enabled
        self.timer_dir = Path(str(config["timer_dir"]))

    @staticmethod
    def timer_unit(task_id: str, generation: int) -> str:
        if TASK_ID_RE.fullmatch(task_id) is None or generation < 1:
            raise ControlError("invalid_timer", "invalid task timer identity")
        return f"agent-vm-native-expire-{task_id}-g{generation}.timer"

    @staticmethod
    def instance_name(task_id: str, generation: int) -> str:
        return f"{task_id}-g{generation}"

    def _systemctl(self, *args: str) -> None:
        if self.enabled:
            self.runner.run(["systemctl", *args], timeout=30)

    def schedule_expiry(self, task_id: str, generation: int, expires_at: str) -> str:
        unit = self.timer_unit(task_id, generation)
        timer_path = self.timer_dir / unit
        content = (
            "[Unit]\n"
            "Description=Expire native agent VM lease\n"
            "After=agent-vm-native-reconcile.service\n\n"
            "[Timer]\n"
            f"OnCalendar={parse_time(expires_at).strftime('%Y-%m-%d %H:%M:%S UTC')}\n"
            "Persistent=yes\n"
            "AccuracySec=1s\n"
            "RandomizedDelaySec=0\n"
            f"Unit=agent-vm-native-expire@{self.instance_name(task_id, generation)}.service\n\n"
            "[Install]\n"
            "WantedBy=timers.target\n"
        )
        if self.enabled:
            self.timer_dir.mkdir(parents=True, exist_ok=True)
            fd, temp_name = tempfile.mkstemp(prefix=f".{unit}.", dir=self.timer_dir)
            try:
                with os.fdopen(fd, "w", encoding="utf-8") as handle:
                    handle.write(content)
                    handle.flush()
                    os.fsync(handle.fileno())
                os.replace(temp_name, timer_path)
            finally:
                if os.path.exists(temp_name):
                    os.unlink(temp_name)
            self._systemctl("daemon-reload")
            self._systemctl("enable", "--now", unit)
            self._systemctl("restart", unit)
        return unit

    def remove_expiry(self, task_id: str, generation: int) -> str:
        unit = self.timer_unit(task_id, generation)
        path = self.timer_dir / unit
        try:
            self._systemctl("disable", "--now", unit)
        except ControlError as exc:
            if exc.code != "command_failed" or not self.enabled:
                raise
            # A crash can follow the durable reservation but precede timer creation.
            # Accept absence only when both native state and the exact file prove it.
            try:
                probe = self.runner.run(
                    ["systemctl", "show", unit, "-p", "LoadState", "-p", "ActiveState",
                     "-p", "FragmentPath", "-p", "UnitFileState"],
                    timeout=10, check=False,
                )
            except ControlError:
                raise exc
            properties = dict(line.split("=", 1) for line in
                              probe.stdout.decode("utf-8", "replace").splitlines() if "=" in line)
            if (probe.returncode not in {0, 1} or properties != {
                    "LoadState": "not-found", "ActiveState": "inactive",
                    "FragmentPath": "", "UnitFileState": ""}
                    or path.exists() or path.is_symlink()):
                raise exc
        if self.enabled:
            try:
                path.unlink()
            except FileNotFoundError:
                pass
            self._systemctl("daemon-reload")
        return unit

    def enable_periodic_units(self) -> None:
        self._systemctl("daemon-reload")
        self._systemctl("enable", "--now", "agent-vm-native-age-gc.timer")
        self._systemctl("enable", "agent-vm-native-reconcile.service")

    def retry_cleanup(self, task_id: str, generation: int) -> None:
        self._systemctl("start", "--no-block", f"agent-vm-native-expire@{self.instance_name(task_id, generation)}.service")


def ipv4_to_int(value: str) -> int:
    parts = value.split(".")
    if len(parts) != 4:
        raise ControlError("config_invalid", f"invalid IPv4 address: {value}")
    numbers = [int(part) for part in parts]
    if any(number < 0 or number > 255 for number in numbers):
        raise ControlError("config_invalid", f"invalid IPv4 address: {value}")
    return (numbers[0] << 24) | (numbers[1] << 16) | (numbers[2] << 8) | numbers[3]


def int_to_ipv4(value: int) -> str:
    return ".".join(str((value >> shift) & 255) for shift in (24, 16, 8, 0))


def parse_listen_ports(output: str) -> set[int]:
    ports = set()
    for line in output.splitlines():
        fields = line.split()
        if len(fields) < 6:
            raise ControlError("port_inventory_unverified", "unrecognized ss output")
        match = re.search(r":(\d+)$", fields[4])
        if not match:
            raise ControlError("port_inventory_unverified", "unrecognized socket endpoint")
        ports.add(int(match[1]))
    return ports



class Control:
    WRITE_ACTIONS = {
        "acquire", "renew", "release", "destroy", "restart", "resize",
        "port-add", "port-remove", "drain-enter", "drain-exit",
        "admin-reconcile", "admin-gc",
    }
    SESSION_ACTIONS = {
        "status", "acquire", "renew", "release", "destroy", "restart",
        "resize", "ssh-info", "port-add", "port-list", "port-remove",
    }
    ACTIVE_STATES = {"preparing", "active"}
    TRANSITION_STATES = {"preparing", "closing", "deleting"}

    def __init__(
        self,
        config: Mapping[str, Any],
        *,
        runner: CommandRunner | None = None,
        store: StateStore | None = None,
        systemd: SystemdManager | None = None,
        clock: Callable[[], dt.datetime] = utc_now,
        resources: CTResources | None = None,
    ):
        self.config = dict(config)
        self.runner = runner or CommandRunner()
        self.store = store or StateStore(Path(str(self.config["state_db"])))
        self.incus = IncusClient(self.runner, self.config)
        self.systemd = systemd or SystemdManager(self.runner, self.config)
        self.clock = clock
        self.resources = resources or CTResources()
        self.lock_path = Path(str(self.config["state_db"])).with_name("control.lock")
        self._native_lock_state = threading.local()

    @contextmanager
    def _global_lock(self):
        self.lock_path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        fd = os.open(self.lock_path, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
        try:
            fcntl.flock(fd, fcntl.LOCK_EX)
            yield
        finally:
            fcntl.flock(fd, fcntl.LOCK_UN)
            os.close(fd)

    def now(self) -> dt.datetime:
        return self.clock().astimezone(dt.timezone.utc)

    @contextmanager
    def _object_lock(self, identity: str, *, blocking: bool = True):
        directory = self.store.path.parent / "locks"
        directory.mkdir(mode=0o700, exist_ok=True)
        digest = hashlib.sha256(identity.encode()).hexdigest()
        fd = os.open(directory / digest, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
        try:
            try:
                fcntl.flock(fd, fcntl.LOCK_EX | (0 if blocking else fcntl.LOCK_NB))
            except BlockingIOError as exc:
                raise ControlError("object_busy", "resource is being changed") from exc
            yield
        finally:
            os.close(fd)

    @contextmanager
    def _native_lock(self, task: Mapping[str, Any]):
        identity = str(task["session_id"])
        held = getattr(self._native_lock_state, "held", {})
        depth = held.get(identity, 0)
        if depth:
            held[identity] = depth + 1
            try:
                yield
            finally:
                held[identity] = depth
            return
        with self._object_lock("native:" + identity):
            held[identity] = 1
            self._native_lock_state.held = held
            try:
                yield
            finally:
                held.pop(identity, None)

    def dispatch(self, request: Mapping[str, Any]) -> dict[str, Any]:
        if not isinstance(request, Mapping):
            raise ControlError("invalid_request", "request must be an object")
        action = request.get("action")
        # Resolve timer ownership from the durable record, never from a guessed
        # resource name. The same object lock serializes native operations.
        session = request.get("session_id")
        if action == "timer-expire":
            task = self.store.get_task(task_id=str(request.get("task_id")))
            session = task["session_id"] if task else "missing-timer"
            if request.get("protocol") != PROTOCOL_VERSION:
                raise ControlError("protocol_mismatch", "unsupported timer protocol")
            self._expire_preempt(request)
        identity = "session:" + str(session) if action in self.SESSION_ACTIONS or action == "timer-expire" else "admin"
        with self._object_lock(identity):
            if action in self.WRITE_ACTIONS:
                op = require_nonempty_string(request.get("operation_id"), "operation_id", SESSION_ID_RE)
                with self._object_lock("operation:" + op):
                    return self._dispatch_locked(request)
            return self._dispatch_locked(request)

    def _expire_preempt(self, request: Mapping[str, Any]) -> None:
        """Close the admission fence and attempt withdrawal before a slow owner
        operation finishes. Port reuse still waits for the object lock and a
        second confirmed cleanup; late native completion cannot reopen a lease.
        """
        task_id = require_nonempty_string(request.get("task_id"), "task_id", TASK_ID_RE)
        generation = require_int(request.get("generation"), "generation", 1)
        with self._global_lock(), self.store.transaction():
            task = self.store.get_task(task_id=task_id)
            if not task or task["generation"] != generation: return
            expiry = parse_time(task.get("lease_expires_at"))
            if task["state"] not in {"active", "preparing"} or (expiry and expiry > self.now()): return
            inactive = parse_time(task.get("inactive_since")) or min(self.now(), expiry or self.now())
            task = self.store.update_task(task_id, state="closing", lease_expires_at=None,
                                         inactive_since=iso_time(inactive),
                                         cleanup_after=iso_time(inactive + dt.timedelta(seconds=self.config["cache_seconds"])))
        # Generation/ownership is checked on every native cleanup. Do not free
        # reservations here: the in-flight operation may still finish later.
        try:
            with self._object_lock("cleanup:" + str(task["session_id"])):
                current = self._cleanup_current(task)
                if current is None:
                    return
                errors, completed = self._cleanup_task(current)
                if errors:
                    self.store.update_task(
                        task_id,
                        last_error=json.dumps({"errors": errors, "completed": completed}),
                    )
        except ControlError as exc:
            self.store.update_task(task_id, last_error=json.dumps(exc.as_dict()))

    def _dispatch_locked(self, request: Mapping[str, Any]) -> dict[str, Any]:
        if not isinstance(request, Mapping):
            raise ControlError("invalid_request", "request must be an object")
        if request.get("protocol") != PROTOCOL_VERSION:
            raise ControlError("protocol_mismatch", f"protocol must be {PROTOCOL_VERSION}")
        action = require_nonempty_string(request.get("action"), "action")
        if action in self.SESSION_ACTIONS:
            session_id = require_nonempty_string(request.get("session_id"), "session_id", SESSION_ID_RE)
        else:
            session_id = request.get("session_id")
            if session_id is not None:
                session_id = require_nonempty_string(session_id, "session_id", SESSION_ID_RE)
        operation_id = request.get("operation_id")
        if action in self.WRITE_ACTIONS:
            operation_id = require_nonempty_string(operation_id, "operation_id", SESSION_ID_RE)
            digest = hashlib.sha256(json.dumps(dict(request), sort_keys=True).encode()).hexdigest()
            cached = self.store.operation(operation_id)
            if cached is not None:
                if cached.get("_request_digest") != digest:
                    raise ControlError("operation_conflict", "operation ID belongs to a different request")
                if cached.get("outcome") == "pending":
                    raise ControlError("operation_incomplete", "previous execution has an uncertain outcome; inspect status and reconcile before another mutation",
                                       {"operation_id": operation_id})
                if cached.get("outcome") == "error":
                    error = cached["error"]
                    raise ControlError(error["code"], error["message"], error.get("detail"))
                response = dict(cached)
                response.pop("_request_digest", None)
                response["server_time"] = iso_time(self.now())
                response["outcome"] = "replayed"
                if session_id:
                    task = self.store.get_task(session_id=session_id)
                    response["current_state"] = self._state_summary(task) if task else None
                    if action == "renew" and task and self._state_summary(task)["effective_lease"]:
                        self._schedule_expiry(task)
                return response
            if "generation" not in request:
                raise ControlError("invalid_request", "write request requires generation")
            require_int(request["generation"], "generation", 0)
            if action != "renew":
                self.store.record_operation(operation_id, action, session_id, request["generation"],
                                            {"outcome": "pending", "_request_digest": digest})
        try:
            result = self._dispatch_action(action, dict(request), session_id)
        except ControlError as exc:
            if operation_id and action in self.WRITE_ACTIONS:
                # A committed renew must remain replayable even if rearming its
                # native timer failed after the atomic deadline/journal commit.
                saved = self.store.operation(operation_id)
                if not saved or saved.get("outcome") != "success":
                    self.store.record_operation(operation_id, action, session_id, request.get("generation"),
                                                {"outcome": "error", "error": exc.as_dict(), "_request_digest": digest})
            if session_id is not None:
                self.store.audit(
                    action,
                    "error",
                    session_id=session_id,
                    generation=request.get("generation"),
                    detail=exc.as_dict(),
                )
            raise
        response = {
            "ok": True,
            "protocol": PROTOCOL_VERSION,
            "server_time": iso_time(self.now()),
            "action": action,
            "session_id": session_id,
            "generation": request.get("generation"),
            "operation_id": operation_id,
            "outcome": "success",
            "result": result,
        }
        if operation_id is not None and action in self.WRITE_ACTIONS and action != "renew":
            self.store.record_operation(
                operation_id,
                action,
                session_id,
                request.get("generation"),
                {**response, "_request_digest": digest},
            )
        if session_id is not None:
            self.store.audit(
                action,
                "ok",
                session_id=session_id,
                generation=request.get("generation"),
                detail=result,
            )
        return response

    def _dispatch_action(
        self, action: str, request: dict[str, Any], session_id: str | None
    ) -> dict[str, Any]:
        handlers: dict[str, Callable[..., dict[str, Any]]] = {
            "doctor": self.doctor,
            "status": self.status,
            "acquire": self.acquire,
            "renew": self.renew,
            "release": self.release,
            "destroy": self.destroy,
            "restart": self.restart,
            "resize": self.resize,
            "ssh-info": self.ssh_info,
            "port-add": self.port_add,
            "port-list": self.port_list,
            "port-remove": self.port_remove,
            "admin-status": self.admin_status,
            "drain-status": self.drain_status,
            "drain-enter": self.drain_enter,
            "drain-exit": self.drain_exit,
            "admin-session-list": self.admin_session_list,
            "admin-reconcile": self.admin_reconcile,
            "admin-gc": self.admin_gc,
            "timer-expire": self.timer_expire,
            "timer-age-gc": self.timer_age_gc,
            "timer-reconcile": self.timer_reconcile,
        }
        handler = handlers.get(action)
        if handler is None:
            raise ControlError("unknown_action", f"unknown action: {action}")
        return handler(request, session_id)

    def _task_for_session(self, session_id: str) -> dict[str, Any]:
        task = self.store.get_task(session_id=session_id)
        if task is None or task["state"] == "absent":
            raise ControlError("task_missing", "no VM lease exists for this session")
        return task

    def _check_generation(self, task: Mapping[str, Any], request: Mapping[str, Any]) -> None:
        generation = require_int(request.get("generation"), "generation", 0)
        if generation != int(task["generation"]):
            raise ControlError(
                "generation_mismatch",
                "request generation does not match current task generation",
                {"current_generation": int(task["generation"])},
            )

    def _base_task(self, session_id: str, generation: int) -> dict[str, Any]:
        now = iso_time(self.now())
        task_id = "avm-" + secrets.token_hex(8)
        return {
            "task_id": task_id,
            "session_id": session_id,
            "generation": generation,
            "state": "preparing",
            "instance_name": f"agent-vm-{task_id}",
            "vm_ip": None,
            "ssh_host_port": None,
            "lease_expires_at": None,
            "inactive_since": None,
            "cleanup_after": None,
            "created_at": now,
            "updated_at": now,
            "last_error": None,
            "image_fingerprint": self.config["image_fingerprint"],
        }

    def _ownership(self, task: Mapping[str, Any]) -> Mapping[str, Any]:
        info = self.incus.config_show(str(task["instance_name"]))
        config = info.get("config", {})
        expected = {"user.agent-vm.managed": "native-v1", "user.agent-vm.task": task["task_id"],
                    "user.agent-vm.session": task["session_id"], "user.agent-vm.generation": str(task["generation"])}
        generations = {str(task["generation"])}
        if task["state"] in {"preparing", "closing"} and task.get("previous_generation") is not None:
            generations.add(str(task["previous_generation"]))
        if not isinstance(config, Mapping) or any(config.get(key) != value for key, value in expected.items() if key != "user.agent-vm.generation") or config.get("user.agent-vm.generation") not in generations:
            raise ControlError("ownership_mismatch", "native instance identity differs from durable ownership; refusing mutation")
        return info

    def _native_operation_current(self, task: Mapping[str, Any]) -> dict[str, Any]:
        current = self.store.get_task(task_id=task["task_id"])
        expiry = parse_time(current.get("lease_expires_at")) if current else None
        if (
            current is None
            or int(current["generation"]) != int(task["generation"])
            or current["instance_name"] != task["instance_name"]
            or current["state"] not in {"preparing", "active"}
            or expiry is None
            or expiry <= self.now()
        ):
            raise ControlError("lease_expired", "native operation was revoked before its effect")
        return current

    def _start_instance(self, task: Mapping[str, Any]) -> None:
        with self._native_lock(task):
            current = self._native_operation_current(task)
            self._ownership(current)
            self.incus.start(current["instance_name"])

    def _stop_instance(self, task: Mapping[str, Any]) -> None:
        with self._native_lock(task):
            current = self._native_operation_current(task)
            self._ownership(current)
            self.incus.stop(current["instance_name"], force=True)

    def _state_summary(self, task: Mapping[str, Any]) -> dict[str, Any]:
        result = {
            "task_id": task["task_id"],
            "session_id": task["session_id"],
            "generation": int(task["generation"]),
            "state": task["state"],
            "instance_name": task["instance_name"],
            "vm_ip": task["vm_ip"],
            "ssh_host_port": task["ssh_host_port"],
            "lease_expires_at": task["lease_expires_at"],
            "inactive_since": task["inactive_since"],
            "cleanup_after": task["cleanup_after"],
        }
        if task.get("last_error"):
            result["last_error"] = json.loads(task["last_error"])
        result["ports"] = self.store.get_ports(task["task_id"])
        result["resources"] = json.loads(task.get("resources_json") or "{}")
        result["resource_reservation"] = json.loads(task.get("resource_reservation_json") or "{}")
        expiry = parse_time(task.get("lease_expires_at"))
        result["effective_lease"] = task["state"] == "active" and expiry is not None and expiry > self.now()
        return result

    def _runtime_summary(self, task: Mapping[str, Any]) -> dict[str, Any]:
        result = self._state_summary(task)
        result["guest_available"] = False
        if task["state"] == "absent":
            result["native_status"] = "absent"
            return result
        try:
            self._ownership(task)
            result["native_status"] = self.incus.instance_info(task["instance_name"])["status"]
            result["guest_available"] = result["effective_lease"] and result["native_status"].lower() == "running"
            if result["effective_lease"] and not result["guest_available"]:
                result["guest_error"] = {"code": "guest_unavailable", "cause": "unknown",
                                         "message": "effective lease has no running guest; inspect before explicit restart"}
        except ControlError as exc:
            result["native_status"] = "unverified"
            result["guest_error"] = exc.as_dict()
        # CT-wide OOM counters cannot attribute an event to this VM.
        return result

    @staticmethod
    def _requested_resources(request: Mapping[str, Any], base: Mapping[str, int], *, active_reuse: bool = False) -> dict[str, int]:
        target = dict(base)
        for key, minimum, maximum in (("cpu", 1, 16), ("memory_mib", 512, 65536), ("disk_gib", 20, 4096)):
            if request.get(key) is not None:
                target[key] = require_int(request[key], key, minimum, maximum)
        if not active_reuse and target["disk_gib"] < base["disk_gib"]:
            raise ControlError("resize_not_growth", "virtual disk cannot shrink", {"current": dict(base), "requested": target})
        return target

    def _ensure_capacity_verified(self) -> dict[str, Any]:
        """Measure only the authorized CT pool filesystem; never execute probes."""
        try:
            usage = shutil.disk_usage(str(self.config["storage_path"]))
        except OSError as exc:
            raise ControlError("capacity_unverified", "cannot measure CT Incus pool filesystem") from exc
        total, free = usage.total, usage.free
        if not isinstance(total, int) or not isinstance(free, int) or not 0 < total or not 0 <= free <= total:
            raise ControlError("capacity_unverified", "invalid CT filesystem capacity")
        layer = {"label": "ct-filesystem", "total": total, "free": free, "kind": "data"}
        layer["low"] = max(int(self.config.get("disk_low_bytes", 10 * 1024**3)), total * 15 // 100)
        layer["target"] = max(int(self.config.get("disk_target_bytes", 20 * 1024**3)), total * 25 // 100)
        if layer["target"] >= total or layer["low"] >= layer["target"]:
            raise ControlError("watermarks_unreachable", "configure reachable CT storage watermarks", layer)
        return {"scope": "ct-filesystem", "observed_at": iso_time(self.now()), "layers": [layer]}


    def _storage_headroom(self, request_cost: int = 0) -> dict[str, Any]:
        facts = self._ensure_capacity_verified()
        facts["request_cost"] = request_cost
        return facts

    def _space_sufficient(self, facts: Mapping[str, Any], cost: int, threshold: str,
                          exclude_task_id: str | None = None) -> bool:
        reserved = sum(int(task.get("reservation_bytes") or 0) for task in self.store.list_tasks()
                       if task["task_id"] != exclude_task_id)
        for layer in facts["layers"]:
            demand = cost + reserved
            if layer["free"] - demand < layer[threshold]: return False
        return True

    def _eligible_cache(self, snapshot: Mapping[str, Any]) -> dict[str, Any]:
        """Caller holds the session lock. Age selects candidates, not eligibility."""
        task = self.store.get_task(task_id=snapshot["task_id"])
        if task is None or task["state"] != "cached" or not task.get("inactive_since"):
            raise ControlError("cache_ineligible", "not an eligible stopped cache")
        if task["generation"] != snapshot["generation"] or task["session_id"] != snapshot["session_id"]:
            raise ControlError("cache_ineligible", "cache ownership or generation changed")
        if parse_time(task.get("lease_expires_at")) and parse_time(task["lease_expires_at"]) > self.now():
            raise ControlError("cache_ineligible", "cache still has an effective deadline")
        info = self._ownership(task)
        if any(device.get("type") == "proxy" for device in info.get("expanded_devices", {}).values()):
            raise ControlError("cache_ineligible", "cache still has native proxy mappings")
        info = self.incus.instance_info(task["instance_name"])
        if str(info.get("status", "")).lower() != "stopped" or self.store.get_ports(task["task_id"]):
            raise ControlError("cache_ineligible", "cache is running or has unreclaimed mappings")
        return task

    def _delete_cache(self, task: Mapping[str, Any]) -> None:
        """Caller owns the session lock; global lock covers only state transitions."""
        task = self._eligible_cache(task)
        with self._global_lock():
            self.store.update_task(task["task_id"], state="deleting")
        try:
            self.incus.delete(task["instance_name"])
            # Native deletion must be confirmed before releasing ownership.
            if any(item.get("name") == task["instance_name"] for item in self.incus.list_instances()):
                raise ControlError("cleanup_incomplete", "native deletion is not confirmed")
        except ControlError as exc:
            self.store.update_task(task["task_id"], last_error=json.dumps(exc.as_dict()))
            raise
        with self._global_lock():
            self.store.update_task(task["task_id"], state="absent", vm_ip=None, ssh_host_port=None,
                                   lease_expires_at=None, inactive_since=None, cleanup_after=None,
                                   reservation_bytes=0, last_error=None)


    def _reclaim_capacity(self, request_cost: int, exclude_task_id: str | None = None) -> list[str]:
        facts = self._storage_headroom(request_cost)
        with self._global_lock():
            if self._space_sufficient(facts, request_cost, "low", exclude_task_id):
                return []
        reclaimed = []
        candidates = sorted((task for task in self.store.list_tasks()
                             if task["state"] == "cached" and task.get("inactive_since")
                             and task["task_id"] != exclude_task_id),
                            key=lambda task: task["inactive_since"])
        for task in candidates:
            facts = self._storage_headroom(request_cost)
            with self._global_lock():
                if self._space_sufficient(facts, request_cost, "target", exclude_task_id): break
            try:
                with self._object_lock("session:" + task["session_id"], blocking=False):
                    self._delete_cache(task)
                reclaimed.append(task["task_id"])
            except ControlError as exc:
                if exc.code == "object_busy" or exc.code == "cache_ineligible": continue
                raise ControlError("reclaim_failed", "exact cache reclamation failed", exc.as_dict()) from exc
        facts = self._storage_headroom(request_cost)
        with self._global_lock():
            if not self._space_sufficient(facts, request_cost, "low", exclude_task_id):
                raise ControlError("capacity_insufficient", "capacity remains insufficient after eligible reclaim",
                                   {"storage": facts, "reclaimed": reclaimed})
        return reclaimed

    def _resource_policy(self) -> dict[str, Any]:
        cpu_budget = require_int(self.config.get("cpu_budget", 0), "cpu_budget", 1)
        memory_budget = require_int(self.config.get("memory_budget_mib", 0), "memory_budget_mib", 512)
        ratios = {"cpu": overcommit_ratio(self.config.get("cpu_overcommit_ratio", 4), "cpu_overcommit_ratio"),
                  "memory_mib": overcommit_ratio(self.config.get("memory_overcommit_ratio", 4), "memory_overcommit_ratio")}
        cpu_capacity, memory = self.resources.cpu(), self.resources.memory()
        base = {"cpu": cpu_budget, "memory_mib": memory_budget}
        capacity = {"cpu": cpu_capacity, "memory_mib": memory["limit_bytes"] // 1024**2}
        if any(base[key] > capacity[key] for key in base):
            raise ControlError("resource_budget_invalid", "base budget exceeds CT-visible capacity",
                               {"base": base, "ct_capacity": capacity})
        project = self.incus.query("/1.0/projects/" + quote(self.config["project"], safe=""), project="default")
        if not isinstance(project, Mapping) or not isinstance(project.get("config"), Mapping):
            raise ControlError("resource_unverified", "project resource configuration cannot be inspected")
        conflicts = {key: project["config"][key] for key in ("limits.cpu", "limits.memory") if project["config"].get(key)}
        if conflicts:
            raise ControlError("project_resource_limits_conflict", "project aggregate quotas count stopped caches; authorized deployment must remove them",
                               {"project": self.config["project"], "conflicting_limits": conflicts})
        count_limits = self._instance_count_limits(project["config"])
        if count_limits:
            raise ControlError("project_instance_limits_conflict", "project instance quotas include stopped caches; authorized deployment must remove them",
                               {"project": self.config["project"], "conflicting_limits": count_limits,
                                "instance_count": len(self.incus.list_instances())})
        return {"base": base, "ratios": {key: float(value) for key, value in ratios.items()},
                "limits": {key: int(Decimal(base[key]) * ratios[key]) for key in base},
                "ct_capacity": capacity, "memory": {**memory, "observed_at": iso_time(self.now())}}

    def _allocation_totals(self, exclude_task_id: str | None = None) -> dict[str, int]:
        totals = {"cpu": 0, "memory_mib": 0}
        for other in self.store.list_tasks():
            if other["task_id"] == exclude_task_id or other["state"] in {"absent", "cached"}: continue
            try:
                value, reservation = json.loads(other["resources_json"]), json.loads(other["resource_reservation_json"])
                for key in totals:
                    sizes = (value[key], reservation.get(key, 0))
                    if any(isinstance(size, bool) or not isinstance(size, int) or size < 0 for size in sizes): raise ValueError()
                    totals[key] += max(sizes)
            except (KeyError, TypeError, ValueError) as exc:
                raise ControlError("resource_unverified", "occupied object lacks verified resource sizes") from exc
        return totals

    def _running_inventory_guard(self) -> None:
        instances = self.incus.list_instances()
        records = {item["instance_name"]: item for item in self.store.list_tasks()}
        for instance in instances:
            if str(instance.get("status", "")).lower() != "stopped":
                record = records.get(instance.get("name"))
                if record is None or record["state"] in {"absent", "cached"}:
                    raise ControlError("resource_unverified", "unaccounted running native instance blocks admission")

    def _resource_diagnostics(self) -> dict[str, Any]:
        try:
            policy = self._resource_policy()
            self._running_inventory_guard()
            with self._global_lock(), self.store.transaction():
                allocated = self._allocation_totals()
            return {"verified": True, **policy, "allocated_and_reserved": allocated,
                    "remaining": {key: policy["limits"][key] - allocated[key] for key in allocated},
                    "memory_pressure": policy["memory"]["available_bytes"] < policy["memory"]["threshold_bytes"]}
        except ControlError as exc:
            return {"verified": False, "error": exc.as_dict()}

    @staticmethod
    def _instance_count_limits(config: Mapping[str, Any]) -> dict[str, Any]:
        # Container-only restrictions do not limit this VM backend.
        return {key: config[key] for key in ("limits.instances", "limits.virtual-machines")
                if config.get(key) not in (None, "")}

    def _instance_diagnostics(self) -> dict[str, Any]:
        try:
            project = self.incus.query("/1.0/projects/" + quote(self.config["project"], safe=""), project="default")
            if not isinstance(project, Mapping) or not isinstance(project.get("config"), Mapping):
                raise ControlError("resource_unverified", "project count configuration cannot be inspected")
            limits = self._instance_count_limits(project["config"])
            instances = self.incus.list_instances()
            running = sum(str(item.get("status", "")).lower() == "running" for item in instances)
            stopped = sum(str(item.get("status", "")).lower() == "stopped" for item in instances)
            cached_names = {task["instance_name"] for task in self.store.list_tasks() if task["state"] == "cached"}
            cached = sum(item.get("name") in cached_names and str(item.get("status", "")).lower() == "stopped" for item in instances)
            return {"verified": True, "total": len(instances), "running": running,
                    "stopped": stopped, "cached": cached, "other": len(instances) - running - stopped, "limits": limits,
                    "policy_compliant": not limits}
        except ControlError as exc:
            return {"verified": False, "error": exc.as_dict()}

    def _default_admission_diagnostics(self, resources: Mapping[str, Any], capacity: Mapping[str, Any],
                                       instances: Mapping[str, Any]) -> dict[str, Any]:
        """Read-only advisory for a default new guest, not an admission reservation."""
        requested = {"cpu": 2, "memory_mib": 4096, "disk_gib": 20}
        reasons = []
        if self.store.get_meta("draining") == "1":
            reasons.append({"code": "draining", "message": "new leases are refused"})
        if not resources.get("verified"):
            reasons.append(resources["error"])
        else:
            exceeded = [key for key in ("cpu", "memory_mib") if resources["remaining"][key] < requested[key]]
            if exceeded:
                reasons.append({"code": "resource_insufficient", "dimensions": exceeded,
                                "requested": requested, "remaining": resources["remaining"]})
            if resources["memory_pressure"]:
                reasons.append({"code": "memory_pressure", "memory": resources["memory"]})
        if not instances.get("verified"):
            reasons.append(instances["error"])
        elif not instances["policy_compliant"] and not any(reason["code"] == "project_instance_limits_conflict" for reason in reasons):
            reasons.append({"code": "project_instance_limits_conflict", "conflicting_limits": instances["limits"]})
        reclaim_required = False
        if "error" in capacity:
            reasons.append(capacity["error"])
        else:
            with self._global_lock(), self.store.transaction():
                reclaim_required = not self._space_sufficient(capacity, self.config["create_cost_bytes"], "low")
            if reclaim_required:
                reasons.append({"code": "disk_reclaim_required", "message": "default creation needs capacity reclamation"})
        return {"requested": requested, "request_cost_bytes": self.config["create_cost_bytes"],
                "ready": not reasons, "reasons": reasons, "reclaim_required": reclaim_required}

    def _reserve_resources(self, task: Mapping[str, Any], resources: Mapping[str, int],
                           *, memory_reduction: bool = False) -> dict[str, Any]:
        # Only a verified, memory-only reduction may bypass new-demand gates.
        # Its old allocation stays reserved until native completion is verified.
        if memory_reduction:
            old = json.loads(task["resources_json"])
            if (task["state"] != "active" or resources != old or json.loads(task["resource_reservation_json"])):
                raise ControlError("resize_unverified", "memory reduction requires an intact current allocation")
            policy = None
        else:
            policy = self._resource_policy()
            memory = policy["memory"]
            if memory["available_bytes"] < memory["threshold_bytes"]:
                raise ControlError("memory_pressure", "CT memory headroom is below the admission threshold", memory)
            self._running_inventory_guard()
        with self._global_lock(), self.store.transaction():
            if self.store.get_meta("draining") == "1":
                raise ControlError("draining", "new leases and capacity changes are refused while draining")
            current = self.store.get_task(task_id=task["task_id"])
            if task["state"] == "active":
                if current is None or current["generation"] != task["generation"]:
                    raise ControlError("generation_mismatch", "reservation generation changed")
                self._require_active(current)
            elif current is not None and (current["state"] not in {"absent", "cached"} or current["generation"] + 1 != task["generation"]):
                raise ControlError("generation_mismatch", "allocation slot changed before reservation")
            totals = self._allocation_totals(task["task_id"])
            projected = {key: totals[key] + resources[key] for key in totals}
            exceeded = [key for key in totals if projected[key] > policy["limits"][key]] if policy else []
            if exceeded:
                raise ControlError("resource_insufficient", "allocation ceiling exceeded: " + ", ".join(exceeded),
                                   {"dimensions": exceeded, "requested": dict(resources), "other_allocated": totals,
                                    "projected": projected, "base": policy["base"], "limits": policy["limits"]})
            record = dict(task, resource_reservation_json=json.dumps(dict(resources), sort_keys=True))
            if not record.get("resources_json"):
                record["resources_json"] = json.dumps(dict(resources), sort_keys=True)
            self.store.put_task(record)
            return record

    def _reserve_native(self, task: Mapping[str, Any], cost: int, *, inventory: set[int] | None = None,
                        ssh_port: int | None = None) -> dict[str, Any]:
        facts = self._storage_headroom(cost)
        native_ips = self._native_ips(task["instance_name"]) if inventory is not None else set()
        with self._global_lock(), self.store.transaction():
            current = self.store.get_task(task_id=task["task_id"])
            if current is None or current["generation"] != task["generation"] or current["state"] not in {"active", "preparing"}:
                raise ControlError("generation_mismatch", "reservation changed before native admission")
            expiry = parse_time(current.get("lease_expires_at"))
            if expiry is None or expiry <= self.now():
                raise ControlError("lease_expired", "lease expired before native admission")
            if self.store.get_meta("draining") == "1": raise ControlError("draining", "native admission is drained")
            if not self._space_sufficient(facts, cost, "low", task["task_id"]):
                raise ControlError("capacity_insufficient", "concurrent disk reservation consumed headroom")
            record = dict(current, reservation_bytes=cost)
            if inventory is not None:
                if task.get("vm_ip") in native_ips:
                    raise ControlError("vm_ip_busy", "cached VM address conflicts with native inventory")
                record["vm_ip"] = task.get("vm_ip") or self._allocate_vm_ip(native_ips)
                record["ssh_host_port"] = self._allocate_port("tcp", ssh_port, inventory=inventory)
            self.store.put_task(record)
            if inventory is not None:
                self.store.put_port(self._new_port(record, "ssh", "tcp", record["ssh_host_port"], 22))
            return record

    def _cancel_resource_reservation(self, previous: Mapping[str, Any]) -> None:
        # Safe only before native effects. Expiry may have closed this generation
        # concurrently, in which case cleanup retains its conservative record.
        with self._global_lock(), self.store.transaction():
            current = self.store.get_task(task_id=previous["task_id"])
            if current and current["generation"] == previous["generation"] and current["state"] == "active":
                self.store.update_task(previous["task_id"], resource_reservation_json=previous["resource_reservation_json"],
                                       reservation_bytes=previous["reservation_bytes"])

    def _new_port(self, task: Mapping[str, Any], name: str, protocol: str, host: int, guest: int) -> dict[str, Any]:
        return {"task_id": task["task_id"], "generation": task["generation"], "port_name": name,
                "protocol": protocol, "host_port": host, "vm_port": guest,
                "device_name": f"agent-vm-{task['task_id']}-{name}-{protocol}", "state": "preparing",
                "created_at": iso_time(self.now())}



    def _require_active(self, task: Mapping[str, Any]) -> None:
        if task["state"] != "active":
            raise ControlError("task_not_active", "the VM lease is not active")
        expiry = parse_time(task.get("lease_expires_at"))
        if expiry is None or expiry <= self.now():
            raise ControlError("lease_expired", "the VM lease has expired")

    def _native_resources(self, task: Mapping[str, Any]) -> dict[str, int]:
        info = self._ownership(task)
        config = info.get("expanded_config", info.get("config", {}))
        try:
            cpu = int(config["limits.cpu"])
            memory = re.fullmatch(r"(\d+)(MiB|GiB)", config["limits.memory"])
            disk = re.fullmatch(r"(\d+)GiB", info["expanded_devices"]["root"]["size"])
            if cpu < 1 or not memory or not disk: raise ValueError()
            return {"cpu": cpu, "memory_mib": int(memory[1]) * (1024 if memory[2] == "GiB" else 1), "disk_gib": int(disk[1])}
        except (ValueError, KeyError, TypeError) as exc:
            raise ControlError("resource_unverified", "native CPU, memory or root size cannot be verified") from exc

    def _host_ports(self) -> set[int]:
        result = self.runner.run(["ss", "-Hlnptu"], timeout=5)
        return parse_listen_ports(result.stdout.decode())


    def _native_ports(self) -> set[int]:
        ports: set[int] = set()
        projects = self.incus.query("/1.0/projects?recursion=1", project="default")
        if not isinstance(projects, list):
            raise ControlError("port_inventory_unverified", "project inventory is unavailable")
        for project in projects:
            name = require_nonempty_string(project.get("name"), "project name")
            instances = self.incus.query("/1.0/instances?recursion=1", project=name)
            networks = self.incus.query("/1.0/networks?recursion=1", project=name)
            if not isinstance(instances, list) or not isinstance(networks, list):
                raise ControlError("port_inventory_unverified", "native inventory is unavailable")
            for instance in instances:
                for device in instance.get("expanded_devices", {}).values():
                    if device.get("type") == "proxy":
                        listen = str(device.get("listen", ""))
                        if listen.startswith(("tcp:", "udp:")):
                            ports |= self._port_numbers(listen.rsplit(":", 1)[-1])
            for network in networks:
                if not network.get("managed") or network.get("type") != "bridge": continue
                forwards = self.incus.query("/1.0/networks/" + quote(network["name"], safe="") + "/forwards?recursion=1", project=name)
                if not isinstance(forwards, list):
                    raise ControlError("port_inventory_unverified", "network forward inventory is unavailable")
                for forward in forwards:
                    if forward.get("config", {}).get("target_address"):
                        ports.update(range(self.config["port_start"], self.config["port_end"] + 1))
                    for item in forward.get("ports", []):
                        ports |= self._port_numbers(str(item.get("listen_port", "")))
        return ports

    @staticmethod
    def _port_numbers(value: str) -> set[int]:
        result = set()
        for part in value.split(","):
            match = re.fullmatch(r"(\d+)(?:-(\d+))?", part.strip())
            if match is None:
                raise ControlError("port_inventory_unverified", "unrecognized native port range")
            start, end = int(match[1]), int(match[2] or match[1])
            if not 1 <= start <= end <= 65535:
                raise ControlError("port_inventory_unverified", "invalid native port range")
            result.update(range(start, end + 1))
        return result


    def _ephemeral_ports(self) -> set[int]:
        try:
            text = Path("/proc/sys/net/ipv4/ip_local_port_range").read_text().split()
            if len(text) != 2 or not 1 <= int(text[0]) <= int(text[1]) <= 65535: raise ValueError()
            return set(range(int(text[0]), int(text[1]) + 1))
        except (OSError, ValueError) as exc:
            raise ControlError("port_inventory_unverified", "cannot inspect host ephemeral port range") from exc

    def _port_inventory(self) -> set[int]:
        return self._host_ports() | self._native_ports() | self._ephemeral_ports()


    def _allocate_port(self, protocol: str, requested: Any = None, *, inventory: set[int]) -> int:
        if protocol not in {"tcp", "udp"}:
            raise ControlError("invalid_request", "protocol must be tcp or udp")
        used = self.store.occupied_ports(protocol) | inventory
        if requested is not None:
            port = require_int(requested, "host_port", self.config["port_start"], self.config["port_end"])
            if port in used: raise ControlError("port_busy", "requested host port is occupied")
            return port
        for port in range(self.config["port_start"], self.config["port_end"] + 1):
            if port not in used: return port
        raise ControlError("port_exhausted", "no host port is available")


    def _allocated_vm_ip(self) -> set[str]:
        return {
            str(task["vm_ip"])
            for task in self.store.list_tasks()
            if task.get("vm_ip") and task["state"] != "absent"
        }

    def _native_ips(self, own_name: str) -> set[str]:
        used = set()
        own_mac, own_ip = None, None
        projects = self.incus.query("/1.0/projects?recursion=1", project="default")
        for project in projects:
            for instance in self.incus.query("/1.0/instances?recursion=1", project=project["name"]):
                if project["name"] == self.config["project"] and instance.get("name") == own_name:
                    nic = instance.get("expanded_devices", {}).get("eth0", {})
                    own_mac = nic.get("hwaddr") or instance.get("config", {}).get("volatile.eth0.hwaddr")
                    own_ip = nic.get("ipv4.address")
                    continue
                for device in instance.get("expanded_devices", {}).values():
                    if self.config["network"] in {device.get("network"), device.get("parent")} and device.get("ipv4.address"):
                        used.add(device["ipv4.address"])
        leases = self.incus.query("/1.0/networks/" + quote(self.config["network"], safe="") + "/leases", project="default")
        if not isinstance(leases, list): raise ControlError("vm_ip_unverified", "network lease inventory is unavailable")
        for lease in leases:
            if own_mac and lease.get("hwaddr", "").lower() == own_mac.lower() and lease.get("address") == own_ip: continue
            if lease.get("address"): used.add(lease["address"])
        return used

    def _allocate_vm_ip(self, native_ips: set[str]) -> str:
        start = ipv4_to_int(str(self.config["vm_ip_start"]))
        end = ipv4_to_int(str(self.config["vm_ip_end"]))
        used = self._allocated_vm_ip() | native_ips
        for value in range(start, end + 1):
            candidate = int_to_ipv4(value)
            if candidate not in used:
                return candidate
        raise ControlError("vm_ip_exhausted", "no VM private IP is available")

    @staticmethod
    def _is_guest_wait_timeout(error: ControlError) -> bool:
        if isinstance(error, GuestWaitTimeout) or error.code == "guest_wait_timeout":
            return True
        detail = error.detail if isinstance(error.detail, Mapping) else {}
        stderr = detail.get("stderr", "")
        return (
            error.code == "command_failed"
            and detail.get("returncode") == 1
            and isinstance(stderr, str)
            and re.search(r"(?:timed?\s*out|timeout)", stderr, re.IGNORECASE) is not None
        )

    @staticmethod
    def _diagnostic_error(error: Exception) -> dict[str, Any]:
        detail: dict[str, Any] = {"type": type(error).__name__}
        if isinstance(error, ControlError):
            detail["code"] = error.code
            if error.code == "command_timeout" and isinstance(error.detail, Mapping):
                for key in ("reaped", "process_group_killed", "process_group_cleanup_error"):
                    if key in error.detail:
                        detail[key] = error.detail[key]
        return detail

    @staticmethod
    def _diagnostic_timeout(deadline_monotonic: float | None) -> float:
        if deadline_monotonic is None:
            return DIAGNOSTIC_COMMAND_TIMEOUT_SECONDS
        remaining = deadline_monotonic - time.monotonic()
        if remaining <= 0:
            raise ControlError("diagnostic_deadline", "shared readiness budget is exhausted")
        return min(float(DIAGNOSTIC_COMMAND_TIMEOUT_SECONDS), remaining)

    @staticmethod
    def _parse_cpu_psi(raw: str) -> dict[str, dict[str, float]]:
        result: dict[str, dict[str, float]] = {}
        for line in raw.splitlines():
            fields = line.split()
            if not fields or fields[0] not in {"some", "full"}:
                continue
            values: dict[str, float] = {}
            for field in fields[1:]:
                name, separator, value = field.partition("=")
                if separator:
                    try:
                        values[name] = float(value)
                    except ValueError:
                        continue
            if values:
                result[fields[0]] = values
        if not result:
            raise ValueError("CPU PSI data is missing")
        return result

    @staticmethod
    def _redact_startup_log(raw: str) -> dict[str, Any]:
        collector_truncated = raw.endswith(DIAGNOSTIC_LOG_TRUNCATION_MARKER)
        if collector_truncated:
            raw = raw[: -len(DIAGNOSTIC_LOG_TRUNCATION_MARKER)]
        private_key = re.compile(
            r"-----BEGIN [A-Z0-9 _-]*PRIVATE KEY-----.*?(?:-----END [A-Z0-9 _-]*PRIVATE KEY-----|$)",
            re.IGNORECASE | re.DOTALL,
        )
        redacted = private_key.sub("[REDACTED PRIVATE KEY]", raw)
        secret_label = (
            r"(?:[A-Za-z0-9]+[_-])*(?:password|passwd|passphrase|secret|token|"
            r"authorization|private[_ -]?key|api[_ -]?key|access[_ -]?key|"
            r"client[_ -]?secret|credential[s]?)(?:[_-][A-Za-z0-9]+)*"
        )
        quoted_assignment = re.compile(
            rf"(?i)(?P<label>[\"']?{secret_label}[\"']?)"
            rf"(?P<separator>\s*[:=]\s*)(?P<quote>[\"'])(?P<value>.*?)(?P=quote)"
        )
        unquoted_assignment = re.compile(
            rf"(?i)(?P<label>[\"']?{secret_label}[\"']?)"
            rf"(?P<separator>\s*[:=]\s*)(?P<value>[^\s,;}}\]]+)"
        )

        def redact_assignment(match: re.Match[str]) -> str:
            quote = match.groupdict().get("quote") or ""
            return f"{match.group('label')}{match.group('separator')}{quote}[REDACTED]{quote}"

        redacted = quoted_assignment.sub(redact_assignment, redacted)
        redacted = unquoted_assignment.sub(redact_assignment, redacted)
        # Shell password words may contain nested substitutions, process
        # substitutions, quoted newlines or continuations. Do not attempt to
        # reproduce shell parsing in a diagnostic redactor: hide the remaining
        # payload after the flag so no shell form can leak through.
        sshpass_argument = re.compile(
            r"(?is)(?P<prefix>\bsshpass\b[ \t]+-p)[ \t]*[\s\S]*"
        )
        redacted = sshpass_argument.sub(
            lambda match: match.group("prefix") + " [REDACTED]", redacted
        )
        redacted = re.sub(
            r"(?i)(\b[A-Za-z][A-Za-z0-9+.-]*://[^/\s:@]+:)([^@\s/]+)(@)",
            r"\1[REDACTED]\3",
            redacted,
        )
        redacted = re.sub(r"(?i)(bearer\s+)[A-Za-z0-9._~+/=-]+", r"\1[REDACTED]", redacted)
        truncated = collector_truncated or len(redacted) > DIAGNOSTIC_LOG_LIMIT
        if truncated:
            redacted = redacted[:DIAGNOSTIC_LOG_LIMIT]
        return {"text": redacted, "truncated": truncated}

    def _qemu_schedstat(self, pid: Any) -> dict[str, Any]:
        if isinstance(pid, bool) or not isinstance(pid, int) or pid < 2:
            raise ValueError("Incus did not report a valid QEMU pid")
        cmdline = Path(f"/proc/{pid}/cmdline").read_bytes().lower()
        if b"qemu" not in cmdline:
            raise ValueError("reported process is not QEMU")
        fields = Path(f"/proc/{pid}/schedstat").read_text().split()
        if len(fields) < 2 or not all(field.isdigit() for field in fields[:2]):
            raise ValueError("QEMU schedstat is invalid")
        return {
            "pid": pid,
            "runtime_seconds": int(fields[0]) / 1_000_000_000,
            "runqueue_wait_seconds": int(fields[1]) / 1_000_000_000,
        }

    def _emit_slow_start_diagnostic(
        self,
        task: Mapping[str, Any],
        *,
        phase: str,
        started_monotonic: float,
        readiness: Mapping[str, Any],
        deadline_monotonic: float | None = None,
    ) -> dict[str, Any]:
        """Emit bounded observation data without changing the native operation."""
        errors: list[dict[str, str]] = []
        payload: dict[str, Any] = {
            "event": "agent-vm-slow-start",
            "task_id": task["task_id"],
            "session_id": task["session_id"],
            "generation": task["generation"],
            "phase": phase,
            "elapsed_seconds": round(max(0.0, time.monotonic() - started_monotonic), 3),
            "budget_seconds": GUEST_READINESS_BUDGET_SECONDS,
            "stage_timings": dict(readiness.get("stage_timings", {})),
            "readiness": dict(readiness),
            "native_status": {},
            "cpu_psi": {},
            "qemu_schedstat": {},
            "startup_log": {},
        }
        try:
            info = self.incus.instance_info(
                task["instance_name"],
                timeout=self._diagnostic_timeout(deadline_monotonic),
                bounded=True,
            )
            payload["native_status"] = {
                key: info.get(key)
                for key in ("status", "pid", "processes")
                if key in info
            }
            try:
                payload["qemu_schedstat"] = self._qemu_schedstat(info.get("pid"))
            except Exception as error:
                errors.append({"source": "qemu_schedstat", **self._diagnostic_error(error)})
        except Exception as error:
            errors.append({"source": "native_status", **self._diagnostic_error(error)})
        try:
            payload["cpu_psi"] = self._parse_cpu_psi(Path("/proc/pressure/cpu").read_text())
        except Exception as error:
            errors.append({"source": "cpu_psi", **self._diagnostic_error(error)})
        try:
            payload["startup_log"] = self._redact_startup_log(
                self.incus.instance_log(
                    task["instance_name"],
                    timeout=self._diagnostic_timeout(deadline_monotonic),
                )
            )
        except Exception as error:
            errors.append({"source": "startup_log", **self._diagnostic_error(error)})
        if errors:
            payload["errors"] = errors
        try:
            self.store.audit(
                "boot-diagnostic",
                "partial" if errors else "ok",
                task_id=task["task_id"],
                session_id=task["session_id"],
                generation=task["generation"],
                detail=payload,
            )
        except Exception as error:
            payload.setdefault("errors", []).append(
                {"source": "audit", **self._diagnostic_error(error)}
            )
        try:
            sys.stderr.write(json.dumps(payload, sort_keys=True) + "\n")
            sys.stderr.flush()
        except Exception as error:
            payload.setdefault("errors", []).append(
                {"source": "stderr", **self._diagnostic_error(error)}
            )
        return payload

    def _readiness_timeout(
        self,
        condition: str,
        started_monotonic: float,
        readiness: dict[str, Any],
        diagnostic: Mapping[str, Any] | None,
    ) -> None:
        elapsed = round(max(0.0, time.monotonic() - started_monotonic), 3)
        readiness["elapsed_seconds"] = elapsed
        readiness["diagnostic"] = {
            "emitted": diagnostic is not None,
            "phase": diagnostic.get("phase") if diagnostic else None,
            "elapsed_seconds": diagnostic.get("elapsed_seconds") if diagnostic else None,
        }
        raise ControlError(
            "guest_readiness_timeout",
            "guest readiness did not complete within the shared startup budget",
            {
                "phase": condition,
                "condition": condition,
                "elapsed_seconds": elapsed,
                "budget_seconds": GUEST_READINESS_BUDGET_SECONDS,
                "readiness": readiness,
            },
        )

    def _wait_guest_ready(
        self,
        task: Mapping[str, Any],
        *,
        stage_timings: Mapping[str, Any] | None = None,
    ) -> dict[str, Any]:
        # Native bounded waits, not a control-owned polling loop or lease renewal.
        started_monotonic = time.monotonic()
        readiness: dict[str, Any] = {
            "budget_seconds": GUEST_READINESS_BUDGET_SECONDS,
            "elapsed_seconds": 0.0,
            "stage_timings": dict(stage_timings or {}),
            "phases": {},
        }
        diagnostic: list[Mapping[str, Any] | None] = [None]

        def remaining(condition: str) -> int:
            current = self.store.get_task(task_id=task["task_id"])
            expiry = parse_time(current.get("lease_expires_at")) if current else None
            if not current or current["generation"] != task["generation"] or current["state"] not in {"preparing", "active"} or not expiry:
                raise ControlError("lease_expired", "guest preparation was revoked")
            if expiry <= self.now():
                raise ControlError("lease_expired", "lease expired while waiting for guest readiness")
            elapsed = max(0.0, time.monotonic() - started_monotonic)
            seconds = int(GUEST_READINESS_BUDGET_SECONDS - elapsed)
            if seconds < 1:
                self._readiness_timeout(condition, started_monotonic, readiness, diagnostic[0])
            lease_seconds = (expiry - self.now()).total_seconds()
            if lease_seconds < 1:
                raise ControlError("lease_expired", "lease expired while waiting for guest readiness")
            return min(seconds, int(lease_seconds))

        def wait_once(condition: str, seconds: int) -> None:
            self.incus.wait(task["instance_name"], condition, seconds)
            remaining(condition)

        def fail_fast(condition: str, error: ControlError) -> None:
            detail = dict(error.detail) if isinstance(error.detail, Mapping) else {"cause": error.as_dict()}
            detail.update({"phase": condition, "readiness": readiness})
            raise ControlError(error.code, error.message, detail) from error

        for condition in ("agent", "ipv4"):
            phase_started = time.monotonic()
            if diagnostic[0] is None:
                first_seconds = min(
                    remaining(condition),
                    max(1, int(GUEST_READINESS_DIAGNOSTIC_SECONDS - max(0.0, time.monotonic() - started_monotonic))),
                )
            else:
                first_seconds = remaining(condition)
            try:
                wait_once(condition, first_seconds)
            except ControlError as error:
                if not self._is_guest_wait_timeout(error):
                    fail_fast(condition, error)
                if diagnostic[0] is None and time.monotonic() - started_monotonic < GUEST_READINESS_DIAGNOSTIC_SECONDS:
                    boundary_seconds = min(
                        remaining(condition),
                        max(1, int(GUEST_READINESS_DIAGNOSTIC_SECONDS - (time.monotonic() - started_monotonic))),
                    )
                    try:
                        wait_once(condition, boundary_seconds)
                    except ControlError as boundary_error:
                        if not self._is_guest_wait_timeout(boundary_error):
                            fail_fast(condition, boundary_error)
                    else:
                        readiness["phases"][condition] = {
                            "elapsed_seconds": round(max(0.0, time.monotonic() - phase_started), 3),
                            "total_elapsed_seconds": round(max(0.0, time.monotonic() - started_monotonic), 3),
                        }
                        continue
                if diagnostic[0] is None:
                    diagnostic[0] = self._emit_slow_start_diagnostic(
                        task,
                        phase=condition,
                        started_monotonic=started_monotonic,
                        readiness=readiness,
                        deadline_monotonic=started_monotonic + GUEST_READINESS_BUDGET_SECONDS,
                    )
                try:
                    wait_once(condition, remaining(condition))
                except ControlError as continuation_error:
                    if self._is_guest_wait_timeout(continuation_error):
                        self._readiness_timeout(condition, started_monotonic, readiness, diagnostic[0])
                    fail_fast(condition, continuation_error)
            readiness["phases"][condition] = {
                "elapsed_seconds": round(max(0.0, time.monotonic() - phase_started), 3),
                "total_elapsed_seconds": round(max(0.0, time.monotonic() - started_monotonic), 3),
            }
        readiness["elapsed_seconds"] = round(max(0.0, time.monotonic() - started_monotonic), 3)
        readiness["diagnostic"] = {
            "emitted": diagnostic[0] is not None,
            "phase": diagnostic[0].get("phase") if diagnostic[0] else None,
            "elapsed_seconds": diagnostic[0].get("elapsed_seconds") if diagnostic[0] else None,
        }
        return readiness

    def _verify_guest_tools(self, instance_name: str) -> None:
        tools = [str(item) for item in self.config["required_guest_tools"]]
        script = (
            "set -eu; for tool in "
            + " ".join(shlex.quote(tool) for tool in tools)
            + "; do command -v \"$tool\" >/dev/null; done; "
            + "if docker compose version >/dev/null 2>&1; then :; "
            + "elif command -v docker-compose >/dev/null 2>&1; then :; "
            + "else exit 1; fi"
        )
        self.incus.exec(instance_name, ["sh", "-lc", script])

    def _guest_ip(self, instance_name: str) -> str:
        info = self.incus.instance_info(instance_name)
        state = info.get("state", info)
        network = state.get("network", {}) if isinstance(state, dict) else {}
        candidates: list[str] = []
        if isinstance(network, dict):
            for name, data in network.items():
                if name == "lo" or not isinstance(data, dict):
                    continue
                addresses = data.get("addresses", [])
                if not isinstance(addresses, list):
                    continue
                for address in addresses:
                    if isinstance(address, dict) and address.get("family") == "inet":
                        value = address.get("address")
                        if isinstance(value, str):
                            candidates.append(value)
        if not candidates:
            raise ControlError("guest_ip_missing", "Incus did not report a guest IPv4 address")
        expected = self._expected_vm_ip
        if expected not in candidates:
            raise ControlError(
                "guest_ip_mismatch",
                "guest IPv4 address does not match the allocated fixed address",
                {"expected": expected, "reported": candidates},
            )
        return expected

    def _install_public_key(self, instance_name: str, public_key: str) -> None:
        public_key = require_nonempty_string(public_key, "public_key")
        if "\n" in public_key or "\r" in public_key:
            raise ControlError("invalid_request", "public_key must contain one line")
        fields = public_key.split()
        if len(fields) < 2 or fields[0] not in {"ssh-ed25519", "ecdsa-sha2-nistp256", "rsa-sha2-512", "ssh-rsa"}:
            raise ControlError("invalid_request", "public_key is not a supported SSH public key")
        script = (
            "set -eu; umask 077; mkdir -p /root/.ssh; "
            "cat > /root/.ssh/authorized_keys; chmod 600 /root/.ssh/authorized_keys"
        )
        self.incus.exec(instance_name, ["sh", "-lc", script], input_data=public_key + "\n")

    def _guest_host_key(self, instance_name: str) -> str:
        result = self.incus.exec(
            instance_name,
            ["sh", "-lc", "cat /etc/ssh/ssh_host_ed25519_key.pub"],
        )
        key = result.stdout.decode("utf-8", "replace").strip()
        fields = key.split()
        if len(fields) < 2 or fields[0] != "ssh-ed25519":
            raise ControlError("guest_host_key_missing", "guest did not provide an ed25519 host key")
        return " ".join(fields[:2])

    def _configure_network(self, instance_name: str, vm_ip: str) -> None:
        for key, value in {"ipv4.address": vm_ip, "security.mac_filtering": "true",
                           "security.ipv4_filtering": "true", "security.ipv6_filtering": "true",
                           "security.port_isolation": "true", "security.acls": self.config["network_acl"],
                           "security.acls.default.ingress.action": "allow", "security.acls.default.egress.action": "allow"}.items():
            self.incus.device_set(instance_name, "eth0", key, value)

    def _network_guard(self) -> None:
        name = require_nonempty_string(self.config.get("network_acl"), "network_acl", PORT_NAME_RE)
        project = self.incus.query("/1.0/projects/" + quote(self.config["project"], safe=""), project="default")
        project_config = project.get("config", {}) if isinstance(project, Mapping) else {}
        if project_config.get("restricted.devices.proxy") != "allow":
            raise ControlError("network_unverified", "Incus project does not allow managed proxy NAT devices")
        acl = self.incus.query("/1.0/network-acls/" + quote(name, safe=""), project="default")
        if acl.get("config", {}).get("user.agent-vm.managed") != "native-v1":
            raise ControlError("network_unverified", "isolation ACL ownership is not verified")
        required = self._isolation_rules()
        fields = ("action", "state", "source", "destination", "protocol", "source_port", "destination_port", "icmp_type", "icmp_code")
        for direction, rules in required.items():
            normalized = [{key: value.get(key, "enabled" if key == "state" else "") for key in fields}
                          for value in acl.get(direction, [])]
            for rule in rules:
                expected = {key: rule.get(key, "enabled" if key == "state" else "") for key in fields}
                if expected not in normalized:
                    raise ControlError("network_unverified", "isolation ACL lacks required peer/host withdrawal rules")

    def _isolation_rules(self) -> dict[str, list[dict[str, str]]]:
        start, end = self.config["vm_ip_start"], self.config["vm_ip_end"]
        return {"ingress": [{"action": "drop", "state": "enabled", "source": f"{start}-{end}"}],
                "egress": [{"action": "drop", "state": "enabled", "destination": f"{start}-{end}"},
                           {"action": "drop", "state": "enabled", "destination": self.config["host_address"]},
                           {"action": "drop", "state": "enabled", "destination": self.config["gateway_ip"], "protocol": "tcp", "destination_port": "22"}]}


    def _add_proxy(self, task: Mapping[str, Any], port: Mapping[str, Any]) -> None:
        with self._native_lock(task):
            current = self._native_operation_current(task)
            native = self._ownership(current).get("devices", {}).get(port["device_name"])
            expected = self._proxy_properties(current, port)
            if native is not None:
                if any(native.get(key) != value for key, value in expected.items()):
                    raise ControlError("ownership_mismatch", "native device does not match reservation")
                return
            self.incus.device_add(current["instance_name"], port["device_name"], "proxy",
                                  {key: value for key, value in expected.items() if key != "type"})
            native = self._ownership(current).get("devices", {}).get(port["device_name"], {})
            if any(native.get(key) != value for key, value in expected.items()):
                raise ControlError("mapping_unverified", "native proxy addition was not confirmed")


    def _conntrack_delete(self, port: Mapping[str, Any], task: Mapping[str, Any]) -> None:
        if shutil.which("conntrack") is None:
            raise ControlError("conntrack_unavailable", "conntrack-tools is required for exact flow withdrawal")
        match = ["-p", str(port["protocol"]), "--orig-dst", str(self.config["host_address"]),
                 "--dport", str(port["host_port"]), "--reply-src", str(task["vm_ip"]),
                 "--reply-port-src", str(port["vm_port"])]
        result = self.runner.run(["conntrack", "-D", *match], check=False, timeout=2)
        if result.returncode != 0 and not (result.returncode == 1 and b"0 flow entries" in result.stderr):
            raise ControlError("conntrack_delete_failed", "exact conntrack deletion failed",
                               {"returncode": result.returncode, "stderr": result.stderr.decode(errors="replace")})
        remaining = self.runner.run(["conntrack", "-L", *match], check=False, timeout=2)
        if remaining.returncode or remaining.stdout.strip():
            raise ControlError("conntrack_unverified", "matching flows remain or cannot be inspected")


    def _cleanup_current(self, task: Mapping[str, Any]) -> dict[str, Any] | None:
        with self._global_lock(), self.store.transaction():
            current = self.store.get_task(task_id=task["task_id"])
            if (
                current is None
                or int(current["generation"]) != int(task["generation"])
                or current["state"] != "closing"
                or current["instance_name"] != task["instance_name"]
            ):
                return None
            return current

    def _cleanup_task(self, task: Mapping[str, Any]) -> tuple[list[str], list[str]]:
        # Serialize all native withdrawal with late start and proxy operations.
        with self._native_lock(task):
            return self._cleanup_task_locked(task)

    def _cleanup_task_locked(self, task: Mapping[str, Any]) -> tuple[list[str], list[str]]:
        errors, completed = [], []
        current = self._cleanup_current(task)
        if current is None:
            return ["stale-cleanup:generation"], completed
        task = current
        try:
            instances = self.incus.list_instances()
            exists = any(item.get("name") == task["instance_name"] for item in instances)
        except ControlError as exc:
            errors.append("inventory:" + exc.code)
            exists = True
        if exists:
            try: self._ownership(task)
            except ControlError as exc: return ["ownership:" + exc.code], []
        for port in self.store.get_ports(task["task_id"]):
            current = self._cleanup_current(task)
            if current is None:
                errors.append("stale-cleanup:generation")
                break
            task = current
            try:
                if exists: self._remove_proxy(task, port)
            except ControlError as exc:
                errors.append("remove-proxy:" + port["device_name"] + ":" + exc.code)
            current = self._cleanup_current(task)
            if current is None:
                errors.append("stale-cleanup:generation")
                break
            task = current
            try:
                self._conntrack_delete(port, task)
                completed.append("withdraw:" + port["device_name"])
            except ControlError as exc:
                errors.append("withdraw:" + port["device_name"] + ":" + exc.code)
        # A mapping or conntrack failure must never suppress the stop attempt.
        if exists:
            current = self._cleanup_current(task)
            if current is None:
                errors.append("stop:stale-cleanup")
            else:
                task = current
                try:
                    self._ownership(task)
                    if self.incus.instance_info(task["instance_name"])["status"].lower() != "stopped":
                        self.incus.stop(task["instance_name"], force=True)
                    if self.incus.instance_info(task["instance_name"])["status"].lower() != "stopped":
                        raise ControlError("stop_unverified", "native instance is not stopped")
                    completed.append("verify-stop")
                except ControlError as exc: errors.append("stop:" + exc.code)
        else:
            completed.append("instance-absent")
        return errors, completed

    def _proxy_properties(self, task: Mapping[str, Any], port: Mapping[str, Any]) -> dict[str, str]:
        return {"type": "proxy", "listen": f"{port['protocol']}:{self.config['host_address']}:{port['host_port']}",
                "connect": f"{port['protocol']}:{task['vm_ip']}:{port['vm_port']}", "nat": "true"}

    def _remove_proxy(self, task: Mapping[str, Any], port: Mapping[str, Any]) -> None:
        info = self._ownership(task)
        devices = info.get("devices", {})
        native = devices.get(port["device_name"])
        if native is not None:
            expected = self._proxy_properties(task, port)
            if any(native.get(key) != value for key, value in expected.items()):
                raise ControlError("ownership_mismatch", "native mapping differs from its reservation")
            self.incus.device_remove(task["instance_name"], port["device_name"])
        after = self._ownership(task)
        if port["device_name"] in after.get("expanded_devices", after.get("devices", {})):
            raise ControlError("mapping_unverified", "native mapping is still present")


    def _schedule_expiry(self, task: Mapping[str, Any]) -> None:
        expires = task.get("lease_expires_at")
        if not expires:
            raise ControlError("lease_missing", "active task has no lease expiry")
        self.systemd.schedule_expiry(str(task["task_id"]), int(task["generation"]), str(expires))

    def _image_guard(self) -> None:
        fingerprint = str(self.config["image_fingerprint"])
        try:
            image = self.incus.query("/1.0/images/" + quote(fingerprint, safe=""))
        except ControlError as exc:
            raise ControlError("image_unavailable", "pinned VM image is unavailable", exc.as_dict()) from exc
        if not isinstance(image, Mapping) or image.get("fingerprint") != fingerprint or image.get("type") != "virtual-machine":
            raise ControlError("image_unverified", "pinned image identity or VM type does not match")

    def doctor(self, request: Mapping[str, Any], session_id: str | None) -> dict[str, Any]:
        issues = []
        try:
            version = self.runner.run(["incus", "version"], timeout=15).stdout.decode().strip()
            self.incus.list_instances()
        except ControlError as exc:
            issues.append(exc.code)
            version = None
        try: self._image_guard()
        except ControlError as exc: issues.append(exc.code)
        try:
            capacity = self._ensure_capacity_verified()
        except ControlError as exc:
            issues.append(exc.code)
            capacity = {"error": exc.as_dict()}
        if shutil.which("conntrack") is None: issues.append("conntrack_missing")
        try: self._network_guard()
        except ControlError as exc: issues.append(exc.code)
        if self.systemd.enabled:
            for unit in ("incus.service", "agent-vm-native-age-gc.timer"):
                result = self.runner.run(["systemctl", "is-active", "--quiet", unit], timeout=5, check=False)
                if result.returncode: issues.append("unit_inactive:" + unit)
            for unit in ("agent-vm-native-expire@.service", "agent-vm-native-reconcile.service"):
                if not (self.systemd.timer_dir / unit).is_file(): issues.append("unit_missing:" + unit)
        resources = self._resource_diagnostics()
        instance_policy = self._instance_diagnostics()
        admission = self._default_admission_diagnostics(resources, capacity, instance_policy)
        return {"ready": not issues, "issues": issues, "incus_version": version, "capacity": capacity,
                "resources": resources, "instances": instance_policy, "default_admission": admission,
                "admission_ready": not issues and admission["ready"],
                "config": {key: self.config[key] for key in ("project", "profile", "image_fingerprint", "network", "pool", "host_address")}}


    def status(self, request: Mapping[str, Any], session_id: str | None) -> dict[str, Any]:
        assert session_id is not None
        task = self.store.get_task(session_id=session_id)
        result = self._runtime_summary(task) if task else {"state": "absent", "generation": 0, "effective_lease": False,
                                                         "native_status": "absent", "guest_available": False}
        admission = self._resource_diagnostics()
        admission["instances"] = self._instance_diagnostics()
        return {**result, "admission": admission}


    def acquire(self, request: Mapping[str, Any], session_id: str | None) -> dict[str, Any]:
        assert session_id is not None
        existing = self.store.get_task(session_id=session_id)
        if existing and existing["state"] == "active":
            self._check_generation(existing, request)
            self._require_active(existing)
            self._ownership(existing)
            resources = json.loads(existing["resources_json"])
            target = self._requested_resources(request, resources, active_reuse=True)
            if target != resources:
                raise ControlError("resource_config_mismatch", "active lease configuration differs; use resize explicitly",
                                   {"current": resources, "requested": target,
                                    "differences": [key for key in resources if resources[key] != target[key]]})
            return self._runtime_summary(existing)
        if existing and existing["state"] not in {"absent", "cached"}:
            raise ControlError("task_transition", "session slot is not available until reconciliation completes")
        expected = existing["generation"] if existing else 0
        if request["generation"] != expected:
            raise ControlError("generation_mismatch", "acquire requires the latest observed generation", {"current_generation": expected})
        if self.store.get_meta("draining") == "1": raise ControlError("draining", "new leases are refused")
        self._network_guard()
        public_key = require_nonempty_string(request.get("public_key"), "public_key")
        if not re.fullmatch(r"ssh-ed25519 [A-Za-z0-9+/=]+(?: [^\r\n]*)?", public_key):
            raise ControlError("invalid_request", "acquire requires a single ed25519 public key")
        cached = bool(existing and existing["state"] == "cached")
        if not cached:
            self._image_guard()
        if cached:
            self._ownership(existing)
            if self.incus.instance_info(existing["instance_name"])["status"].lower() != "stopped" or self.store.get_ports(existing["task_id"]):
                raise ControlError("cache_ineligible", "cached instance is not safely stopped")
        old = json.loads(existing["resources_json"]) if cached else {"cpu": 2, "memory_mib": 4096, "disk_gib": 20}
        if cached and self._native_resources(existing) != old:
            raise ControlError("resource_unverified", "cached native sizes differ from durable resource sizes")
        resources = self._requested_resources(request, old)
        cost = int(self.config["resume_cost_bytes"] if cached else self.config["create_cost_bytes"])
        if cached and resources["disk_gib"] > old["disk_gib"]: cost += int(self.config["resize_cost_bytes"])
        task = self._base_task(session_id, expected + 1)
        if existing:
            task.update(task_id=existing["task_id"], instance_name=existing["instance_name"], created_at=existing["created_at"],
                        vm_ip=existing["vm_ip"] if cached else None)
        if cached:
            task.update(previous_generation=expected, inactive_since=existing["inactive_since"], cleanup_after=existing["cleanup_after"])
        task["resources_json"] = json.dumps(old if cached else resources)
        # The deadline/timer exists before either starting the guest or exposing NAT.
        task["lease_expires_at"] = iso_time(self.now() + dt.timedelta(seconds=self.config["initial_lease_seconds"]))
        reserved = {key: max(old[key], resources[key]) for key in old} if cached else resources
        task = self._reserve_resources(task, reserved)
        completed = []
        phase = "reservation"
        stage_timings: dict[str, Any] = {}
        readiness = None
        try:
            phase = "expiry-timer"
            self._schedule_expiry(task)
            reclaimed = self._reclaim_capacity(cost, task["task_id"])
            task = self._reserve_native(task, cost, inventory=self._port_inventory(), ssh_port=request.get("ssh_host_port"))
            if cached:
                phase = "native-generation"
                self.incus.config_set(task["instance_name"], "user.agent-vm.generation", str(task["generation"]))
            else:
                phase = "native-init"
                metadata = {"user.agent-vm.managed": "native-v1", "user.agent-vm.task": task["task_id"],
                            "user.agent-vm.session": session_id, "user.agent-vm.generation": str(task["generation"])}
                self.incus.init(self.config["image_fingerprint"], task["instance_name"], metadata)
                self._configure_network(task["instance_name"], task["vm_ip"])
            phase = "native-config"
            for key, native_key, suffix in (("cpu", "limits.cpu", ""), ("memory_mib", "limits.memory", "MiB")):
                if not cached or resources[key] != old[key]:
                    self.incus.config_set(task["instance_name"], native_key, str(resources[key]) + suffix)
                    completed.append(key)
            if not cached or resources["disk_gib"] != old["disk_gib"]:
                self.incus.device_set(task["instance_name"], "root", "size", f"{resources['disk_gib']}GiB")
                completed.append("virtual_disk")
            self._ownership(task)
            if self._native_resources(task) != resources:
                raise ControlError("resource_unverified", "native sizes differ from the session reservation")
            phase = "native-start"
            native_started = time.monotonic()
            self._start_instance(task)
            stage_timings["native_start_seconds"] = round(max(0.0, time.monotonic() - native_started), 3)
            phase = "guest-readiness"
            readiness = self._wait_guest_ready(task, stage_timings=stage_timings)
            phase = "guest-post-check"
            post_check_started = time.monotonic()
            self._expected_vm_ip = task["vm_ip"]
            self._verify_guest_tools(task["instance_name"])
            self._guest_ip(task["instance_name"])
            guest = self._grow_guest_root(task, resources["disk_gib"])
            completed.append("guest_filesystem")
            self._install_public_key(task["instance_name"], public_key)
            stage_timings["post_check_seconds"] = round(max(0.0, time.monotonic() - post_check_started), 3)
            phase = "access-exposure"
            if parse_time(task["lease_expires_at"]) <= self.now(): raise ControlError("lease_expired", "lease expired during preparation")
            port = self.store.get_port(task["task_id"], "ssh", "tcp")
            self._add_proxy(task, port)
            if parse_time(task["lease_expires_at"]) <= self.now(): raise ControlError("lease_expired", "lease expired while exposing SSH")
            with self._global_lock(), self.store.transaction():
                current = self.store.get_task(task_id=task["task_id"])
                if current["state"] != "preparing" or parse_time(current["lease_expires_at"]) <= self.now():
                    raise ControlError("lease_expired", "preparation was revoked before commit")
                self.store.update_port(task["task_id"], "ssh", "tcp", state="active")
                task = self.store.update_task(task["task_id"], state="active", reservation_bytes=0, previous_generation=None,
                                              resource_reservation_json='{}', resources_json=json.dumps(resources),
                                              inactive_since=None, cleanup_after=None, last_error=None)
            return {**self._runtime_summary(task), "reclaimed": reclaimed, "guest_filesystem": guest,
                    "readiness": readiness, "stage_timings": stage_timings}
        except ControlError as exc:
            cleanup = {"ok": False, "error": {"code": "not_attempted", "message": "cleanup was not attempted"}}
            try:
                cleanup["result"] = self._release_task(self.store.get_task(task_id=task["task_id"]))
                cleanup["ok"] = True
            except ControlError as cleanup_error:
                cleanup = {"ok": False, "error": cleanup_error.as_dict()}
            detail = dict(exc.detail) if isinstance(exc.detail, Mapping) else {"cause": exc.as_dict()}
            failure_phase = detail.get("phase", phase)
            detail.update({"phase": failure_phase, "operation_phase": phase, "completed": completed,
                           "requested": resources, "cleanup": cleanup})
            if readiness is not None:
                detail["readiness"] = readiness
            error = {"code": "acquire_partial", **detail, "error": exc.as_dict()}
            self.store.update_task(task["task_id"], last_error=json.dumps(error))
            raise ControlError(exc.code, exc.message, detail) from exc


    def renew(self, request: Mapping[str, Any], session_id: str | None) -> dict[str, Any]:
        assert session_id is not None
        with self._global_lock(), self.store.transaction():
            task = self._task_for_session(session_id)
            self._check_generation(task, request)
            self._require_active(task)
            if self.store.get_meta("draining") == "1": raise ControlError("draining", "extensions are refused while draining")
            expiry = min(parse_time(task["lease_expires_at"]) + dt.timedelta(seconds=self.config["renew_seconds"]),
                         self.now() + dt.timedelta(seconds=self.config["max_lease_seconds"]))
            task = self.store.update_task(task["task_id"], lease_expires_at=iso_time(expiry))
            result = self._state_summary(task)
            response = {"ok": True, "protocol": PROTOCOL_VERSION, "server_time": iso_time(self.now()),
                        "action": "renew", "session_id": session_id, "generation": request["generation"],
                        "operation_id": request["operation_id"], "outcome": "success", "result": result,
                        "_request_digest": hashlib.sha256(json.dumps(dict(request), sort_keys=True).encode()).hexdigest()}
            self.store.record_operation(request["operation_id"], "renew", session_id, request["generation"], response)
        self._schedule_expiry(task)
        return result


    def release(self, request: Mapping[str, Any], session_id: str | None) -> dict[str, Any]:
        assert session_id is not None
        task = self._task_for_session(session_id)
        self._check_generation(task, request)
        return self._release_task(task)

    def _release_task(self, task: Mapping[str, Any]) -> dict[str, Any]:
        requested_state = task.get("state")
        with self._object_lock("cleanup:" + str(task["session_id"])):
            current = self.store.get_task(task_id=task["task_id"])
            if current is None or int(current["generation"]) != int(task["generation"]):
                raise ControlError("generation_mismatch", "cleanup generation changed")
            task = current
            if task["state"] in {"cached", "absent"} and requested_state != "closing":
                return self._state_summary(task)
            expiry = parse_time(task.get("lease_expires_at"))
            inactive = parse_time(task.get("inactive_since")) or min(self.now(), expiry or self.now())
            task = self.store.update_task(task["task_id"], state="closing", lease_expires_at=None,
                                         inactive_since=iso_time(inactive),
                                         cleanup_after=iso_time(inactive + dt.timedelta(seconds=self.config["cache_seconds"])))
            errors, completed = self._cleanup_task(task)
            if errors:
                self.store.update_task(task["task_id"], last_error=json.dumps({"errors": errors, "completed": completed}))
                # Reuse the native expiry oneshot's bounded Restart=on-failure path.
                self.systemd.retry_cleanup(task["task_id"], task["generation"])
                raise ControlError("cleanup_incomplete", "entry/port reservations remain closed until reconciliation succeeds", errors)
            self.systemd.remove_expiry(task["task_id"], task["generation"])
            absent = not any(item.get("name") == task["instance_name"] for item in self.incus.list_instances())
            if not absent and task.get("previous_generation") is not None:
                self._ownership(task)
                self.incus.config_set(task["instance_name"], "user.agent-vm.generation", str(task["generation"]))
            resources = self._native_resources(task) if not absent else {}
            with self._global_lock(), self.store.transaction():
                for port in self.store.get_ports(task["task_id"]):
                    self.store.remove_port(task["task_id"], port["port_name"], port["protocol"])
                task = self.store.update_task(task["task_id"], state="absent" if absent else "cached",
                                             ssh_host_port=None, reservation_bytes=0, last_error=None, previous_generation=None, resource_reservation_json='{}',
                                             resources_json=json.dumps(resources),
                                             vm_ip=None if absent else task["vm_ip"])
            return self._state_summary(task)


    def destroy(self, request: Mapping[str, Any], session_id: str | None) -> dict[str, Any]:
        assert session_id is not None
        task = self.store.get_task(session_id=session_id)
        if task is None: raise ControlError("task_missing", "no owned instance exists")
        self._check_generation(task, request)
        if task["state"] == "absent": return self._state_summary(task)
        if task["state"] == "deleting":
            raise ControlError("task_transition", "deletion requires reconciliation")
        if task["state"] != "cached":
            self._release_task(task)
            task = self.store.get_task(task_id=task["task_id"])
        if task["state"] == "cached": self._delete_cache(task)
        return self._state_summary(self.store.get_task(task_id=task["task_id"]))


    def restart(self, request: Mapping[str, Any], session_id: str | None) -> dict[str, Any]:
        assert session_id is not None
        task = self._task_for_session(session_id)
        self._check_generation(task, request)
        self._require_active(task)
        if self.store.get_meta("draining") == "1":
            raise ControlError("draining", "restart work is refused while draining")
        self._ownership(task)
        self._require_window(task)
        phase = "native-stop"
        stage_timings: dict[str, Any] = {}
        readiness = None
        try:
            self._stop_instance(task)
            phase = "native-start"
            native_started = time.monotonic()
            self._start_instance(task)
            stage_timings["native_start_seconds"] = round(max(0.0, time.monotonic() - native_started), 3)
            phase = "guest-readiness"
            readiness = self._wait_guest_ready(task, stage_timings=stage_timings)
            phase = "guest-post-check"
            post_check_started = time.monotonic()
            self._expected_vm_ip = task["vm_ip"]
            self._guest_ip(task["instance_name"])
            self._verify_guest_tools(task["instance_name"])
            stage_timings["post_check_seconds"] = round(max(0.0, time.monotonic() - post_check_started), 3)
            with self._global_lock(), self.store.transaction():
                current = self.store.get_task(task_id=task["task_id"])
                self._check_generation(current, request)
                self._require_active(current)
                task = current
            return {**self._state_summary(task), "readiness": readiness, "stage_timings": stage_timings}
        except ControlError as exc:
            cleanup = {"ok": False, "error": {"code": "not_attempted", "message": "cleanup was not attempted"}}
            try:
                cleanup["result"] = self._release_task(self.store.get_task(task_id=task["task_id"]))
                cleanup["ok"] = True
            except ControlError as cleanup_error:
                cleanup = {"ok": False, "error": cleanup_error.as_dict()}
            detail = dict(exc.detail) if isinstance(exc.detail, Mapping) else {"cause": exc.as_dict()}
            failure_phase = detail.get("phase", phase)
            detail.update({"phase": failure_phase, "operation_phase": phase, "cleanup": cleanup})
            if readiness is not None:
                detail["readiness"] = readiness
            self.store.update_task(
                task["task_id"],
                last_error=json.dumps({"code": "restart_partial", **detail, "error": exc.as_dict()}),
            )
            raise ControlError(exc.code, exc.message, detail) from exc

    def _require_window(self, task: Mapping[str, Any], *, minimum_seconds: int = GUEST_READINESS_BUDGET_SECONDS) -> None:
        # Do not begin a slow native start while its expiry is imminent. Native
        # operations are bounded and the timer owns the same object lock.
        if (parse_time(task["lease_expires_at"]) - self.now()).total_seconds() < minimum_seconds:
            raise ControlError("lease_window_short", "explicitly renew before starting a potentially slow operation")


    def resize(self, request: Mapping[str, Any], session_id: str | None) -> dict[str, Any]:
        assert session_id is not None
        task = self._task_for_session(session_id)
        self._check_generation(task, request)
        self._require_active(task)
        self._ownership(task)
        old = json.loads(task["resources_json"])
        values = {"cpu": request.get("cpu"), "memory_mib": request.get("memory_mib"), "disk_gib": request.get("disk_gib")}
        if all(value is None for value in values.values()): raise ControlError("invalid_request", "resize requires at least one resource")
        target = self._requested_resources(request, old)
        if not isinstance(request.get("live_memory", False), bool) or not isinstance(request.get("allow_restart", False), bool):
            raise ControlError("invalid_request", "resize mode flags must be booleans")
        if request.get("live_memory", False):
            if values["memory_mib"] is None or values["cpu"] is not None or values["disk_gib"] is not None or request.get("allow_restart", False):
                raise ControlError("invalid_request", "live resize requires memory only and forbids restart allowance")
            return self._resize_live_memory(task, request, old, target)
        # Native VM root growth becomes guest-visible after restart.
        # Report that interruption before any reservation, reclaim or mutation.
        previous_reservation = json.loads(task["resource_reservation_json"])
        recovering = bool(previous_reservation)
        needs_restart = target != old or recovering
        if needs_restart and not request.get("allow_restart", False):
            return {"applied": False, "plan": "restart-required-for-resource-change", "requested": target, **self._state_summary(task)}
        if needs_restart and self.store.get_meta("draining") == "1":
            raise ControlError("draining", "restart work is refused while draining")
        self._require_window(task)
        cost = self.config["resize_cost_bytes"] if target["disk_gib"] > old["disk_gib"] else 0
        reclaimed = []
        if needs_restart:
            reserved = {key: max(old[key], target[key], previous_reservation.get(key, 0)) for key in old}
            self._reserve_resources(task, reserved)
            try:
                reclaimed = self._reclaim_capacity(cost, task["task_id"])
                self._reserve_native(task, cost)
            except ControlError:
                self._cancel_resource_reservation(task)
                raise
        completed = []
        guest = None
        phase = "native-stop"
        stage_timings: dict[str, Any] = {}
        readiness = None
        try:
            if needs_restart: self._stop_instance(task)
            phase = "native-config"
            for key, native_key, suffix in (("cpu", "limits.cpu", ""), ("memory_mib", "limits.memory", "MiB")):
                if target[key] != old[key] or recovering:
                    self.incus.config_set(task["instance_name"], native_key, str(target[key]) + suffix)
                    completed.append(key)
            if target["disk_gib"] > old["disk_gib"]:
                self.incus.device_set(task["instance_name"], "root", "size", f"{target['disk_gib']}GiB")
                info = self._ownership(task)
                if info.get("expanded_devices", {}).get("root", {}).get("size") != f"{target['disk_gib']}GiB":
                    raise ControlError("resize_unverified", "native root size does not match request")
                completed.append("virtual_disk")
            if needs_restart:
                phase = "native-start"
                native_started = time.monotonic()
                self._start_instance(task)
                stage_timings["native_start_seconds"] = round(max(0.0, time.monotonic() - native_started), 3)
                phase = "guest-readiness"
                readiness = self._wait_guest_ready(task, stage_timings=stage_timings)
            phase = "guest-post-check"
            post_check_started = time.monotonic()
            if self._native_resources(task) != target:
                raise ControlError("resize_unverified", "native resource sizes differ from the requested sizes")
            self._require_active(task)
            self._expected_vm_ip = task["vm_ip"]
            self._guest_ip(task["instance_name"])
            if values["disk_gib"] is not None:
                guest = self._grow_guest_root(task, target["disk_gib"])
                completed.append("guest_filesystem")
            stage_timings["post_check_seconds"] = round(max(0.0, time.monotonic() - post_check_started), 3)
            with self._global_lock(), self.store.transaction():
                current = self.store.get_task(task_id=task["task_id"])
                self._check_generation(current, request)
                self._require_active(current)
                task = self.store.update_task(task["task_id"], resources_json=json.dumps(target), reservation_bytes=0,
                                              resource_reservation_json='{}', last_error=None)
            return {"applied": True, "completed": completed, "guest_filesystem": guest, "reclaimed": reclaimed,
                    "readiness": readiness, "stage_timings": stage_timings, **self._state_summary(task)}
        except ControlError as exc:
            # Native changes cannot be rolled back safely (especially disk growth).
            # Retain conservative reservations and report every observed stage.
            cleanup = {"performed": needs_restart, "ok": True}
            if needs_restart:
                try:
                    cleanup["result"] = self._release_task(self.store.get_task(task_id=task["task_id"]))
                except ControlError as cleanup_error:
                    cleanup = {"performed": True, "ok": False, "error": cleanup_error.as_dict()}
            detail = dict(exc.detail) if isinstance(exc.detail, Mapping) else {"cause": exc.as_dict()}
            detail.update({"phase": phase, "completed": completed, "requested": target, "cleanup": cleanup})
            if readiness is not None:
                detail["readiness"] = readiness
            stored = {"code": "resize_partial", **detail, "error": exc.as_dict()}
            self.store.update_task(task["task_id"], last_error=json.dumps(stored))
            raise ControlError("resize_partial", "resize did not complete; inspect native and guest sizes before retrying", detail) from exc

    def _guest_boot_id(self, task: Mapping[str, Any]) -> str:
        state = self.incus.query(f"/1.0/instances/{quote(task['instance_name'], safe='')}/state")
        if not isinstance(state, Mapping) or str(state.get("status", "")).lower() != "running":
            raise ControlError("guest_unavailable", "live memory adjustment requires a running guest")
        result = self.incus.exec(task["instance_name"], ["cat", "/proc/sys/kernel/random/boot_id"], timeout=15)
        try:
            return str(uuid.UUID(result.stdout.decode().strip()))
        except (ValueError, UnicodeError) as exc:
            raise ControlError("resize_unverified", "guest boot identity is unavailable") from exc

    def _resize_live_memory(self, task: Mapping[str, Any], request: Mapping[str, Any],
                            old: Mapping[str, int], target: Mapping[str, int]) -> dict[str, Any]:
        self._require_window(task, minimum_seconds=180)
        if json.loads(task["resource_reservation_json"]):
            raise ControlError("resize_incomplete", "a previous partial resize requires inspection and explicit restart recovery or release")
        if self._native_resources(task) != old:
            raise ControlError("resize_unverified", "native configuration differs from the recorded allocation")
        before = self._guest_boot_id(task)
        if target == old:
            return {"applied": False, "mode": "live-memory", "plan": "unchanged", **self._state_summary(task)}
        reserved = {key: max(old[key], target[key]) for key in old}
        self._reserve_resources(task, reserved, memory_reduction=target["memory_mib"] < old["memory_mib"])
        completed = []
        try:
            # Recheck expiry's lock-independent fence immediately before the native
            # effect. Incus itself performs bounded balloon/hotplug verification.
            current = self.store.get_task(task_id=task["task_id"])
            self._check_generation(current, request)
            self._require_active(current)
        except ControlError:
            self._cancel_resource_reservation(task)
            raise
        attempts = []
        def apply_memory() -> None:
            try:
                self.incus.config_set(task["instance_name"], "limits.memory", f"{target['memory_mib']}MiB")
            except ControlError as error:
                attempts.append({"ok": False, "error": error.as_dict()})
                raise
            attempts.append({"ok": True})
        try:
            try:
                apply_memory()
            except ControlError as first_error:
                # Incus 7.5.1 can time out its five-second balloon check while
                # the guest continues converging. Only this exact native error
                # permits one same-target completion attempt, never a poll loop.
                first_detail = first_error.detail if isinstance(first_error.detail, Mapping) else {}
                stderr = first_detail.get("stderr", "")
                expected = rf"Error: Failed updating memory limit: Failed setting memory to {target['memory_mib']}MiB \(currently (?P<current>\d+)MiB\) as it was taking too long"
                shrinking_timeout = re.fullmatch(expected, stderr.strip()) if isinstance(stderr, str) else None
                if (first_error.code != "command_failed" or first_detail.get("returncode") != 1
                        or target["memory_mib"] >= old["memory_mib"] or shrinking_timeout is None
                        or int(shrinking_timeout["current"]) <= target["memory_mib"]):
                    raise
                current = self.store.get_task(task_id=task["task_id"])
                self._check_generation(current, request)
                self._require_active(current)
                self._require_window(current, minimum_seconds=MEMORY_SETTLE_SECONDS + 60)
                # Reapplying an already persisted target might be a native
                # config no-op, which cannot prove effective memory completion.
                if (json.loads(current["resource_reservation_json"]) != reserved
                        or self._native_resources(task) != old or self._guest_boot_id(task) != before):
                    raise ControlError("resize_unverified", "native rollback or guest continuity is unverified after balloon timeout")
                self._reserve_resources(task, reserved, memory_reduction=target["memory_mib"] < old["memory_mib"])
                # Large balloons keep converging after Incus's five-second
                # check. One bounded settle interval precedes the only retry.
                # Expiry can still preempt this object and withdraw access.
                time.sleep(MEMORY_SETTLE_SECONDS)
                current = self.store.get_task(task_id=task["task_id"])
                self._check_generation(current, request)
                self._require_active(current)
                self._require_window(current, minimum_seconds=60)
                if (json.loads(current["resource_reservation_json"]) != reserved
                        or self._native_resources(task) != old or self._guest_boot_id(task) != before):
                    raise ControlError("resize_unverified", "native rollback, reservation or guest continuity changed during balloon settlement")
                self._reserve_resources(task, reserved, memory_reduction=True)
                apply_memory()
            completed.append("native_memory_update")
            if self._native_resources(task) != target:
                raise ControlError("resize_unverified", "native memory configuration does not match the request")
            after = self._guest_boot_id(task)
            if after != before:
                raise ControlError("resize_unverified", "guest boot identity changed during live adjustment")
            completed.append("guest_continuity")
            with self._global_lock(), self.store.transaction():
                current = self.store.get_task(task_id=task["task_id"])
                self._check_generation(current, request)
                self._require_active(current)
                task = self.store.update_task(task["task_id"], resources_json=json.dumps(target),
                                              resource_reservation_json='{}', last_error=None)
            return {"applied": True, "mode": "live-memory", "completed": completed, "native_attempts": attempts,
                    "verification": {"source": "incus-live-update", "configured_memory_mib": target["memory_mib"],
                                     "guest_boot_id": after}, **self._state_summary(task)}
        except ControlError as exc:
            detail = {"code": "resize_partial", "mode": "live-memory", "completed": completed,
                      "requested": dict(target), "native_attempts": attempts, "error": exc.as_dict()}
            # A timed-out balloon can already have changed native memory. Never
            # assume rollback, lower accounting, restart, or stop an active lease.
            with self._global_lock(), self.store.transaction():
                current = self.store.get_task(task_id=task["task_id"])
                if current and current["generation"] == task["generation"]:
                    self.store.update_task(task["task_id"], last_error=json.dumps(detail))
            raise ControlError("resize_partial", "live memory adjustment is unverified; conservative allocation retained, no restart fallback", detail) from exc

    def _grow_guest_root(self, task: Mapping[str, Any], disk_gib: int) -> dict[str, Any]:
        script = r"""
import json, os, subprocess, sys

def run(args):
    return subprocess.run(args, check=True, capture_output=True, text=True, timeout=45).stdout
source = run(['findmnt', '-n', '-o', 'SOURCE', '/']).strip()
source = os.path.realpath(source)
fstype = run(['findmnt', '-n', '-o', 'FSTYPE', '/']).strip()
name = os.path.basename(source)
partition = '/sys/class/block/' + name + '/partition'
if not os.path.isfile(partition) or fstype not in ('ext4', 'xfs'):
    raise RuntimeError('automatic growth supports a direct ext4/xfs root partition only')
number = open(partition).read().strip()
parent = run(['lsblk', '-n', '-o', 'PKNAME', source]).strip()
if not parent or '/' in parent: raise RuntimeError('ambiguous root disk')
disk = '/dev/' + parent
size = int(run(['blockdev', '--getsize64', disk]).strip())
if size < int(sys.argv[1]) * 1024**3: raise RuntimeError('guest virtual disk size is stale')
before = os.statvfs('/').f_blocks * os.statvfs('/').f_frsize
grown = subprocess.run(['growpart', disk, number], capture_output=True, text=True, timeout=45)
if grown.returncode and not (grown.returncode == 1 and 'NOCHANGE' in grown.stdout):
    raise RuntimeError('growpart failed: ' + grown.stderr)
run(['resize2fs', source] if fstype == 'ext4' else ['xfs_growfs', '/'])
after = os.statvfs('/').f_blocks * os.statvfs('/').f_frsize
if after <= before and after < size * 0.85: raise RuntimeError('root filesystem did not grow')
print(json.dumps({'virtual_bytes': size, 'filesystem_before': before, 'filesystem_bytes': after, 'filesystem': fstype}))
"""
        result = self.incus.exec(task["instance_name"], ["python3", "-c", script, str(disk_gib)], timeout=120)
        try:
            value = json.loads(result.stdout)
            if not isinstance(value, dict) or value["virtual_bytes"] < disk_gib * 1024**3: raise ValueError()
            return value
        except (ValueError, KeyError, TypeError) as exc:
            raise ControlError("guest_resize_unverified", "guest capacity report is invalid") from exc


    def ssh_info(self, request: Mapping[str, Any], session_id: str | None) -> dict[str, Any]:
        assert session_id is not None
        task = self._task_for_session(session_id)
        self._require_active(task)
        self._ownership(task)
        port = self.store.get_port(task["task_id"], "ssh", "tcp")
        if not port or port["state"] != "active" or port["generation"] != task["generation"]:
            raise ControlError("mapping_unverified", "managed SSH reservation is not active")
        host_key = self._guest_host_key(str(task["instance_name"]))
        self._require_active(self.store.get_task(task_id=task["task_id"]))
        alias = f"agent-vm-{task['task_id']}-g{task['generation']}"
        return {
            "task_id": task["task_id"],
            "generation": int(task["generation"]),
            "host": self.config["host_address"],
            "port": int(task["ssh_host_port"]),
            "user": "root",
            "host_key_alias": alias,
            "host_key": host_key,
        }

    def port_add(self, request: Mapping[str, Any], session_id: str | None) -> dict[str, Any]:
        assert session_id is not None
        task = self._task_for_session(session_id)
        self._check_generation(task, request)
        self._require_active(task)
        self._ownership(task)
        name = require_nonempty_string(request.get("name"), "name", PORT_NAME_RE)
        if name == "ssh": raise ControlError("invalid_request", "ssh is reserved")
        protocol = request.get("port_protocol", "tcp")
        if protocol not in {"tcp", "udp"}: raise ControlError("invalid_request", "unsupported port protocol")
        guest = require_int(request.get("vm_port"), "vm_port", 1, 65535)
        existing = self.store.get_port(task["task_id"], name, protocol)
        if existing:
            if existing["state"] != "active": raise ControlError("cleanup_incomplete", "port reservation is transitional")
            if existing["vm_port"] != guest or request.get("host_port", existing["host_port"]) != existing["host_port"]:
                raise ControlError("port_exists", "existing mapping has different settings")
            with self._global_lock(), self.store.transaction():
                current = self.store.get_task(task_id=task["task_id"])
                self._check_generation(current, request)
                self._require_active(current)
                task = current
            try:
                self._add_proxy(task, existing)
                with self._global_lock(), self.store.transaction():
                    current = self.store.get_task(task_id=task["task_id"])
                    self._check_generation(current, request)
                    self._require_active(current)
                return self.store.get_port(task["task_id"], name, protocol)
            except ControlError:
                current = self.store.get_task(task_id=task["task_id"])
                same_generation = bool(
                    current
                    and int(current["generation"]) == int(existing["generation"])
                    and int(existing["generation"]) == int(task["generation"])
                )
                stale = not same_generation or not current or current["state"] != "active"
                if same_generation and current and current["state"] == "active":
                    expiry = parse_time(current.get("lease_expires_at"))
                    stale = expiry is None or expiry <= self.now()
                if stale and same_generation:
                    try:
                        with self._native_lock(task):
                            current = self.store.get_task(task_id=task["task_id"])
                            if (
                                current is not None
                                and int(current["generation"]) == int(existing["generation"])
                                and current["state"] != "absent"
                            ):
                                current_port = self.store.get_port(task["task_id"], name, protocol)
                                if current_port is None or int(current_port["generation"]) == int(existing["generation"]):
                                    cleanup_port = current_port or existing
                                    self._remove_proxy(task, cleanup_port)
                                    self._conntrack_delete(cleanup_port, task)
                                    if current_port:
                                        self.store.remove_port(task["task_id"], name, protocol)
                    except ControlError:
                        current_port = self.store.get_port(task["task_id"], name, protocol)
                        if current_port and int(current_port["generation"]) == int(existing["generation"]):
                            self.store.update_port(task["task_id"], name, protocol, state="closing")
                raise
        inventory = self._port_inventory()
        with self._global_lock(), self.store.transaction():
            self._require_active(self.store.get_task(task_id=task["task_id"]))
            host = self._allocate_port(protocol, request.get("host_port"), inventory=inventory)
            port = self._new_port(task, name, protocol, host, guest)
            self.store.put_port(port)
        try:
            self._add_proxy(task, port)
            with self._global_lock(), self.store.transaction():
                current = self.store.get_task(task_id=task["task_id"])
                self._check_generation(current, request)
                self._require_active(current)
                self.store.update_port(task["task_id"], name, protocol, state="active")
            return self.store.get_port(task["task_id"], name, protocol)
        except ControlError:
            try:
                with self._native_lock(task):
                    current = self.store.get_task(task_id=task["task_id"])
                    if current and int(current["generation"]) == int(port["generation"]):
                        self._remove_proxy(task, port)
                        self._conntrack_delete(port, task)
                        current_port = self.store.get_port(task["task_id"], name, protocol)
                        if current_port and int(current_port["generation"]) == int(port["generation"]):
                            self.store.remove_port(task["task_id"], name, protocol)
            except ControlError:
                current_port = self.store.get_port(task["task_id"], name, protocol)
                if current_port and int(current_port["generation"]) == int(port["generation"]):
                    self.store.update_port(task["task_id"], name, protocol, state="closing")
            raise


    def port_list(self, request: Mapping[str, Any], session_id: str | None) -> dict[str, Any]:
        assert session_id is not None
        task = self._task_for_session(session_id)
        self._check_generation(task, request)
        self._require_active(task)
        return {"task_id": task["task_id"], "ports": self.store.get_ports(task["task_id"])}

    def port_remove(self, request: Mapping[str, Any], session_id: str | None) -> dict[str, Any]:
        assert session_id is not None
        task = self._task_for_session(session_id)
        self._check_generation(task, request)
        self._require_active(task)
        name = require_nonempty_string(request.get("name"), "name", PORT_NAME_RE)
        protocol = request.get("port_protocol", "tcp")
        if name == "ssh" or protocol not in {"tcp", "udp"}: raise ControlError("invalid_request", "invalid project mapping")
        port = self.store.get_port(task["task_id"], name, protocol)
        if port:
            self.store.update_port(task["task_id"], name, protocol, state="closing")
            with self._native_lock(task):
                self._remove_proxy(task, port)
                self._conntrack_delete(port, task)
                self.store.remove_port(task["task_id"], name, protocol)
        return {"task_id": task["task_id"], "name": name, "protocol": protocol, "removed": True}


    def admin_status(self, request: Mapping[str, Any], session_id: str | None) -> dict[str, Any]:
        return {"draining": self.store.get_meta("draining") == "1",
                "tasks": [self._runtime_summary(task) for task in self.store.list_tasks()],
                "doctor": self.doctor(request, session_id)}


    def drain_status(self, request: Mapping[str, Any], session_id: str | None) -> dict[str, Any]:
        return {"draining": self.store.get_meta("draining") == "1"}

    def drain_enter(self, request: Mapping[str, Any], session_id: str | None) -> dict[str, Any]:
        with self._global_lock(): self.store.set_meta("draining", "1")
        return {"draining": True}


    def drain_exit(self, request: Mapping[str, Any], session_id: str | None) -> dict[str, Any]:
        with self._global_lock(): self.store.set_meta("draining", "0")
        return {"draining": False}


    def admin_session_list(self, request: Mapping[str, Any], session_id: str | None) -> dict[str, Any]:
        return {"tasks": [self._runtime_summary(task) for task in self.store.list_tasks()]}

    def admin_reconcile(self, request: Mapping[str, Any], session_id: str | None) -> dict[str, Any]:
        apply = request.get("apply", False)
        if not isinstance(apply, bool):
            raise ControlError("invalid_request", "reconcile apply must be a JSON boolean")
        actions = []
        for snapshot in self.store.list_tasks():
            try:
                with self._object_lock("session:" + snapshot["session_id"], blocking=False):
                    task = self.store.get_task(task_id=snapshot["task_id"])
                    if task["state"] == "absent": continue
                    item = {"task_id": task["task_id"], "action": "inspect"}
                    if task["state"] == "deleting":
                        item["action"] = "finish-delete"
                        if apply:
                            if parse_time(task.get("lease_expires_at")) and parse_time(task["lease_expires_at"]) > self.now():
                                raise ControlError("cache_ineligible", "deletion retry has an effective deadline")
                            exists = any(native.get("name") == task["instance_name"] for native in self.incus.list_instances())
                            if exists:
                                owned = self._ownership(task)
                                if any(device.get("type") == "proxy" for device in owned.get("expanded_devices", {}).values()):
                                    raise ControlError("cache_ineligible", "deletion retry still has native proxy mappings")
                                if self.incus.instance_info(task["instance_name"])["status"].lower() != "stopped" or self.store.get_ports(task["task_id"]):
                                    raise ControlError("cache_ineligible", "deletion retry is not safely stopped")
                                self.incus.delete(task["instance_name"])
                            if any(native.get("name") == task["instance_name"] for native in self.incus.list_instances()):
                                raise ControlError("cleanup_incomplete", "deletion is not confirmed")
                            with self._global_lock():
                                self.store.update_task(task["task_id"], state="absent", vm_ip=None, ssh_host_port=None,
                                                       lease_expires_at=None, inactive_since=None, cleanup_after=None,
                                                       reservation_bytes=0, resource_reservation_json="{}", last_error=None)
                    elif task["state"] in {"closing", "preparing", "failed"} or (task["state"] == "active" and not self._state_summary(task)["effective_lease"]):
                        item["action"] = "close-incomplete-or-expired"
                        if apply: self._release_task(task)
                    else:
                        self._ownership(task)
                        status = self.incus.instance_info(task["instance_name"])["status"].lower()
                        if task["state"] == "cached" and status != "stopped":
                            item["action"] = "stop-unexpected-running-cache"
                            if apply: self._release_task(dict(task, state="closing"))
                        elif task["state"] == "active":
                            item["action"] = "rearm-current-expiry"
                            if status != "running": raise ControlError("state_mismatch", "active guest is not running")
                            if apply:
                                self._schedule_expiry(task)
                                for port in self.store.get_ports(task["task_id"]):
                                    if port["state"] == "closing":
                                        with self._native_lock(task):
                                            self._remove_proxy(task, port)
                                            self._conntrack_delete(port, task)
                                            self.store.remove_port(task["task_id"], port["port_name"], port["protocol"])
                                    elif port["state"] == "preparing":
                                        # A crash before port-add acknowledgement is closed,
                                        # never silently reopened during reconciliation.
                                        with self._native_lock(task):
                                            self._remove_proxy(task, port)
                                            self._conntrack_delete(port, task)
                                            self.store.remove_port(task["task_id"], port["port_name"], port["protocol"])
                    actions.append(item)
            except ControlError as exc:
                actions.append({"task_id": snapshot["task_id"], "error": exc.as_dict()})
        return {"dry_run": not apply, "actions": actions}


    def admin_gc(self, request: Mapping[str, Any], session_id: str | None, *, age_filtered: bool = False) -> dict[str, Any]:
        apply = request.get("apply", False)
        if not isinstance(apply, bool):
            raise ControlError("invalid_request", "GC apply must be a JSON boolean")
        candidates = []
        for snapshot in self.store.list_tasks():
            try:
                with self._object_lock("session:" + snapshot["session_id"], blocking=False):
                    task = self.store.get_task(task_id=snapshot["task_id"])
                    if task is None or task["state"] != "cached" or not task["inactive_since"]: continue
                    if task["generation"] != snapshot["generation"]: continue
                    if age_filtered and parse_time(task["inactive_since"]) + dt.timedelta(seconds=self.config["cache_seconds"]) > self.now(): continue
                    task = self._eligible_cache(task)
                    candidate = {"task_id": task["task_id"], "instance_name": task["instance_name"],
                                 "generation": task["generation"], "inactive_since": task["inactive_since"]}
                    if apply: self._delete_cache(task)
                    candidates.append(candidate)
            except ControlError as exc:
                if exc.code in {"cache_ineligible", "object_busy"}: continue
                candidates.append({"task_id": snapshot["task_id"], "error": exc.as_dict()})
        return {"dry_run": not apply, "selection": "age" if age_filtered else "eligible",
                "minimum_idle_seconds": self.config["cache_seconds"] if age_filtered else 0, "candidates": candidates}


    def timer_expire(self, request: Mapping[str, Any], session_id: str | None) -> dict[str, Any]:
        task_id = require_nonempty_string(request.get("task_id"), "task_id", TASK_ID_RE)
        generation = require_int(request.get("generation"), "generation", 1)
        task = self.store.get_task(task_id=task_id)
        if task is None or int(task["generation"]) != generation:
            return {"task_id": task_id, "reason": "missing" if task is None else "stale-generation", "action": "noop"}
        if task["state"] == "closing": return self._release_task(task)
        if task["state"] not in {"active", "preparing"}: return {"action": "noop", "reason": "inactive"}
        expiry = parse_time(task.get("lease_expires_at"))
        if expiry and expiry > self.now():
            self._schedule_expiry(task)
            return {"action": "rearmed", "reason": "not-expired"}
        return self._release_task(task)


    def timer_age_gc(self, request: Mapping[str, Any], session_id: str | None) -> dict[str, Any]:
        return self.admin_gc({"apply": True}, session_id, age_filtered=True)

    def timer_reconcile(self, request: Mapping[str, Any], session_id: str | None) -> dict[str, Any]:
        return self.admin_reconcile({"apply": True}, session_id)


def request_from_stdin() -> dict[str, Any]:
    raw = sys.stdin.buffer.read(2 * 1024 * 1024 + 1)
    if len(raw) > 2 * 1024 * 1024:
        raise ControlError("request_too_large", "request exceeds 2 MiB")
    try:
        value = json.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ControlError("invalid_request", "stdin must contain one UTF-8 JSON object") from exc
    if not isinstance(value, dict):
        raise ControlError("invalid_request", "stdin JSON root must be an object")
    return value


def parse_timer_args(argv: Sequence[str]) -> dict[str, Any] | None:
    if len(argv) == 2 and argv[0] == "--timer-expire":
        value = argv[1]
        match = re.fullmatch(r"([a-z0-9][a-z0-9-]{0,47})-g([0-9]+)", value)
        if match is None:
            raise ControlError("invalid_timer", "invalid expiry timer instance")
        return {
            "protocol": PROTOCOL_VERSION,
            "action": "timer-expire",
            "task_id": match.group(1),
            "generation": int(match.group(2)),
        }
    if len(argv) == 1 and argv[0] == "--timer-age-gc":
        return {"protocol": PROTOCOL_VERSION, "action": "timer-age-gc"}
    if len(argv) == 1 and argv[0] == "--timer-reconcile":
        return {"protocol": PROTOCOL_VERSION, "action": "timer-reconcile"}
    return None


def parse_config_args(argv: Sequence[str]) -> tuple[Path, list[str]]:
    """Extract an optional deployment-selected config path from CLI arguments."""
    configured = os.environ.get(CONFIG_ENVIRONMENT_VARIABLE)
    config_path = Path(configured) if configured else DEFAULT_CONFIG_PATH
    remaining: list[str] = []
    index = 0
    while index < len(argv):
        argument = argv[index]
        if argument == "--config":
            if index + 1 >= len(argv) or not argv[index + 1]:
                raise ControlError("invalid_request", "--config requires a path")
            config_path = Path(argv[index + 1])
            index += 2
            continue
        remaining.append(argument)
        index += 1
    return config_path, remaining


def main(argv: Sequence[str] | None = None) -> int:
    argv = list(argv if argv is not None else sys.argv[1:])
    try:
        config_path, command_argv = parse_config_args(argv)
        config = load_config(config_path)
        request = parse_timer_args(command_argv)
        if request is None:
            if command_argv:
                raise ControlError("invalid_request", "unknown command-line arguments")
            request = request_from_stdin()
        control = Control(config)
        response = control.dispatch(request)
        sys.stdout.write(json.dumps(response, sort_keys=True) + "\n")
        return 0
    except ControlError as exc:
        sys.stderr.write(json.dumps({"ok": False, "error": exc.as_dict()}, sort_keys=True) + "\n")
        return 2
    except Exception as exc:
        sys.stderr.write(
            json.dumps(
                {"ok": False, "error": {"code": "internal_error", "message": type(exc).__name__}},
                sort_keys=True,
            )
            + "\n"
        )
        return 3


if __name__ == "__main__":
    raise SystemExit(main())
