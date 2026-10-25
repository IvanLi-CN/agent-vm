#!/usr/bin/env python3
"""One-shot control plane for native Incus agent VMs.

The process handles one JSON request from stdin and emits one JSON response.
It intentionally has no listener, worker thread, or background loop.
"""

from __future__ import annotations

import argparse
from collections import Counter
from contextlib import ExitStack, contextmanager, nullcontext
import ctypes
import datetime as dt
from decimal import Decimal
import fcntl
import hashlib
import json
import math
import os
import ipaddress
from pathlib import Path, PurePosixPath
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


try:
    from release_manifest import (
        ACTION_NAMES,
        LEGACY_RELEASE_ID,
        ManifestError,
        check_compatibility,
        legacy_manifest,
        load_runtime_manifest,
        manifest_metadata,
        verify_runtime_bundle,
    )
except ModuleNotFoundError:
    sys.path.insert(0, str(Path(__file__).resolve().parent))
    from release_manifest import (
        ACTION_NAMES,
        LEGACY_RELEASE_ID,
        ManifestError,
        check_compatibility,
        legacy_manifest,
        load_runtime_manifest,
        manifest_metadata,
        verify_runtime_bundle,
    )


PROTOCOL_VERSION = 4
SCHEMA_VERSION = 1
SEMANTIC_REVISION = 1
MEMORY_SETTLE_SECONDS = 45
CPU_STARTUP_PRESSURE_FULL_AVG10_PERCENT = 20.0
TRANSITIONAL_ADMISSION_WAIT_SECONDS = 30
TRANSITIONAL_ADMISSION_POLL_SECONDS = 0.25
TRANSITIONAL_ADMISSION_ERROR_MESSAGE = "transitional native instance blocks admission"
GUEST_READINESS_BUDGET_SECONDS = 300
GUEST_READINESS_DIAGNOSTIC_SECONDS = 120
DIAGNOSTIC_COMMAND_TIMEOUT_SECONDS = 5
NATIVE_INIT_TIMEOUT_SECONDS = 300
NATIVE_INIT_ABSENCE_RETRY_SECONDS = 2
DIAGNOSTIC_LOG_LIMIT = 8192
DIAGNOSTIC_LOG_TRUNCATION_MARKER = "\n[TRUNCATED BY DIAGNOSTIC LIMIT]"
DEFAULT_CONFIG_PATH = Path("/etc/agent-vm-native/config.json")
DEFAULT_TIMER_DIR = Path("/etc/systemd/system")
DEFAULT_RELEASE_FENCE_PATH = Path("/run/agent-vm-native/release-activation.fence")
DEFAULT_RELEASE_MANIFEST_PATH = Path(__file__).resolve().with_name("release-manifest.json")
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
RESOURCE_KEYS = frozenset(("cpu", "memory_mib", "disk_gib"))
TASK_STATES = frozenset(("absent", "cached", "preparing", "active", "closing", "deleting", "failed"))


GUEST_ROOT_GROWTH_SCRIPT = r"""
import ctypes, json, os, re, selectors, shutil, signal, subprocess, sys, time

TARGET_BYTES = int(sys.argv[1]) * 1024**3
MINIMUM_FILESYSTEM_RATIO = 95
COMMAND_TIMEOUT = 45
STDOUT_LIMIT = 8192
STDERR_LIMIT = 4000
REPORT_STDOUT_LIMIT = 1024
REPORT_STDERR_LIMIT = 512
capacity = {"requested_bytes": TARGET_BYTES, "verified": False}
attempts = []


def clip(value, limit=4000, *, tail=True):
    if isinstance(value, bytes):
        value = value.decode("utf-8", "replace")
    value = str(value or "")
    return value[-limit:] if tail else value[:limit]


def compact_failure(value, stdout_limit, stderr_limit):
    if not isinstance(value, dict):
        return value
    compacted = dict(value)
    for key, limit, tail in (("stdout", stdout_limit, False), ("stderr", stderr_limit, True)):
        if key not in compacted:
            continue
        raw = compacted[key]
        compacted[key] = clip(raw, limit, tail=tail)
        compacted[key + "_truncated"] = bool(value.get(key + "_truncated")) or len(
            str(raw or "").encode("utf-8", "replace")
        ) > limit
    retry_of = compacted.get("retry_of")
    if isinstance(retry_of, dict):
        compacted["retry_of"] = compact_failure(retry_of, stdout_limit, stderr_limit)
    failure_capacity = compacted.get("capacity")
    if isinstance(failure_capacity, dict):
        failure_capacity = dict(failure_capacity)
        for key in ("filesystem", "source", "parent", "partition"):
            if isinstance(failure_capacity.get(key), str):
                failure_capacity[key] = clip(failure_capacity[key], 256, tail=False)
        tools = failure_capacity.get("tools")
        if isinstance(tools, dict):
            failure_capacity["tools"] = {
                key: clip(value, 256, tail=False) if isinstance(value, str) else value
                for key, value in tools.items()
            }
        records = failure_capacity.get("attempts")
        if isinstance(records, list):
            compacted_records = []
            for record in records:
                if not isinstance(record, dict):
                    compacted_records.append(record)
                    continue
                compacted_record = dict(record)
                for key in ("error", "refresh_error"):
                    if isinstance(compacted_record.get(key), dict):
                        compacted_record[key] = compact_failure(
                            compacted_record[key], stdout_limit, stderr_limit
                        )
                compacted_records.append(compacted_record)
            failure_capacity["attempts"] = compacted_records
        compacted["capacity"] = failure_capacity
    return compacted


def transient_failure(command, stdout, stderr):
    if not command:
        return False
    if command[0] not in {"growpart", "resize2fs", "xfs_growfs", "blockdev"}:
        return False
    if command[0] == "blockdev" and "--rereadpt" not in command:
        return False
    text = (str(stdout or "") + "\n" + str(stderr or "")).lower()
    return any(marker in text for marker in (
        "device or resource busy",
        "resource temporarily unavailable",
        "old partition table",
        "re-read partition table",
        "reread partition table",
    ))


class GuestFailure(Exception):
    def __init__(self, phase, command=None, returncode=None, stdout="", stderr="",
                 *, cause=None, transient=False, retry_of=None,
                 stdout_truncated=False, stderr_truncated=False,
                 process_group_killed=None, process_group_cleanup_error=None,
                 reaped=None, pipes_drained=None):
        super().__init__(phase)
        self.phase = phase
        self.command = list(command or [])
        self.returncode = returncode
        self.stdout = stdout
        self.stderr = stderr
        self.cause = cause
        self.transient = transient
        self.retry_of = retry_of
        self.stdout_truncated = stdout_truncated
        self.stderr_truncated = stderr_truncated
        self.process_group_killed = process_group_killed
        self.process_group_cleanup_error = process_group_cleanup_error
        self.reaped = reaped
        self.pipes_drained = pipes_drained

    def as_dict(self, *, report=False, stdout_limit=None, stderr_limit=None):
        value = {
            "phase": self.phase,
            "command": self.command,
            "returncode": self.returncode,
            "stdout": clip(self.stdout, STDOUT_LIMIT, tail=False),
            "stderr": clip(self.stderr, STDERR_LIMIT),
            "capacity": dict(capacity),
            "transient": self.transient,
            "stdout_truncated": self.stdout_truncated,
            "stderr_truncated": self.stderr_truncated,
        }
        if self.cause:
            value["cause"] = self.cause
        if self.retry_of:
            value["retry_of"] = self.retry_of
        if self.process_group_killed is not None:
            value["process_group_killed"] = self.process_group_killed
            value["process_group_cleanup_error"] = self.process_group_cleanup_error
        if self.reaped is not None:
            value["reaped"] = self.reaped
        if self.pipes_drained is not None:
            value["pipes_drained"] = self.pipes_drained
        if not report:
            return value
        return compact_failure(
            value,
            REPORT_STDOUT_LIMIT if stdout_limit is None else stdout_limit,
            REPORT_STDERR_LIMIT if stderr_limit is None else stderr_limit,
        )


def run(command, phase, *, check=True):
    stdout = bytearray()
    stderr = bytearray()
    stdout_truncated = False
    stderr_truncated = False

    def retain_stdout(chunk):
        nonlocal stdout_truncated
        available = max(0, STDOUT_LIMIT - len(stdout))
        if available:
            stdout.extend(chunk[:available])
        if len(chunk) > available:
            stdout_truncated = True

    def retain_stderr(chunk):
        nonlocal stderr_truncated
        stderr.extend(chunk)
        if len(stderr) > STDERR_LIMIT:
            stderr_truncated = True
            del stderr[:-STDERR_LIMIT]

    def read_ready(selector, events):
        for key, _ in events:
            try:
                chunk = os.read(key.fileobj.fileno(), 65536)
            except (OSError, ValueError):
                chunk = b""
            if not chunk:
                try:
                    selector.unregister(key.fileobj)
                except (KeyError, ValueError):
                    pass
                continue
            if key.data == "stdout":
                retain_stdout(chunk)
            else:
                retain_stderr(chunk)

    def drain_until(selector, deadline, track_process_tree=None):
        while selector.get_map():
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                return False
            if track_process_tree is not None:
                track_process_tree()
            read_ready(selector, selector.select(min(0.1, remaining)))
        return True

    def process_group_identities(pgid):
        members = []
        complete = True
        try:
            entries = os.scandir("/proc")
        except OSError as error:
            return [], False, type(error).__name__
        with entries:
            for entry in entries:
                if not entry.name.isdigit():
                    continue
                try:
                    with open(os.path.join(entry.path, "stat"), encoding="utf-8") as handle:
                        raw_stat = handle.read()
                    close_paren = raw_stat.rfind(")")
                    fields = raw_stat[close_paren + 2:].split()
                    if close_paren < 0 or len(fields) < 20:
                        raise ValueError("invalid process stat")
                    if fields[0] != "Z" and int(fields[2]) == pgid:
                        members.append((int(entry.name), int(fields[19])))
                except FileNotFoundError:
                    continue
                except (OSError, ValueError):
                    complete = False
        return members, complete, None

    def process_group_members(pgid):
        identities, complete, inspection_error = process_group_identities(pgid)
        return [pid for pid, _ in identities], complete, inspection_error

    def process_group_members_unchanged(pgid, tracked):
        members, complete, inspection_error = process_group_identities(pgid)
        if not complete:
            return False, inspection_error or "process_group_inspection_failed"
        if any(tracked.get(pid) != starttime for pid, starttime in members):
            return False, "process_group_identity_unverified"
        for _ in range(5):
            if not members:
                return True, None
            killed, cleanup_error = kill_process_identities(members)
            if not killed:
                return False, cleanup_error
            members, complete, inspection_error = process_group_identities(pgid)
            if not complete:
                return False, inspection_error or "process_group_inspection_failed"
            if any(tracked.get(pid) != starttime for pid, starttime in members):
                return False, "process_group_identity_unverified"
        return (not members), "process_group_members_remain" if members else None

    def process_tree_members(root_pid):
        parents = {}
        complete = True
        try:
            entries = os.scandir("/proc")
        except OSError:
            return [], False
        with entries:
            for entry in entries:
                if not entry.name.isdigit():
                    continue
                try:
                    with open(os.path.join(entry.path, "stat"), encoding="utf-8") as handle:
                        raw_stat = handle.read()
                    close_paren = raw_stat.rfind(")")
                    fields = raw_stat[close_paren + 2:].split()
                    if close_paren < 0 or len(fields) < 20:
                        raise ValueError("invalid process stat")
                    if fields[0] != "Z":
                        parents[int(entry.name)] = (int(fields[1]), int(fields[19]))
                except FileNotFoundError:
                    continue
                except (OSError, ValueError):
                    complete = False
        if root_pid not in parents:
            return [], complete
        members = {root_pid}
        frontier = [root_pid]
        while frontier:
            parent = frontier.pop()
            for pid, (ppid, _) in parents.items():
                if ppid == parent and pid not in members:
                    members.add(pid)
                    frontier.append(pid)
        return sorted((pid, parents[pid][1]) for pid in members), complete

    def live_processes(identities):
        live = []
        complete = True
        for pid, starttime in identities:
            try:
                with open(os.path.join("/proc", str(pid), "stat"), encoding="utf-8") as handle:
                    raw_stat = handle.read()
                close_paren = raw_stat.rfind(")")
                fields = raw_stat[close_paren + 2:].split()
                if close_paren < 0 or len(fields) < 20:
                    raise ValueError("invalid process stat")
                if int(fields[19]) != starttime:
                    complete = False
                    continue
                if fields[0] != "Z":
                    live.append((pid, starttime))
            except FileNotFoundError:
                continue
            except (OSError, ValueError):
                complete = False
        return live, complete

    def process_identity(pid):
        try:
            with open(os.path.join("/proc", str(pid), "stat"), encoding="utf-8") as handle:
                raw_stat = handle.read()
            close_paren = raw_stat.rfind(")")
            fields = raw_stat[close_paren + 2:].split()
            if close_paren < 0 or len(fields) < 20 or fields[0] == "Z":
                return None
            return pid, int(fields[19])
        except (FileNotFoundError, OSError, ValueError):
            return None

    def enable_child_subreaper():
        if not os.path.isdir("/proc"):
            return False
        try:
            libc = ctypes.CDLL(None, use_errno=True)
            prctl = libc.prctl
            prctl.argtypes = [ctypes.c_int, ctypes.c_ulong, ctypes.c_ulong, ctypes.c_ulong, ctypes.c_ulong]
            prctl.restype = ctypes.c_int
            return prctl(36, 1, 0, 0, 0) == 0
        except (AttributeError, OSError, TypeError):
            return False

    def discover_owned_processes(owner_pid, baseline, tracked):
        members, complete = process_tree_members(owner_pid)
        if not complete:
            return [], False, "process_owner_tree_inspection_failed"
        for pid, starttime in members:
            if pid == owner_pid:
                continue
            baseline_starttime = baseline.get(pid)
            if baseline_starttime is not None:
                if baseline_starttime != starttime:
                    return [], False, "process_owner_identity_unverified"
                continue
            previous = tracked.get(pid)
            if previous is not None and previous != starttime:
                return [], False, "process_owner_identity_unverified"
            tracked[pid] = starttime
        return sorted(tracked.items()), True, None

    def kill_owned_processes(owner_pid, baseline, tracked):
        cleanup_error = None
        for _ in range(5):
            identities, complete, discovery_error = discover_owned_processes(owner_pid, baseline, tracked)
            if not complete:
                return False, discovery_error
            live, live_complete = live_processes(identities)
            if not live_complete:
                return False, "process_owner_identity_unverified"
            if not live:
                return True, cleanup_error
            killed, cleanup_error = kill_process_identities(live)
            if not killed:
                return False, cleanup_error
        identities, complete, discovery_error = discover_owned_processes(owner_pid, baseline, tracked)
        if not complete:
            return False, discovery_error
        live, live_complete = live_processes(identities)
        if live_complete and not live:
            return True, cleanup_error
        return False, cleanup_error or "process_owner_members_remain"

    def open_verified_pidfd(identity):
        opener = getattr(os, "pidfd_open", None)
        if opener is None:
            return None, "pidfd_unavailable"
        pid, _ = identity
        try:
            pidfd = opener(pid)
        except ProcessLookupError:
            return None, None
        except OSError as error:
            return None, type(error).__name__
        current, complete = live_processes([identity])
        if not complete:
            os.close(pidfd)
            return None, "process_identity_unverified"
        if not current:
            os.close(pidfd)
            return None, None
        return pidfd, None

    def kill_process_identities(identities):
        sender = getattr(signal, "pidfd_send_signal", None)
        if sender is None:
            return False, "pidfd_signal_unavailable"
        cleanup_error = None
        deadline = time.monotonic() + 0.25
        while True:
            members, complete = live_processes(identities)
            if not complete:
                return False, "process_identity_unverified"
            if not members:
                return True, cleanup_error
            for identity in members:
                pidfd, open_error = open_verified_pidfd(identity)
                if pidfd is None:
                    if open_error is not None:
                        return False, open_error
                    continue
                try:
                    sender(pidfd, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                except OSError as error:
                    cleanup_error = type(error).__name__
                finally:
                    os.close(pidfd)
            if time.monotonic() >= deadline:
                break
            time.sleep(min(0.01, max(0, deadline - time.monotonic())))
        members, complete = live_processes(identities)
        if complete and not members:
            return True, cleanup_error
        return False, cleanup_error or "process_members_remain"

    def kill_process_group_members(pgid, *, required_identity=None):
        if not os.path.isdir("/proc"):
            return False, "process_group_identity_unavailable"
        identities, complete, inspection_error = process_group_identities(pgid)
        if not complete:
            return False, inspection_error or "process_group_inspection_failed"
        cleanup_error = None
        known = {pid: starttime for pid, starttime in identities}
        for attempt in range(5):
            if attempt:
                identities, complete, inspection_error = process_group_identities(pgid)
                if not complete:
                    return False, inspection_error or "process_group_inspection_failed"
            if not identities:
                return True, cleanup_error
            if any(known.get(pid) != starttime for pid, starttime in identities):
                return False, "process_group_identity_unverified"
            if attempt == 0 and required_identity is not None and required_identity not in identities:
                return False, "process_group_identity_unverified"
            killed, cleanup_error = kill_process_identities(identities)
            if not killed:
                return False, cleanup_error
        members, complete, inspection_error = process_group_members(pgid)
        if complete and not members:
            return True, cleanup_error
        return False, cleanup_error or inspection_error or "process_group_members_remain"

    procfs_available = os.path.isdir("/proc")
    owner_pid = os.getpid()
    owner_baseline = {}
    tracked_owned_members = {}
    owner_tracking_enabled = False
    owner_tracking_error = None
    if procfs_available:
        owner_tracking_enabled = enable_child_subreaper()
        if not owner_tracking_enabled:
            owner_tracking_error = "process_owner_boundary_unavailable"
        else:
            baseline_members, baseline_complete = process_tree_members(owner_pid)
            if not baseline_complete:
                owner_tracking_enabled = False
                owner_tracking_error = "process_owner_tree_inspection_failed"
            else:
                owner_baseline = dict(baseline_members)

    process = None
    selector = selectors.DefaultSelector()
    try:
        try:
            process = subprocess.Popen(
                command,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                start_new_session=True,
            )
        except FileNotFoundError:
            raise GuestFailure(phase, command, cause="command_missing")
        tracked_group_members = {}
        tracked_group_valid = False
        leader_identity = process_identity(process.pid) if procfs_available else None

        # Keep detached descendants in the one-shot guest process's owner tree.
        def track_process_tree():
            nonlocal leader_identity, tracked_group_valid
            nonlocal owner_tracking_enabled, owner_tracking_error
            if not procfs_available:
                return
            if owner_tracking_enabled:
                _, owner_complete, discovery_error = discover_owned_processes(
                    owner_pid, owner_baseline, tracked_owned_members
                )
                if not owner_complete:
                    owner_tracking_enabled = False
                    owner_tracking_error = discovery_error
            if leader_identity is None and process.poll() is None:
                leader_identity = process_identity(process.pid)
            group_members, group_complete, _ = process_group_identities(process.pid)
            if not group_complete or not group_members:
                return
            if leader_identity is None:
                return
            for pid, starttime in group_members:
                previous = tracked_group_members.get(pid)
                if previous is not None and previous != starttime:
                    tracked_group_valid = False
                    return
                tracked_group_members[pid] = starttime
            tracked_group_valid = True

        track_process_tree()
        for stream, name in ((process.stdout, "stdout"), (process.stderr, "stderr")):
            if stream is not None:
                selector.register(stream, selectors.EVENT_READ, name)
        deadline = time.monotonic() + COMMAND_TIMEOUT
        if not drain_until(selector, deadline, track_process_tree):
            raise subprocess.TimeoutExpired(command, COMMAND_TIMEOUT)
        remaining = max(0, deadline - time.monotonic())
        try:
            returncode = process.wait(timeout=remaining)
            if procfs_available:
                if not owner_tracking_enabled:
                    raise GuestFailure(
                        phase,
                        command,
                        returncode,
                        cause="command_cleanup_failed",
                        process_group_killed=False,
                        process_group_cleanup_error=owner_tracking_error or "process_owner_boundary_unavailable",
                    )
                owned_members, owner_complete, discovery_error = discover_owned_processes(
                    owner_pid, owner_baseline, tracked_owned_members
                )
                if not owner_complete:
                    raise GuestFailure(
                        phase,
                        command,
                        returncode,
                        cause="command_cleanup_failed",
                        process_group_killed=False,
                        process_group_cleanup_error=discovery_error or "process_owner_tree_inspection_failed",
                    )
                owned_live, live_complete = live_processes(owned_members)
                if not live_complete:
                    raise GuestFailure(
                        phase,
                        command,
                        returncode,
                        cause="command_cleanup_failed",
                        process_group_killed=False,
                        process_group_cleanup_error="process_owner_identity_unverified",
                    )
                if owned_live:
                    owner_killed, owner_cleanup_error = kill_owned_processes(
                        owner_pid, owner_baseline, tracked_owned_members
                    )
                    raise GuestFailure(
                        phase,
                        command,
                        returncode,
                        cause="command_cleanup_failed",
                        process_group_killed=owner_killed,
                        process_group_cleanup_error=owner_cleanup_error or "process_owner_members_remain",
                    )
        except subprocess.TimeoutExpired:
            raise
    except subprocess.TimeoutExpired as error:
        process_group_killed = False
        process_group_cleanup_error = None
        root_identity = leader_identity if os.path.isdir("/proc") else None
        root_live = []
        root_verified = not os.path.isdir("/proc")
        root_identity_missing = leader_identity is not None and os.path.isdir("/proc")
        if root_identity is not None:
            root_live, root_identity_complete = live_processes([root_identity])
            root_verified = bool(root_live)
            root_identity_missing = root_identity_complete and not root_live
        elif leader_identity is not None and os.path.isdir("/proc"):
            root_live, root_identity_complete = live_processes([leader_identity])
            root_identity_missing = root_identity_complete and not root_live
        if root_verified:
            try:
                if os.path.isdir("/proc"):
                    os.killpg(process.pid, 0)
                    process_group_killed, fallback_error = kill_process_group_members(
                        process.pid, required_identity=leader_identity
                    )
                    if fallback_error:
                        process_group_cleanup_error = fallback_error
                else:
                    os.killpg(process.pid, signal.SIGKILL)
                    process_group_killed = True
            except ProcessLookupError:
                if os.path.isdir("/proc"):
                    process_group_killed, fallback_error = kill_process_group_members(
                        process.pid, required_identity=leader_identity
                    )
                    if fallback_error:
                        process_group_cleanup_error = fallback_error
                else:
                    process_group_killed = True
            except (AttributeError, PermissionError, OSError) as group_error:
                process_group_cleanup_error = type(group_error).__name__
                if os.path.isdir("/proc"):
                    process_group_killed, fallback_error = kill_process_group_members(
                        process.pid, required_identity=leader_identity
                    )
                    if fallback_error:
                        process_group_cleanup_error += ":" + fallback_error
                else:
                    try:
                        process.kill()
                    except ProcessLookupError:
                        pass
        elif root_identity_missing and tracked_group_valid:
            process_group_killed, process_group_cleanup_error = process_group_members_unchanged(
                process.pid, tracked_group_members
            )
        else:
            process_group_cleanup_error = "process_identity_unverified"
        if procfs_available:
            if owner_tracking_enabled:
                owner_killed, owner_cleanup_error = kill_owned_processes(
                    owner_pid, owner_baseline, tracked_owned_members
                )
            else:
                owner_killed = False
                owner_cleanup_error = owner_tracking_error or "process_owner_boundary_unavailable"
            if not owner_killed:
                process_group_killed = False
                process_group_cleanup_error = (
                    f"{process_group_cleanup_error}:{owner_cleanup_error}"
                    if process_group_cleanup_error else owner_cleanup_error
                )
        reaped = False
        try:
            process.wait(timeout=2)
            reaped = True
        except (subprocess.TimeoutExpired, ChildProcessError):
            pass
        pipes_drained = drain_until(selector, time.monotonic() + 1)
        raise GuestFailure(
            phase,
            command,
            stdout=bytes(stdout).decode("utf-8", "replace"),
            stderr=bytes(stderr).decode("utf-8", "replace"),
            cause="command_timeout",
            stdout_truncated=stdout_truncated,
            stderr_truncated=stderr_truncated,
            process_group_killed=process_group_killed,
            process_group_cleanup_error=process_group_cleanup_error,
            reaped=reaped,
            pipes_drained=pipes_drained,
        ) from error
    except FileNotFoundError:
        raise GuestFailure(phase, command, cause="command_missing")
    finally:
        selector.close()
        for stream in (getattr(process, "stdout", None), getattr(process, "stderr", None)):
            if stream is not None:
                try:
                    stream.close()
                except OSError:
                    pass
    stdout = bytes(stdout).decode("utf-8", "replace")
    stderr = bytes(stderr).decode("utf-8", "replace")
    if check and returncode:
        raise GuestFailure(
            phase,
            command,
            returncode,
            stdout,
            stderr,
            transient=transient_failure(command, stdout, stderr),
            stdout_truncated=stdout_truncated,
            stderr_truncated=stderr_truncated,
        )
    result = subprocess.CompletedProcess(command, returncode, stdout, stderr)
    result.stdout_truncated = stdout_truncated
    result.stderr_truncated = stderr_truncated
    return result


def one_line(result, phase, command):
    lines = [line.strip() for line in result.stdout.splitlines() if line.strip()]
    if len(lines) != 1:
        raise GuestFailure(phase, command, result.returncode, result.stdout, result.stderr,
                           cause="ambiguous_output",
                           stdout_truncated=getattr(result, "stdout_truncated", False),
                           stderr_truncated=getattr(result, "stderr_truncated", False))
    return lines[0]


def number_from(command, phase, key):
    result = run(command, phase)
    raw = result.stdout.strip()
    if not re.fullmatch(r"[0-9]+", raw):
        raise GuestFailure(phase, command, result.returncode, result.stdout, result.stderr,
                           cause="invalid_integer",
                           stdout_truncated=getattr(result, "stdout_truncated", False),
                           stderr_truncated=getattr(result, "stderr_truncated", False))
    value = int(raw)
    capacity[key] = value
    return value


def filesystem_bytes():
    try:
        value = os.statvfs("/")
        result = value.f_blocks * value.f_frsize
    except (AttributeError, OSError, ValueError) as error:
        raise GuestFailure("filesystem-diagnostic", ["statvfs", "/"], cause=type(error).__name__) from error
    if result <= 0:
        raise GuestFailure("filesystem-diagnostic", ["statvfs", "/"], cause="non_positive_capacity")
    capacity["filesystem_bytes"] = result
    return result


def inspect_layout():
    source_command = ["findmnt", "-n", "-o", "SOURCE", "-T", "/"]
    source = one_line(run(source_command, "topology-source"), "topology-source", source_command)
    source = os.path.realpath(source)
    fstype_command = ["findmnt", "-n", "-o", "FSTYPE", "-T", "/"]
    fstype = one_line(run(fstype_command, "topology-filesystem"), "topology-filesystem", fstype_command)
    capacity.update({"source": source, "filesystem": fstype})
    if not source.startswith("/dev/") or fstype not in ("ext4", "xfs"):
        raise GuestFailure(
            "capability-check",
            source_command,
            cause="automatic growth supports direct ext4/xfs root partitions only",
        )
    name = os.path.basename(source)
    partition_marker = "/sys/class/block/" + name + "/partition"
    if not re.fullmatch(r"[A-Za-z0-9_.-]+", name) or not os.path.isfile(partition_marker):
        raise GuestFailure("topology-check", ["test", "-f", partition_marker], cause="root_is_not_partition")
    try:
        with open(partition_marker, encoding="ascii") as handle:
            number = handle.read().strip()
    except OSError as error:
        raise GuestFailure("topology-check", ["cat", partition_marker], cause=type(error).__name__) from error
    if not re.fullmatch(r"[0-9]+", number):
        raise GuestFailure("topology-check", ["cat", partition_marker], cause="invalid_partition_number")

    parent_command = ["lsblk", "-n", "-o", "PKNAME", source]
    parent_lines = [line.strip() for line in run(parent_command, "topology-parent").stdout.splitlines() if line.strip()]
    if len(parent_lines) != 1 or "/" in parent_lines[0]:
        raise GuestFailure("topology-check", parent_command, cause="ambiguous_root_disk")
    parent = parent_lines[0]
    disk = "/dev/" + parent
    source_type_command = ["lsblk", "-n", "-o", "TYPE", source]
    if one_line(run(source_type_command, "topology-type"), "topology-type", source_type_command) != "part":
        raise GuestFailure("topology-check", source_type_command, cause="root_is_not_partition")
    disk_type_command = ["lsblk", "-n", "-o", "TYPE", disk]
    if one_line(run(disk_type_command, "topology-type"), "topology-type", disk_type_command) != "disk":
        raise GuestFailure("topology-check", disk_type_command, cause="parent_is_not_disk")

    filesystem_tool = "resize2fs" if fstype == "ext4" else "xfs_growfs"
    required_tools = ["findmnt", "lsblk", "blockdev", "growpart", filesystem_tool]
    tools = {tool: shutil.which(tool) for tool in required_tools}
    capacity.update({"source": source, "filesystem": fstype, "parent": disk, "partition": number, "tools": tools})
    missing = [tool for tool, path in tools.items() if not path]
    if missing:
        raise GuestFailure("capability-check", ["command", "-v", missing[0]], cause="missing_guest_tool")

    disk_bytes = number_from(["blockdev", "--getsize64", disk], "capacity-diagnostic", "disk_bytes")
    partition_bytes = number_from(["blockdev", "--getsize64", source], "capacity-diagnostic", "partition_bytes")
    filesystem_size = filesystem_bytes()
    capacity["filesystem_threshold_bytes"] = TARGET_BYTES * MINIMUM_FILESYSTEM_RATIO // 100
    if disk_bytes < TARGET_BYTES:
        raise GuestFailure(
            "capacity-diagnostic",
            ["blockdev", "--getsize64", disk],
            0,
            cause="guest_virtual_disk_size_is_stale",
        )
    if partition_bytes > disk_bytes:
        raise GuestFailure("topology-check", ["blockdev", "--getsize64", source], 0,
                           cause="partition_exceeds_parent_disk")
    capacity["verified"] = True
    return {
        "source": source,
        "fstype": fstype,
        "disk": disk,
        "number": number,
        "filesystem_tool": filesystem_tool,
        "filesystem_bytes": filesystem_size,
        "disk_bytes": disk_bytes,
        "partition_bytes": partition_bytes,
    }


def filesystem_is_complete():
    return capacity.get("filesystem_bytes", 0) >= capacity.get("filesystem_threshold_bytes", TARGET_BYTES)


def verify_partition(layout):
    required = TARGET_BYTES * MINIMUM_FILESYSTEM_RATIO // 100
    if capacity.get("partition_bytes", 0) < required:
        raise GuestFailure(
            "partition-verify",
            ["blockdev", "--getsize64", layout["source"]],
            0,
            cause="stale_partition_capacity_below_target",
            transient=True,
        )


def settle_partition(layout):
    reread_command = ["blockdev", "--rereadpt", layout["disk"]]
    reread = run(reread_command, "partition-reread", check=False)
    if reread.returncode and not transient_failure(reread_command, reread.stdout, reread.stderr):
        raise GuestFailure(
            "partition-reread", reread_command, reread.returncode, reread.stdout, reread.stderr,
            stdout_truncated=getattr(reread, "stdout_truncated", False),
            stderr_truncated=getattr(reread, "stderr_truncated", False),
        )
    if shutil.which("udevadm"):
        settle_command = ["udevadm", "settle", "--timeout=5"]
        settled = run(settle_command, "partition-settle", check=False)
        if settled.returncode and not transient_failure(settle_command, settled.stdout, settled.stderr):
            raise GuestFailure(
                "partition-settle", settle_command, settled.returncode, settled.stdout, settled.stderr,
                stdout_truncated=getattr(settled, "stdout_truncated", False),
                stderr_truncated=getattr(settled, "stderr_truncated", False),
            )
    time.sleep(1)


def report(status, before, layout):
    capacity["status"] = status
    capacity["filesystem_before"] = before.get("filesystem_bytes")
    capacity["attempts"] = list(attempts)
    return {
        "status": status,
        "virtual_bytes": capacity["disk_bytes"],
        "partition_bytes": capacity["partition_bytes"],
        "filesystem_before": before.get("filesystem_bytes"),
        "filesystem_bytes": capacity["filesystem_bytes"],
        "filesystem_threshold_bytes": capacity["filesystem_threshold_bytes"],
        "filesystem": layout["fstype"],
        "source": layout["source"],
        "parent": layout["disk"],
        "partition": layout["number"],
        "attempts": len(attempts),
        "capacity": dict(capacity),
    }


def emit_failure(failure):
    payload = {"ok": False, "error": failure.as_dict()}
    encoded = json.dumps(payload, sort_keys=True, separators=(",", ":"))
    size = len(encoded.encode("utf-8"))
    if size <= STDERR_LIMIT:
        print(encoded, file=sys.stderr)
        return
    if size <= STDOUT_LIMIT:
        print(encoded, file=sys.stdout)
        return
    for stdout_limit, stderr_limit in ((1024, 512), (512, 256), (256, 128), (128, 64)):
        payload = {
            "ok": False,
            "error": failure.as_dict(
                report=True,
                stdout_limit=stdout_limit,
                stderr_limit=stderr_limit,
            ),
        }
        encoded = json.dumps(payload, sort_keys=True, separators=(",", ":"))
        size = len(encoded.encode("utf-8"))
        if size <= STDERR_LIMIT:
            print(encoded, file=sys.stderr)
            return
        if size <= STDOUT_LIMIT:
            print(encoded, file=sys.stdout)
            return
    print(encoded, file=sys.stdout)


try:
    layout = inspect_layout()
    before = dict(capacity)
    capacity["filesystem_before"] = before.get("filesystem_bytes")
    if filesystem_is_complete():
        print(json.dumps(report("already_grown", before, layout), sort_keys=True))
        sys.exit(0)

    filesystem_command = [layout["filesystem_tool"], layout["source"] if layout["fstype"] == "ext4" else "/"]
    for attempt in (1, 2):
        attempt_record = {"attempt": attempt}
        attempts.append(attempt_record)
        failure = None
        try:
            growpart_command = ["growpart", layout["disk"], layout["number"]]
            grown = run(growpart_command, "partition-grow", check=False)
            if grown.returncode and not (grown.returncode == 1 and "NOCHANGE" in grown.stdout.upper()):
                failure = GuestFailure(
                    "partition-grow",
                    growpart_command,
                    grown.returncode,
                    grown.stdout,
                    grown.stderr,
                    transient=transient_failure(growpart_command, grown.stdout, grown.stderr),
                    stdout_truncated=getattr(grown, "stdout_truncated", False),
                    stderr_truncated=getattr(grown, "stderr_truncated", False),
                )
            if failure is None:
                layout = inspect_layout()
                verify_partition(layout)
                if filesystem_is_complete():
                    print(json.dumps(report("already_grown", before, layout), sort_keys=True))
                    sys.exit(0)
                grown_filesystem = run(filesystem_command, "filesystem-grow", check=False)
                if grown_filesystem.returncode:
                    failure = GuestFailure(
                        "filesystem-grow",
                        filesystem_command,
                        grown_filesystem.returncode,
                        grown_filesystem.stdout,
                        grown_filesystem.stderr,
                        transient=transient_failure(filesystem_command, grown_filesystem.stdout, grown_filesystem.stderr),
                        stdout_truncated=getattr(grown_filesystem, "stdout_truncated", False),
                        stderr_truncated=getattr(grown_filesystem, "stderr_truncated", False),
                    )
                else:
                    layout = inspect_layout()
                    if not filesystem_is_complete():
                        failure = GuestFailure(
                            "filesystem-verify",
                            filesystem_command,
                            0,
                            cause="filesystem_capacity_below_target",
                        )
        except GuestFailure as error:
            failure = error
        if failure is None:
            status = "grown_after_retry" if attempt > 1 else "grown"
            print(json.dumps(report(status, before, layout), sort_keys=True))
            sys.exit(0)
        if attempt > 1 and isinstance(attempts[-2].get("error"), dict):
            failure.retry_of = attempts[-2]["error"]
        attempt_record["error"] = failure.as_dict(report=True)
        if not failure.transient or attempt == 2:
            raise failure
        try:
            settle_partition(layout)
            layout = inspect_layout()
        except GuestFailure as refresh_failure:
            refresh_failure.retry_of = attempt_record["error"]
            attempt_record["refresh_error"] = refresh_failure.as_dict(report=True)
            if not refresh_failure.transient:
                raise refresh_failure

    raise GuestFailure("filesystem-verify", filesystem_command, cause="retry_exhausted")
except GuestFailure as failure:
    capacity["attempts"] = list(attempts)
    emit_failure(failure)
    sys.exit(1)
except Exception as error:
    capacity["attempts"] = list(attempts)
    print(json.dumps({
        "ok": False,
        "error": {
            "phase": "guest-root-growth",
            "command": ["python3", "-c", "guest-root-growth"],
            "returncode": 1,
            "stdout": "",
            "stderr": "",
            "capacity": dict(capacity),
            "transient": False,
            "stdout_truncated": False,
            "stderr_truncated": False,
            "cause": type(error).__name__,
        },
    }, sort_keys=True), file=sys.stderr)
    sys.exit(1)
"""


def runtime_artifact_paths(config: Mapping[str, Any], manifest_path: Path, manifest: Mapping[str, Any]) -> dict[str, Path]:
    return {
        item["path"]: (
            DEFAULT_TIMER_DIR / Path(item["path"]).name
            if item["role"] == "unit"
            else manifest_path.parent / Path(*PurePosixPath(item["path"]).parts)
        )
        for item in manifest["artifacts"]
    }


def parse_resource_mapping(raw: str | None, label: str, *, allow_empty: bool) -> dict[str, int]:
    if not isinstance(raw, str):
        raise ValueError(label)
    value = json.loads(raw)
    keys = frozenset(value) if isinstance(value, Mapping) else None
    allowed = ({frozenset(), RESOURCE_KEYS} if allow_empty else {RESOURCE_KEYS})
    if not isinstance(value, Mapping) or keys not in allowed:
        raise ValueError(label)
    if any(isinstance(item, bool) or not isinstance(item, int) or item < 0 for item in value.values()):
        raise ValueError(label)
    return dict(value)


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


def percentage(value: Any, name: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise ControlError("config_invalid", f"{name} must be a finite percentage between zero and 100")
    try:
        number = Decimal(str(value))
        if not number.is_finite() or number < 0 or number > 100:
            raise ValueError()
    except (ValueError, ArithmeticError) as exc:
        raise ControlError("config_invalid", f"{name} must be a finite percentage between zero and 100") from exc
    return float(number)


def parse_cpu_psi(raw: str) -> dict[str, dict[str, float]]:
    result: dict[str, dict[str, float]] = {}
    for line in raw.splitlines():
        fields = line.split()
        if not fields:
            continue
        if fields[0] not in {"some", "full"}:
            raise ValueError("unknown CPU PSI line")
        if fields[0] in result:
            raise ValueError("duplicate CPU PSI line")
        values: dict[str, float] = {}
        for field in fields[1:]:
            name, separator, value = field.partition("=")
            if not separator or name in values:
                raise ValueError("invalid CPU PSI field")
            if name == "total" and re.fullmatch(r"[0-9]+", value) is None:
                raise ValueError("invalid CPU PSI total")
            try:
                number = float(value)
            except ValueError as exc:
                raise ValueError("invalid CPU PSI value") from exc
            if not math.isfinite(number) or number < 0 or (name.startswith("avg") and number > 100):
                raise ValueError("CPU PSI value is out of range")
            values[name] = number
        if "avg10" not in values:
            raise ValueError("CPU PSI avg10 is missing")
        result[fields[0]] = values
    return normalize_cpu_psi(result)


def normalize_cpu_psi(value: Any) -> dict[str, dict[str, float]]:
    if not isinstance(value, Mapping) or any(scope not in value for scope in ("some", "full")):
        raise ValueError("CPU PSI data is missing")
    required = {"avg10", "avg60", "avg300", "total"}
    result: dict[str, dict[str, float]] = {}
    for scope in ("some", "full"):
        values = value[scope]
        if not isinstance(values, Mapping) or set(values) != required:
            raise ValueError("CPU PSI fields are incomplete")
        normalized: dict[str, float] = {}
        for name in required:
            number = values[name]
            if isinstance(number, bool):
                raise ValueError("CPU PSI value is invalid")
            try:
                number = float(number)
            except (TypeError, ValueError, OverflowError) as exc:
                raise ValueError("CPU PSI value is invalid") from exc
            if not math.isfinite(number) or number < 0 or (name.startswith("avg") and number > 100):
                raise ValueError("CPU PSI value is out of range")
            normalized[name] = number
        result[scope] = normalized
    if any(result["full"][name] > result["some"][name] for name in required):
        raise ValueError("CPU PSI full pressure exceeds some pressure")
    return result


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
        try:
            number = int(value.strip())
        except (ValueError, OverflowError) as exc:
            raise ControlError("resource_unverified", "CT resource metric is invalid", {"metric": metric}) from exc
        if positive and number == 0:
            raise ControlError("resource_unverified", "CT capacity must be positive", {"metric": metric})
        return number

    def cpu(self) -> int:
        capacity = len(os.sched_getaffinity(0)) if hasattr(os, "sched_getaffinity") else (os.cpu_count() or 0)
        _cgroup_path, cgroup_paths = self._cgroup_metric_paths("cpu.max")
        for cpu_max_path in cgroup_paths:
            fields = self._read(cpu_max_path).split()
            if len(fields) != 2:
                raise ControlError("resource_unverified", "invalid CT CPU quota")
            period = self._unsigned(fields[1], "cpu.max period", positive=True)
            if fields[0] != "max":
                capacity = min(capacity, self._unsigned(fields[0], "cpu.max quota", positive=True) // period)
        return capacity

    def _verify_cgroup2_mount(self) -> None:
        matches = []
        for line in self._read("/proc/self/mountinfo").splitlines():
            before, separator, after = line.partition(" - ")
            if not separator:
                continue
            left, right = before.split(), after.split()
            if len(left) >= 5 and right and left[3] == "/" and left[4] == "/sys/fs/cgroup":
                matches.append((right[0], left[3]))
        if matches != [("cgroup2", "/")]:
            raise ControlError(
                "resource_unverified",
                "CT cgroup v2 mount cannot be verified",
                {"metric": "/proc/self/mountinfo", "mountpoint": "/sys/fs/cgroup"},
            )

    def current_cgroup_path(self) -> str:
        lines = [line for line in self._read("/proc/self/cgroup").splitlines() if line.strip()]
        if len(lines) != 1:
            raise ControlError("resource_unverified", "CT cgroup path cannot be verified",
                               {"metric": "/proc/self/cgroup"})
        fields = lines[0].split(":", 2)
        if len(fields) != 3 or fields[0] != "0" or fields[1] != "":
            raise ControlError("resource_unverified", "CT cgroup path is not unified",
                               {"metric": "/proc/self/cgroup"})
        path = fields[2]
        if path != "/" and (
            re.fullmatch(r"/[^\x00/]+(?:/[^\x00/]+)*", path) is None
            or any(part in {".", ".."} for part in path.split("/"))
        ):
            raise ControlError("resource_unverified", "CT cgroup path is invalid",
                               {"metric": "/proc/self/cgroup"})
        return path

    def _cgroup_metric_path(self, metric: str) -> tuple[str, str]:
        cgroup_path, paths = self._cgroup_metric_paths(metric)
        return cgroup_path, paths[0]

    def _cgroup_metric_paths(self, metric: str) -> tuple[str, list[str]]:
        self._verify_cgroup2_mount()
        cgroup_path = self.current_cgroup_path()
        components = [] if cgroup_path == "/" else cgroup_path.strip("/").split("/")
        paths = []
        for depth in range(len(components), -1, -1):
            ancestor = "/" + "/".join(components[:depth]) if depth else ""
            paths.append("/sys/fs/cgroup" + ancestor + "/" + metric)
        return cgroup_path, paths

    def cpu_pressure(self) -> dict[str, Any]:
        cgroup_path, pressure_path = self._cgroup_metric_path("cpu.pressure")
        try:
            values = parse_cpu_psi(self._read(pressure_path))
        except ValueError as exc:
            raise ControlError("resource_unverified", "CT CPU pressure metric is invalid",
                               {"metric": pressure_path}) from exc
        return {
            "scope": "ct-cpu-pressure",
            "sources": ["cgroupfs"],
            "mountpoint": "/sys/fs/cgroup",
            "mount_root": "/",
            "filesystem": "cgroup2",
            "cgroup_path": cgroup_path,
            "metric": pressure_path,
            **values,
        }

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

    def _optional_read(self, path: str) -> str | None:
        try:
            return self._read(path)
        except ControlError as exc:
            if exc.code == "resource_unverified":
                return None
            raise

    def _memory_events(self) -> dict[str, int] | None:
        raw = self._optional_read("/sys/fs/cgroup/memory.events")
        if raw is None:
            return None
        values: dict[str, int] = {}
        for line in raw.splitlines():
            fields = line.split()
            if len(fields) != 2 or fields[0] not in {"oom", "oom_kill", "oom_group_kill"}:
                continue
            if fields[0] in values:
                return None
            try:
                values[fields[0]] = self._unsigned(fields[1], "memory.events." + fields[0])
            except ControlError:
                # OOM counters are advisory. A malformed optional counter must
                # not turn an otherwise verified memory capacity into a gate.
                return None
        return values or None

    def memory(self) -> dict[str, Any]:
        raw_limit = self._read("/sys/fs/cgroup/memory.max").strip()
        lxcfs = self._lxcfs_meminfo()
        sources = []
        current = None
        if raw_limit == "max":
            if lxcfs is None:
                raise ControlError("resource_unverified", "unlimited CT cgroup requires a verified LXCfs memory view")
            limit, available = lxcfs["MemTotal"], lxcfs["MemAvailable"]
            raw_current = self._optional_read("/sys/fs/cgroup/memory.current")
            if raw_current is not None:
                current = self._unsigned(raw_current, "memory.current")
                if current > limit:
                    raise ControlError("resource_unverified", "CT memory accounting exceeds its LXCfs capacity")
        else:
            limit = self._unsigned(raw_limit, "memory.max", positive=True)
            current = self._unsigned(self._read("/sys/fs/cgroup/memory.current"), "memory.current")
            if current > limit:
                raise ControlError("resource_unverified", "CT memory accounting exceeds its cgroup limit")
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
                if current is not None and current > limit:
                    raise ControlError("resource_unverified", "CT memory accounting exceeds its effective capacity")
                available = min(available, lxcfs["MemAvailable"], limit)
        if lxcfs is not None:
            sources.append("lxcfs")
        return {"scope": "ct-memory", "sources": sources, "limit_bytes": limit,
                "available_bytes": available, "current_bytes": current,
                "threshold_bytes": max(1024**3, (limit + 9) // 10),
                "oom_events": self._memory_events()}


def utc_now() -> dt.datetime:
    return dt.datetime.now(dt.timezone.utc)


def iso_time(value: dt.datetime | None = None) -> str:
    value = value or utc_now()
    return value.astimezone(dt.timezone.utc).replace(microsecond=0).isoformat()


def parse_time(value: str | None) -> dt.datetime | None:
    if not value:
        return None
    if not isinstance(value, str):
        raise ControlError("invalid_time", "timestamp must be a string")
    try:
        return dt.datetime.fromisoformat(value.replace("Z", "+00:00")).astimezone(
            dt.timezone.utc
        )
    except (AttributeError, TypeError, ValueError) as exc:
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
        "cpu_startup_pressure_full_avg10_percent": CPU_STARTUP_PRESSURE_FULL_AVG10_PERCENT,
        "create_cost_bytes": 4 * 1024**3,
        "resume_cost_bytes": 1024**3,
        "resize_cost_bytes": 1024**3,
        "required_guest_tools": ["python3", "sshd", "rsync", "docker", "growpart"],
        "timer_dir": str(DEFAULT_TIMER_DIR),
        "release_fence_path": str(DEFAULT_RELEASE_FENCE_PATH),
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
    config["cpu_startup_pressure_full_avg10_percent"] = percentage(
        config["cpu_startup_pressure_full_avg10_percent"],
        "cpu_startup_pressure_full_avg10_percent",
    )
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
    config["release_fence_path"] = require_nonempty_string(config["release_fence_path"], "release_fence_path")
    if not config["release_fence_path"].startswith("/"):
        raise ControlError("config_invalid", "release_fence_path must be absolute")
    if Path(config["release_fence_path"]) != DEFAULT_RELEASE_FENCE_PATH:
        raise ControlError("config_invalid", "release_fence_path must use the fixed activation fence")
    return config


class CommandRunner:
    """Small subprocess boundary that never invokes a shell implicitly."""

    _PR_SET_CHILD_SUBREAPER = 36

    @staticmethod
    def _process_identity(pid: int) -> tuple[int, int] | None:
        try:
            raw_stat = (Path("/proc") / str(pid) / "stat").read_text()
            close_paren = raw_stat.rfind(")")
            fields = raw_stat[close_paren + 2 :].split()
            if close_paren < 0 or len(fields) < 20 or fields[0] == "Z":
                return None
            return pid, int(fields[19])
        except (FileNotFoundError, OSError, ValueError):
            return None

    @classmethod
    def _enable_child_subreaper(cls) -> bool:
        if not Path("/proc").is_dir():
            return False
        try:
            libc = ctypes.CDLL(None, use_errno=True)
            prctl = libc.prctl
            prctl.argtypes = [
                ctypes.c_int,
                ctypes.c_ulong,
                ctypes.c_ulong,
                ctypes.c_ulong,
                ctypes.c_ulong,
            ]
            prctl.restype = ctypes.c_int
            return prctl(cls._PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0) == 0
        except (AttributeError, OSError, TypeError):
            return False

    @classmethod
    def _discover_owned_processes(
        cls,
        owner_pid: int,
        baseline: Mapping[int, int],
        tracked: dict[int, int],
    ) -> tuple[list[tuple[int, int]], bool, str | None]:
        members, complete = cls._process_tree_members(owner_pid)
        if not complete:
            return [], False, "process_owner_tree_inspection_failed"
        for pid, starttime in members:
            if pid == owner_pid:
                continue
            baseline_starttime = baseline.get(pid)
            if baseline_starttime is not None:
                if baseline_starttime != starttime:
                    return [], False, "process_owner_identity_unverified"
                continue
            previous = tracked.get(pid)
            if previous is not None and previous != starttime:
                return [], False, "process_owner_identity_unverified"
            tracked[pid] = starttime
        return sorted(tracked.items()), True, None

    @classmethod
    def _kill_owned_processes(
        cls,
        owner_pid: int,
        baseline: Mapping[int, int],
        tracked: dict[int, int],
    ) -> tuple[bool, str | None]:
        cleanup_error = None
        for _ in range(5):
            identities, complete, discovery_error = cls._discover_owned_processes(
                owner_pid, baseline, tracked
            )
            if not complete:
                return False, discovery_error
            live, live_complete = cls._live_processes(identities)
            if not live_complete:
                return False, "process_owner_identity_unverified"
            if not live:
                return True, cleanup_error
            killed, cleanup_error = cls._kill_process_identities(live)
            if not killed:
                return False, cleanup_error
        identities, complete, discovery_error = cls._discover_owned_processes(
            owner_pid, baseline, tracked
        )
        if not complete:
            return False, discovery_error
        live, live_complete = cls._live_processes(identities)
        if live_complete and not live:
            return True, cleanup_error
        return False, cleanup_error or "process_owner_members_remain"

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
            def captured(value: bytes | str | None) -> str:
                if isinstance(value, bytes):
                    return value.decode("utf-8", "replace")[-4000:]
                return str(value or "")[-4000:]

            raise ControlError(
                "command_timeout",
                f"command timed out: {argv[0]}",
                {"stdout": captured(exc.stdout), "stderr": captured(exc.stderr)},
            ) from exc
        if check and result.returncode != 0:
            stderr = result.stderr.decode("utf-8", "replace").strip()
            raise ControlError(
                "command_failed",
                f"command failed ({result.returncode}): {argv[0]}",
                {"returncode": result.returncode, "stderr": stderr[-4000:]},
            )
        return result

    @staticmethod
    def _process_group_identities(pgid: int) -> tuple[list[tuple[int, int]], bool]:
        members: list[tuple[int, int]] = []
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
                if close_paren < 0 or len(fields) < 20:
                    raise ValueError("invalid process stat")
                if fields[0] != "Z" and int(fields[2]) == pgid:
                    members.append((int(entry.name), int(fields[19])))
            except FileNotFoundError:
                continue
            except (OSError, ValueError):
                complete = False
        return members, complete

    @classmethod
    def _process_group_members(cls, pgid: int) -> tuple[list[int], bool]:
        identities, complete = cls._process_group_identities(pgid)
        return [pid for pid, _ in identities], complete

    @staticmethod
    def _process_tree_members(root_pid: int) -> tuple[list[tuple[int, int]], bool]:
        parents: dict[int, tuple[int, int]] = {}
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
                if close_paren < 0 or len(fields) < 20:
                    raise ValueError("invalid process stat")
                if fields[0] != "Z":
                    parents[int(entry.name)] = (int(fields[1]), int(fields[19]))
            except FileNotFoundError:
                continue
            except (OSError, ValueError):
                complete = False
        if root_pid not in parents:
            return [], complete
        members = {root_pid}
        frontier = [root_pid]
        while frontier:
            parent = frontier.pop()
            for pid, (ppid, _) in parents.items():
                if ppid == parent and pid not in members:
                    members.add(pid)
                    frontier.append(pid)
        return sorted((pid, parents[pid][1]) for pid in members), complete

    @staticmethod
    def _live_processes(identities: Sequence[tuple[int, int]]) -> tuple[list[tuple[int, int]], bool]:
        live: list[tuple[int, int]] = []
        complete = True
        for pid, starttime in identities:
            try:
                raw_stat = (Path("/proc") / str(pid) / "stat").read_text()
                close_paren = raw_stat.rfind(")")
                fields = raw_stat[close_paren + 2 :].split()
                if close_paren < 0 or len(fields) < 20:
                    raise ValueError("invalid process stat")
                if int(fields[19]) != starttime:
                    complete = False
                    continue
                if fields[0] != "Z":
                    live.append((pid, starttime))
            except FileNotFoundError:
                continue
            except (OSError, ValueError):
                complete = False
        return live, complete

    @classmethod
    def _open_verified_pidfd(cls, identity: tuple[int, int]) -> tuple[int | None, str | None]:
        opener = getattr(os, "pidfd_open", None)
        if opener is None:
            return None, "pidfd_unavailable"
        pid, _ = identity
        try:
            pidfd = opener(pid)
        except ProcessLookupError:
            return None, None
        except OSError as error:
            return None, type(error).__name__
        current, complete = cls._live_processes([identity])
        if not complete:
            os.close(pidfd)
            return None, "process_identity_unverified"
        if not current:
            os.close(pidfd)
            return None, None
        return pidfd, None

    @classmethod
    def _kill_process_identities(
        cls, identities: Sequence[tuple[int, int]]
    ) -> tuple[bool, str | None]:
        sender = getattr(signal, "pidfd_send_signal", None)
        if sender is None:
            return False, "pidfd_signal_unavailable"
        cleanup_error = None
        deadline = time.monotonic() + 0.25
        while True:
            members, complete = cls._live_processes(identities)
            if not complete:
                return False, "process_identity_unverified"
            if not members:
                return True, cleanup_error
            for identity in members:
                pidfd, open_error = cls._open_verified_pidfd(identity)
                if pidfd is None:
                    if open_error is not None:
                        return False, open_error
                    continue
                try:
                    sender(pidfd, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                except OSError as error:
                    cleanup_error = type(error).__name__
                finally:
                    os.close(pidfd)
            if time.monotonic() >= deadline:
                break
            time.sleep(min(0.01, max(0, deadline - time.monotonic())))
        members, complete = cls._live_processes(identities)
        if complete and not members:
            return True, cleanup_error
        return False, cleanup_error or "process_members_remain"

    @classmethod
    def _kill_process_group_members(
        cls,
        pgid: int,
        *,
        required_identity: tuple[int, int] | None = None,
    ) -> tuple[bool, str | None]:
        if not Path("/proc").is_dir():
            return False, "process_group_identity_unavailable"
        identities, complete = cls._process_group_identities(pgid)
        if not complete:
            return False, "process_group_inspection_failed"
        cleanup_error = None
        known = {pid: starttime for pid, starttime in identities}
        for attempt in range(5):
            if attempt:
                identities, complete = cls._process_group_identities(pgid)
                if not complete:
                    return False, "process_group_inspection_failed"
            if not identities:
                return True, cleanup_error
            if any(known.get(pid) != starttime for pid, starttime in identities):
                return False, "process_group_identity_unverified"
            if attempt == 0 and required_identity is not None and required_identity not in identities:
                return False, "process_group_identity_unverified"
            killed, cleanup_error = cls._kill_process_identities(identities)
            if not killed:
                return False, cleanup_error
        members, complete = cls._process_group_members(pgid)
        if complete and not members:
            return True, cleanup_error
        return False, cleanup_error or "process_group_members_remain"

    @classmethod
    def _kill_verified_process_group_members(
        cls, pgid: int, tracked: Mapping[int, int]
    ) -> tuple[bool, str | None]:
        members, complete = cls._process_group_identities(pgid)
        if not complete:
            return False, "process_group_inspection_failed"
        if any(tracked.get(pid) != starttime for pid, starttime in members):
            return False, "process_group_identity_unverified"
        cleanup_error = None
        for _ in range(5):
            if not members:
                return True, cleanup_error
            killed, cleanup_error = cls._kill_process_identities(members)
            if not killed:
                return False, cleanup_error
            members, complete = cls._process_group_identities(pgid)
            if not complete:
                return False, "process_group_inspection_failed"
            if any(tracked.get(pid) != starttime for pid, starttime in members):
                return False, "process_group_identity_unverified"
        return (not members), "process_group_members_remain" if members else None

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
        procfs_available = Path("/proc").is_dir()
        owner_pid = os.getpid()
        owner_baseline: dict[int, int] = {}
        tracked_owned_members: dict[int, int] = {}
        owner_tracking_enabled = False
        owner_tracking_error: str | None = None
        if procfs_available:
            owner_tracking_enabled = self._enable_child_subreaper()
            if not owner_tracking_enabled:
                owner_tracking_error = "process_owner_boundary_unavailable"
            else:
                baseline_members, baseline_complete = self._process_tree_members(owner_pid)
                if not baseline_complete:
                    owner_tracking_enabled = False
                    owner_tracking_error = "process_owner_tree_inspection_failed"
                else:
                    owner_baseline = dict(baseline_members)
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
        stderr_truncated = False
        selector = selectors.DefaultSelector()
        streams = ((process.stdout, "stdout"), (process.stderr, "stderr"))
        for stream, name in streams:
            if stream is not None:
                selector.register(stream, selectors.EVENT_READ, name)

        tracked_group_members: dict[int, int] = {}
        tracked_group_valid = False
        leader_identity: tuple[int, int] | None = None
        if procfs_available:
            leader_identity = self._process_identity(process.pid)

        # The subreaper owner tree is the containment boundary for descendants
        # that leave the original process group or outlive the command leader.
        def track_process_tree() -> None:
            nonlocal leader_identity, tracked_group_valid
            nonlocal owner_tracking_enabled, owner_tracking_error
            if not procfs_available:
                return
            if owner_tracking_enabled:
                _, owner_complete, discovery_error = self._discover_owned_processes(
                    owner_pid, owner_baseline, tracked_owned_members
                )
                if not owner_complete:
                    owner_tracking_enabled = False
                    owner_tracking_error = discovery_error
            if leader_identity is None and process.poll() is None:
                leader_identity = self._process_identity(process.pid)
            group_members, group_complete = self._process_group_identities(process.pid)
            if not group_complete or not group_members:
                return
            if leader_identity is None:
                return
            for pid, starttime in group_members:
                previous = tracked_group_members.get(pid)
                if previous is not None and previous != starttime:
                    tracked_group_valid = False
                    return
                tracked_group_members[pid] = starttime
            tracked_group_valid = True

        track_process_tree()

        def retain_stdout(chunk: bytes) -> None:
            nonlocal stdout_truncated
            available = max(0, stdout_limit - len(stdout))
            if available:
                stdout.extend(chunk[:available])
            if len(chunk) > available:
                stdout_truncated = True

        def retain_stderr(chunk: bytes) -> None:
            nonlocal stderr_truncated
            stderr.extend(chunk)
            if len(stderr) > stderr_limit:
                stderr_truncated = True
                del stderr[:-stderr_limit]

        def read_ready(events) -> None:
            for key, _ in events:
                try:
                    chunk = os.read(key.fileobj.fileno(), 65536)
                except (OSError, ValueError):
                    chunk = b""
                if not chunk:
                    selector.unregister(key.fileobj)
                    key.fileobj.close()
                    continue
                if key.data == "stdout":
                    retain_stdout(chunk)
                else:
                    retain_stderr(chunk)

        started = time.monotonic()
        try:
            while selector.get_map():
                remaining = timeout - (time.monotonic() - started)
                if remaining <= 0:
                    raise subprocess.TimeoutExpired(argv[0], timeout)
                track_process_tree()
                events = selector.select(min(0.1, remaining))
                if not events:
                    continue
                read_ready(events)
            remaining = timeout - (time.monotonic() - started)
            if remaining <= 0:
                raise subprocess.TimeoutExpired(argv[0], timeout)
            returncode = process.wait(timeout=remaining)
            if procfs_available:
                if not owner_tracking_enabled:
                    raise ControlError(
                        "command_failed",
                        f"command ownership could not be verified: {argv[0]}",
                        {
                            "returncode": returncode,
                            "process_group_killed": False,
                            "process_group_cleanup_error": owner_tracking_error or "process_owner_boundary_unavailable",
                        },
                    )
                owned_members, owner_complete, discovery_error = self._discover_owned_processes(
                    owner_pid, owner_baseline, tracked_owned_members
                )
                if not owner_complete:
                    raise ControlError(
                        "command_failed",
                        f"command ownership could not be verified: {argv[0]}",
                        {
                            "returncode": returncode,
                            "process_group_killed": False,
                            "process_group_cleanup_error": discovery_error or "process_owner_tree_inspection_failed",
                        },
                    )
                owned_live, live_complete = self._live_processes(owned_members)
                if not live_complete:
                    raise ControlError(
                        "command_failed",
                        f"command ownership could not be verified: {argv[0]}",
                        {
                            "returncode": returncode,
                            "process_group_killed": False,
                            "process_group_cleanup_error": "process_owner_identity_unverified",
                        },
                    )
                if owned_live:
                    owner_killed, owner_cleanup_error = self._kill_owned_processes(
                        owner_pid, owner_baseline, tracked_owned_members
                    )
                    raise ControlError(
                        "command_failed",
                        f"command left child processes behind: {argv[0]}",
                        {
                            "returncode": returncode,
                            "process_group_killed": owner_killed,
                            "process_group_cleanup_error": owner_cleanup_error or "process_owner_members_remain",
                        },
                    )
        except subprocess.TimeoutExpired as exc:
            reaped = False
            process_group_killed = False
            process_group_cleanup_error = None
            root_identity = leader_identity if Path("/proc").is_dir() else None
            root_live: list[tuple[int, int]] = []
            root_verified = not Path("/proc").is_dir()
            root_identity_missing = leader_identity is not None and Path("/proc").is_dir()
            if root_identity is not None:
                root_live, root_identity_complete = self._live_processes([root_identity])
                root_verified = bool(root_live)
                root_identity_missing = root_identity_complete and not root_live
            elif leader_identity is not None and Path("/proc").is_dir():
                root_live, root_identity_complete = self._live_processes([leader_identity])
                root_identity_missing = root_identity_complete and not root_live
            if root_verified:
                try:
                    if Path("/proc").is_dir():
                        os.killpg(process.pid, 0)
                        process_group_killed, fallback_error = self._kill_process_group_members(
                            process.pid, required_identity=leader_identity
                        )
                        if fallback_error:
                            process_group_cleanup_error = fallback_error
                    else:
                        os.killpg(process.pid, signal.SIGKILL)
                        process_group_killed = True
                except ProcessLookupError:
                    if Path("/proc").is_dir():
                        process_group_killed, fallback_error = self._kill_process_group_members(
                            process.pid, required_identity=leader_identity
                        )
                        if fallback_error:
                            process_group_cleanup_error = fallback_error
                    else:
                        process_group_killed = True
                except OSError as group_error:
                    process_group_cleanup_error = type(group_error).__name__
                    if Path("/proc").is_dir():
                        process_group_killed, fallback_error = self._kill_process_group_members(
                            process.pid, required_identity=leader_identity
                        )
                        if fallback_error:
                            process_group_cleanup_error += ":" + fallback_error
                    else:
                        try:
                            process.kill()
                        except ProcessLookupError:
                            pass
            elif root_identity_missing and tracked_group_valid:
                process_group_killed, process_group_cleanup_error = self._kill_verified_process_group_members(
                    process.pid, tracked_group_members
                )
            else:
                process_group_cleanup_error = "process_identity_unverified"
            if procfs_available:
                if owner_tracking_enabled:
                    owner_killed, owner_cleanup_error = self._kill_owned_processes(
                        owner_pid, owner_baseline, tracked_owned_members
                    )
                else:
                    owner_killed = False
                    owner_cleanup_error = owner_tracking_error or "process_owner_boundary_unavailable"
                if not owner_killed:
                    process_group_killed = False
                    process_group_cleanup_error = (
                        f"{process_group_cleanup_error}:{owner_cleanup_error}"
                        if process_group_cleanup_error else owner_cleanup_error
                    )
            pipes_drained = False
            try:
                process.wait(timeout=1)
                reaped = True
            except (subprocess.TimeoutExpired, ChildProcessError):
                pass
            drain_deadline = time.monotonic() + 1
            while selector.get_map() and time.monotonic() < drain_deadline:
                events = selector.select(max(0, drain_deadline - time.monotonic()))
                if not events:
                    break
                read_ready(events)
            pipes_drained = not selector.get_map()
            raise ControlError(
                "command_timeout",
                f"command timed out: {argv[0]}",
                {
                    "stdout": bytes(stdout).decode("utf-8", "replace"),
                    "stderr": bytes(stderr).decode("utf-8", "replace"),
                    "stdout_truncated": stdout_truncated,
                    "stderr_truncated": stderr_truncated,
                    "reaped": reaped,
                    "pipes_drained": pipes_drained,
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
        result.stderr_truncated = stderr_truncated
        if check and returncode != 0:
            detail = {
                "returncode": returncode,
                "stderr": result.stderr.decode("utf-8", "replace").strip()[-4000:],
                "stdout_truncated": stdout_truncated,
                "stderr_truncated": stderr_truncated,
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
        bounded: bool = False,
    ) -> subprocess.CompletedProcess[bytes]:
        command = self.prefix + list(args)
        if bounded:
            if input_data is not None:
                raise ControlError("invalid_request", "bounded Incus commands do not accept stdin")
            return self.runner.run_bounded(
                command,
                timeout=timeout,
                stdout_limit=DIAGNOSTIC_LOG_LIMIT,
                stderr_limit=4000,
                check=check,
            )
        return self.runner.run(command, input_data=input_data, timeout=timeout, check=check)

    def json(self, args: Sequence[str], *, timeout: float = 60) -> Any:
        result = self.call([*args, "--format", "json"], timeout=timeout)
        try:
            return json.loads(result.stdout.decode("utf-8"))
        except json.JSONDecodeError as exc:
            raise ControlError("incus_invalid_output", "Incus returned invalid JSON") from exc

    def list_instances(self) -> list[dict[str, Any]]:
        value = self.json(["list"])
        if (
            not isinstance(value, list)
            or any(
                not isinstance(item, dict)
                or not isinstance(item.get("name"), str)
                or not item["name"]
                for item in value
            )
        ):
            raise ControlError("incus_invalid_output", "Incus list output is not an array")
        return value

    def instance_info(
        self, name: str, *, timeout: float = 30, bounded: bool = False
    ) -> dict[str, Any]:
        value = self.query(
            f"/1.0/instances/{quote(name, safe='')}/state",
            timeout=timeout,
            bounded=bounded,
        )
        if not isinstance(value, dict) or not isinstance(value.get("status"), str) or not value["status"]:
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
            ],
            timeout=NATIVE_INIT_TIMEOUT_SECONDS,
            bounded=True,
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
        bounded: bool = False,
    ) -> subprocess.CompletedProcess[bytes]:
        if bounded:
            if input_data is not None:
                raise ControlError("invalid_request", "bounded Incus exec does not accept input")
            return self.runner.run_bounded(
                self.prefix + ["exec", name, "--", *argv],
                timeout=timeout,
                stdout_limit=DIAGNOSTIC_LOG_LIMIT,
                stderr_limit=4000,
                check=check,
            )
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
        self.connection: sqlite3.Connection | None = None
        self.depth = 0
        self._unusable = False
        phase = "connect"
        try:
            self.connection = sqlite3.connect(str(path), timeout=10, check_same_thread=False)
            self.path.chmod(0o600)
            self.connection.row_factory = sqlite3.Row
            phase = "wal-init"
            self.connection.execute("PRAGMA journal_mode=WAL")
            self.connection.execute("PRAGMA synchronous=FULL")
            self.connection.execute("PRAGMA foreign_keys=ON")
            phase = "schema-init"
            self._schema()
        except sqlite3.Error as exc:
            if self.connection is not None:
                self.connection.close()
            raise self._sqlite_failure(exc, phase=phase, sql_operation="initialize", state_committed=False) from exc

    @staticmethod
    def _sqlite_failure(
        error: sqlite3.Error,
        *,
        phase: str,
        sql_operation: str,
        state_committed: bool | None,
        rollback_error: sqlite3.Error | None = None,
    ) -> ControlError:
        detail = {
            "phase": phase,
            "sqlite_code": getattr(error, "sqlite_errorcode", None),
            "sqlite_name": getattr(error, "sqlite_errorname", type(error).__name__),
            "sql_operation": sql_operation,
            "state_committed": state_committed,
        }
        if rollback_error is not None:
            detail["rollback_error"] = {
                "sqlite_code": getattr(rollback_error, "sqlite_errorcode", None),
                "sqlite_name": getattr(rollback_error, "sqlite_errorname", type(rollback_error).__name__),
            }
        return ControlError("sqlite_unavailable", "state database cannot be verified", detail)

    def _ensure_usable(self, *, phase: str, sql_operation: str) -> None:
        if getattr(self, "_unusable", False):
            raise ControlError(
                "sqlite_unavailable",
                "state database connection is unusable and must be reopened",
                {"phase": phase, "sql_operation": sql_operation, "state_committed": None},
            )

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
            self._ensure_usable(phase="connection", sql_operation="write")
            outer = self.depth == 0
            if outer:
                try:
                    self.connection.execute("BEGIN IMMEDIATE")
                except sqlite3.Error as exc:
                    raise self._sqlite_failure(exc, phase="begin", sql_operation="write", state_committed=False) from exc
            self.depth += 1
            try:
                yield
            except sqlite3.Error as exc:
                rollback_error = None
                if outer:
                    try:
                        self.connection.rollback()
                    except sqlite3.Error as rollback_exc:
                        rollback_error = rollback_exc
                        self._unusable = True
                raise self._sqlite_failure(
                    exc,
                    phase="write",
                    sql_operation="write",
                    state_committed=None if rollback_error else False,
                    rollback_error=rollback_error,
                ) from exc
            except BaseException as exc:
                if outer:
                    try:
                        self.connection.rollback()
                    except sqlite3.Error as rollback_error:
                        self._unusable = True
                        failure = self._sqlite_failure(
                            rollback_error,
                            phase="rollback",
                            sql_operation="write",
                            state_committed=None,
                        )
                        failure.detail["original_error"] = {
                            "type": type(exc).__name__,
                            "message": str(exc),
                        }
                        raise failure from exc
                raise
            else:
                if outer:
                    try:
                        self.connection.commit()
                    except sqlite3.Error as exc:
                        rollback_error = None
                        try:
                            self.connection.rollback()
                        except sqlite3.Error as rollback_exc:
                            rollback_error = rollback_exc
                            self._unusable = True
                        raise self._sqlite_failure(
                            exc,
                            phase="commit",
                            sql_operation="write",
                            state_committed=None,
                            rollback_error=rollback_error,
                        ) from exc
            finally:
                self.depth -= 1

    def _row(self, query: str, args: Sequence[Any] = ()) -> sqlite3.Row | None:
        with self._lock:
            self._ensure_usable(phase="read", sql_operation="read")
            try:
                return self.connection.execute(query, args).fetchone()
            except sqlite3.Error as exc:
                raise self._sqlite_failure(exc, phase="read", sql_operation="read", state_committed=None) from exc

    def _rows(self, query: str, args: Sequence[Any] = ()) -> list[sqlite3.Row]:
        with self._lock:
            self._ensure_usable(phase="read", sql_operation="read")
            try:
                return list(self.connection.execute(query, args).fetchall())
            except sqlite3.Error as exc:
                raise self._sqlite_failure(exc, phase="read", sql_operation="read", state_committed=None) from exc

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
        try:
            value = json.loads(row["response_json"])
        except (TypeError, ValueError) as exc:
            raise ControlError("operation_unverified", "operation journal record is invalid",
                               {"operation_id": operation_id}) from exc
        if not isinstance(value, Mapping):
            raise ControlError("operation_unverified", "operation journal record is not an object",
                               {"operation_id": operation_id})
        record = dict(value)
        digest = record.get("_request_digest")
        if not isinstance(digest, str) or not digest:
            raise ControlError("operation_unverified", "operation journal digest is invalid",
                               {"operation_id": operation_id})
        outcome = record.get("outcome")
        if outcome == "pending":
            return record
        if outcome == "error":
            error = record.get("error")
            if (not isinstance(error, Mapping)
                    or not isinstance(error.get("code"), str)
                    or not error["code"]
                    or not isinstance(error.get("message"), str)
                    or not error["message"]):
                raise ControlError("operation_unverified", "operation journal error is invalid",
                                   {"operation_id": operation_id})
            return record
        if outcome == "success":
            if (record.get("ok") is not True
                    or record.get("protocol") != PROTOCOL_VERSION
                    or not isinstance(record.get("action"), str)
                    or not record["action"]
                    or not isinstance(record.get("operation_id"), str)
                    or not record["operation_id"]
                    or not isinstance(record.get("result"), Mapping)):
                raise ControlError("operation_unverified", "operation journal success is invalid",
                                   {"operation_id": operation_id})
            return record
        raise ControlError("operation_unverified", "operation journal outcome is invalid",
                           {"operation_id": operation_id})

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

    def audit_summary(self) -> dict[str, dict[str, int]]:
        operations: Counter[str] = Counter()
        image_unpack_failures: Counter[str] = Counter()
        warnings: Counter[str] = Counter()

        def mappings(value: Any):
            if isinstance(value, Mapping):
                yield value
                for child in value.values():
                    yield from mappings(child)
            elif isinstance(value, list):
                for child in value:
                    yield from mappings(child)

        for row in self._rows("SELECT action, outcome, detail_json FROM audit"):
            detail: Mapping[str, Any] = {}
            if row["detail_json"]:
                try:
                    parsed = json.loads(row["detail_json"])
                    if isinstance(parsed, Mapping):
                        detail = parsed
                except (TypeError, json.JSONDecodeError):
                    detail = {}
            nested = list(mappings(detail))

            def deepest(value: Any, keys: tuple[str, ...]) -> str | None:
                if isinstance(value, Mapping):
                    for branch in (value.get("error"), value.get("detail")):
                        result = deepest(branch, keys)
                        if result is not None:
                            return result
                    for key in keys:
                        if value.get(key) is not None:
                            return str(value[key])
                    for child in value.values():
                        result = deepest(child, keys)
                        if result is not None:
                            return result
                elif isinstance(value, list):
                    for child in value:
                        result = deepest(child, keys)
                        if result is not None:
                            return result
                return None

            code = deepest(detail, ("code",)) or "unknown"
            phase = deepest(detail, ("phase", "operation_phase")) or "unknown"
            operations[f"{row['action']}|{row['outcome']}|{phase}"] += 1
            for item in nested:
                warning_list = item.get("warnings")
                if isinstance(warning_list, list):
                    for warning in warning_list:
                        if isinstance(warning, Mapping) and warning.get("code"):
                            warnings[str(warning["code"])] += 1
            if row["outcome"] == "error" and code in {"memory_pressure", "resource_unverified", "capacity_insufficient"}:
                warnings[code] += 1
            image_text = " ".join(
                str(item.get(key, ""))
                for item in nested
                for key in ("code", "phase", "operation_phase", "message")
            ).lower()
            if row["outcome"] == "error" and "unpack" in image_text and any(
                    token in image_text for token in ("image", "rootfs")):
                image_unpack_failures[f"{phase}|{code}"] += 1
        return {"operations": dict(operations), "image_unpack_failures": dict(image_unpack_failures),
                "warnings": dict(warnings)}

    def pending_operations(self) -> list[dict[str, Any]]:
        pending = []
        for row in self._rows(
            "SELECT operation_id, action, session_id, generation, response_json, created_at "
            "FROM operations ORDER BY created_at, operation_id"
        ):
            try:
                response = json.loads(row["response_json"])
            except (TypeError, json.JSONDecodeError) as exc:
                raise ControlError("operation_unverified", "operation journal response is malformed") from exc
            if not isinstance(response, Mapping) or response.get("outcome") != "pending":
                continue
            pending.append({
                "operation_id": row["operation_id"],
                "action": row["action"],
                "session_id": row["session_id"],
                "generation": row["generation"],
                "created_at": row["created_at"],
            })
        return pending

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
        self.static_unit_dir = DEFAULT_TIMER_DIR

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

    def expiry_active(self, task_id: str, generation: int, expires_at: str) -> bool:
        unit = self.timer_unit(task_id, generation)
        service_unit = f"agent-vm-native-expire@{self.instance_name(task_id, generation)}.service"
        if not self.enabled:
            return True
        path = self.timer_dir / unit
        if path.is_symlink() or not path.is_file():
            return False
        try:
            expected_calendar = "OnCalendar=" + parse_time(expires_at).strftime("%Y-%m-%d %H:%M:%S UTC")
            content = path.read_text(encoding="utf-8")
            lines = content.splitlines()
            result = self.runner.run(
                ["systemctl", "show", unit, "-p", "ActiveState", "-p", "FragmentPath", "-p", "UnitFileState"],
                timeout=10,
                check=False,
            )
        except (ControlError, OSError, TypeError, ValueError):
            return False
        values = dict(
            line.split("=", 1)
            for line in result.stdout.decode(errors="replace").splitlines()
            if "=" in line
        )
        return (
            result.returncode == 0
            and values.get("ActiveState") == "active"
            and values.get("FragmentPath") == str(path)
            and values.get("UnitFileState") == "enabled"
            and lines.count(f"Unit={service_unit}") == 1
            and lines.count(expected_calendar) == 1
            and lines.count("Persistent=yes") == 1
        )

    def service_asset_active(self, unit: str, required_lines: Sequence[str]) -> bool:
        if not self.enabled:
            return True
        path = self.static_unit_dir / unit
        if path.is_symlink() or not path.is_file():
            return False
        try:
            lines = path.read_text(encoding="utf-8").splitlines()
            if unit.endswith("@.service"):
                result = self.runner.run(["systemctl", "cat", unit], timeout=10, check=False)
                output = result.stdout.decode(errors="replace")
                return (
                    result.returncode == 0
                    and f"# {path}\n" in output
                    and all(line in lines for line in required_lines)
                )
            result = self.runner.run(
                ["systemctl", "show", unit, "-p", "LoadState", "-p", "FragmentPath"],
                timeout=10,
                check=False,
            )
        except (ControlError, OSError, TypeError, ValueError):
            return False
        values = dict(
            line.split("=", 1)
            for line in result.stdout.decode(errors="replace").splitlines()
            if "=" in line
        )
        return (
            result.returncode == 0
            and values.get("LoadState") == "loaded"
            and values.get("FragmentPath") == str(path)
            and all(line in lines for line in required_lines)
        )

    def enabled_unit_active(self, unit: str) -> bool:
        if not self.enabled:
            return True
        try:
            result = self.runner.run(
                ["systemctl", "show", unit, "-p", "ActiveState", "-p", "UnitFileState"],
                timeout=10,
                check=False,
            )
        except (ControlError, OSError, TypeError, ValueError):
            return False
        values = dict(
            line.split("=", 1)
            for line in result.stdout.decode(errors="replace").splitlines()
            if "=" in line
        )
        return (
            result.returncode == 0
            and values.get("ActiveState") == "active"
            and values.get("UnitFileState") == "enabled"
        )

    def enabled_service_ready(self, unit: str) -> bool:
        """Verify a loaded, enabled service without requiring a oneshot to stay active."""
        if not self.enabled:
            return True
        try:
            result = self.runner.run(
                ["systemctl", "show", unit, "-p", "LoadState", "-p", "UnitFileState"],
                timeout=10,
                check=False,
            )
        except (ControlError, OSError, TypeError, ValueError):
            return False
        values = dict(
            line.split("=", 1)
            for line in result.stdout.decode(errors="replace").splitlines()
            if "=" in line
        )
        return (
            result.returncode == 0
            and values.get("LoadState") == "loaded"
            and values.get("UnitFileState") == "enabled"
        )

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

    @classmethod
    def _is_write_action(cls, action: str, request: Mapping[str, Any] | None = None) -> bool:
        # A dry-run reconciliation is an observation used by release health
        # checks.  Applying reconciliation remains a journaled write.
        if (
            action == "admin-reconcile"
            and request is not None
            and request.get("apply", False) is False
            and "operation_id" not in request
            and "generation" not in request
        ):
            return False
        return action in cls.WRITE_ACTIONS

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
        configured_manifest = self.config.get("release_manifest_path")
        self.release_manifest_path = Path(str(configured_manifest)) if configured_manifest else DEFAULT_RELEASE_MANIFEST_PATH
        self.release_fence_path = Path(str(self.config.get("release_fence_path", DEFAULT_RELEASE_FENCE_PATH)))
        self.release_manifest: dict[str, Any] | None = None
        self.release_manifest_present = False
        self.release_artifacts_verified = True
        self.release_manifest_error: ControlError | None = None
        self.release_warnings: list[dict[str, Any]] = []
        try:
            (
                self.release_manifest,
                self.release_manifest_present,
                self.release_artifacts_verified,
                self.release_warnings,
            ) = load_runtime_manifest(self.release_manifest_path)
        except ManifestError as exc:
            self.release_manifest_error = ControlError(exc.code, exc.message, exc.detail)
        if self.release_manifest_error is None and self.release_manifest is not None:
            try:
                verify_runtime_bundle(
                    self.release_manifest,
                    runtime_artifact_paths(self.config, self.release_manifest_path, self.release_manifest),
                )
            except ManifestError as exc:
                self.release_artifacts_verified = False
                self.release_manifest_error = ControlError(exc.code, exc.message, exc.detail)
        if self.release_manifest_error is not None and store is None:
            # Do not open SQLite/WAL or create the state directory for an
            # invalid installed release.  Tests may inject a store to inspect
            # the preflight error without changing this production boundary.
            raise self.release_manifest_error
        self.store = store or StateStore(Path(str(self.config["state_db"])))
        self.incus = IncusClient(self.runner, self.config)
        self.systemd = systemd or SystemdManager(self.runner, self.config)
        self.clock = clock
        self.resources = resources or CTResources()
        self.lock_path = Path(str(self.config["state_db"])).with_name("control.lock")
        self.admission_lock_path = Path(str(self.config["state_db"])).with_name("admission.lock")
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

    @contextmanager
    def _admission_lock(self):
        self.admission_lock_path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        fd = os.open(self.admission_lock_path, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
        try:
            fcntl.flock(fd, fcntl.LOCK_EX)
            yield
        finally:
            fcntl.flock(fd, fcntl.LOCK_UN)
            os.close(fd)

    def now(self) -> dt.datetime:
        return self.clock().astimezone(dt.timezone.utc)

    def _release_metadata(self) -> dict[str, Any]:
        return manifest_metadata(
            self.release_manifest,
            present=self.release_manifest_present,
            verified=self.release_artifacts_verified,
            legacy=self.release_manifest is None,
        )

    @staticmethod
    def _response_compatibility() -> dict[str, int]:
        return {"schema": SCHEMA_VERSION, "semantic_revision": SEMANTIC_REVISION}

    def _release_activation_fenced(self) -> bool:
        try:
            info = self.release_fence_path.lstat()
        except FileNotFoundError:
            return False
        except OSError as exc:
            raise ControlError("release_fence_unverified", "release activation fence cannot be inspected") from exc
        if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode) or (
            self.release_fence_path == DEFAULT_RELEASE_FENCE_PATH
            and (info.st_uid != 0 or stat.S_IMODE(info.st_mode) != 0o600)
        ):
            raise ControlError("release_fence_unverified", "release activation fence is not a regular file")
        return True

    @staticmethod
    def _merge_warnings(*groups: Iterable[Mapping[str, Any]]) -> list[dict[str, Any]]:
        result: list[dict[str, Any]] = []
        seen: set[tuple[Any, Any, Any]] = set()
        for group in groups:
            for item in group:
                value = dict(item)
                identity = (value.get("code"), value.get("action"), value.get("message"))
                if identity not in seen:
                    seen.add(identity)
                    result.append(value)
        return result

    def _preflight_release(self, request: Mapping[str, Any]) -> list[dict[str, Any]]:
        """Check release identity and compatibility before locks/journals/effects."""
        if self.release_manifest_error is not None:
            raise self.release_manifest_error
        if request.get("protocol") != PROTOCOL_VERSION:
            raise ControlError("protocol_mismatch", f"protocol must be {PROTOCOL_VERSION}")
        action = request.get("action")
        if not isinstance(action, str) or not action:
            raise ControlError("invalid_request", "action must be a non-empty string")
        if action not in ACTION_NAMES:
            raise ControlError("unknown_action", f"unknown action: {action}")
        manifest = self.release_manifest or legacy_manifest()
        if self.release_manifest is not None:
            try:
                verify_runtime_bundle(
                    self.release_manifest,
                    runtime_artifact_paths(self.config, self.release_manifest_path, self.release_manifest),
                )
            except ManifestError as exc:
                raise ControlError(exc.code, exc.message, exc.detail) from exc
        try:
            warnings = check_compatibility(manifest, action, request.get("compatibility"))
        except ManifestError as exc:
            raise ControlError(exc.code, exc.message, exc.detail) from exc
        if self.release_manifest is None:
            warnings.append({
                "code": "legacy_control_release",
                "action": action,
                "message": "control release manifest is absent; using legacy protocol-4 compatibility",
            })
        return self._merge_warnings(self.release_warnings, warnings)

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
        compatibility_warnings = self._preflight_release(request)
        # Resolve timer ownership from the durable record, never from a guessed
        # resource name. The same object lock serializes native operations.
        session = request.get("session_id")
        if action == "timer-expire":
            task = self.store.get_task(task_id=str(request.get("task_id")))
            session = task["session_id"] if task else "missing-timer"
            if request.get("protocol") != PROTOCOL_VERSION:
                raise ControlError("protocol_mismatch", "unsupported timer protocol")
        identity = "session:" + str(session) if action in self.SESSION_ACTIONS or action == "timer-expire" else "admin"
        with self._object_lock(identity):
            if action == "timer-expire":
                # Serialize expiry with the owner operation before changing its
                # durable state. Native cleanup must not race a slow acquire,
                # restart, resize, or proxy mutation.
                self._expire_preempt(request)
            if self._is_write_action(action, request):
                op = require_nonempty_string(request.get("operation_id"), "operation_id", SESSION_ID_RE)
                with self._object_lock("operation:" + op):
                    return self._dispatch_locked(request, compatibility_warnings)
            return self._dispatch_locked(request, compatibility_warnings)

    def _annotate_state_error(
        self,
        error: ControlError,
        *,
        action: str,
        operation_id: str | None,
    ) -> ControlError:
        if error.code != "sqlite_unavailable" or not operation_id or action not in self.WRITE_ACTIONS:
            return error
        detail = dict(error.detail) if isinstance(error.detail, Mapping) else {}
        journal_lookup_failed = False
        try:
            journal = self.store.operation(operation_id)
        except ControlError:
            journal = None
            journal_lookup_failed = True
        if journal_lookup_failed:
            detail["state_committed"] = None
        elif journal is None:
            detail["state_committed"] = detail.get("state_committed")
        elif journal.get("outcome") in {"success", "error"}:
            detail["state_committed"] = True
        else:
            detail["state_committed"] = None
        return ControlError(error.code, error.message, detail)

    @staticmethod
    def _native_init_absence_retry_record(task: Mapping[str, Any]) -> Mapping[str, Any] | None:
        raw_error = task.get("last_error")
        if not isinstance(raw_error, str) or not raw_error:
            return None
        try:
            error = json.loads(raw_error)
        except (TypeError, ValueError):
            return None
        if not isinstance(error, Mapping) or error.get("native_init_absence_retry") is not True:
            return None
        return error

    @classmethod
    def _native_init_absence_retry_pending(cls, task: Mapping[str, Any]) -> bool:
        return cls._native_init_absence_retry_record(task) is not None

    def _native_init_absence_retry_ready(self, task: Mapping[str, Any]) -> bool:
        record = self._native_init_absence_retry_record(task)
        if record is None:
            return False
        try:
            retry_after = parse_time(record.get("native_init_absence_retry_after"))
        except (AttributeError, ControlError, TypeError, ValueError):
            return False
        if retry_after is None:
            return False
        return retry_after <= self.now()

    def _native_init_retry_after(self, task: Mapping[str, Any]) -> str:
        record = self._native_init_absence_retry_record(task) or {}
        try:
            retry_after = parse_time(record.get("native_init_absence_retry_after"))
        except (AttributeError, ControlError, TypeError, ValueError):
            retry_after = None
        if retry_after is None or retry_after <= self.now():
            retry_after = self.now() + dt.timedelta(seconds=NATIVE_INIT_ABSENCE_RETRY_SECONDS)
        return iso_time(retry_after)

    @staticmethod
    def _native_init_absence_checks(task: Mapping[str, Any]) -> int:
        record = Control._native_init_absence_retry_record(task) or {}
        value = record.get("native_init_absence_checks", 0)
        if isinstance(value, bool) or not isinstance(value, int) or value < 0:
            return 0
        return value

    @staticmethod
    def _native_init_cleanup_verified(task: Mapping[str, Any]) -> bool | None:
        record = Control._native_init_absence_retry_record(task) or {}
        value = record.get("native_init_cleanup_verified")
        return value if isinstance(value, bool) else None

    def _pending_native_init_for_task(self, task: Mapping[str, Any]) -> Mapping[str, Any] | None:
        try:
            pending = self.store.pending_operations()
        except ControlError as exc:
            raise ControlError(
                "operation_unverified",
                "pending acquire operations cannot be verified",
                {"task_id": task["task_id"], "cause": exc.as_dict()},
            ) from exc
        matches = [
            item for item in pending
            if item.get("action") == "acquire"
            and item.get("session_id") == task.get("session_id")
            and item.get("generation") in {
                task.get("generation"),
                task.get("generation") - 1
                if isinstance(task.get("generation"), int) and not isinstance(task.get("generation"), bool)
                else None,
            }
        ]
        if len(matches) > 1:
            raise ControlError(
                "operation_unverified",
                "multiple pending native initialization operations match the task",
                {"task_id": task["task_id"]},
            )
        return matches[0] if matches else None

    def _recovered_native_init_operation(
        self,
        task: Mapping[str, Any],
        pending: Mapping[str, Any],
    ) -> tuple[str, dict[str, Any]]:
        operation_id = pending.get("operation_id")
        if not isinstance(operation_id, str) or not operation_id:
            raise ControlError("operation_unverified", "pending acquire operation ID is invalid")
        record = self.store.operation(operation_id)
        digest = record.get("_request_digest") if isinstance(record, Mapping) else None
        if not isinstance(digest, str) or not digest:
            raise ControlError(
                "operation_unverified",
                "pending acquire request digest is invalid",
                {"operation_id": operation_id},
            )
        retry_record = self._native_init_absence_retry_record(task) or {}
        original = retry_record.get("error")
        if isinstance(original, Mapping) and isinstance(original.get("code"), str) and original.get("code"):
            code = original["code"]
            message = original.get("message")
            detail = original.get("detail")
            if not isinstance(message, str) or not message:
                message = "native initialization did not complete"
            recovered_detail = dict(detail) if isinstance(detail, Mapping) else {}
        else:
            code = "acquire_partial"
            message = "native initialization did not complete"
            recovered_detail = {}
        recovered_detail.update({
            "phase": "native-init",
            "operation_phase": "native-init",
            "native_init_recovered_absent": True,
            "outcome_verified": True,
        })
        return operation_id, {
            "outcome": "error",
            "error": {"code": code, "message": message, "detail": recovered_detail},
            "_request_digest": digest,
        }

    @staticmethod
    def _native_init_timeout_cleanup_verified(error: ControlError) -> bool | None:
        detail = error.detail if isinstance(error.detail, Mapping) else {}
        cleanup_fields = (
            "process_group_killed",
            "process_group_cleanup_error",
            "reaped",
            "pipes_drained",
        )
        if not any(key in detail for key in cleanup_fields):
            return True
        required = ("process_group_killed",)
        if error.code == "command_timeout":
            required += ("reaped", "pipes_drained")
        return all(detail.get(key) is True for key in required) and not detail.get("process_group_cleanup_error")

    @staticmethod
    def _operation_outcome_uncertain(action: str, error: ControlError) -> bool:
        """Keep a journal pending only when a native effect may have occurred."""
        detail = error.detail if isinstance(error.detail, Mapping) else {}
        if error.code == "sqlite_unavailable" and detail.get("state_committed") is not False:
            return True
        if detail.get("native_init_absence_retry") is True:
            return True
        cleanup = detail.get("cleanup")
        if isinstance(cleanup, Mapping) and cleanup.get("ok") is False:
            return True
        if detail.get("native_init_absence_retry") is False:
            return False
        if isinstance(detail.get("outcome_verified"), bool):
            return not detail["outcome_verified"]
        cleanup_result = cleanup.get("result") if isinstance(cleanup, Mapping) else None
        if (isinstance(cleanup, Mapping) and cleanup.get("ok")
                and isinstance(cleanup_result, Mapping)
                and cleanup_result.get("state") in {"cached", "absent"}):
            return False
        if error.code in {"acquire_partial", "resize_partial", "restart_partial", "cleanup_incomplete", "operation_unverified"}:
            return True
        phases = {str(detail.get(key)) for key in ("phase", "operation_phase") if detail.get(key) is not None}
        native_phases = {
            "native-generation", "native-init", "native-config", "native-stop", "native-start",
            "guest-readiness", "guest-post-check", "access-exposure", "port-add", "port-remove", "cache-delete",
        }
        return bool(phases & native_phases)

    def _expire_preempt(self, request: Mapping[str, Any]) -> None:
        """Close the admission fence and perform a confirmed native withdrawal.

        The public timer dispatch holds the session object lock before calling
        this method, so a slow owner operation cannot change native state while
        expiry is withdrawing it.
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
                    with self._global_lock(), self.store.transaction():
                        latest = self.store.get_task(task_id=task_id)
                        if latest and latest["generation"] == generation and latest["state"] == "closing":
                            self.store.update_task(
                                task_id,
                                last_error=json.dumps({"errors": errors, "completed": completed}),
                            )
        except ControlError as exc:
            with self._global_lock(), self.store.transaction():
                latest = self.store.get_task(task_id=task_id)
                if latest and latest["generation"] == generation and latest["state"] == "closing":
                    self.store.update_task(task_id, last_error=json.dumps(exc.as_dict()))

    def _validate_cached_success(
        self,
        action: str,
        operation_id: str,
        session_id: str | None,
        request: Mapping[str, Any],
        cached: Mapping[str, Any],
    ) -> None:
        """Require a replay record to match both its request and durable state."""
        if (cached.get("action") != action
                or cached.get("operation_id") != operation_id
                or cached.get("session_id") != session_id
                or cached.get("generation") != request.get("generation")):
            raise ControlError("operation_unverified", "operation journal identity does not match the request",
                               {"operation_id": operation_id})
        result = cached.get("result")
        if not isinstance(result, Mapping):
            raise ControlError("operation_unverified", "operation journal result is invalid",
                               {"operation_id": operation_id})
        if action in {"drain-enter", "drain-exit"}:
            expected = action == "drain-enter"
            if result.get("draining") is not expected or (self.store.get_meta("draining") == "1") is not expected:
                raise ControlError("operation_unverified", "drain state does not match the journal result",
                                   {"operation_id": operation_id, "expected_draining": expected})
            return
        if action in {"acquire", "renew", "release", "destroy", "restart", "resize"}:
            if session_id is None:
                raise ControlError("operation_unverified", "session-scoped operation has no session",
                                   {"operation_id": operation_id})
            task = self.store.get_task(session_id=session_id)
            if task is None:
                raise ControlError("operation_unverified", "task state is unavailable for journal replay",
                                   {"operation_id": operation_id})
            try:
                current = self._state_summary(task)
            except (ControlError, KeyError, TypeError, ValueError) as exc:
                raise ControlError("operation_unverified", "task state cannot be verified for journal replay",
                                   {"operation_id": operation_id}) from exc
            durable_fields = (
                "task_id", "session_id", "generation", "state", "instance_name", "vm_ip",
                "ssh_host_port", "lease_expires_at", "inactive_since", "cleanup_after",
                "ports", "resources", "resource_reservation", "last_error",
            )
            if any(result.get(key) != current.get(key) for key in durable_fields):
                raise ControlError("operation_unverified", "task state does not match the journal result",
                                   {"operation_id": operation_id})
            if action in {"acquire", "restart", "resize"}:
                try:
                    if task.get("state") != "active":
                        raise ControlError("operation_unverified", "replayed operation is no longer active",
                                           {"operation_id": operation_id})
                    if not self._state_summary(task)["effective_lease"]:
                        raise ControlError("operation_unverified", "replayed operation lease is no longer effective",
                                           {"operation_id": operation_id})
                    self._ownership(task)
                    if self.incus.instance_info(task["instance_name"])["status"].lower() != "running":
                        raise ControlError("operation_unverified", "replayed guest is not running",
                                           {"operation_id": operation_id})
                    if self._native_resources(task) != result.get("resources"):
                        raise ControlError("operation_unverified", "replayed native resources differ from the journal",
                                           {"operation_id": operation_id})
                except ControlError as exc:
                    if exc.code == "operation_unverified":
                        raise
                    raise ControlError("operation_unverified", "replayed native state cannot be verified",
                                       {"operation_id": operation_id}) from exc
                except (KeyError, TypeError, ValueError) as exc:
                    raise ControlError("operation_unverified", "replayed native state cannot be verified",
                                       {"operation_id": operation_id}) from exc
            elif action in {"release", "destroy"}:
                try:
                    if task.get("state") == "cached":
                        self._ownership(task)
                        if self.incus.instance_info(task["instance_name"])["status"].lower() != "stopped":
                            raise ControlError("operation_unverified", "replayed cache is still running",
                                               {"operation_id": operation_id})
                        if self.store.get_ports(task["task_id"]):
                            raise ControlError("operation_unverified", "replayed cache still has ports",
                                               {"operation_id": operation_id})
                        owned = self._ownership(task)
                        if any(device.get("type") == "proxy" for device in owned.get("expanded_devices", {}).values()):
                            raise ControlError("operation_unverified", "replayed cache still has native proxies",
                                               {"operation_id": operation_id})
                    elif task.get("state") == "absent":
                        if any(native.get("name") == task["instance_name"] for native in self.incus.list_instances()):
                            raise ControlError("operation_unverified", "replayed absent instance still exists",
                                               {"operation_id": operation_id})
                    else:
                        raise ControlError("operation_unverified", "replayed cleanup state is invalid",
                                           {"operation_id": operation_id})
                except ControlError as exc:
                    if exc.code == "operation_unverified":
                        raise
                    raise ControlError("operation_unverified", "replayed native cleanup state cannot be verified",
                                       {"operation_id": operation_id}) from exc
                except (KeyError, TypeError, ValueError) as exc:
                    raise ControlError("operation_unverified", "replayed native cleanup state cannot be verified",
                                       {"operation_id": operation_id}) from exc
            return
        if action == "port-add":
            if session_id is None:
                raise ControlError("operation_unverified", "port operation has no session",
                                   {"operation_id": operation_id})
            task = self.store.get_task(session_id=session_id)
            if task is None:
                raise ControlError("operation_unverified", "task state is unavailable for journal replay",
                                   {"operation_id": operation_id})
            try:
                if task.get("state") != "active" or not self._state_summary(task)["effective_lease"]:
                    raise ControlError("operation_unverified", "port replay lease is no longer effective",
                                       {"operation_id": operation_id})
            except ControlError:
                raise
            except (KeyError, TypeError, ValueError) as exc:
                raise ControlError("operation_unverified", "port replay lease cannot be verified",
                                   {"operation_id": operation_id}) from exc
            try:
                generation = int(task["generation"])
            except (KeyError, TypeError, ValueError) as exc:
                raise ControlError("operation_unverified", "task generation cannot be verified for journal replay",
                                   {"operation_id": operation_id}) from exc
            port_name = result.get("port_name")
            protocol = result.get("protocol")
            port = (self.store.get_port(task["task_id"], port_name, protocol)
                    if isinstance(port_name, str) and isinstance(protocol, str)
                    and protocol in {"tcp", "udp"} else None)
            if (port is None
                    or port.get("state") != "active"
                    or port.get("generation") != generation
                    or any(result.get(key) != port.get(key) for key in
                           ("task_id", "port_name", "protocol", "host_port", "vm_port", "generation"))):
                raise ControlError("operation_unverified", "port state does not match the journal result",
                                   {"operation_id": operation_id})
            try:
                owned = self._ownership(task)
                native = owned.get("devices", {}).get(port["device_name"])
                expected = self._proxy_properties(task, port)
                if not isinstance(native, Mapping) or any(native.get(key) != value for key, value in expected.items()):
                    raise ControlError("operation_unverified", "port mapping does not match the journal result",
                                       {"operation_id": operation_id})
            except ControlError as exc:
                if exc.code == "operation_unverified":
                    raise
                raise ControlError("operation_unverified", "replayed port mapping cannot be verified",
                                   {"operation_id": operation_id}) from exc
            except (KeyError, TypeError, ValueError) as exc:
                raise ControlError("operation_unverified", "replayed port mapping cannot be verified",
                                   {"operation_id": operation_id}) from exc
            return
        if action == "port-remove":
            if session_id is None:
                raise ControlError("operation_unverified", "port operation has no session",
                                   {"operation_id": operation_id})
            task = self.store.get_task(session_id=session_id)
            name = request.get("name")
            protocol = request.get("port_protocol", "tcp")
            try:
                generation = int(task["generation"]) if task is not None else None
                effective_lease = bool(task and task.get("state") == "active" and self._state_summary(task)["effective_lease"])
            except (KeyError, TypeError, ValueError) as exc:
                raise ControlError("operation_unverified", "task generation cannot be verified for journal replay",
                                   {"operation_id": operation_id}) from exc
            if (task is None or generation != request.get("generation") or task.get("state") != "active"
                    or not effective_lease
                    or result.get("task_id") != task.get("task_id")
                    or result.get("name") != name or result.get("protocol") != protocol
                    or result.get("removed") is not True
                    or not isinstance(name, str) or not isinstance(protocol, str)
                    or protocol not in {"tcp", "udp"}
                    or self.store.get_port(task["task_id"], name, protocol)):
                raise ControlError("operation_unverified", "removed port still exists after journal success",
                                   {"operation_id": operation_id})
            return
        if action in {"admin-gc", "timer-age-gc"}:
            self._validate_cached_gc_result(operation_id, result)
            return
        if action in {"admin-reconcile", "timer-reconcile"}:
            self._validate_cached_reconcile_result(operation_id, result)

    def _validate_cached_gc_result(self, operation_id: str, result: Mapping[str, Any]) -> None:
        if (not isinstance(result.get("dry_run"), bool)
                or result.get("selection") not in {"eligible", "age"}
                or isinstance(result.get("minimum_idle_seconds"), bool)
                or not isinstance(result.get("minimum_idle_seconds"), int)
                or not isinstance(result.get("candidates"), list)):
            raise ControlError("operation_unverified", "GC journal result is invalid",
                               {"operation_id": operation_id})
        for candidate in result["candidates"]:
            if not isinstance(candidate, Mapping) or not isinstance(candidate.get("task_id"), str):
                raise ControlError("operation_unverified", "GC journal candidate is invalid",
                                   {"operation_id": operation_id})
            if "error" in candidate:
                error = candidate["error"]
                if (not isinstance(error, Mapping) or not isinstance(error.get("code"), str)
                        or not isinstance(error.get("message"), str)):
                    raise ControlError("operation_unverified", "GC journal error is invalid",
                                       {"operation_id": operation_id})
                continue
            if (not isinstance(candidate.get("instance_name"), str)
                    or isinstance(candidate.get("generation"), bool)
                    or not isinstance(candidate.get("generation"), int)
                    or not isinstance(candidate.get("inactive_since"), str)):
                raise ControlError("operation_unverified", "GC journal candidate is incomplete",
                                   {"operation_id": operation_id})
            task = self.store.get_task(task_id=candidate["task_id"])
            if result["dry_run"]:
                if (task is None or task.get("state") != "cached"
                        or any(candidate.get(key) != task.get(key) for key in
                               ("task_id", "instance_name", "generation", "inactive_since"))):
                    raise ControlError("operation_unverified", "GC preview no longer matches durable state",
                                       {"operation_id": operation_id})
            elif task is not None:
                try:
                    current_generation = int(task["generation"])
                except (KeyError, TypeError, ValueError) as exc:
                    raise ControlError("operation_unverified", "GC task generation cannot be verified",
                                       {"operation_id": operation_id}) from exc
                if task.get("state") != "absent" or current_generation != candidate["generation"]:
                    raise ControlError("operation_unverified", "GC deletion is not reflected in durable state",
                                       {"operation_id": operation_id})
                try:
                    if any(native.get("name") == candidate["instance_name"] for native in self.incus.list_instances()):
                        raise ControlError("operation_unverified", "GC-deleted native instance still exists",
                                           {"operation_id": operation_id})
                except ControlError as exc:
                    if exc.code == "operation_unverified":
                        raise
                    raise ControlError("operation_unverified", "GC native deletion cannot be verified",
                                       {"operation_id": operation_id}) from exc
            else:
                raise ControlError("operation_unverified", "GC task state is unavailable for replay",
                                   {"operation_id": operation_id})

    @staticmethod
    def _inventory_mappings(value: Any, code: str, label: str) -> list[Mapping[str, Any]]:
        if not isinstance(value, list) or any(not isinstance(item, Mapping) for item in value):
            raise ControlError(code, f"{label} inventory is malformed")
        return value

    def _validate_cached_absent_reconcile(
        self,
        task: Mapping[str, Any],
        generation: int,
        *,
        dry_run: bool,
        operation_id: str,
    ) -> None:
        owner = task.get("session_id")
        if not isinstance(owner, str) or not owner:
            raise ControlError(
                "operation_unverified",
                "absent resource owner cannot be verified",
                {"operation_id": operation_id},
            )
        task_id = task.get("task_id")
        with self._object_lock("session:" + owner, blocking=False):
            current = self.store.get_task(task_id=task_id)
            if (
                current is None
                or current.get("state") != "absent"
                or current.get("generation") != generation
            ):
                raise ControlError(
                    "operation_unverified",
                    "absent resource task changed during replay",
                    {"operation_id": operation_id},
                )
            instance_name = current.get("instance_name")
        try:
            exists = any(
                native.get("name") == instance_name
                for native in self.incus.list_instances()
            )
        except ControlError as exc:
            raise ControlError(
                "operation_unverified",
                "absent resource cleanup cannot be verified",
                {"operation_id": operation_id},
            ) from exc
        with self._object_lock("session:" + owner, blocking=False):
            current = self.store.get_task(task_id=task_id)
            if (
                current is None
                or current.get("state") != "absent"
                or current.get("generation") != generation
            ):
                raise ControlError(
                    "operation_unverified",
                    "absent resource task changed during replay",
                    {"operation_id": operation_id},
                )
            if exists or self.store.get_ports(current["task_id"]):
                raise ControlError(
                    "operation_unverified",
                    "absent resource cleanup is no longer verified",
                    {"operation_id": operation_id},
                )
            try:
                resource_error = reservation_error = False
                try:
                    resources = parse_resource_mapping(
                        current.get("resources_json"), "resources", allow_empty=False
                    )
                except (TypeError, ValueError):
                    resource_error = True
                    resources = None
                try:
                    reservation = parse_resource_mapping(
                        current.get("resource_reservation_json"), "reservation", allow_empty=True
                    )
                except (TypeError, ValueError):
                    reservation_error = True
                    reservation = None
            except ControlError as exc:
                if exc.code == "operation_unverified":
                    raise
                raise ControlError(
                    "operation_unverified",
                    "absent resource cleanup cannot be verified",
                    {"operation_id": operation_id},
                ) from exc
            if dry_run:
                return
            if (
                resource_error
                or reservation_error
                or resources != {"cpu": 0, "memory_mib": 0, "disk_gib": 0}
                or reservation
                or current.get("reservation_bytes") != 0
            ):
                raise ControlError(
                    "operation_unverified",
                    "absent resource repair is not reflected in durable state",
                    {"operation_id": operation_id},
                )

    def _validate_cached_reconcile_result(self, operation_id: str, result: Mapping[str, Any]) -> None:
        if (not isinstance(result.get("dry_run"), bool)
                or not isinstance(result.get("actions"), list)):
            raise ControlError("operation_unverified", "reconcile journal result is invalid",
                               {"operation_id": operation_id})
        allowed = {
            "inspect", "finish-delete", "close-incomplete-or-expired",
            "stop-unexpected-running-cache", "rearm-current-expiry",
            "verify-absent-resources",
        }
        for item in result["actions"]:
            if (not isinstance(item, Mapping) or not isinstance(item.get("task_id"), str)
                    or isinstance(item.get("generation"), bool)
                    or not isinstance(item.get("generation"), int)):
                raise ControlError("operation_unverified", "reconcile journal action is invalid",
                                   {"operation_id": operation_id})
            if "error" in item:
                error = item["error"]
                if (not isinstance(error, Mapping) or not isinstance(error.get("code"), str)
                        or not isinstance(error.get("message"), str)):
                    raise ControlError("operation_unverified", "reconcile journal error is invalid",
                                       {"operation_id": operation_id})
            if item.get("action") not in allowed:
                raise ControlError("operation_unverified", "reconcile journal action is unknown",
                                   {"operation_id": operation_id})
            task = self.store.get_task(task_id=item["task_id"])
            try:
                generation = int(task["generation"]) if task is not None else None
            except (KeyError, TypeError, ValueError) as exc:
                raise ControlError("operation_unverified", "reconcile task generation cannot be verified",
                                   {"operation_id": operation_id}) from exc
            if task is None or generation != item["generation"]:
                raise ControlError("operation_unverified", "reconcile task generation no longer matches",
                                   {"operation_id": operation_id})
            if "error" in item:
                # An action-level error is already the committed outcome. A
                # replay must return that same structured error, but it must
                # never claim that a newer task generation was checked.
                continue
            dry_states = {
                "inspect": {"active", "cached", "closing", "preparing", "failed", "deleting"},
                "finish-delete": {"deleting"},
                "close-incomplete-or-expired": {"active", "closing", "preparing", "failed"},
                "stop-unexpected-running-cache": {"cached"},
                "rearm-current-expiry": {"active"},
                "verify-absent-resources": {"absent"},
            }
            applied_states = {
                "inspect": {"active", "cached", "closing", "preparing", "failed", "deleting"},
                "finish-delete": {"absent"},
                "close-incomplete-or-expired": {"cached", "absent"},
                "stop-unexpected-running-cache": {"cached", "absent"},
                "rearm-current-expiry": {"active"},
                "verify-absent-resources": {"absent"},
            }
            expected_states = dry_states if result["dry_run"] else applied_states
            if task.get("state") not in expected_states[item["action"]]:
                raise ControlError("operation_unverified", "reconcile result is not reflected in durable state",
                                   {"operation_id": operation_id, "action": item["action"]})
            if item["action"] == "verify-absent-resources":
                self._validate_cached_absent_reconcile(
                    task,
                    generation,
                    dry_run=result["dry_run"],
                    operation_id=operation_id,
                )
            if not result["dry_run"]:
                action_name = item["action"]
                if action_name == "rearm-current-expiry":
                    try:
                        if not self._state_summary(task)["effective_lease"]:
                            raise ControlError("operation_unverified", "reconciled lease is no longer effective",
                                               {"operation_id": operation_id})
                        self._ownership(task)
                        if self.incus.instance_info(task["instance_name"])["status"].lower() != "running":
                            raise ControlError("operation_unverified", "reconciled guest is not running",
                                               {"operation_id": operation_id})
                        if not self.systemd.expiry_active(task["task_id"], generation, task["lease_expires_at"]):
                            raise ControlError("operation_unverified", "reconciled expiry timer is not active",
                                               {"operation_id": operation_id})
                    except ControlError as exc:
                        if exc.code == "operation_unverified":
                            raise
                        raise ControlError("operation_unverified", "reconciled native state cannot be verified",
                                           {"operation_id": operation_id}) from exc
                    except (KeyError, TypeError, ValueError) as exc:
                        raise ControlError("operation_unverified", "reconciled native state cannot be verified",
                                           {"operation_id": operation_id}) from exc
                elif action_name in {"close-incomplete-or-expired", "stop-unexpected-running-cache"}:
                    if task.get("state") == "cached":
                        try:
                            self._ownership(task)
                            if self.incus.instance_info(task["instance_name"])["status"].lower() != "stopped":
                                raise ControlError("operation_unverified", "reconciled cache is still running",
                                                   {"operation_id": operation_id})
                            if self.store.get_ports(task["task_id"]):
                                raise ControlError("operation_unverified", "reconciled cache still has ports",
                                                   {"operation_id": operation_id})
                            owned = self._ownership(task)
                            if any(device.get("type") == "proxy" for device in owned.get("expanded_devices", {}).values()):
                                raise ControlError("operation_unverified", "reconciled cache still has native proxies",
                                                   {"operation_id": operation_id})
                        except ControlError as exc:
                            if exc.code == "operation_unverified":
                                raise
                            raise ControlError("operation_unverified", "reconciled cache state cannot be verified",
                                               {"operation_id": operation_id}) from exc
                        except (KeyError, TypeError, ValueError) as exc:
                            raise ControlError("operation_unverified", "reconciled cache state cannot be verified",
                                               {"operation_id": operation_id}) from exc
                elif action_name == "finish-delete" or task.get("state") == "absent":
                    try:
                        exists = any(native.get("name") == task.get("instance_name") for native in self.incus.list_instances())
                    except ControlError as exc:
                        raise ControlError("operation_unverified", "reconciled native state cannot be verified",
                                           {"operation_id": operation_id}) from exc
                    if exists:
                        raise ControlError("operation_unverified", "reconciled native instance still exists",
                                           {"operation_id": operation_id})

    def _dispatch_locked(
        self,
        request: Mapping[str, Any],
        compatibility_warnings: Iterable[Mapping[str, Any]] = (),
    ) -> dict[str, Any]:
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
        if action == "admin-metrics" and (
            "session_id" in request or "operation_id" in request
        ):
            raise ControlError("invalid_request", "admin-metrics is sessionless and read-only")
        operation_id = request.get("operation_id")
        is_write = self._is_write_action(action, request)
        if is_write:
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
                self._validate_cached_success(action, operation_id, session_id, request, cached)
                response = dict(cached)
                response.pop("_request_digest", None)
                response["server_time"] = iso_time(self.now())
                response["outcome"] = "replayed"
                cached_warnings = response.get("warnings", [])
                response["warnings"] = self._merge_warnings(
                    cached_warnings if isinstance(cached_warnings, list) else [],
                    compatibility_warnings,
                )
                response.setdefault("response", self._response_compatibility())
                response["release"] = self._release_metadata()
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
            result = self._dispatch_action(action, dict(request), session_id, compatibility_warnings)
        except ControlError as exc:
            try:
                outcome_uncertain = self._operation_outcome_uncertain(action, exc)
                if not outcome_uncertain and action in {"release", "destroy"} and session_id is not None:
                    try:
                        current_task = self.store.get_task(session_id=session_id)
                    except ControlError:
                        outcome_uncertain = True
                    else:
                        outcome_uncertain = bool(
                            current_task and current_task.get("state") in {"closing", "deleting"}
                        )
                detail = dict(exc.detail) if isinstance(exc.detail, Mapping) else {}
                detail.setdefault("outcome_verified", not outcome_uncertain)
                exc = ControlError(exc.code, exc.message, detail)
                if operation_id and is_write:
                    # A committed renew must remain replayable even if rearming its
                    # native timer failed after the atomic deadline/journal commit.
                    saved = self.store.operation(operation_id)
                    if not saved:
                        self.store.record_operation(operation_id, action, session_id, request.get("generation"),
                                                    {"outcome": "error", "error": exc.as_dict(), "_request_digest": digest})
                    elif saved.get("outcome") == "pending" and not self._operation_outcome_uncertain(action, exc):
                        self.store.record_operation(operation_id, action, session_id, request.get("generation"),
                                                    {"outcome": "error", "error": exc.as_dict(), "_request_digest": digest})
                if session_id is not None or is_write:
                    self.store.audit(
                        action,
                        "error",
                        session_id=session_id,
                        generation=request.get("generation"),
                        detail=exc.as_dict(),
                    )
            except ControlError as state_error:
                raise self._annotate_state_error(state_error, action=action, operation_id=operation_id) from state_error
            raise exc
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
            "response": self._response_compatibility(),
            "release": self._release_metadata(),
            "warnings": self._merge_warnings(compatibility_warnings),
        }
        try:
            if operation_id is not None and is_write and action != "renew":
                self.store.record_operation(
                    operation_id,
                    action,
                    session_id,
                    request.get("generation"),
                    {**response, "_request_digest": digest},
                )
            if session_id is not None or is_write:
                self.store.audit(
                    action,
                    "ok",
                    session_id=session_id,
                    generation=request.get("generation"),
                    detail=result,
                )
        except ControlError as state_error:
            raise self._annotate_state_error(state_error, action=action, operation_id=operation_id) from state_error
        return response

    def _dispatch_action(
        self,
        action: str,
        request: dict[str, Any],
        session_id: str | None,
        compatibility_warnings: Iterable[Mapping[str, Any]] = (),
    ) -> dict[str, Any]:
        if action == "renew":
            return self.renew(request, session_id, compatibility_warnings)
        handlers: dict[str, Callable[..., dict[str, Any]]] = {
            "doctor": self.doctor,
            "status": self.status,
            "acquire": self.acquire,
            "release": self.release,
            "destroy": self.destroy,
            "restart": self.restart,
            "resize": self.resize,
            "ssh-info": self.ssh_info,
            "port-add": self.port_add,
            "port-list": self.port_list,
            "port-remove": self.port_remove,
            "admin-status": self.admin_status,
            "admin-metrics": self.admin_metrics,
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
        self._validate_task_state(task)
        return task

    @staticmethod
    def _validate_task_state(task: Mapping[str, Any]) -> None:
        state = task.get("state")
        if state not in TASK_STATES:
            raise ControlError(
                "resource_unverified",
                "task state cannot be verified; reconciliation is required",
                {"task_id": task.get("task_id"), "state": state},
            )

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
        if not isinstance(info, Mapping):
            raise ControlError("ownership_mismatch", "native instance inventory is not an object; refusing mutation")
        config = info.get("config")
        devices = info.get("devices")
        expanded_devices = info.get("expanded_devices")
        expected = {"user.agent-vm.managed": "native-v1", "user.agent-vm.task": task["task_id"],
                    "user.agent-vm.session": task["session_id"], "user.agent-vm.generation": str(task["generation"])}
        generations = {str(task["generation"])}
        if task["state"] in {"preparing", "closing"} and task.get("previous_generation") is not None:
            generations.add(str(task["previous_generation"]))
        if (
            not isinstance(config, Mapping)
            or not isinstance(devices, Mapping)
            or not isinstance(expanded_devices, Mapping)
            or any(not isinstance(device, Mapping) for device in devices.values())
            or any(not isinstance(device, Mapping) for device in expanded_devices.values())
            or any(config.get(key) != value for key, value in expected.items() if key != "user.agent-vm.generation")
            or config.get("user.agent-vm.generation") not in generations
        ):
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
            self._network_guard()
            self._verify_network_device(current)
            self.incus.start(current["instance_name"])

    def _stop_instance(self, task: Mapping[str, Any]) -> None:
        with self._native_lock(task):
            current = self._native_operation_current(task)
            self._ownership(current)
            self.incus.stop(current["instance_name"], force=True)

    def _state_summary(self, task: Mapping[str, Any]) -> dict[str, Any]:
        self._validate_task_state(task)
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
            try:
                last_error = json.loads(task["last_error"])
            except (TypeError, ValueError) as exc:
                raise ControlError("resource_unverified", "task error state cannot be verified") from exc
            if not isinstance(last_error, Mapping):
                raise ControlError("resource_unverified", "task error state cannot be verified")
            result["last_error"] = dict(last_error)
        result["ports"] = self.store.get_ports(task["task_id"])
        resource_state_error = None
        try:
            resources = parse_resource_mapping(task.get("resources_json"), "resources", allow_empty=False)
            resource_reservation = parse_resource_mapping(task.get("resource_reservation_json"), "reservation", allow_empty=True)
        except (TypeError, ValueError) as exc:
            if (
                task.get("state") == "absent"
                and str(task.get("resources_json", "")).strip() in {"", "{}"}
                and str(task.get("resource_reservation_json", "")).strip() in {"", "{}"}
            ):
                resources = None
                resource_reservation = None
                resource_state_error = {
                    "code": "resource_unverified",
                    "message": "absent task has no durable resource allocation",
                }
            else:
                raise ControlError("resource_unverified", "task resource state cannot be verified") from exc
        result["resources"] = resources
        result["resource_reservation"] = resource_reservation
        if resource_state_error is not None:
            result["resource_state"] = {"verified": False, "error": resource_state_error}
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
        """Measure the configured Incus dir pool, never an unverified filesystem."""
        try:
            pool = self.incus.query("/1.0/storage-pools/" + quote(str(self.config["pool"]), safe=""),
                                    project="default")
            if not isinstance(pool, Mapping):
                raise ValueError("pool response is not an object")
            if pool.get("name") not in (None, self.config["pool"]):
                raise ValueError("pool name does not match configuration")
            if pool.get("driver") != "dir":
                raise ControlError("resource_unverified", "configured Incus pool is not a directory pool",
                                   {"pool": self.config["pool"], "driver": pool.get("driver")})
            pool_config = pool.get("config")
            source = pool_config.get("source") if isinstance(pool_config, Mapping) else None
            configured_path = os.path.normpath(str(self.config["storage_path"]))
            if not isinstance(source, str) or not source or os.path.normpath(source) != configured_path:
                raise ControlError("resource_unverified", "Incus pool source differs from configured storage path",
                                   {"pool": self.config["pool"], "pool_source": source,
                                    "storage_path": configured_path})
        except ControlError:
            raise
        except (KeyError, TypeError, ValueError) as exc:
            raise ControlError("resource_unverified", "Incus pool source cannot be verified",
                               {"pool": self.config.get("pool")}) from exc
        try:
            usage = shutil.disk_usage(str(self.config["storage_path"]))
        except OSError as exc:
            raise ControlError("resource_unverified", "cannot measure CT Incus pool filesystem") from exc
        total, free = usage.total, usage.free
        if not isinstance(total, int) or not isinstance(free, int) or not 0 < total or not 0 <= free <= total:
            raise ControlError("resource_unverified", "invalid CT filesystem capacity")
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

    @staticmethod
    def _durable_reservation_bytes(value: Any) -> int:
        if value is None or (isinstance(value, int) and not isinstance(value, bool) and value == 0):
            return 0
        if isinstance(value, bool) or not isinstance(value, int) or value < 0:
            raise ValueError("reservation_bytes must be a non-negative integer")
        return value

    def _space_sufficient(self, facts: Mapping[str, Any], cost: int, threshold: str,
                          exclude_task_id: str | None = None) -> bool:
        try:
            reserved = sum(
                self._durable_reservation_bytes(task.get("reservation_bytes"))
                for task in self.store.list_tasks()
                if task["task_id"] != exclude_task_id
            )
        except (TypeError, ValueError) as exc:
            raise ControlError("resource_unverified", "durable disk reservation is malformed") from exc
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
            detail = dict(exc.detail) if isinstance(exc.detail, Mapping) else {}
            detail["operation_phase"] = "cache-delete"
            detail["outcome_verified"] = False
            annotated = ControlError(exc.code, exc.message, detail)
            self.store.update_task(task["task_id"], last_error=json.dumps(annotated.as_dict()))
            raise annotated from exc
        with self._global_lock():
            self.store.update_task(task["task_id"], state="absent", vm_ip=None, ssh_host_port=None,
                                   lease_expires_at=None, inactive_since=None, cleanup_after=None,
                                   resources_json=json.dumps({"cpu": 0, "memory_mib": 0, "disk_gib": 0}),
                                   resource_reservation_json="{}", reservation_bytes=0, last_error=None)


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
                detail = dict(exc.as_dict())
                detail["operation_phase"] = "cache-delete"
                detail["outcome_verified"] = False
                raise ControlError("reclaim_failed", "exact cache reclamation failed", detail) from exc
        facts = self._storage_headroom(request_cost)
        with self._global_lock():
            if not self._space_sufficient(facts, request_cost, "low", exclude_task_id):
                raise ControlError("capacity_insufficient", "capacity remains insufficient after eligible reclaim",
                                   {"storage": facts, "reclaimed": reclaimed})
        return reclaimed

    def _resource_policy(self, *, memory_observation: Mapping[str, Any] | None = None) -> dict[str, Any]:
        cpu_budget = require_int(self.config.get("cpu_budget", 0), "cpu_budget", 1)
        memory_budget = require_int(self.config.get("memory_budget_mib", 0), "memory_budget_mib", 512)
        ratios = {"cpu": overcommit_ratio(self.config.get("cpu_overcommit_ratio", 4), "cpu_overcommit_ratio"),
                  "memory_mib": overcommit_ratio(self.config.get("memory_overcommit_ratio", 4), "memory_overcommit_ratio")}
        cpu_capacity = self.resources.cpu()
        memory = self.resources.memory() if memory_observation is None else memory_observation
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

    def _cpu_pressure_observation(self) -> dict[str, Any]:
        try:
            observation = self.resources.cpu_pressure()
        except ControlError as exc:
            return {"verified": False, "error": exc.as_dict()}
        except AttributeError:
            return {"verified": False, "error": {
                "code": "resource_unverified",
                "message": "CT CPU pressure observation is unavailable",
            }}
        if not isinstance(observation, Mapping):
            return {"verified": False, "error": {
                "code": "resource_unverified",
                "message": "CT CPU pressure observation is not an object",
            }}
        sources = observation.get("sources")
        if (observation.get("scope") != "ct-cpu-pressure"
                or sources != ["cgroupfs"]
                or observation.get("mountpoint") != "/sys/fs/cgroup"
                or observation.get("mount_root") != "/"
                or observation.get("filesystem") != "cgroup2"):
            return {"verified": False, "error": {
                "code": "resource_unverified",
                "message": "CT CPU pressure observation is not CT-local",
            }}
        cgroup_path = observation.get("cgroup_path")
        if (not isinstance(cgroup_path, str)
                or (cgroup_path != "/" and (
                    re.fullmatch(r"/[^\x00/]+(?:/[^\x00/]+)*", cgroup_path) is None
                    or any(part in {".", ".."} for part in cgroup_path.split("/"))
                ))):
            return {"verified": False, "error": {
                "code": "resource_unverified",
                "message": "CT CPU pressure cgroup path is invalid",
            }}
        expected_metric = "/sys/fs/cgroup" + ("" if cgroup_path == "/" else cgroup_path) + "/cpu.pressure"
        if observation.get("metric") != expected_metric:
            return {"verified": False, "error": {
                "code": "resource_unverified",
                "message": "CT CPU pressure metric path is not current-cgroup local",
            }}
        try:
            psi = normalize_cpu_psi(observation)
        except ValueError:
            return {"verified": False, "error": {
                "code": "resource_unverified",
                "message": "CT CPU pressure observation is malformed",
            }}
        result = dict(observation)
        result.update(psi)
        result.update({"verified": True, "observed_at": iso_time(self.now())})
        return result

    def _cpu_startup_pressure_diagnostics(
        self,
        *,
        allocated_cpu: int,
        requested_cpu: int,
        capacity_cpu: int,
        observation: Mapping[str, Any] | None = None,
    ) -> dict[str, Any]:
        observation = observation if observation is not None else self._cpu_pressure_observation()
        threshold = percentage(
            self.config.get(
                "cpu_startup_pressure_full_avg10_percent",
                CPU_STARTUP_PRESSURE_FULL_AVG10_PERCENT,
            ),
            "cpu_startup_pressure_full_avg10_percent",
        )
        detail: dict[str, Any] = {
            "threshold_full_avg10_percent": threshold,
            "allocated_cpu": allocated_cpu,
            "requested_cpu": requested_cpu,
            "projected_cpu": allocated_cpu + requested_cpu,
            "ct_capacity_cpu": capacity_cpu,
            "physically_oversubscribed": allocated_cpu + requested_cpu > capacity_cpu,
            "startup_blocked": False,
        }
        if not observation.get("verified"):
            detail.update({"verified": False, "error": observation.get("error")})
            return detail
        try:
            full = observation["full"]
            some = observation["some"]
            values = {}
            for name in ("avg10", "avg60", "avg300", "total"):
                full_value = float(full[name])
                some_value = float(some[name])
                if (not math.isfinite(full_value) or not math.isfinite(some_value)
                        or full_value > some_value):
                    raise ValueError()
                values[name] = (full_value, some_value)
        except (KeyError, TypeError, ValueError) as exc:
            detail.update({
                "verified": False,
                "error": {
                    "code": "resource_unverified",
                    "message": "CT CPU pressure observation is incomplete",
                },
            })
            return detail
        detail.update({
            "verified": True,
            "full_avg10_percent": values["avg10"][0],
            "some_avg10_percent": values["avg10"][1],
        })
        detail["startup_blocked"] = detail["physically_oversubscribed"] and values["avg10"][0] >= threshold
        return detail

    def _enforce_cpu_startup_pressure(
        self,
        *,
        allocated_cpu: int,
        requested_cpu: int,
        capacity_cpu: int,
    ) -> dict[str, Any]:
        detail = self._cpu_startup_pressure_diagnostics(
            allocated_cpu=allocated_cpu,
            requested_cpu=requested_cpu,
            capacity_cpu=capacity_cpu,
        )
        if not detail["verified"]:
            raise ControlError(
                "resource_unverified",
                "CT CPU pressure observation is not verified",
                detail,
            )
        if detail["startup_blocked"]:
            raise ControlError(
                "resource_pressure",
                "CPU scheduler pressure blocks VM startup",
                detail,
            )
        return detail

    def _allocation_breakdown(self, exclude_task_id: str | None = None) -> dict[str, dict[str, int]]:
        totals = {key: {name: 0 for name in ("allocated", "reserved", "allocated_and_reserved")}
                  for key in ("cpu", "memory_mib")}
        for other in self.store.list_tasks():
            state = other.get("state")
            if other["task_id"] == exclude_task_id or state in {"absent", "cached"}: continue
            if state not in {"preparing", "active", "closing", "deleting", "failed"}:
                raise ControlError(
                    "resource_unverified",
                    "task state cannot be verified",
                    {"task_id": other.get("task_id"), "state": state},
                )
            try:
                value = parse_resource_mapping(other["resources_json"], "resources", allow_empty=False)
                reservation = parse_resource_mapping(other["resource_reservation_json"], "reservation", allow_empty=True)
                for key in totals:
                    sizes = (value[key], reservation.get(key, 0))
                    if any(isinstance(size, bool) or not isinstance(size, int) or size < 0 for size in sizes): raise ValueError()
                    totals[key]["allocated"] += sizes[0]
                    totals[key]["reserved"] += sizes[1]
                    totals[key]["allocated_and_reserved"] += max(sizes)
            except (KeyError, TypeError, ValueError) as exc:
                raise ControlError("resource_unverified", "occupied object lacks verified resource sizes") from exc
        return totals

    def _allocation_totals(self, exclude_task_id: str | None = None) -> dict[str, int]:
        return {key: value["allocated_and_reserved"] for key, value in self._allocation_breakdown(exclude_task_id).items()}

    def _running_inventory_guard(self) -> None:
        instances = self.incus.list_instances()
        records = {item["instance_name"]: item for item in self.store.list_tasks()}
        for record in records.values():
            if record.get("state") not in {"absent", "cached", "preparing", "active", "closing", "deleting", "failed"}:
                raise ControlError(
                    "resource_unverified",
                    "task state cannot be verified; reconciliation is required",
                    {"task_id": record.get("task_id"), "state": record.get("state")},
                )
            if record["state"] != "absent":
                continue
            if any(instance.get("name") == record["instance_name"] for instance in instances):
                raise ControlError(
                    "resource_unverified",
                    "absent task still has a native instance; reconciliation is required",
                    {"task_id": record.get("task_id")},
                )
            if self.store.get_ports(record["task_id"]):
                raise ControlError(
                    "cleanup_incomplete",
                    "absent task still has port mappings; reconciliation is required",
                    {"task_id": record.get("task_id")},
                )
            try:
                resources = parse_resource_mapping(record.get("resources_json"), "resources", allow_empty=False)
                reservation = parse_resource_mapping(record.get("resource_reservation_json"), "reservation", allow_empty=True)
                reservation_bytes = self._durable_reservation_bytes(record.get("reservation_bytes"))
            except (TypeError, ValueError) as exc:
                raise ControlError(
                    "resource_unverified",
                    "absent task resource state cannot be verified; reconciliation is required",
                    {"task_id": record.get("task_id")},
                ) from exc
            if (
                resources != {"cpu": 0, "memory_mib": 0, "disk_gib": 0}
                or reservation
                or reservation_bytes != 0
            ):
                raise ControlError(
                    "resource_unverified",
                    "absent task resource state is not reconciled; reconciliation is required",
                    {"task_id": record.get("task_id")},
                )
        for instance in instances:
            record = records.get(instance.get("name"))
            stopped = str(instance.get("status", "")).lower() == "stopped"
            if stopped:
                if record is not None and record["state"] not in {"absent", "cached"}:
                    if record["state"] != "active":
                        raise ControlError("resource_unverified", TRANSITIONAL_ADMISSION_ERROR_MESSAGE)
                    self._verify_native_allocation(record)
                continue
            if record is None or record["state"] in {"absent", "cached"}:
                raise ControlError("resource_unverified", "unaccounted running native instance blocks admission")
            if record["state"] != "active":
                raise ControlError("resource_unverified", TRANSITIONAL_ADMISSION_ERROR_MESSAGE)
            self._verify_native_allocation(record)

    def _resource_diagnostics(
        self,
        *,
        global_locked: bool = False,
        capacity_observation: Mapping[str, Any] | None = None,
    ) -> dict[str, Any]:
        memory = None
        memory_error = None
        try:
            memory = self.resources.memory()
        except ControlError as exc:
            memory_error = exc
        cpu_pressure = self._cpu_pressure_observation()
        policy = None
        try:
            if memory_error is not None:
                raise memory_error
            policy = self._resource_policy(memory_observation=memory)
            # Native inventory calls can block on the Incus socket. Keep them
            # outside the admission transaction; only the durable accounting
            # read and disk projection need the short global lock.
            self._running_inventory_guard()
            disk_capacity = (
                capacity_observation
                if capacity_observation is not None
                else self._storage_headroom(0)
            )
            lock = nullcontext() if global_locked else self._global_lock()
            with lock, self.store.transaction():
                breakdown = self._allocation_breakdown()
                disk = self._disk_admission_diagnostics(disk_capacity)
            if not disk.get("verified"):
                raise ControlError("resource_unverified", "disk admission state is not verified", disk.get("error"))
            allocated = {key: value["allocated"] for key, value in breakdown.items()}
            reserved = {key: value["reserved"] for key, value in breakdown.items()}
            allocated_and_reserved = {key: value["allocated_and_reserved"] for key, value in breakdown.items()}
            return {"verified": True, **policy, "allocated": allocated, "reserved": reserved,
                    "allocated_and_reserved": allocated_and_reserved,
                    "remaining": {key: policy["limits"][key] - allocated_and_reserved[key] for key in allocated_and_reserved},
                    "memory_pressure": policy["memory"]["available_bytes"] < policy["memory"]["threshold_bytes"],
                    "cpu_pressure": cpu_pressure,
                    "disk_admission": disk}
        except ControlError as exc:
            result = {
                "verified": False,
                "error": exc.as_dict(),
                "base": None,
                "ratios": None,
                "limits": None,
                "ct_capacity": None,
                "memory": None if memory is None else {**memory, "observed_at": iso_time(self.now())},
                "memory_pressure": None,
                "cpu_pressure": cpu_pressure,
                "allocated": None,
                "reserved": None,
                "allocated_and_reserved": None,
                "remaining": None,
                "disk_admission": None,
            }
            if policy is not None:
                result.update(policy)
            if memory is not None:
                result["memory_pressure"] = memory["available_bytes"] < memory["threshold_bytes"]
            return result

    def _disk_admission_diagnostics(self, capacity: Mapping[str, Any] | None) -> dict[str, Any]:
        if not isinstance(capacity, Mapping) or not isinstance(capacity.get("layers"), list):
            return {"verified": False, "error": {"code": "resource_unverified", "message": "disk capacity is unavailable"}}
        try:
            layers = capacity["layers"]
            if not layers:
                raise ValueError()
            allocated = reserved = allocated_and_reserved = 0
            durable_reserved = 0
            for task in self.store.list_tasks():
                if task["state"] in {"absent", "cached"}:
                    continue
                resources = parse_resource_mapping(task["resources_json"], "resources", allow_empty=False)
                reservation = parse_resource_mapping(task["resource_reservation_json"], "reservation", allow_empty=True)
                sizes = (resources["disk_gib"], reservation.get("disk_gib", 0))
                if any(isinstance(size, bool) or not isinstance(size, int) or size < 0 for size in sizes):
                    raise ValueError()
                allocated += sizes[0] * 1024**3
                reserved += sizes[1] * 1024**3
                allocated_and_reserved += max(sizes) * 1024**3
                durable_reserved += self._durable_reservation_bytes(task.get("reservation_bytes"))
            total = sum(int(layer["total"]) for layer in layers)
            free_after_low = min(int(layer["free"]) - int(layer["low"]) for layer in layers)
            limit = sum(int(layer["total"]) - int(layer["low"]) for layer in layers)
            if total <= 0 or limit <= 0 or durable_reserved < 0:
                raise ValueError()
            used = total - sum(int(layer["free"]) for layer in layers)
            physical_limit = limit
            physical_remaining = max(0, free_after_low - durable_reserved)
            return {"verified": True,
                    "guest_allocation": {"allocated_bytes": allocated, "reserved_bytes": reserved,
                                         "allocated_and_reserved_bytes": allocated_and_reserved},
                    "physical_admission": {"allocated_bytes": used, "reserved_bytes": durable_reserved,
                                           "allocated_and_reserved_bytes": used + durable_reserved,
                                           "remaining_bytes": physical_remaining, "limit_bytes": physical_limit,
                                           "capacity_bytes": total, "free_bytes": sum(int(layer["free"]) for layer in layers),
                                           "durable_reserved_bytes": durable_reserved}}
        except (KeyError, TypeError, ValueError):
            return {"verified": False, "error": {"code": "resource_unverified", "message": "invalid disk admission capacity"}}

    @staticmethod
    def _instance_count_limits(config: Mapping[str, Any]) -> dict[str, Any]:
        # Container-only restrictions do not limit this VM backend.
        return {key: config[key] for key in ("limits.instances", "limits.virtual-machines")
                if config.get(key) not in (None, "", -1, "-1")}

    def _instance_diagnostics(self) -> dict[str, Any]:
        try:
            project = self.incus.query("/1.0/projects/" + quote(self.config["project"], safe=""), project="default")
            if not isinstance(project, Mapping) or not isinstance(project.get("config"), Mapping):
                raise ControlError("resource_unverified", "project count configuration cannot be inspected")
            limits = self._instance_count_limits(project["config"])
            instances = self.incus.list_instances()
            running = sum(str(item.get("status", "")).lower() == "running" for item in instances)
            stopped = sum(str(item.get("status", "")).lower() == "stopped" for item in instances)
            instance_names = {str(item.get("name")) for item in instances}
            preparing = sum(task["state"] == "preparing" and task["instance_name"] not in instance_names
                            for task in self.store.list_tasks())
            cached_names = {task["instance_name"] for task in self.store.list_tasks() if task["state"] == "cached"}
            cached = sum(item.get("name") in cached_names and str(item.get("status", "")).lower() == "stopped" for item in instances)
            slot_limit = None
            for value in limits.values():
                try:
                    candidate = int(value)
                except (TypeError, ValueError) as exc:
                    raise ControlError("resource_unverified", "instance slot limit is not numeric") from exc
                if candidate >= 0:
                    if slot_limit is not None and slot_limit != candidate:
                        raise ControlError("resource_unverified", "instance slot limits disagree")
                    slot_limit = candidate
            allocated_and_reserved = len(instances) + preparing
            return {"verified": True, "total": len(instances), "running": running,
                    "stopped": stopped, "cached": cached, "other": len(instances) - running - stopped, "limits": limits,
                    "policy_compliant": not limits,
                    "admission": {"allocated": len(instances), "reserved": preparing,
                                   "allocated_and_reserved": allocated_and_reserved,
                                   "remaining": None if slot_limit is None else max(0, slot_limit - allocated_and_reserved),
                                   "limit": slot_limit, "capacity": None}}
        except ControlError as exc:
            return {"verified": False, "error": exc.as_dict()}

    def _default_admission_diagnostics(
        self,
        resources: Mapping[str, Any],
        capacity: Mapping[str, Any],
        instances: Mapping[str, Any],
        *,
        global_locked: bool = False,
    ) -> dict[str, Any]:
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
        warnings = []
        if resources.get("memory_pressure"):
            warnings.append({"code": "memory_pressure", "memory": resources.get("memory")})
        cpu_startup_pressure = None
        if resources.get("verified"):
            cpu_startup_pressure = self._cpu_startup_pressure_diagnostics(
                allocated_cpu=resources["allocated_and_reserved"]["cpu"],
                requested_cpu=requested["cpu"],
                capacity_cpu=resources["ct_capacity"]["cpu"],
                observation=resources.get("cpu_pressure"),
            )
            if cpu_startup_pressure["startup_blocked"]:
                reasons.append({"code": "resource_pressure", "message": "CPU scheduler pressure blocks VM startup",
                                "detail": cpu_startup_pressure})
            elif not cpu_startup_pressure["verified"]:
                reasons.append({"code": "resource_unverified",
                                "message": "CT CPU pressure observation is not verified",
                                "detail": cpu_startup_pressure})
        if not instances.get("verified"):
            reasons.append(instances["error"])
        elif not instances["policy_compliant"] and not any(reason["code"] == "project_instance_limits_conflict" for reason in reasons):
            reasons.append({"code": "project_instance_limits_conflict", "conflicting_limits": instances["limits"]})
        reclaim_required = None
        if "error" in capacity:
            reasons.append(capacity["error"])
        elif resources.get("verified"):
            lock = nullcontext() if global_locked else self._global_lock()
            with lock, self.store.transaction():
                reclaim_required = not self._space_sufficient(capacity, self.config["create_cost_bytes"], "low")
            if reclaim_required:
                reasons.append({"code": "disk_reclaim_required", "message": "default creation needs capacity reclamation"})
        return {"requested": requested, "request_cost_bytes": self.config["create_cost_bytes"],
                "ready": not reasons, "reasons": reasons, "reclaim_required": reclaim_required,
                "observations": {"memory_pressure": resources.get("memory_pressure"),
                                 "cpu_startup_pressure": cpu_startup_pressure},
                "warnings": warnings}

    def _reserve_resources(self, task: Mapping[str, Any], resources: Mapping[str, int],
                           *, memory_reduction: bool = False,
                           startup_required: bool = True,
                           startup_cpu: int | None = None,
                           admission_epoch: str | None = None) -> dict[str, Any]:
        # Only a verified, memory-only reduction may bypass new-demand gates.
        # Its old allocation stays reserved until native completion is verified.
        if memory_reduction:
            old = self._durable_resources(task, "resources_json", allow_empty=False)
            reservation = self._durable_resources(task, "resource_reservation_json", allow_empty=True)
            if (task["state"] != "active" or resources != old or reservation):
                raise ControlError("resize_unverified", "memory reduction requires an intact current allocation")
            policy = None
        else:
            policy = self._resource_policy()
            self._running_inventory_guard()
            disk = self._disk_admission_diagnostics(self._storage_headroom(0))
            if not disk.get("verified"):
                raise ControlError("resource_unverified", "disk admission state is not verified", disk.get("error"))
        with self._admission_lock(), self._global_lock(), self.store.transaction():
            if self.store.get_meta("draining") == "1":
                raise ControlError("draining", "new leases and capacity changes are refused while draining")
            current_epoch = self.store.get_meta("admission_epoch") or "initial"
            if admission_epoch is not None and current_epoch != admission_epoch:
                raise ControlError("draining", "admission state changed while the request was preparing")
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
            if policy and startup_required:
                self._enforce_cpu_startup_pressure(
                    allocated_cpu=totals["cpu"],
                    requested_cpu=resources["cpu"] if startup_cpu is None else startup_cpu,
                    capacity_cpu=policy["ct_capacity"]["cpu"],
                )
            record = dict(task, resource_reservation_json=json.dumps(dict(resources), sort_keys=True))
            if not record.get("resources_json"):
                record["resources_json"] = json.dumps(dict(resources), sort_keys=True)
            self.store.put_task(record)
            return record

    def _reserve_resources_with_transition_wait(
        self,
        task: Mapping[str, Any],
        resources: Mapping[str, int],
        *,
        startup_cpu: int | None = None,
        admission_epoch: str | None = None,
    ) -> dict[str, Any]:
        """Wait for an unrelated native cleanup to leave the admission fence.

        Lease expiry cleanup is deliberately outside the global admission lock
        while it performs native Incus work. A new acquire may therefore observe
        a short-lived transitional instance even though the durable state is
        healthy. Keep the same operation pending during that narrow window;
        every other unverified condition still fails closed immediately.
        """
        started = time.monotonic()
        attempts = 0
        while True:
            try:
                return self._reserve_resources(
                    task,
                    resources,
                    startup_cpu=startup_cpu,
                    admission_epoch=admission_epoch,
                )
            except ControlError as exc:
                if (exc.code != "resource_unverified"
                        or exc.message != TRANSITIONAL_ADMISSION_ERROR_MESSAGE):
                    raise
                elapsed = time.monotonic() - started
                remaining = TRANSITIONAL_ADMISSION_WAIT_SECONDS - elapsed
                if remaining <= 0:
                    detail = dict(exc.detail) if isinstance(exc.detail, Mapping) else {}
                    detail.update({
                        "admission_wait_seconds": round(max(0.0, elapsed), 3),
                        "admission_retry_count": attempts,
                    })
                    raise ControlError(exc.code, exc.message, detail) from exc
                attempts += 1
                time.sleep(min(TRANSITIONAL_ADMISSION_POLL_SECONDS, remaining))

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

    @staticmethod
    def _durable_resources(task: Mapping[str, Any], field: str, *, allow_empty: bool) -> dict[str, int]:
        try:
            return parse_resource_mapping(task.get(field), field, allow_empty=allow_empty)
        except (TypeError, ValueError) as exc:
            raise ControlError("resource_unverified", f"task {field} cannot be verified") from exc



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

    def _verify_native_allocation(self, task: Mapping[str, Any]) -> None:
        expected = self._durable_resources(task, "resources_json", allow_empty=False)
        reservation = self._durable_resources(task, "resource_reservation_json", allow_empty=True)
        if reservation:
            expected = reservation
            native = self._native_resources(task)
            if any(native[key] > expected[key] for key in expected):
                raise ControlError("resource_unverified", "native resources exceed the durable reservation")
        elif self._native_resources(task) != expected:
            raise ControlError("resource_unverified", "native resources differ from durable allocation")

    def _verify_network_device(self, task: Mapping[str, Any]) -> None:
        info = self._ownership(task)
        devices = info.get("expanded_devices")
        eth0 = devices.get("eth0") if isinstance(devices, Mapping) else None
        expected = {
            "type": "nic",
            "network": self.config["network"],
            "ipv4.address": task["vm_ip"],
            "security.mac_filtering": "true",
            "security.ipv4_filtering": "true",
            "security.ipv6_filtering": "true",
            "security.port_isolation": "true",
            "security.acls": self.config["network_acl"],
            "security.acls.default.ingress.action": "allow",
            "security.acls.default.egress.action": "allow",
        }
        if not isinstance(eth0, Mapping) or any(eth0.get(key) != value for key, value in expected.items()):
            raise ControlError("ownership_mismatch", "native network isolation differs from the managed configuration")

    def _host_ports(self) -> set[int]:
        result = self.runner.run(["ss", "-Hlnptu"], timeout=5)
        return parse_listen_ports(result.stdout.decode())


    def _native_ports(self) -> set[int]:
        ports: set[int] = set()
        projects = self.incus.query("/1.0/projects?recursion=1", project="default")
        projects = self._inventory_mappings(projects, "port_inventory_unverified", "project")
        for project in projects:
            try:
                name = require_nonempty_string(project.get("name"), "project name")
            except ControlError as exc:
                raise ControlError("port_inventory_unverified", "project inventory contains an invalid name") from exc
            instances = self.incus.query("/1.0/instances?recursion=1", project=name)
            networks = self.incus.query("/1.0/networks?recursion=1", project=name)
            instances = self._inventory_mappings(instances, "port_inventory_unverified", "instance")
            networks = self._inventory_mappings(networks, "port_inventory_unverified", "network")
            for instance in instances:
                try:
                    require_nonempty_string(instance.get("name"), "instance name")
                except ControlError as exc:
                    raise ControlError("port_inventory_unverified", "instance inventory contains an invalid name") from exc
                if "expanded_devices" not in instance:
                    raise ControlError("port_inventory_unverified", "instance devices are unavailable")
                devices = instance.get("expanded_devices")
                if not isinstance(devices, Mapping) or any(not isinstance(device, Mapping) for device in devices.values()):
                    raise ControlError("port_inventory_unverified", "instance devices are malformed")
                for device in devices.values():
                    if device.get("type") == "proxy":
                        listen = device.get("listen")
                        if not isinstance(listen, str) or not listen:
                            raise ControlError("port_inventory_unverified", "proxy listen address is malformed")
                        if listen.startswith(("tcp:", "udp:")):
                            ports |= self._port_numbers(listen.rsplit(":", 1)[-1])
                        elif not listen.startswith(("unix:", "vsock:")):
                            raise ControlError("port_inventory_unverified", "proxy listen address is unrecognized")
            for network in networks:
                network_name = require_nonempty_string(network.get("name"), "network name")
                if not network.get("managed") or network.get("type") != "bridge": continue
                forwards = self.incus.query("/1.0/networks/" + quote(network_name, safe="") + "/forwards?recursion=1", project=name)
                forwards = self._inventory_mappings(forwards, "port_inventory_unverified", "forward")
                for forward in forwards:
                    if "config" not in forward or "ports" not in forward:
                        raise ControlError("port_inventory_unverified", "network forward fields are unavailable")
                    config = forward.get("config")
                    if not isinstance(config, Mapping):
                        raise ControlError("port_inventory_unverified", "network forward config is malformed")
                    if config.get("target_address"):
                        ports.update(range(self.config["port_start"], self.config["port_end"] + 1))
                    forward_ports = forward.get("ports")
                    if not isinstance(forward_ports, list) or any(not isinstance(item, Mapping) for item in forward_ports):
                        raise ControlError("port_inventory_unverified", "network forward ports are malformed")
                    for item in forward_ports:
                        listen_port = item.get("listen_port")
                        if not isinstance(listen_port, str):
                            raise ControlError("port_inventory_unverified", "network forward listen port is malformed")
                        ports |= self._port_numbers(listen_port)
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
        projects = self._inventory_mappings(projects, "vm_ip_unverified", "project")
        for project in projects:
            name = require_nonempty_string(project.get("name"), "project name")
            instances = self._inventory_mappings(
                self.incus.query("/1.0/instances?recursion=1", project=name),
                "vm_ip_unverified",
                "instance",
            )
            for instance in instances:
                try:
                    require_nonempty_string(instance.get("name"), "instance name")
                except ControlError as exc:
                    raise ControlError("vm_ip_unverified", "instance inventory contains an invalid name") from exc
                if "expanded_devices" not in instance:
                    raise ControlError("vm_ip_unverified", "instance devices are unavailable")
                devices = instance.get("expanded_devices")
                if not isinstance(devices, Mapping) or any(not isinstance(device, Mapping) for device in devices.values()):
                    raise ControlError("vm_ip_unverified", "instance devices are malformed")
                if name == self.config["project"] and instance.get("name") == own_name:
                    nic = devices.get("eth0", {})
                    if not isinstance(nic, Mapping):
                        raise ControlError("vm_ip_unverified", "instance network device is malformed")
                    instance_config = instance.get("config", {})
                    if not isinstance(instance_config, Mapping):
                        raise ControlError("vm_ip_unverified", "instance config is malformed")
                    own_mac = nic.get("hwaddr") or instance_config.get("volatile.eth0.hwaddr")
                    own_ip = nic.get("ipv4.address")
                    if own_mac is not None and not isinstance(own_mac, str):
                        raise ControlError("vm_ip_unverified", "instance MAC address is malformed")
                    if own_ip is not None and not isinstance(own_ip, str):
                        raise ControlError("vm_ip_unverified", "instance IP address is malformed")
                    if own_ip:
                        try:
                            ipaddress.IPv4Address(own_ip)
                        except ipaddress.AddressValueError as exc:
                            raise ControlError("vm_ip_unverified", "instance IP address is invalid") from exc
                    continue
                for device in devices.values():
                    network = device.get("network")
                    parent = device.get("parent")
                    address = device.get("ipv4.address")
                    if ((network is not None and not isinstance(network, str))
                            or (parent is not None and not isinstance(parent, str))):
                        raise ControlError("vm_ip_unverified", "instance network identity is malformed")
                    if address is not None and not isinstance(address, str):
                        raise ControlError("vm_ip_unverified", "instance IP address is malformed")
                    if address:
                        try:
                            ipaddress.IPv4Address(address)
                        except ipaddress.AddressValueError as exc:
                            raise ControlError("vm_ip_unverified", "instance IP address is invalid") from exc
                    if self.config["network"] in {network, parent} and address:
                        used.add(address)
        leases = self.incus.query("/1.0/networks/" + quote(self.config["network"], safe="") + "/leases", project="default")
        leases = self._inventory_mappings(leases, "vm_ip_unverified", "network lease")
        for lease in leases:
            address = lease.get("address")
            hwaddr = lease.get("hwaddr")
            if not isinstance(address, str) or not address:
                raise ControlError("vm_ip_unverified", "network lease address is malformed")
            try:
                ipaddress.IPv4Address(address)
            except ipaddress.AddressValueError as exc:
                raise ControlError("vm_ip_unverified", "network lease address is invalid") from exc
            if hwaddr is not None and not isinstance(hwaddr, str):
                raise ControlError("vm_ip_unverified", "network lease MAC address is malformed")
            if own_mac and hwaddr and hwaddr.lower() == own_mac.lower() and address == own_ip:
                continue
            used.add(address)
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
                for key in ("reaped", "pipes_drained", "process_group_killed", "process_group_cleanup_error"):
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
        return parse_cpu_psi(raw)

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
            observation = self._cpu_pressure_observation()
            if observation.get("verified"):
                payload["cpu_psi"] = observation
            else:
                error = observation.get("error")
                if isinstance(error, Mapping):
                    errors.append({
                        "source": "cpu_psi",
                        "type": "ControlError",
                        "code": error.get("code", "resource_unverified"),
                    })
                else:
                    errors.append({"source": "cpu_psi", "type": "ResourceObservationError"})
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
        if not isinstance(project, Mapping) or not isinstance(project.get("config"), Mapping):
            raise ControlError("network_unverified", "Incus project response is malformed")
        project_config = project["config"]
        if project_config.get("restricted.devices.proxy") != "allow":
            raise ControlError("network_unverified", "Incus project does not allow managed proxy NAT devices")
        acl = self.incus.query("/1.0/network-acls/" + quote(name, safe=""), project="default")
        if not isinstance(acl, Mapping) or not isinstance(acl.get("config"), Mapping):
            raise ControlError("network_unverified", "isolation ACL response is not an object")
        if acl["config"].get("user.agent-vm.managed") != "native-v1":
            raise ControlError("network_unverified", "isolation ACL ownership is not verified")
        required = self._isolation_rules()
        fields = ("action", "state", "source", "destination", "protocol", "source_port", "destination_port", "icmp_type", "icmp_code")
        for direction, rules in required.items():
            actual = acl.get(direction)
            if not isinstance(actual, list) or any(not isinstance(value, Mapping) for value in actual):
                raise ControlError("network_unverified", "isolation ACL rules are malformed")
            normalized = [{key: value.get(key, "enabled" if key == "state" else "") for key in fields}
                          for value in actual]
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
        pending_proxy_removals: dict[str, tuple[Mapping[str, Any], str, bool]] = {}
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
            proxy_error = None
            try:
                if exists: self._remove_proxy(task, port)
            except ControlError as exc:
                proxy_error = "remove-proxy:" + port["device_name"] + ":" + exc.code
            current = self._cleanup_current(task)
            if current is None:
                errors.append("stale-cleanup:generation")
                break
            task = current
            conntrack_ok = False
            try:
                self._conntrack_delete(port, task)
                conntrack_ok = True
            except ControlError as exc:
                errors.append("withdraw:" + port["device_name"] + ":" + exc.code)
            if proxy_error is None and conntrack_ok:
                completed.append("withdraw:" + port["device_name"])
            elif proxy_error is not None:
                pending_proxy_removals[port["device_name"]] = (port, proxy_error, conntrack_ok)
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
        # A stopped instance makes a late retry safe and avoids retaining a
        # native proxy after a transient removal failure.
        for device_name, (port, proxy_error, conntrack_ok) in pending_proxy_removals.items():
            current = self._cleanup_current(task)
            if current is None:
                errors.extend([proxy_error, "remove-proxy-retry:" + device_name + ":stale-cleanup"])
                continue
            task = current
            try:
                self._remove_proxy(task, port)
            except ControlError as exc:
                errors.extend([proxy_error, "remove-proxy-retry:" + device_name + ":" + exc.code])
            else:
                if conntrack_ok:
                    completed.append("withdraw:" + device_name)
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

    def doctor(
        self,
        request: Mapping[str, Any],
        session_id: str | None,
    ) -> dict[str, Any]:
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
                try:
                    result = self.runner.run(["systemctl", "is-active", "--quiet", unit], timeout=5, check=False)
                except ControlError:
                    issues.append("unit_unverified:" + unit)
                else:
                    if result.returncode: issues.append("unit_inactive:" + unit)
            if not self.systemd.enabled_unit_active("agent-vm-native-age-gc.timer"):
                issues.append("unit_disabled:agent-vm-native-age-gc.timer")
            for unit in ("agent-vm-native-expire@.service", "agent-vm-native-reconcile.service"):
                required = {
                    "agent-vm-native-expire@.service": (
                        "ExecStart=/usr/bin/python3 /opt/agent-vm-native/agent_vm_control.py --timer-expire %i",
                    ),
                    "agent-vm-native-reconcile.service": (
                        "ExecStart=/usr/bin/python3 /opt/agent-vm-native/agent_vm_control.py --timer-reconcile",
                    ),
                }[unit]
                if not self.systemd.service_asset_active(unit, required):
                    issues.append("unit_unverified:" + unit)
                if unit == "agent-vm-native-reconcile.service" and not self.systemd.enabled_service_ready(unit):
                    issues.append("unit_disabled:" + unit)
            if not self.systemd.service_asset_active(
                "agent-vm-native-age-gc.service",
                ("ExecStart=/usr/bin/python3 /opt/agent-vm-native/agent_vm_control.py --timer-age-gc",),
            ):
                issues.append("unit_unverified:agent-vm-native-age-gc.service")
            for task in self.store.list_tasks():
                if task.get("state") != "active":
                    continue
                try:
                    valid_timer = self.systemd.expiry_active(
                        str(task["task_id"]), int(task["generation"]), str(task["lease_expires_at"])
                    )
                except (ControlError, KeyError, TypeError, ValueError):
                    valid_timer = False
                if not valid_timer:
                    issues.append("timer_unverified:" + str(task.get("task_id")))
        resources = self._resource_diagnostics(
            capacity_observation=capacity if "error" not in capacity else None,
        )
        instance_policy = self._instance_diagnostics()
        admission = self._default_admission_diagnostics(
            resources, capacity, instance_policy
        )
        return {"ready": not issues, "issues": issues, "incus_version": version, "capacity": capacity,
                "resources": resources, "instances": instance_policy, "default_admission": admission,
                "admission_ready": not issues and admission["ready"],
                "release": self._release_metadata(),
                "release_warnings": self._merge_warnings(self.release_warnings),
                "config": {key: self.config[key] for key in ("project", "profile", "image_fingerprint", "network", "pool", "host_address")}}


    def status(self, request: Mapping[str, Any], session_id: str | None) -> dict[str, Any]:
        assert session_id is not None
        with self._global_lock():
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
            resources = self._durable_resources(existing, "resources_json", allow_empty=False)
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
        with self._admission_lock():
            if self.store.get_meta("draining") == "1":
                raise ControlError("draining", "new leases are refused")
            admission_epoch = self.store.get_meta("admission_epoch") or "initial"
        self._network_guard()
        public_key = require_nonempty_string(request.get("public_key"), "public_key")
        if not re.fullmatch(r"ssh-ed25519 [A-Za-z0-9+/=]+(?: [^\r\n]*)?", public_key):
            raise ControlError("invalid_request", "acquire requires a single ed25519 public key")
        cached = bool(existing and existing["state"] == "cached")
        if not cached:
            self._image_guard()
        if cached:
            self._verify_network_device(existing)
            if self.incus.instance_info(existing["instance_name"])["status"].lower() != "stopped" or self.store.get_ports(existing["task_id"]):
                raise ControlError("cache_ineligible", "cached instance is not safely stopped")
        old = self._durable_resources(existing, "resources_json", allow_empty=False) if cached else {"cpu": 2, "memory_mib": 4096, "disk_gib": 20}
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
        task = self._reserve_resources_with_transition_wait(
            task,
            reserved,
            startup_cpu=resources["cpu"],
            admission_epoch=admission_epoch,
        )
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
                if self.store.get_meta("draining") == "1":
                    raise ControlError("draining", "new lease entered drain before commit")
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
            native_init_cleanup_verified = (
                self._native_init_timeout_cleanup_verified(exc)
                if phase == "native-init"
                else None
            )
            native_init_uncertain = phase == "native-init" and (
                exc.code == "command_timeout" or native_init_cleanup_verified is False
            )
            try:
                cleanup["result"] = self._release_task(
                    self.store.get_task(task_id=task["task_id"]),
                    require_native_init_absence_retry=native_init_uncertain,
                    native_init_cleanup_verified=native_init_cleanup_verified,
                    protect_native_init_residue=phase == "native-init",
                    recover_pending_acquire=False,
                )
                cleanup["ok"] = True
            except ControlError as cleanup_error:
                cleanup = {"ok": False, "error": cleanup_error.as_dict()}
            detail = dict(exc.detail) if isinstance(exc.detail, Mapping) else {"cause": exc.as_dict()}
            failure_phase = detail.get("phase", phase)
            detail.update({"phase": failure_phase, "operation_phase": phase, "completed": completed,
                           "requested": resources, "cleanup": cleanup})
            if native_init_uncertain:
                detail["native_init_absence_retry"] = True
                detail["native_init_cleanup_verified"] = native_init_cleanup_verified
                cleanup_error = cleanup.get("error") if isinstance(cleanup, Mapping) else None
                cleanup_detail = cleanup_error.get("detail") if isinstance(cleanup_error, Mapping) else None
                detail["native_init_absence_retry_after"] = (
                    cleanup_detail.get("native_init_absence_retry_after")
                    if isinstance(cleanup_detail, Mapping)
                    and cleanup_detail.get("native_init_absence_retry_after")
                    else iso_time(self.now() + dt.timedelta(seconds=NATIVE_INIT_ABSENCE_RETRY_SECONDS))
                )
                if isinstance(cleanup_detail, Mapping):
                    detail["native_init_absence_checks"] = cleanup_detail.get(
                        "native_init_absence_checks", 1
                    )
                    detail["native_init_cleanup_verified"] = cleanup_detail.get(
                        "native_init_cleanup_verified", native_init_cleanup_verified
                    )
            elif phase == "native-init":
                detail["native_init_absence_retry"] = False
            if readiness is not None:
                detail["readiness"] = readiness
            error = {"code": "acquire_partial", **detail, "error": exc.as_dict()}
            self.store.update_task(task["task_id"], last_error=json.dumps(error))
            raise ControlError(exc.code, exc.message, detail) from exc


    def renew(
        self,
        request: Mapping[str, Any],
        session_id: str | None,
        compatibility_warnings: Iterable[Mapping[str, Any]] = (),
    ) -> dict[str, Any]:
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
                        "response": self._response_compatibility(),
                        "release": self._release_metadata(),
                        "warnings": self._merge_warnings(compatibility_warnings),
                        "_request_digest": hashlib.sha256(json.dumps(dict(request), sort_keys=True).encode()).hexdigest()}
            self.store.record_operation(request["operation_id"], "renew", session_id, request["generation"], response)
        self._schedule_expiry(task)
        return result


    def release(self, request: Mapping[str, Any], session_id: str | None) -> dict[str, Any]:
        assert session_id is not None
        task = self._task_for_session(session_id)
        self._check_generation(task, request)
        return self._release_task(task)

    def _release_task(
        self,
        task: Mapping[str, Any],
        *,
        require_native_init_absence_retry: bool = False,
        native_init_cleanup_verified: bool | None = None,
        # A failed native init may leave a stopped, partially unpacked guest.
        # Never turn that residue into a reusable cache entry.
        protect_native_init_residue: bool = False,
        recover_pending_acquire: bool = True,
    ) -> dict[str, Any]:
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
            with self._global_lock(), self.store.transaction():
                task = self.store.update_task(task["task_id"], state="closing", lease_expires_at=None,
                                             inactive_since=iso_time(inactive),
                                             cleanup_after=iso_time(inactive + dt.timedelta(seconds=self.config["cache_seconds"])))
            errors, completed = self._cleanup_task(task)
            if errors:
                self.store.update_task(task["task_id"], last_error=json.dumps({"errors": errors, "completed": completed}))
                # Reuse the native expiry oneshot's bounded Restart=on-failure path.
                self.systemd.retry_cleanup(task["task_id"], task["generation"])
                raise ControlError("cleanup_incomplete", "entry/port reservations remain closed until reconciliation succeeds", errors)
            try:
                instances = self.incus.list_instances()
            except ControlError as exc:
                detail = {"native_absence": "unverified", "cause": exc.as_dict()}
                self.store.update_task(task["task_id"], last_error=json.dumps(detail))
                self.systemd.retry_cleanup(task["task_id"], task["generation"])
                raise ControlError(
                    "cleanup_incomplete",
                    "native absence cannot be verified; delayed reconciliation is required",
                    detail,
                ) from exc
            absent = not any(item.get("name") == task["instance_name"] for item in instances)
            try:
                pending_acquire = (
                    self._pending_native_init_for_task(task)
                    if recover_pending_acquire
                    else None
                )
            except ControlError as exc:
                detail = {"native_absence": "unverified", "cause": exc.as_dict()}
                self.store.update_task(task["task_id"], last_error=json.dumps(detail))
                self.systemd.retry_cleanup(task["task_id"], task["generation"])
                raise ControlError(
                    "cleanup_incomplete",
                    "pending native initialization cannot be verified; delayed reconciliation is required",
                    detail,
                ) from exc
            delayed_absence = self._native_init_absence_retry_pending(task)
            retry_required = require_native_init_absence_retry or delayed_absence or pending_acquire is not None
            retry_ready = self._native_init_absence_retry_ready(task)
            cleanup_verified = (
                native_init_cleanup_verified
                if native_init_cleanup_verified is not None
                else self._native_init_cleanup_verified(task)
            )
            native_init_retry_required = (
                require_native_init_absence_retry
                or delayed_absence
                or (protect_native_init_residue and not absent)
            )
            if native_init_retry_required and not absent:
                if cleanup_verified is True:
                    try:
                        with self._native_lock(task):
                            current = self._cleanup_current(task)
                            if current is None:
                                raise ControlError("generation_mismatch", "cleanup generation changed")
                            self._ownership(current)
                            self.incus.delete(current["instance_name"])
                            if any(
                                item.get("name") == current["instance_name"]
                                for item in self.incus.list_instances()
                            ):
                                raise ControlError(
                                    "cleanup_incomplete",
                                    "uncertain native init instance deletion is not confirmed",
                                )
                        absent = True
                    except ControlError as exc:
                        detail = {
                            "native_init_absence_retry": True,
                            "outcome_verified": False,
                            "native_absence": "present",
                            "native_init_cleanup_verified": cleanup_verified,
                            "cause": exc.as_dict(),
                        }
                        prior = self._native_init_absence_retry_record(task) or {}
                        if isinstance(prior.get("error"), Mapping):
                            detail["error"] = prior["error"]
                        self.store.update_task(task["task_id"], last_error=json.dumps(detail))
                        self.systemd.retry_cleanup(task["task_id"], task["generation"])
                        raise ControlError(
                            "cleanup_incomplete",
                            "uncertain native init instance cannot be removed; delayed reconciliation is required",
                            detail,
                        ) from exc
                else:
                    detail = {
                        "native_init_absence_retry": True,
                        "outcome_verified": False,
                        "native_absence": "present",
                        "native_init_cleanup_verified": cleanup_verified,
                    }
                    prior = self._native_init_absence_retry_record(task) or {}
                    if isinstance(prior.get("error"), Mapping):
                        detail["error"] = prior["error"]
                    self.store.update_task(task["task_id"], last_error=json.dumps(detail))
                    self.systemd.retry_cleanup(task["task_id"], task["generation"])
                    raise ControlError(
                        "cleanup_incomplete",
                        "native init cleanup is unverified; delayed reconciliation is required",
                        detail,
                    )
            absence_checks = self._native_init_absence_checks(task)
            if absent and retry_required:
                absence_checks += 1
            stable_absence = retry_ready and absence_checks >= 2
            absence_verified = cleanup_verified is True or (
                cleanup_verified is None and pending_acquire is not None and stable_absence
            )
            if absent and retry_required and not (stable_absence and absence_verified):
                retry_after = self._native_init_retry_after(task)
                detail = {
                    "native_init_absence_retry": True,
                    "outcome_verified": False,
                    "native_absence": "pending",
                    "native_init_absence_retry_after": retry_after,
                    "native_init_absence_checks": absence_checks,
                    "native_init_cleanup_verified": cleanup_verified,
                }
                prior = self._native_init_absence_retry_record(task) or {}
                if isinstance(prior.get("error"), Mapping):
                    detail["error"] = prior["error"]
                self.store.update_task(task["task_id"], last_error=json.dumps(detail))
                self.systemd.retry_cleanup(task["task_id"], task["generation"])
                raise ControlError(
                    "cleanup_incomplete",
                    "native init result remains unresolved; delayed reconciliation is required",
                    detail,
                )
            recovered_operation = None
            if absent and pending_acquire is not None and stable_absence and absence_verified:
                try:
                    recovered_operation = self._recovered_native_init_operation(task, pending_acquire)
                except ControlError as exc:
                    detail = {
                        "native_absence": "verified",
                        "outcome_verified": False,
                        "cause": exc.as_dict(),
                    }
                    self.store.update_task(task["task_id"], last_error=json.dumps(detail))
                    self.systemd.retry_cleanup(task["task_id"], task["generation"])
                    raise ControlError(
                        "cleanup_incomplete",
                        "native init operation cannot be finalized; delayed reconciliation is required",
                        detail,
                    ) from exc
            self.systemd.remove_expiry(task["task_id"], task["generation"])
            if not absent and task.get("previous_generation") is not None:
                self._ownership(task)
                self.incus.config_set(task["instance_name"], "user.agent-vm.generation", str(task["generation"]))
            if absent:
                try:
                    resources = parse_resource_mapping(task.get("resources_json"), "resources", allow_empty=False)
                except (TypeError, ValueError) as exc:
                    raise ControlError("resource_unverified", "absent task resources cannot be verified") from exc
            else:
                resources = self._native_resources(task)
            with self._global_lock(), self.store.transaction():
                for port in self.store.get_ports(task["task_id"]):
                    self.store.remove_port(task["task_id"], port["port_name"], port["protocol"])
                if recovered_operation is not None:
                    operation_id, operation = recovered_operation
                    self.store.record_operation(
                        operation_id,
                        "acquire",
                        task["session_id"],
                        task["generation"],
                        operation,
                    )
                task = self.store.update_task(task["task_id"], state="absent" if absent else "cached",
                                             ssh_host_port=None, reservation_bytes=0, last_error=None, previous_generation=None, resource_reservation_json='{}',
                                             resources_json=json.dumps(resources),
                                             vm_ip=None if absent else task["vm_ip"])
            return self._state_summary(task)


    def destroy(self, request: Mapping[str, Any], session_id: str | None) -> dict[str, Any]:
        assert session_id is not None
        task = self.store.get_task(session_id=session_id)
        if task is None: raise ControlError("task_missing", "no owned instance exists")
        self._validate_task_state(task)
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
        phase = "native-stop"
        stage_timings: dict[str, Any] = {}
        readiness = None
        native_admitted = False
        native_touched = False
        try:
            # The short fence admits the restart atomically with the drain check.
            # Native stop/start stays outside the global lock; a drain that lands
            # after this fence does not interrupt an already admitted operation.
            with self._global_lock():
                if self.store.get_meta("draining") == "1":
                    raise ControlError("draining", "restart work is refused while draining")
                self._ownership(task)
                self._verify_native_allocation(task)
                self._enforce_cpu_startup_pressure(
                    allocated_cpu=self._allocation_totals()["cpu"],
                    requested_cpu=0,
                    capacity_cpu=self.resources.cpu(),
                )
                self._network_guard()
                self._verify_network_device(task)
                self._require_window(task)
                native_admitted = True
            native_touched = True
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
            if not native_admitted:
                raise
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
        old = self._durable_resources(task, "resources_json", allow_empty=False)
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
        previous_reservation = self._durable_resources(task, "resource_reservation_json", allow_empty=True)
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
            self._reserve_resources(task, reserved, startup_cpu=target["cpu"])
            try:
                reclaimed = self._reclaim_capacity(cost, task["task_id"])
                self._reserve_native(task, cost)
            except ControlError:
                self._cancel_resource_reservation(task)
                raise
        completed = []
        guest = None
        guest_growth_attempted = False
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
                guest_growth_attempted = True
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
            cleanup_performed = needs_restart or guest_growth_attempted
            cleanup = {"performed": cleanup_performed, "ok": True}
            if cleanup_performed:
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
        if self._durable_resources(task, "resource_reservation_json", allow_empty=True):
            raise ControlError("resize_incomplete", "a previous partial resize requires inspection and explicit restart recovery or release")
        if self._native_resources(task) != old:
            raise ControlError("resize_unverified", "native configuration differs from the recorded allocation")
        before = self._guest_boot_id(task)
        if target == old:
            return {"applied": False, "mode": "live-memory", "plan": "unchanged", **self._state_summary(task)}
        reserved = {key: max(old[key], target[key]) for key in old}
        self._reserve_resources(
            task,
            reserved,
            memory_reduction=target["memory_mib"] < old["memory_mib"],
            startup_required=False,
        )
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
                if (self._durable_resources(current, "resource_reservation_json", allow_empty=True) != reserved
                        or self._native_resources(task) != old or self._guest_boot_id(task) != before):
                    raise ControlError("resize_unverified", "native rollback or guest continuity is unverified after balloon timeout")
                self._reserve_resources(task, reserved, memory_reduction=target["memory_mib"] < old["memory_mib"], startup_required=False)
                # Large balloons keep converging after Incus's five-second
                # check. One bounded settle interval precedes the only retry.
                # Expiry can still preempt this object and withdraw access.
                time.sleep(MEMORY_SETTLE_SECONDS)
                current = self.store.get_task(task_id=task["task_id"])
                self._check_generation(current, request)
                self._require_active(current)
                self._require_window(current, minimum_seconds=60)
                if (self._durable_resources(current, "resource_reservation_json", allow_empty=True) != reserved
                        or self._native_resources(task) != old or self._guest_boot_id(task) != before):
                    raise ControlError("resize_unverified", "native rollback, reservation or guest continuity changed during balloon settlement")
                self._reserve_resources(task, reserved, memory_reduction=True, startup_required=False)
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
        command = ["python3", "-c", GUEST_ROOT_GROWTH_SCRIPT, str(disk_gib)]
        try:
            result = self.incus.exec(task["instance_name"], command, timeout=120, check=False, bounded=True)
        except ControlError as exc:
            underlying = exc.as_dict()
            transport = underlying.get("detail")
            if not isinstance(transport, Mapping):
                transport = {}
            stdout_truncated = transport.get("stdout_truncated") is True
            stderr_truncated = transport.get("stderr_truncated") is True
            raise ControlError(
                "guest_resize_unverified",
                "guest root filesystem growth command was not completed",
                {
                    "phase": "guest-exec",
                    "command": ["incus", "exec", task["instance_name"], "--", "python3", "-c", "guest-root-growth"],
                    "returncode": None,
                    "stdout": str(transport.get("stdout") or ""),
                    "stderr": str(transport.get("stderr") or ""),
                    "stdout_truncated": stdout_truncated,
                    "stderr_truncated": stderr_truncated,
                    "guest_capacity": {"verified": False, "error": underlying},
                },
            ) from exc
        stdout = result.stdout.decode("utf-8", "replace")
        stderr = result.stderr.decode("utf-8", "replace")
        stdout_truncated = getattr(result, "stdout_truncated", False) is True
        stderr_truncated = getattr(result, "stderr_truncated", False) is True
        if stdout_truncated or stderr_truncated:
            raise ControlError(
                "guest_resize_unverified",
                "guest root filesystem growth report was truncated",
                {
                    "phase": "guest-exec",
                    "command": ["incus", "exec", task["instance_name"], "--", "python3", "-c", "guest-root-growth"],
                    "returncode": result.returncode,
                    "stdout": stdout,
                    "stderr": stderr,
                    "stdout_truncated": stdout_truncated,
                    "stderr_truncated": stderr_truncated,
                    "guest_capacity": {"verified": False},
                },
            )

        failure_capacity_keys = {
            "requested_bytes", "disk_bytes", "partition_bytes", "filesystem_bytes",
            "filesystem_before", "filesystem_threshold_bytes", "filesystem", "source",
            "parent", "partition", "tools", "verified",
        }
        max_failure_retry_depth = 2

        def valid_guest_failure(error: Any, *, complete: bool) -> bool:
            pending = [(error, 0)]
            while pending:
                current, depth = pending.pop()
                required = {
                    "phase", "command", "returncode", "stdout", "stderr", "capacity", "transient",
                    "stdout_truncated", "stderr_truncated",
                }
                if not isinstance(current, Mapping) or not required.issubset(current):
                    return False
                failure_capacity = current["capacity"]
                if (
                    not isinstance(current["phase"], str)
                    or not isinstance(current["command"], list)
                    or not current["command"]
                    or any(not isinstance(item, str) for item in current["command"])
                    or (current["returncode"] is not None and (isinstance(current["returncode"], bool) or not isinstance(current["returncode"], int)))
                    or not isinstance(current["stdout"], str)
                    or not isinstance(current["stderr"], str)
                    or len(current["stdout"].encode("utf-8", "replace")) > DIAGNOSTIC_LOG_LIMIT
                    or len(current["stderr"].encode("utf-8", "replace")) > 4000
                    or not isinstance(failure_capacity, Mapping)
                    or not isinstance(failure_capacity.get("verified"), bool)
                    or isinstance(failure_capacity.get("requested_bytes"), bool)
                    or not isinstance(failure_capacity.get("requested_bytes"), int)
                    or not isinstance(current["transient"], bool)
                    or not isinstance(current["stdout_truncated"], bool)
                    or not isinstance(current["stderr_truncated"], bool)
                    or ("reaped" in current and not isinstance(current["reaped"], bool))
                    or ("pipes_drained" in current and not isinstance(current["pipes_drained"], bool))
                    or ("process_group_killed" in current and not isinstance(current["process_group_killed"], bool))
                    or (
                        "process_group_cleanup_error" in current
                        and current["process_group_cleanup_error"] is not None
                        and not isinstance(current["process_group_cleanup_error"], str)
                    )
                    or ("cause" in current and not isinstance(current["cause"], str))
                    or (
                        current.get("cause") == "command_timeout"
                        and (
                            not isinstance(current.get("reaped"), bool)
                            or not isinstance(current.get("pipes_drained"), bool)
                            or not isinstance(current.get("process_group_killed"), bool)
                            or "process_group_cleanup_error" not in current
                        )
                    )
                ):
                    return False
                if complete:
                    if not failure_capacity_keys.issubset(failure_capacity):
                        return False
                    for key in (
                        "disk_bytes", "partition_bytes", "filesystem_bytes",
                        "filesystem_before", "filesystem_threshold_bytes",
                    ):
                        if isinstance(failure_capacity.get(key), bool) or not isinstance(failure_capacity.get(key), int):
                            return False
                    if (
                        not isinstance(failure_capacity.get("filesystem"), str)
                        or not isinstance(failure_capacity.get("source"), str)
                        or not isinstance(failure_capacity.get("parent"), str)
                        or not isinstance(failure_capacity.get("partition"), str)
                        or not isinstance(failure_capacity.get("tools"), Mapping)
                    ):
                        return False
                if "retry_of" in current:
                    if depth >= max_failure_retry_depth or not isinstance(current["retry_of"], Mapping):
                        return False
                    pending.append((current["retry_of"], depth + 1))
            return True

        def matches_guest_failure(reference: Mapping[str, Any], candidate: Mapping[str, Any]) -> bool:
            return all(candidate.get(key) == reference.get(key) for key in (
                "phase", "command", "returncode", "stdout", "stderr", "capacity", "transient", "cause",
                "stdout_truncated", "stderr_truncated", "reaped", "pipes_drained",
                "process_group_killed", "process_group_cleanup_error",
            ))

        def same_guest_failure(reference: Mapping[str, Any], candidate: Mapping[str, Any]) -> bool:
            return all(candidate.get(key) == reference.get(key) for key in (
                "phase", "command", "returncode", "stdout", "stderr", "transient", "cause", "retry_of",
                "stdout_truncated", "stderr_truncated", "reaped", "pipes_drained",
                "process_group_killed", "process_group_cleanup_error",
            ))

        def valid_guest_attempts(attempts: Any) -> bool:
            if not isinstance(attempts, list) or not attempts or len(attempts) > 2:
                return False
            for index, record in enumerate(attempts):
                if (
                    not isinstance(record, Mapping)
                    or isinstance(record.get("attempt"), bool)
                    or not isinstance(record.get("attempt"), int)
                    or record.get("attempt") != index + 1
                    or ("error" in record and not valid_guest_failure(record["error"], complete=True))
                    or ("refresh_error" in record and not valid_guest_failure(record["refresh_error"], complete=True))
                ):
                    return False
                if "refresh_error" in record:
                    refresh_error = record["refresh_error"]
                    previous_error = record.get("error")
                    if (
                        not isinstance(refresh_error.get("retry_of"), Mapping)
                        or not isinstance(previous_error, Mapping)
                        or not matches_guest_failure(previous_error, refresh_error["retry_of"])
                    ):
                        return False
                    if refresh_error.get("transient") is False and index != len(attempts) - 1:
                        return False
                    if refresh_error.get("transient") is True and index == len(attempts) - 1:
                        return False
            return True

        def valid_guest_failure_report(error: Any) -> bool:
            if not isinstance(error, Mapping) or not valid_guest_failure(error, complete=False):
                return False
            failure_capacity = error["capacity"]
            attempts = failure_capacity.get("attempts") if isinstance(failure_capacity, Mapping) else None
            if not isinstance(attempts, list) or len(attempts) > 2:
                return False
            if not attempts:
                return error["capacity"].get("verified") is False and "retry_of" not in error
            if not valid_guest_failure(error, complete=True) or not valid_guest_attempts(attempts):
                return False
            final_record = attempts[-1]
            final_error = final_record.get("refresh_error") or final_record.get("error")
            if not isinstance(final_error, Mapping) or not same_guest_failure(error, final_error):
                return False
            if "retry_of" in error:
                previous = (
                    final_record.get("error")
                    if "refresh_error" in final_record
                    else attempts[-2].get("error") if len(attempts) > 1 else None
                )
                if not isinstance(previous, Mapping) or not matches_guest_failure(previous, error["retry_of"]):
                    return False
            return True

        guest_error = None
        for line in reversed((stderr + "\n" + stdout).splitlines()):
            try:
                parsed = json.loads(line)
            except (TypeError, ValueError):
                continue
            if isinstance(parsed, Mapping) and isinstance(parsed.get("error"), Mapping):
                guest_error = dict(parsed["error"])
                break
        if result.returncode != 0:
            guest_error = guest_error or {}
            if not valid_guest_failure_report(guest_error):
                raise ControlError(
                    "guest_resize_unverified",
                    "guest failure report is invalid",
                    {
                        "phase": "guest-report",
                        "command": ["incus", "exec", task["instance_name"], "--", "python3", "-c", "guest-root-growth"],
                        "returncode": result.returncode,
                        "stderr": stderr,
                        "stdout": stdout,
                        "cause": "invalid_failure_report",
                        "guest_capacity": {"verified": False},
                    },
                )
            detail = {
                "phase": guest_error.get("phase", "guest-exec"),
                "command": guest_error.get(
                    "command",
                    ["incus", "exec", task["instance_name"], "--", "python3", "-c", "guest-root-growth"],
                ),
                "returncode": guest_error.get("returncode", result.returncode),
                "stderr": guest_error.get("stderr", stderr),
                "stdout": guest_error.get("stdout", stdout),
                "control_stderr": stderr,
                "guest_capacity": guest_error.get("capacity", {"verified": False}),
            }
            for key in (
                "transient", "retry_of", "cause", "stdout_truncated", "stderr_truncated",
                "reaped", "pipes_drained", "process_group_killed", "process_group_cleanup_error",
            ):
                if key in guest_error:
                    detail[key] = guest_error[key]
            raise ControlError(
                "guest_resize_unverified",
                "guest root filesystem growth is unverified",
                detail,
            )
        try:
            value = json.loads(stdout)
            if not isinstance(value, dict):
                raise ValueError()
            status = value.get("status")
            if status not in {"grown", "grown_after_retry", "already_grown"}:
                raise ValueError()
            for key in ("virtual_bytes", "partition_bytes", "filesystem_before", "filesystem_bytes",
                        "filesystem_threshold_bytes", "attempts"):
                if isinstance(value.get(key), bool) or not isinstance(value.get(key), int):
                    raise ValueError()
            capacity = value.get("capacity")
            if not isinstance(capacity, Mapping) or capacity.get("verified") is not True:
                raise ValueError()
            required_capacity = {
                "status", "requested_bytes", "disk_bytes", "partition_bytes", "filesystem_bytes",
                "filesystem_before", "filesystem_threshold_bytes", "filesystem", "source", "parent", "partition", "tools",
                "attempts",
            }
            if not required_capacity.issubset(capacity):
                raise ValueError()
            for key in ("requested_bytes", "disk_bytes", "partition_bytes", "filesystem_bytes",
                        "filesystem_before", "filesystem_threshold_bytes"):
                if isinstance(capacity.get(key), bool) or not isinstance(capacity.get(key), int):
                    raise ValueError()
            if (
                not isinstance(capacity.get("attempts"), list)
                or value["attempts"] > 2
                or len(capacity["attempts"]) > 2
                or len(capacity["attempts"]) != value["attempts"]
            ):
                raise ValueError()

            failure_capacity_keys = {
                "requested_bytes", "disk_bytes", "partition_bytes", "filesystem_bytes",
                "filesystem_before", "filesystem_threshold_bytes", "filesystem", "source",
                "parent", "partition", "tools", "verified",
            }
            max_failure_retry_depth = 2

            def failure_capacity_matches_report(failure_capacity: Mapping[str, Any]) -> bool:
                return all(
                    failure_capacity.get(key) == capacity.get(key)
                    for key in (
                        "requested_bytes", "disk_bytes", "filesystem_before",
                        "filesystem_threshold_bytes", "filesystem", "source", "parent",
                        "partition", "tools", "verified",
                    )
                )

            def valid_failure(error: Any) -> bool:
                pending = [(error, 0)]
                while pending:
                    current, depth = pending.pop()
                    required = {
                        "phase", "command", "returncode", "stdout", "stderr", "capacity", "transient",
                        "stdout_truncated", "stderr_truncated",
                    }
                    if not isinstance(current, Mapping) or not required.issubset(current):
                        return False
                    failure_capacity = current["capacity"]
                    if (
                        not isinstance(current["phase"], str)
                        or not isinstance(current["command"], list)
                        or not current["command"]
                        or any(not isinstance(item, str) for item in current["command"])
                        or (current["returncode"] is not None and (isinstance(current["returncode"], bool) or not isinstance(current["returncode"], int)))
                        or not isinstance(current["stdout"], str)
                        or not isinstance(current["stderr"], str)
                        or len(current["stdout"].encode("utf-8", "replace")) > DIAGNOSTIC_LOG_LIMIT
                        or len(current["stderr"].encode("utf-8", "replace")) > 4000
                        or not isinstance(failure_capacity, Mapping)
                        or not failure_capacity_keys.issubset(failure_capacity)
                        or failure_capacity.get("verified") is not True
                        or not isinstance(current["transient"], bool)
                        or not isinstance(current["stdout_truncated"], bool)
                        or not isinstance(current["stderr_truncated"], bool)
                        or ("reaped" in current and not isinstance(current["reaped"], bool))
                        or ("pipes_drained" in current and not isinstance(current["pipes_drained"], bool))
                        or ("process_group_killed" in current and not isinstance(current["process_group_killed"], bool))
                        or (
                            "process_group_cleanup_error" in current
                            and current["process_group_cleanup_error"] is not None
                            and not isinstance(current["process_group_cleanup_error"], str)
                        )
                        or ("cause" in current and not isinstance(current["cause"], str))
                        or (
                            current.get("cause") == "command_timeout"
                            and (
                                not isinstance(current.get("reaped"), bool)
                                or not isinstance(current.get("pipes_drained"), bool)
                                or not isinstance(current.get("process_group_killed"), bool)
                                or "process_group_cleanup_error" not in current
                            )
                        )
                    ):
                        return False
                    for key in (
                        "requested_bytes", "disk_bytes", "partition_bytes", "filesystem_bytes",
                        "filesystem_before", "filesystem_threshold_bytes",
                    ):
                        if isinstance(failure_capacity.get(key), bool) or not isinstance(failure_capacity.get(key), int):
                            return False
                    if (
                        not isinstance(failure_capacity.get("filesystem"), str)
                        or not isinstance(failure_capacity.get("source"), str)
                        or not isinstance(failure_capacity.get("parent"), str)
                        or not isinstance(failure_capacity.get("partition"), str)
                        or not isinstance(failure_capacity.get("tools"), Mapping)
                        or not failure_capacity_matches_report(failure_capacity)
                    ):
                        return False
                    if "retry_of" in current:
                        if depth >= max_failure_retry_depth or not isinstance(current["retry_of"], Mapping):
                            return False
                        pending.append((current["retry_of"], depth + 1))
                return True

            def matches_failure(reference: Mapping[str, Any], candidate: Mapping[str, Any]) -> bool:
                return all(candidate.get(key) == reference.get(key) for key in (
                    "phase", "command", "returncode", "stdout", "stderr", "capacity", "transient", "cause",
                    "stdout_truncated", "stderr_truncated", "reaped", "pipes_drained",
                ))

            attempt_records = capacity["attempts"]
            for index, record in enumerate(attempt_records):
                if (
                    not isinstance(record, Mapping)
                    or isinstance(record.get("attempt"), bool)
                    or not isinstance(record.get("attempt"), int)
                    or record.get("attempt") != index + 1
                    or ("error" in record and not valid_failure(record["error"]))
                    or ("refresh_error" in record and not valid_failure(record["refresh_error"]))
                ):
                    raise ValueError()
                if "refresh_error" in record:
                    refresh_error = record["refresh_error"]
                    previous_error = record.get("error")
                    if (
                        refresh_error.get("transient") is not True
                        or not isinstance(refresh_error.get("retry_of"), Mapping)
                        or not isinstance(previous_error, Mapping)
                        or not matches_failure(previous_error, refresh_error["retry_of"])
                    ):
                        raise ValueError()
            if status in {"grown_after_retry", "already_grown"} and len(attempt_records) == 2:
                first_error = attempt_records[0].get("error") if attempt_records else None
                if (
                    len(attempt_records) != 2
                    or not valid_failure(first_error)
                    or first_error.get("transient") is not True
                    or "error" in attempt_records[1]
                    or "refresh_error" in attempt_records[1]
                ):
                    raise ValueError()
            elif any("error" in record for record in attempt_records):
                raise ValueError()
            target_bytes = disk_gib * 1024**3
            expected_threshold = target_bytes * 95 // 100
            filesystem = value.get("filesystem")
            source = value.get("source")
            parent = value.get("parent")
            partition = value.get("partition")
            direct_device = lambda item: isinstance(item, str) and item.startswith("/dev/") and item.count("/") == 2
            parent_name = parent.rsplit("/", 1)[-1] if isinstance(parent, str) else ""
            expected_source = (
                parent + ("p" if parent_name[-1:].isdigit() else "") + partition
                if isinstance(parent, str) and isinstance(partition, str)
                else ""
            )
            tools = capacity.get("tools")
            required_tools = {"findmnt", "lsblk", "blockdev", "growpart", "resize2fs" if filesystem == "ext4" else "xfs_growfs"}
            if value["partition"] != capacity["partition"]:
                raise ValueError()
            if (
                capacity["status"] != status
                or capacity["requested_bytes"] != target_bytes
                or value["virtual_bytes"] != capacity["disk_bytes"]
                or value["partition_bytes"] != capacity["partition_bytes"]
                or value["filesystem_bytes"] != capacity["filesystem_bytes"]
                or value["filesystem_threshold_bytes"] != capacity["filesystem_threshold_bytes"]
                or value["filesystem"] != capacity["filesystem"]
                or value["source"] != capacity["source"]
                or value["parent"] != capacity["parent"]
                or capacity["filesystem_before"] != value["filesystem_before"]
                or value["virtual_bytes"] < target_bytes
                or value["partition_bytes"] < target_bytes * 95 // 100
                or value["filesystem_before"] < 0
                or value["filesystem_before"] > value["filesystem_bytes"]
                or value["filesystem_bytes"] < value["filesystem_threshold_bytes"]
                or (
                    status in {"grown", "grown_after_retry"}
                    and (
                        value["filesystem_before"] >= expected_threshold
                        or value["filesystem_bytes"] <= value["filesystem_before"]
                    )
                )
                or (
                    status == "already_grown"
                    and value["attempts"] == 0
                    and value["filesystem_before"] < expected_threshold
                )
                or capacity["disk_bytes"] < target_bytes
                or capacity["partition_bytes"] > capacity["disk_bytes"]
                or capacity["partition_bytes"] < 0
                or capacity["filesystem_before"] < 0
                or capacity["filesystem_bytes"] > capacity["partition_bytes"]
                or capacity["filesystem_threshold_bytes"] != expected_threshold
                or filesystem not in {"ext4", "xfs"}
                or not direct_device(source)
                or not direct_device(parent)
                or source == parent
                or source != expected_source
                or not isinstance(partition, str)
                or re.fullmatch(r"[0-9]+", partition) is None
                or not isinstance(tools, Mapping)
                or any(not isinstance(tools.get(tool), str) or not tools[tool] for tool in required_tools)
                or value["attempts"] < 0
                or value["attempts"] > 2
                or (status == "grown" and value["attempts"] != 1)
                or (status == "grown_after_retry" and value["attempts"] != 2)
                or (status == "already_grown" and value["attempts"] not in {0, 1, 2})
            ):
                raise ValueError()
            return value
        except (RecursionError, ValueError, KeyError, TypeError) as exc:
            raise ControlError(
                "guest_resize_unverified",
                "guest capacity report is invalid",
                {
                    "phase": "guest-report",
                    "command": ["incus", "exec", task["instance_name"], "--", "python3", "-c", "guest-root-growth"],
                    "returncode": result.returncode,
                    "stderr": stderr,
                    "stdout": stdout,
                    "guest_capacity": {"verified": False},
                },
            ) from exc


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
            except ControlError as exc:
                cleanup_verified = False
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
                                    cleanup_verified = True
                    except ControlError:
                        current_port = self.store.get_port(task["task_id"], name, protocol)
                        if current_port and int(current_port["generation"]) == int(existing["generation"]):
                            self.store.update_port(task["task_id"], name, protocol, state="closing")
                detail = dict(exc.detail) if isinstance(exc.detail, Mapping) else {}
                detail.setdefault("operation_phase", "port-add")
                detail["outcome_verified"] = cleanup_verified
                raise ControlError(exc.code, exc.message, detail) from exc
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
        except ControlError as exc:
            cleanup_verified = False
            try:
                with self._native_lock(task):
                    current = self.store.get_task(task_id=task["task_id"])
                    if current and int(current["generation"]) == int(port["generation"]):
                        self._remove_proxy(task, port)
                        self._conntrack_delete(port, task)
                        current_port = self.store.get_port(task["task_id"], name, protocol)
                        if current_port and int(current_port["generation"]) == int(port["generation"]):
                            self.store.remove_port(task["task_id"], name, protocol)
                        cleanup_verified = True
            except ControlError:
                current_port = self.store.get_port(task["task_id"], name, protocol)
                if current_port and int(current_port["generation"]) == int(port["generation"]):
                    self.store.update_port(task["task_id"], name, protocol, state="closing")
            detail = dict(exc.detail) if isinstance(exc.detail, Mapping) else {}
            detail.setdefault("operation_phase", "port-add")
            detail["outcome_verified"] = cleanup_verified
            raise ControlError(exc.code, exc.message, detail) from exc


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
            try:
                with self._native_lock(task):
                    self._remove_proxy(task, port)
                    self._conntrack_delete(port, task)
                    self.store.remove_port(task["task_id"], name, protocol)
            except ControlError as exc:
                detail = dict(exc.detail) if isinstance(exc.detail, Mapping) else {}
                detail.setdefault("operation_phase", "port-remove")
                detail["outcome_verified"] = False
                raise ControlError(exc.code, exc.message, detail) from exc
        return {"task_id": task["task_id"], "name": name, "protocol": protocol, "removed": True}


    def admin_status(self, request: Mapping[str, Any], session_id: str | None) -> dict[str, Any]:
        with self._global_lock():
            draining = self.store.get_meta("draining") == "1"
            task_snapshots = self.store.list_tasks()
        tasks = [self._runtime_summary(task) for task in task_snapshots]
        return {"draining": draining, "tasks": tasks, "doctor": self.doctor(request, session_id)}

    def admin_metrics(self, request: Mapping[str, Any], session_id: str | None) -> dict[str, Any]:
        doctor = self.doctor(request, session_id)
        with self._global_lock():
            tasks = self.store.list_tasks()
            task_states = Counter(str(task["state"]) for task in tasks)
            resources = doctor.get("resources", {})
            memory = resources.get("memory") or {} if isinstance(resources, Mapping) else {}
            warnings = list(doctor.get("default_admission", {}).get("warnings", []))
            audit = self.store.audit_summary()
            disk_admission = resources.get("disk_admission") if isinstance(resources, Mapping) else None
            if not isinstance(disk_admission, Mapping):
                disk_admission = self._disk_admission_diagnostics(doctor.get("capacity"))
            return {
                "observed_at": iso_time(self.now()),
                "up": True,
                "draining": self.store.get_meta("draining") == "1",
                "admission_ready": bool(doctor.get("admission_ready")),
                "reclaim_required": doctor.get("default_admission", {}).get("reclaim_required"),
                "resources": resources,
                "capacity": doctor.get("capacity"),
                "disk_admission": disk_admission,
                "ct_cpu": {
                    "capacity_vcpu": (resources.get("ct_capacity") or {}).get("cpu")
                    if isinstance(resources, Mapping) else None,
                    "pressure": resources.get("cpu_pressure") if isinstance(resources, Mapping) else None,
                    "startup_pressure": doctor.get("default_admission", {}).get("observations", {}).get(
                        "cpu_startup_pressure"
                    ),
                },
                "ct_memory": {
                    "capacity_bytes": memory.get("limit_bytes"),
                    "current_bytes": memory.get("current_bytes"),
                    "available_bytes": memory.get("available_bytes"),
                    "pressure_threshold_bytes": memory.get("threshold_bytes"),
                    "pressure": resources.get("memory_pressure") if isinstance(resources, Mapping) else None,
                    "oom_events": memory.get("oom_events"),
                },
                "instances": doctor.get("instances"),
                "task_states": dict(task_states),
                "operations": audit["operations"],
                "pending_operations": self.store.pending_operations(),
                "image_unpack_failures": audit["image_unpack_failures"],
                "warnings": warnings,
                "warning_counts": audit["warnings"],
                "doctor": {"ready": doctor.get("ready"), "issues": doctor.get("issues", [])},
                "release": self._release_metadata(),
                "release_warnings": self._merge_warnings(self.release_warnings),
            }


    def drain_status(self, request: Mapping[str, Any], session_id: str | None) -> dict[str, Any]:
        return {"draining": self.store.get_meta("draining") == "1"}

    def drain_enter(self, request: Mapping[str, Any], session_id: str | None) -> dict[str, Any]:
        with self._admission_lock(), self._global_lock():
            self.store.set_meta("draining", "1")
        return {"draining": True}


    def drain_exit(self, request: Mapping[str, Any], session_id: str | None) -> dict[str, Any]:
        if self.store.get_meta("draining") != "1":
            return {"draining": False}
        operation_id = request.get("operation_id")
        pending = [
            item for item in self.store.pending_operations()
            if item.get("operation_id") != operation_id
        ]
        if pending:
            raise ControlError(
                "reconciliation_required",
                "drain exit requires all pending operations to be reconciled",
                {"pending_operations": pending},
            )
        snapshots = sorted(self.store.list_tasks(), key=lambda task: str(task.get("session_id", "")))
        # Session operations acquire their session lock before admission.lock.
        # Keep that order here too; taking admission.lock first can deadlock an
        # in-flight acquire that is waiting to re-check the drain fence.
        with ExitStack() as locks:
            for snapshot in snapshots:
                owner = snapshot.get("session_id")
                if not isinstance(owner, str) or not owner:
                    raise ControlError("resource_unverified", "drain state contains an invalid session owner")
                locks.enter_context(self._object_lock("session:" + owner, blocking=False))
            for task in snapshots:
                if task.get("state") in {"preparing", "closing", "deleting", "failed"}:
                    raise ControlError("reconciliation_required", "drain exit requires all task transitions to be reconciled",
                                       {"task_id": task.get("task_id"), "state": task.get("state")})
            health = self.doctor(request, session_id)
            resources = health.get("resources")
            instances = health.get("instances")
            capacity = health.get("capacity")
            if (
                not health.get("ready")
                or not isinstance(resources, Mapping)
                or not resources.get("verified")
                or not isinstance(instances, Mapping)
                or not instances.get("verified")
                or not isinstance(capacity, Mapping)
                or "error" in capacity
            ):
                raise ControlError(
                    "recovery_unverified",
                    "drain exit requires verified control health and capacity state",
                    {"issues": health.get("issues", []), "admission": health.get("default_admission")},
                )
            for task in self.store.list_tasks():
                state = task.get("state")
                if state == "absent":
                    continue
                if state in {"preparing", "closing", "deleting", "failed"}:
                    raise ControlError("reconciliation_required", "drain exit requires all task transitions to be reconciled",
                                       {"task_id": task.get("task_id"), "state": state})
                if state == "active":
                    summary = self._state_summary(task)
                    if not summary["effective_lease"]:
                        raise ControlError("reconciliation_required", "active task has no effective lease",
                                           {"task_id": task.get("task_id")})
                    self._ownership(task)
                    self._network_guard()
                    self._verify_network_device(task)
                    if self.incus.instance_info(task["instance_name"])["status"].lower() != "running":
                        raise ControlError("state_mismatch", "active task guest is not running",
                                           {"task_id": task.get("task_id")})
                    if not self.systemd.expiry_active(task["task_id"], int(task["generation"]), task["lease_expires_at"]):
                        raise ControlError("timer_unverified", "active task expiry timer is not verified",
                                           {"task_id": task.get("task_id")})
                    continue
                if state == "cached":
                    self._ownership(task)
                    if self.incus.instance_info(task["instance_name"])["status"].lower() != "stopped":
                        raise ControlError("state_mismatch", "cached task guest is not stopped",
                                           {"task_id": task.get("task_id")})
                    if self.store.get_ports(task["task_id"]):
                        raise ControlError("cleanup_incomplete", "cached task still has port mappings",
                                           {"task_id": task.get("task_id")})
                    owned = self._ownership(task)
                    if any(device.get("type") == "proxy" for device in owned.get("expanded_devices", {}).values()):
                        raise ControlError("cleanup_incomplete", "cached task still has native proxy mappings",
                                           {"task_id": task.get("task_id")})
                    continue
                raise ControlError("resource_unverified", "drain state contains an unknown task state",
                                   {"task_id": task.get("task_id"), "state": state})
            # New reservations are fenced by draining=1 while the checks above
            # run. Commit the exit only after reacquiring admission.lock and
            # rechecking the fence, with the same order as acquire.
            with self._admission_lock():
                if self._release_activation_fenced():
                    raise ControlError(
                        "release_activation_in_progress",
                        "drain exit is blocked while release activation is fenced",
                    )
                if self.store.get_meta("draining") == "1":
                    with self._global_lock():
                        self.store.set_meta("admission_epoch", uuid.uuid4().hex)
                        self.store.set_meta("draining", "0")
        return {"draining": False}


    def admin_session_list(self, request: Mapping[str, Any], session_id: str | None) -> dict[str, Any]:
        return {"tasks": [self._runtime_summary(task) for task in self.store.list_tasks()]}

    def admin_reconcile(self, request: Mapping[str, Any], session_id: str | None) -> dict[str, Any]:
        apply = request.get("apply", False)
        if not isinstance(apply, bool):
            raise ControlError("invalid_request", "reconcile apply must be a JSON boolean")
        actions = []
        native_instance_names: set[str] | None = None

        def absent_native_instance_names() -> set[str]:
            nonlocal native_instance_names
            if native_instance_names is None:
                native_instance_names = {
                    native["name"] for native in self.incus.list_instances()
                }
            return native_instance_names

        for snapshot in self.store.list_tasks():
            snapshot_generation = snapshot.get("generation")
            if isinstance(snapshot_generation, bool) or not isinstance(snapshot_generation, int):
                snapshot_generation = None
            item = {
                "task_id": snapshot.get("task_id"),
                "generation": snapshot_generation,
                "action": "inspect",
            }
            try:
                with self._object_lock("session:" + snapshot["session_id"], blocking=False):
                    task = self.store.get_task(task_id=snapshot["task_id"])
                    if isinstance(task.get("generation"), bool) or not isinstance(task.get("generation"), int):
                        raise ControlError("resource_unverified", "task generation cannot be verified")
                    item = {"task_id": task["task_id"], "generation": task["generation"], "action": "inspect"}
                    if task.get("state") not in {"absent", "cached", "preparing", "active", "closing", "deleting", "failed"}:
                        raise ControlError(
                            "resource_unverified",
                            "task state cannot be verified; reconciliation is required",
                            {"task_id": task["task_id"], "state": task.get("state")},
                        )
                    if task["state"] == "absent":
                        item["action"] = "verify-absent-resources"
                        native_exists = task["instance_name"] in absent_native_instance_names()
                        if native_exists:
                            raise ControlError("resource_unverified", "absent task still has a native instance")
                        if self.store.get_ports(task["task_id"]):
                            raise ControlError("cleanup_incomplete", "absent task still has port mappings")
                        repair_required = False
                        try:
                            resources = parse_resource_mapping(
                                task.get("resources_json"), "resources", allow_empty=False
                            )
                            reservation = parse_resource_mapping(
                                task.get("resource_reservation_json"), "reservation", allow_empty=True
                            )
                            if (
                                resources != {"cpu": 0, "memory_mib": 0, "disk_gib": 0}
                                or reservation
                            ):
                                repair_required = True
                        except (TypeError, ValueError):
                            repair_required = True
                        if (
                            isinstance(task.get("reservation_bytes"), bool)
                            or not isinstance(task.get("reservation_bytes"), int)
                            or task.get("reservation_bytes") != 0
                        ):
                            repair_required = True
                        if apply and repair_required:
                            native_exists = any(
                                native.get("name") == task["instance_name"]
                                for native in self.incus.list_instances()
                            )
                            if native_exists:
                                raise ControlError(
                                    "resource_unverified",
                                    "absent task still has a native instance",
                                )
                            if self.store.get_ports(task["task_id"]):
                                raise ControlError(
                                    "cleanup_incomplete",
                                    "absent task still has port mappings",
                                )
                            with self._global_lock(), self.store.transaction():
                                current = self.store.get_task(task_id=task["task_id"])
                                if current is None or current["state"] != "absent":
                                    raise ControlError("generation_mismatch", "absent task changed during reconciliation")
                                self.store.update_task(
                                    task["task_id"],
                                    resources_json=json.dumps({"cpu": 0, "memory_mib": 0, "disk_gib": 0}),
                                    resource_reservation_json="{}",
                                    reservation_bytes=0,
                                    last_error=None,
                                )
                        actions.append(item)
                        continue
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
                        if task["state"] == "active":
                            self._network_guard()
                            self._verify_network_device(task)
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
                item["error"] = exc.as_dict()
                actions.append(item)
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


def main(argv: Sequence[str] | None = None) -> int:
    argv = list(argv if argv is not None else sys.argv[1:])
    request: dict[str, Any] | None = None
    try:
        request = parse_timer_args(argv)
        if request is None:
            if argv:
                raise ControlError("invalid_request", "unknown command-line arguments")
            request = request_from_stdin()
        config = load_config()
        control = Control(config)
        response = control.dispatch(request)
        sys.stdout.write(json.dumps(response, sort_keys=True) + "\n")
        return 0
    except ControlError as exc:
        envelope: dict[str, Any] = {"ok": False, "protocol": PROTOCOL_VERSION}
        if request is not None:
            for key in ("action", "session_id", "generation", "operation_id"):
                if key in request:
                    envelope[key] = request[key]
        envelope["error"] = exc.as_dict()
        sys.stderr.write(json.dumps(envelope, sort_keys=True) + "\n")
        return 2
    except Exception as exc:
        envelope = {"ok": False, "protocol": PROTOCOL_VERSION}
        if request is not None:
            for key in ("action", "session_id", "generation", "operation_id"):
                if key in request:
                    envelope[key] = request[key]
        envelope["error"] = {"code": "internal_error", "message": type(exc).__name__}
        sys.stderr.write(json.dumps(envelope, sort_keys=True) + "\n")
        return 3


if __name__ == "__main__":
    raise SystemExit(main())
