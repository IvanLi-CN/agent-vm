#!/usr/bin/env python3
"""Native Incus agent VM client.

The client sends one bounded JSON request to the configured control host. VM
access then uses a generation-bound native OpenSSH configuration.
"""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import sys
import tempfile
from typing import Any, Sequence
import uuid


PROTOCOL_VERSION = 4
VERSION = "0.4.0-native"
REMOTE_CONTROL = "/opt/agent-vm-native/agent_vm_control.py"
DEFAULT_CONTROL_TIMEOUT_SECONDS = 240
LONG_CONTROL_TIMEOUT_SECONDS = 600
SESSION_ENV = "AGENT_VM_SESSION_ID"
CONTROL_HOST_ENV = "AGENT_VM_CONTROL_HOST"
REMOTE_CONTROL_ENV = "AGENT_VM_REMOTE_CONTROL"
DEFAULT_CONTROL_HOST = "agent-vm-host"
DEFAULT_REMOTE_CONTROL = "/opt/agent-vm-native/agent_vm_control.py"
LOCAL_CONFIG_ROOT = Path(os.environ.get("XDG_CONFIG_HOME", str(Path.home() / ".config"))) / "agent-vm"
LOCAL_STATE_ROOT = Path(os.environ.get("XDG_STATE_HOME", str(Path.home() / ".local/state"))) / "agent-vm"
SESSION_ID_RE = re.compile(r"^[A-Za-z0-9_.:@/-]{1,160}$")
NAME_RE = re.compile(r"^[a-z][a-z0-9-]{0,30}$")


class CliError(Exception):
    def __init__(self, code: str, message: str, detail: Any = None, exit_code: int = 2):
        super().__init__(message)
        self.code = code
        self.message = message
        self.detail = detail
        self.exit_code = exit_code

    def as_dict(self) -> dict[str, Any]:
        value = {"code": self.code, "message": self.message}
        if self.detail is not None:
            value["detail"] = self.detail
        return value


def fail(code: str, message: str, detail: Any = None, exit_code: int = 2) -> None:
    raise CliError(code, message, detail, exit_code)


def session_id(explicit: str | None = None, *, required: bool = True) -> str | None:
    value = explicit if explicit is not None else os.environ.get(SESSION_ENV)
    if value is None and not required:
        return None
    if not value:
        fail("missing_session", f"{SESSION_ENV} must be set before a remote operation")
    if SESSION_ID_RE.fullmatch(value) is None:
        fail("invalid_session", f"{SESSION_ENV} contains unsupported characters")
    return value


def json_output(value: Any) -> None:
    sys.stdout.write(json.dumps(value, sort_keys=True, indent=2) + "\n")


def local_session_dir(value: str) -> Path:
    digest = hashlib.sha256(value.encode("utf-8")).hexdigest()[:24]
    return LOCAL_STATE_ROOT / "sessions" / digest


def read_session_state(value: str) -> dict[str, Any]:
    path = local_session_dir(value) / "state.json"
    try:
        with path.open("r", encoding="utf-8") as handle:
            state = json.load(handle)
    except FileNotFoundError:
        fail("local_state_missing", "local session state is missing; run acquire first")
    except (OSError, json.JSONDecodeError):
        fail("local_state_invalid", "local session state cannot be read")
    if not isinstance(state, dict):
        fail("local_state_invalid", "local session state is not an object")
    return state


def write_session_state(value: str, state: dict[str, Any]) -> None:
    directory = local_session_dir(value)
    directory.mkdir(mode=0o700, parents=True, exist_ok=True)
    path = directory / "state.json"
    temp = path.with_name(f".{path.name}.{os.getpid()}")
    with temp.open("w", encoding="utf-8") as handle:
        json.dump(state, handle, sort_keys=True)
        handle.write("\n")
        handle.flush()
        os.fchmod(handle.fileno(), 0o600)
        os.fsync(handle.fileno())
    os.replace(temp, path)


def current_generation(value: str) -> int:
    state = read_session_state(value)
    generation = state.get("generation")
    if isinstance(generation, bool) or not isinstance(generation, int) or generation < 1:
        fail("local_state_invalid", "local session state has no valid generation")
    return generation


def control_timeout(action: str, fields: dict[str, Any] | None = None) -> int:
    if action in {"acquire", "restart"}:
        return LONG_CONTROL_TIMEOUT_SECONDS
    if action == "resize" and fields and fields.get("allow_restart") is True:
        return LONG_CONTROL_TIMEOUT_SECONDS
    return DEFAULT_CONTROL_TIMEOUT_SECONDS


def control_request(action: str, *, session: str | None, generation: int | None = None,
                    write: bool = False, operation_id: str | None = None,
                    fields: dict[str, Any] | None = None) -> dict[str, Any]:
    if write and session is None: fail("missing_session", f"{SESSION_ENV} is required")
    request = {"protocol": PROTOCOL_VERSION, "action": action}
    if session is not None: request["session_id"] = session
    if generation is not None: request["generation"] = generation
    if write: request["operation_id"] = operation_id or str(uuid.uuid4())
    if fields: request.update(fields)
    if write:
        directory = local_session_dir(session) / "operations"
        directory.mkdir(mode=0o700, parents=True, exist_ok=True)
        op_path = directory / (hashlib.sha256(request["operation_id"].encode()).hexdigest() + ".json")
        if op_path.exists():
            try: old = json.loads(op_path.read_text())
            except (ValueError, OSError): fail("local_journal_invalid", "operation journal is unreadable")
            if old["request"] != request: fail("operation_conflict", "operation ID already describes another request")
        else:
            with op_path.open("x", encoding="utf-8") as handle:
                os.fchmod(handle.fileno(), 0o600)
                json.dump({"request": request, "outcome": "pending"}, handle, sort_keys=True)
                handle.flush()
                os.fsync(handle.fileno())
    try:
        control_host = os.environ.get(CONTROL_HOST_ENV, DEFAULT_CONTROL_HOST)
        remote_control = os.environ.get(REMOTE_CONTROL_ENV, DEFAULT_REMOTE_CONTROL)
        process = subprocess.run(["ssh", "-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "StrictHostKeyChecking=yes", "--", control_host,
                                  shlex.join(["python3", remote_control])],
                                 input=json.dumps(request, sort_keys=True).encode(), stdout=subprocess.PIPE,
                                 stderr=None, check=False, timeout=control_timeout(action, fields))
    except FileNotFoundError: fail("command_missing", "ssh is not installed")
    except subprocess.TimeoutExpired:
        fail("remote_timeout", "control outcome is uncertain; retry the same operation ID",
             {"operation_id": request.get("operation_id")})
    if process.stderr:
        sys.stderr.buffer.write(process.stderr)
        sys.stderr.flush()
    if process.returncode:
        fail("remote_failed", "remote request failed; retry only with its original operation ID",
             {"operation_id": request.get("operation_id")}, process.returncode)
    try: response = json.loads(process.stdout)
    except (ValueError, UnicodeDecodeError): fail("invalid_remote_response", "control did not return JSON")
    if not isinstance(response, dict) or response.get("protocol") != PROTOCOL_VERSION or response.get("action") != action or response.get("session_id") != session:
        fail("protocol_mismatch", "response protocol or request identity differs")
    if not response.get("ok") or not isinstance(response.get("result"), dict) or not isinstance(response.get("server_time"), str):
        fail("invalid_remote_response", "invalid control response envelope")
    if write and response.get("operation_id") != request["operation_id"]:
        fail("protocol_mismatch", "operation identity differs")
    return response



def run_command(argv: Sequence[str], *, input_data: bytes | None = None, output: bool = False) -> int:
    try:
        process = subprocess.run(list(argv), input=input_data, check=False)
    except FileNotFoundError:
        fail("command_missing", f"command not found: {argv[0]}")
    return process.returncode


def make_ssh_key(session: str) -> tuple[Path, str]:
    directory = local_session_dir(session) / "keys"
    directory.mkdir(mode=0o700, parents=True, exist_ok=True)
    private_key = directory / "id_ed25519"
    public_key = private_key.with_suffix(".pub")
    if not private_key.exists() or not public_key.exists():
        try:
            process = subprocess.run(
                ["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(private_key)],
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                check=False,
            )
        except FileNotFoundError as exc:
            fail("command_missing", "ssh-keygen is not installed")
        if process.returncode != 0:
            fail("keygen_failed", "ssh-keygen failed", process.stderr.decode("utf-8", "replace"))
    os.chmod(private_key, 0o600)
    try:
        key = public_key.read_text(encoding="utf-8").strip()
    except OSError:
        fail("key_missing", "generated public key cannot be read")
    if not key:
        fail("key_invalid", "generated public key is empty")
    return private_key, key


def ssh_paths(session: str, info: dict[str, Any]) -> tuple[Path, Path]:
    task_id = str(info["task_id"])
    generation = int(info["generation"])
    directory = LOCAL_CONFIG_ROOT / "sessions" / hashlib.sha256(session.encode("utf-8")).hexdigest()[:24]
    directory.mkdir(mode=0o700, parents=True, exist_ok=True)
    return directory / f"agent-vm-{task_id}-g{generation}.ssh", directory / f"agent-vm-{task_id}-g{generation}.known_hosts"


def write_ssh_config(session: str, info: dict[str, Any], private_key: Path) -> Path:
    host = str(info["host"])
    port = int(info["port"])
    alias = str(info["host_key_alias"])
    try: ipaddress.IPv4Address(host)
    except ValueError: fail("invalid_remote_response", "invalid VM endpoint address")
    if not 1024 <= port <= 65535 or not re.fullmatch(r"agent-vm-[a-z0-9-]+-g[1-9][0-9]*", alias):
        fail("invalid_remote_response", "invalid VM endpoint identity")
    if not re.fullmatch(r"avm-[a-z0-9-]+", str(info["task_id"])) or int(info["generation"]) < 1:
        fail("invalid_remote_response", "invalid task identity")
    host_key = str(info["host_key"])
    fields = host_key.split()
    if len(fields) != 2 or fields[0] != "ssh-ed25519" or not re.fullmatch(r"[A-Za-z0-9+/=]+", fields[1]):
        fail("invalid_host_key", "control returned an invalid trusted host key")
    config_path, known_hosts_path = ssh_paths(session, info)
    known_hosts = f"{alias} {host_key}\n"
    config = (
        "Host agent-vm\n"
        f"  HostName {host}\n"
        f"  Port {port}\n"
        "  User root\n"
        f"  HostKeyAlias {alias}\n"
        "  StrictHostKeyChecking yes\n"
        "  IdentitiesOnly yes\n"
        f"  IdentityFile {ssh_config_path(private_key)}\n"
        f"  UserKnownHostsFile {ssh_config_path(known_hosts_path)}\n"
        "  ControlMaster no\n"
        "  ControlPath none\n"
    )
    for path, content in ((known_hosts_path, known_hosts), (config_path, config)):
        temp = path.with_name(f".{path.name}.{os.getpid()}")
        with temp.open("w", encoding="utf-8") as handle:
            handle.write(content)
            handle.flush()
            os.fchmod(handle.fileno(), 0o600)
            os.fsync(handle.fileno())
        os.replace(temp, path)
    return config_path


def ssh_config_path(path: Path) -> str:
    value = str(path.resolve())
    if any(ch in value for ch in ('\n', '\r', '\0')):
        fail("invalid_path", "SSH configuration path contains a control character")
    return '"' + value.replace('\\', '\\\\').replace('"', '\\"').replace('%', '%%') + '"'


def fetch_ssh_info(session: str) -> tuple[dict[str, Any], dict[str, Any], Path]:
    response = control_request("ssh-info", session=session)
    info = response["result"]
    if not isinstance(info, dict):
        fail("invalid_remote_response", "ssh-info result is not an object")
    state = read_session_state(session)
    private_key = Path(str(state["private_key"]))
    if state.get("task_id") != info["task_id"] or state.get("generation") != info["generation"]:
        fail("generation_mismatch", "connection material belongs to another lease; acquire the current lease first")
    if not private_key.is_file():
        fail("key_missing", f"private key is missing: {private_key}")
    config_path = write_ssh_config(session, info, private_key)
    _, known_hosts = ssh_paths(session, info)
    info.update(identity_file=str(private_key), known_hosts_file=str(known_hosts), ssh_config=str(config_path))
    return response, info, config_path


def parse_memory(value: str) -> int:
    match = re.fullmatch(r"([0-9]+)(MiB|GiB|M|G)", value, re.IGNORECASE)
    if match is None:
        fail("invalid_size", "memory must use MiB, GiB, M or G")
    amount = int(match.group(1))
    unit = match.group(2).lower()
    return amount if unit == "mib" or unit == "m" else amount * 1024


def parse_disk(value: str) -> int:
    match = re.fullmatch(r"([0-9]+)(GiB|G)", value, re.IGNORECASE)
    if match is None:
        fail("invalid_size", "disk must use GiB or G")
    return int(match.group(1))


def add_common_mutation_options(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--operation-id", help="reuse this ID when retrying a mutation")


def add_session_option(parser: argparse.ArgumentParser, *, suppress_default: bool = False) -> None:
    options = {"help": f"session identity override (defaults to {SESSION_ENV})"}
    if suppress_default:
        options["default"] = argparse.SUPPRESS
    parser.add_argument("--session-id", **options)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="agent-vm")
    parser.add_argument("--version", action="version", version=f"%(prog)s {VERSION}")
    add_session_option(parser)
    subparsers = parser.add_subparsers(dest="command", required=True)
    version = subparsers.add_parser("version")
    add_session_option(version, suppress_default=True)
    help_parser = subparsers.add_parser("help")
    add_session_option(help_parser, suppress_default=True)

    doctor = subparsers.add_parser("doctor")
    add_session_option(doctor, suppress_default=True)
    status = subparsers.add_parser("status")
    add_session_option(status, suppress_default=True)
    acquire = subparsers.add_parser("acquire")
    add_session_option(acquire, suppress_default=True)
    acquire.add_argument("--ssh-host-port", type=int)
    acquire.add_argument("--cpu", type=int)
    acquire.add_argument("--memory")
    acquire.add_argument("--disk")
    add_common_mutation_options(acquire)
    for name in ("renew", "release", "destroy", "restart"):
        command = subparsers.add_parser(name)
        add_session_option(command, suppress_default=True)
        add_common_mutation_options(command)
    resize = subparsers.add_parser("resize")
    add_session_option(resize, suppress_default=True)
    resize.add_argument("--cpu", type=int)
    resize.add_argument("--memory")
    resize.add_argument("--disk")
    mode = resize.add_mutually_exclusive_group()
    mode.add_argument("--allow-restart", action="store_true")
    mode.add_argument("--live", action="store_true", help="adjust memory only without restarting; no automatic fallback")
    add_common_mutation_options(resize)
    ssh_info = subparsers.add_parser("ssh-info")
    add_session_option(ssh_info, suppress_default=True)
    ssh_info.add_argument("--format", choices=("json", "ssh-config"), default="json")
    ssh = subparsers.add_parser("ssh")
    add_session_option(ssh, suppress_default=True)
    ssh.add_argument("ssh_args", nargs=argparse.REMAINDER)
    execute = subparsers.add_parser("exec")
    add_session_option(execute, suppress_default=True)
    execute.add_argument("argv", nargs=argparse.REMAINDER)
    sync = subparsers.add_parser("sync")
    add_session_option(sync, suppress_default=True)
    sync.add_argument("--name", required=True)
    upload = subparsers.add_parser("upload")
    add_session_option(upload, suppress_default=True)
    upload.add_argument("source")
    upload.add_argument("destination")
    download = subparsers.add_parser("download")
    add_session_option(download, suppress_default=True)
    download.add_argument("source")
    download.add_argument("destination")
    export = subparsers.add_parser("export")
    add_session_option(export, suppress_default=True)
    export.add_argument("path")

    port = subparsers.add_parser("port")
    add_session_option(port, suppress_default=True)
    port_sub = port.add_subparsers(dest="port_command", required=True)
    port_add = port_sub.add_parser("add")
    add_session_option(port_add, suppress_default=True)
    port_add.add_argument("name")
    port_add.add_argument("--guest-port", type=int, required=True)
    port_add.add_argument("--protocol", choices=("tcp", "udp"), default="tcp")
    port_add.add_argument("--host-port", type=int)
    add_common_mutation_options(port_add)
    port_list = port_sub.add_parser("list")
    add_session_option(port_list, suppress_default=True)
    port_remove = port_sub.add_parser("remove")
    add_session_option(port_remove, suppress_default=True)
    port_remove.add_argument("name")
    port_remove.add_argument("--protocol", choices=("tcp", "udp"), default="tcp")
    add_common_mutation_options(port_remove)

    admin = subparsers.add_parser("admin")
    add_session_option(admin, suppress_default=True)
    admin_sub = admin.add_subparsers(dest="admin_command", required=True)
    admin_status = admin_sub.add_parser("status")
    add_session_option(admin_status, suppress_default=True)
    admin_metrics = admin_sub.add_parser("metrics")
    add_session_option(admin_metrics, suppress_default=True)
    admin_session = admin_sub.add_parser("session")
    add_session_option(admin_session, suppress_default=True)
    session_sub = admin_session.add_subparsers(dest="session_command", required=True)
    session_list = session_sub.add_parser("list")
    add_session_option(session_list, suppress_default=True)
    drain = admin_sub.add_parser("drain")
    add_session_option(drain, suppress_default=True)
    drain.add_argument("drain_command", choices=("enter", "status", "exit"))
    reconcile = admin_sub.add_parser("reconcile")
    add_session_option(reconcile, suppress_default=True)
    reconcile.add_argument("--apply", action="store_true")
    add_common_mutation_options(reconcile)
    gc = admin_sub.add_parser("gc")
    add_session_option(gc, suppress_default=True)
    gc.add_argument("--apply", action="store_true")
    add_common_mutation_options(gc)
    return parser


def operation_id(args: argparse.Namespace) -> str:
    value = getattr(args, "operation_id", None)
    return value or str(uuid.uuid4())


def mutation(
    action: str,
    args: argparse.Namespace,
    *,
    fields: dict[str, Any] | None = None,
    generation: int | None = None,
    session: str | None = None,
) -> dict[str, Any]:
    session = session if session is not None else session_id(getattr(args, "session_id", None))
    if generation is None and action not in {"acquire", "drain-enter", "drain-exit", "admin-reconcile", "admin-gc"}:
        assert session is not None
        generation = current_generation(session)
    return control_request(
        action,
        session=session,
        generation=0 if generation is None and action == "acquire" else generation,
        write=True,
        operation_id=operation_id(args),
        fields=fields,
    )


def command_acquire(args: argparse.Namespace) -> int:
    session = session_id(args.session_id)
    private_key, public_key = make_ssh_key(session)
    # Observation never renews. Reuse a saved operation's original generation
    # when retrying after a lost acknowledgement.
    generation = control_request("status", session=session)["result"]["generation"]
    requested_op = getattr(args, "operation_id", None)
    if requested_op:
        journal = local_session_dir(session) / "operations" / (hashlib.sha256(requested_op.encode()).hexdigest() + ".json")
        if journal.is_file(): generation = json.loads(journal.read_text())["request"]["generation"]
    fields = {"public_key": public_key}
    if args.ssh_host_port is not None: fields["ssh_host_port"] = args.ssh_host_port
    if args.cpu is not None: fields["cpu"] = args.cpu
    if args.memory is not None: fields["memory_mib"] = parse_memory(args.memory)
    if args.disk is not None: fields["disk_gib"] = parse_disk(args.disk)
    response = mutation("acquire", args, generation=generation, session=session, fields=fields)
    result = response.get("current_state", response["result"])
    if result is None or not result.get("effective_lease"):
        fail("lease_unavailable", "replayed allocation no longer has an active lease; inspect status")
    write_session_state(session, {"session_id": session, "task_id": result["task_id"],
                                 "generation": result["generation"], "private_key": str(private_key)})
    json_output(response)
    return 0



def command_ssh_info(args: argparse.Namespace) -> int:
    session = session_id(args.session_id)
    assert session is not None
    response, info, config_path = fetch_ssh_info(session)
    if args.format == "json":
        json_output(response)
    else:
        sys.stdout.write(config_path.read_text(encoding="utf-8"))
    return 0


def command_ssh(args: argparse.Namespace) -> int:
    session = session_id(args.session_id)
    assert session is not None
    _, _, config_path = fetch_ssh_info(session)
    ssh_args = list(args.ssh_args)
    if ssh_args and ssh_args[0] == "--":
        ssh_args = ssh_args[1:]
    return run_command(["ssh", "-F", str(config_path), "agent-vm", *ssh_args])


def command_exec(args: argparse.Namespace) -> int:
    session = session_id(args.session_id)
    argv = list(args.argv)
    if not argv or argv[0] != "--": fail("invalid_request", "exec requires -- followed by a program and its arguments")
    argv = argv[1:]
    if not argv or any("\0" in value for value in argv): fail("invalid_request", "invalid execution argv")
    _, _, config_path = fetch_ssh_info(session)
    return run_command(["ssh", "-F", str(config_path), "-o", "BatchMode=yes", "--", "agent-vm", shlex.join(argv)])



def command_transfer(args: argparse.Namespace, direction: str) -> int:
    session = session_id(args.session_id)
    remote = args.destination if direction == "upload" else args.source
    if not remote.startswith("/") or "\0" in remote: fail("invalid_path", "remote path must be absolute")
    _, _, config_path = fetch_ssh_info(session)
    local = str(Path(args.source if direction == "upload" else args.destination).absolute())
    source, destination = (local, "agent-vm:" + remote) if direction == "upload" else ("agent-vm:" + remote, local)
    return run_command(["scp", "-F", str(config_path), "-o", "BatchMode=yes", "--", source, destination])



def command_export(args: argparse.Namespace) -> int:
    if not args.path.startswith("/") or "\0" in args.path: fail("invalid_path", "export path must be absolute")
    _, _, config_path = fetch_ssh_info(session_id(args.session_id))
    relative = args.path.lstrip("/") or "."
    # The tar stream alone owns stdout. No server-side archive is created.
    return run_command(["ssh", "-F", str(config_path), "-o", "BatchMode=yes", "--", "agent-vm",
                        shlex.join(["tar", "-C", "/", "-cf", "-", "--", relative])])



def command_sync(args: argparse.Namespace) -> int:
    session = session_id(args.session_id)
    if re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_-]{0,63}", args.name) is None:
        fail("invalid_name", "--name must be one safe path segment")
    helper = Path(__file__).with_name("sync_worktree.py")
    if not helper.is_file(): fail("sync_helper_missing", "sync helper is missing")
    return run_command([
        sys.executable,
        str(helper),
        "--name",
        args.name,
        "--cli",
        str(Path(__file__).resolve()),
        "--session-id",
        session,
    ])



def command_port(args: argparse.Namespace) -> int:
    session = session_id(args.session_id)
    assert session is not None
    generation = current_generation(session)
    if args.port_command == "list":
        response = control_request("port-list", session=session, generation=generation)
    elif args.port_command == "add":
        response = mutation(
            "port-add",
            args,
            fields={
                "name": args.name,
                "vm_port": args.guest_port,
                "port_protocol": args.protocol,
                **({"host_port": args.host_port} if args.host_port is not None else {}),
            },
            generation=generation,
            session=session,
        )
    else:
        response = mutation(
            "port-remove",
            args,
            fields={"name": args.name, "port_protocol": args.protocol},
            generation=generation,
            session=session,
        )
    json_output(response)
    return 0


def command_admin(args: argparse.Namespace) -> int:
    if args.admin_command == "metrics":
        if args.session_id is not None:
            fail("invalid_request", "admin metrics does not accept --session-id")
        response = control_request("admin-metrics", session=None)
        json_output(response)
        return 0
    session = session_id(args.session_id, required=True)
    if args.admin_command == "status":
        response = control_request("admin-status", session=session)
    elif args.admin_command == "session":
        response = control_request("admin-session-list", session=session)
    elif args.admin_command == "drain":
        if args.drain_command == "status":
            response = control_request("drain-status", session=session)
        else:
            response = mutation(
                f"drain-{args.drain_command}",
                args,
                generation=0,
                session=session,
            )
    elif args.admin_command == "reconcile":
        response = mutation(
            "admin-reconcile",
            args,
            fields={"apply": bool(args.apply)},
            generation=0,
            session=session,
        )
    else:
        response = mutation(
            "admin-gc",
            args,
            fields={"apply": bool(args.apply)},
            generation=0,
            session=session,
        )
    json_output(response)
    return 0


def dispatch(args: argparse.Namespace) -> int:
    if args.command == "version":
        json_output({"version": VERSION, "protocol": PROTOCOL_VERSION})
        return 0
    if args.command == "help":
        build_parser().print_help()
        return 0
    if args.command == "doctor":
        response = control_request("doctor", session=session_id(args.session_id, required=False))
        json_output(response)
        return 0
    if args.command == "status":
        session = session_id(args.session_id)
        response = control_request("status", session=session)
        json_output(response)
        return 0
    if args.command == "acquire":
        return command_acquire(args)
    if args.command in {"renew", "release", "destroy", "restart"}:
        response = mutation(args.command, args)
        json_output(response)
        return 0
    if args.command == "resize":
        if args.live and (args.memory is None or args.cpu is not None or args.disk is not None):
            raise CliError("invalid_request", "--live requires only --memory; CPU/disk changes use --allow-restart")
        fields: dict[str, Any] = {"allow_restart": bool(args.allow_restart), "live_memory": bool(args.live)}
        if args.cpu is not None:
            fields["cpu"] = args.cpu
        if args.memory is not None:
            fields["memory_mib"] = parse_memory(args.memory)
        if args.disk is not None:
            fields["disk_gib"] = parse_disk(args.disk)
        response = mutation("resize", args, fields=fields)
        json_output(response)
        return 0
    if args.command == "ssh-info":
        return command_ssh_info(args)
    if args.command == "ssh":
        return command_ssh(args)
    if args.command == "exec":
        return command_exec(args)
    if args.command == "sync":
        return command_sync(args)
    if args.command == "upload":
        return command_transfer(args, "upload")
    if args.command == "download":
        return command_transfer(args, "download")
    if args.command == "export":
        return command_export(args)
    if args.command == "port":
        return command_port(args)
    if args.command == "admin":
        return command_admin(args)
    fail("unknown_command", "unknown command")
    return 2


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    try:
        args = parser.parse_args(list(argv) if argv is not None else None)
        return dispatch(args)
    except CliError as exc:
        sys.stderr.write(json.dumps({"ok": False, "error": exc.as_dict()}, sort_keys=True) + "\n")
        return exc.exit_code
    except KeyboardInterrupt:
        return 130


if __name__ == "__main__":
    raise SystemExit(main())
