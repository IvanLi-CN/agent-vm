#!/usr/bin/env python3
"""Synchronize Git-selected source without changing an Agent VM lease."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import stat
import subprocess
import sys
import tempfile
import uuid

OWNER = ".agent-vm-sync-owner.json"
NAME = re.compile(r"[A-Za-z0-9][A-Za-z0-9_-]{0,63}\Z")

# Executed as a bounded Python action inside the guest, never installed as a service.
REMOTE = r'''
import ctypes, errno, fcntl, hashlib, json, os, pathlib, shutil, stat, sys

OWNER = ".agent-vm-sync-owner.json"
TAG = "agent-vm-source-v1"
r = json.load(sys.stdin)
name, tx, action = r["name"], r["tx"], r["action"]
import re
if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_-]{0,63}", name):
    raise ValueError("unsafe workspace name")
if not re.fullmatch(r"[0-9a-f]{32}", tx):
    raise ValueError("unsafe transaction")
parent = pathlib.Path("/workspace/worktrees") / name
src = parent / "src"
stage = parent / (".agent-vm-src-stage." + tx)
record = parent / (".agent-vm-sync-" + tx + ".json")

def directory(p, create=False):
    cur = pathlib.Path("/")
    for part in p.parts[1:]:
        cur = cur / part
        try:
            s = cur.lstat()
        except FileNotFoundError:
            if not create: raise
            try: cur.mkdir(mode=0o700)
            except FileExistsError: pass
            s = cur.lstat()
        if not stat.S_ISDIR(s.st_mode) or stat.S_ISLNK(s.st_mode):
            raise ValueError("non-directory or symlink: " + str(cur))
    if str(p.resolve(strict=True)) != str(p):
        raise ValueError("non-canonical directory")

def read_json(p):
    fd = os.open(p, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(fd) as f:
        if not stat.S_ISREG(os.fstat(f.fileno()).st_mode):
            raise ValueError("metadata is not a regular file")
        return json.load(f)

def file_sha256(p):
    h = hashlib.sha256()
    with p.open("rb") as f:
        for block in iter(lambda: f.read(1024 * 1024), b""):
            h.update(block)
    return h.hexdigest()

def durable(p, obj):
    tmp = p.with_name(p.name + ".new")
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    try:
        with os.fdopen(fd, "w") as f:
            json.dump(obj, f, sort_keys=True)
            f.flush(); os.fsync(f.fileno())
        os.replace(tmp, p)
        fsync_parent()
    finally:
        if tmp.exists(): tmp.unlink()

def fsync_parent():
    fd = os.open(parent, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try: os.fsync(fd)
    finally: os.close(fd)

def owned(p):
    directory(p)
    m = read_json(p / OWNER)
    if m.get("helper") != TAG or m.get("name") != name:
        raise ValueError("source is not owned by this helper")
    return m

def identity(p):
    if not os.path.lexists(p): return None
    m = owned(p)
    s = p.lstat()
    return [s.st_dev, s.st_ino, m["tx"]]

def remove_owned(p):
    owned(p)
    shutil.rmtree(p)
    fsync_parent()

directory(parent, create=(action == "begin"))
lock = os.open(parent / ".agent-vm-sync.lock", os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
if not stat.S_ISREG(os.fstat(lock).st_mode): raise ValueError("invalid lock")
fcntl.flock(lock, fcntl.LOCK_EX)
try:
    if action == "begin":
        baseline = identity(src)
        stage.mkdir(mode=0o700)  # exclusive, never adopts a stranger's staging directory
        durable(stage / OWNER, {"helper": TAG, "name": name, "tx": tx})
        durable(record, {"phase": "prepared", "baseline": baseline, "tx": tx})
        print(json.dumps({"stage": str(stage), "tx": tx}))
    elif action == "commit":
        state = read_json(record)
        if state["phase"] != "prepared": raise ValueError("transaction is not prepared")
        if identity(src) != state["baseline"]:
            raise ValueError("source changed during transfer; retry with a new transaction")
        if owned(stage)["tx"] != tx: raise ValueError("wrong stage owner")
        expected = r["manifest"]
        actual = {}
        for root, dirs, files in os.walk(stage, followlinks=False):
            for entry in list(dirs) + files:
                p = pathlib.Path(root) / entry
                rel = str(p.relative_to(stage))
                s = p.lstat()
                if stat.S_ISDIR(s.st_mode): continue
                if rel == OWNER: continue
                if stat.S_ISLNK(s.st_mode):
                    actual[rel] = {"link": os.readlink(p)}
                elif stat.S_ISREG(s.st_mode):
                    actual[rel] = {"sha256": file_sha256(p), "mode": stat.S_IMODE(s.st_mode)}
                else: raise ValueError("unsupported staged file: " + rel)
        if actual != expected: raise ValueError("staged files differ from source manifest")
        # Files and directories must be durable before the directory transaction.
        for root, dirs, files in os.walk(stage, followlinks=False):
            for entry in files:
                p = pathlib.Path(root) / entry
                if p.is_symlink(): continue
                fd = os.open(p, os.O_RDONLY | os.O_NOFOLLOW)
                try: os.fsync(fd)
                finally: os.close(fd)
            fd = os.open(root, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
            try: os.fsync(fd)
            finally: os.close(fd)
        if state["baseline"] is None:
            os.rename(stage, src)
        else:
            libc = ctypes.CDLL(None, use_errno=True)
            exchange = getattr(libc, "renameat2", None)
            if exchange is None: raise RuntimeError("atomic directory exchange is unavailable")
            if exchange(-100, os.fsencode(stage), -100, os.fsencode(src), 2) != 0:
                e = ctypes.get_errno()
                raise OSError(e, os.strerror(e))
        fsync_parent()
        state["phase"] = "committed"
        durable(record, state)
        print(json.dumps({"committed": True, "tx": tx, "target": str(src)}))
    elif action in ("abort", "finish"):
        state = read_json(record)
        # Source marker detects a commit even if control died before recording it.
        committed = os.path.lexists(src) and owned(src)["tx"] == tx
        if action == "abort" and (committed or state["phase"] == "committed"):
            print(json.dumps({"unverified": True, "record": str(record)}))
        else:
            if action == "finish" and not committed:
                raise ValueError("commit is not confirmed; retaining recovery material")
            if os.path.lexists(stage): remove_owned(stage)
            record.unlink(); fsync_parent()
            print(json.dumps({"cleaned": True}))
    else: raise ValueError("unknown action")
finally:
    os.close(lock)
'''


class SyncFailure(Exception):
    def __init__(self, message: str, code: int = 1):
        super().__init__(message)
        self.code = code


def run(argv: list[str], **kwargs) -> subprocess.CompletedProcess:
    return subprocess.run(argv, check=False, **kwargs)


def file_sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def selected_files(root: Path) -> tuple[list[bytes], dict]:
    modules = run(["git", "-C", str(root), "ls-files", "--stage", "-z"], capture_output=True)
    if modules.returncode:
        raise SyncFailure("cannot inspect Git index", modules.returncode)
    if any(entry.startswith(b"160000 ") for entry in modules.stdout.split(b"\0")):
        raise SyncFailure("submodules are unsupported; use explicit native upload/rsync")
    result = run(["git", "-C", str(root), "ls-files", "-co", "--exclude-standard", "-z"], capture_output=True)
    if result.returncode:
        raise SyncFailure("cannot select Git files", result.returncode)
    names, manifest = [], {}
    for raw in sorted(set(result.stdout.split(b"\0")) - {b""}):
        parts = raw.split(b"/")
        if raw.startswith(b"/") or any(p in (b"", b".", b"..") for p in parts):
            raise SyncFailure("unsafe Git file path")
        if b".git" in parts:
            continue
        name = os.fsdecode(raw)
        if name == OWNER:
            raise SyncFailure("source uses the helper's reserved ownership filename")
        source = root / name
        # lstat on the leaf alone would follow a replaced parent symlink.
        parent = root
        absent = False
        for part in parts[:-1]:
            parent = parent / os.fsdecode(part)
            try:
                mode = parent.lstat().st_mode
            except FileNotFoundError:
                absent = True
                break
            if not stat.S_ISDIR(mode) or stat.S_ISLNK(mode):
                raise SyncFailure("source parent is not a real directory: " + repr(str(parent)))
        if absent:
            continue
        try:
            s = source.lstat()
        except FileNotFoundError:
            continue  # tracked deletion
        if stat.S_ISLNK(s.st_mode):
            item = {"link": os.readlink(source)}
        elif stat.S_ISREG(s.st_mode):
            item = {"sha256": file_sha256(source), "mode": stat.S_IMODE(s.st_mode)}
        else:
            raise SyncFailure("unsupported source file: " + repr(name))
        names.append(raw)
        manifest[name] = item
    return names, manifest


def remote(ssh: str, config: Path, payload: dict) -> subprocess.CompletedProcess:
    return run([ssh, "-F", str(config), "-o", "BatchMode=yes", "agent-vm",
                "python3 -c " + shlex.quote(REMOTE)], input=json.dumps(payload).encode(), capture_output=True)


def sync(name: str, cli: str = "agent-vm", ssh: str = "ssh", rsync: str = "rsync",
         session_id: str | None = None) -> int:
    if not NAME.fullmatch(name):
        raise SyncFailure("--name must be a safe single segment (letters, digits, underscore, hyphen)")
    git = run(["git", "rev-parse", "--show-toplevel"], capture_output=True)
    if git.returncode:
        raise SyncFailure("run sync inside a Git worktree", git.returncode)
    root = Path(os.fsdecode(git.stdout.rstrip(b"\n"))).resolve()
    names, manifest = selected_files(root)  # reject submodules before any remote action
    info_argv = [cli, "ssh-info", "--format", "ssh-config"]
    if session_id is not None:
        info_argv.extend(["--session-id", session_id])
    info = run(info_argv, capture_output=True)
    if info.returncode:
        if info.stderr: sys.stderr.buffer.write(info.stderr)
        raise SyncFailure("cannot obtain an active VM SSH configuration", info.returncode)
    if not info.stdout.strip():
        raise SyncFailure("CLI returned empty SSH configuration")
    tx = uuid.uuid4().hex
    base = {"name": name, "tx": tx}
    prepared, committing = False, False
    with tempfile.TemporaryDirectory(prefix="agent-vm-sync-") as temp:
        config = Path(temp) / "ssh_config"
        fd = os.open(config, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "wb") as f: f.write(info.stdout)
        listing = Path(temp) / "files.nul"
        listing.write_bytes(b"\0".join(names) + (b"\0" if names else b""))
        listing.chmod(0o600)
        try:
            begin = remote(ssh, config, {**base, "action": "begin"})
            if begin.returncode:
                sys.stderr.buffer.write(begin.stderr)
                raise SyncFailure("remote preparation failed; existing source was retained", begin.returncode)
            prepared = True
            stage = json.loads(begin.stdout)["stage"]
            expected = f"/workspace/worktrees/{name}/.agent-vm-src-stage.{tx}"
            if stage != expected: raise SyncFailure("remote returned an unexpected stage path")
            transport = shlex.join([ssh, "-F", str(config), "-o", "BatchMode=yes"])
            transfer = run([rsync, "-a", "--from0", "--files-from=" + str(listing), "--relative",
                            "-e", transport, str(root) + "/", "agent-vm:" + stage + "/"])
            if transfer.returncode:
                raise SyncFailure("transfer failed; existing source was retained", transfer.returncode)
            committing = True
            commit = remote(ssh, config, {**base, "action": "commit", "manifest": manifest})
            if commit.returncode:
                sys.stderr.buffer.write(commit.stderr)
                # SSH failure or control crash can occur after rename, before acknowledgement.
                raise SyncFailure(f"commit unverified (exit {commit.returncode}); retain recovery record "
                                  f"/workspace/worktrees/{name}/.agent-vm-sync-{tx}.json", 75)
            try:
                response = json.loads(commit.stdout)
            except (ValueError, UnicodeError) as e:
                raise SyncFailure("commit acknowledgement unreadable; recovery material retained", 75) from e
            if response.get("committed") is not True or response.get("tx") != tx:
                raise SyncFailure("commit acknowledgement is invalid; recovery material retained", 75)
            finish = remote(ssh, config, {**base, "action": "finish"})
            if finish.returncode:
                sys.stderr.buffer.write(finish.stderr)
                print("source committed; cleanup incomplete, recovery material retained", file=sys.stderr)
            print(json.dumps({"synced": True, "target": f"/workspace/worktrees/{name}/src", "files": len(names)}))
            return 0
        finally:
            if prepared and not committing:
                try:
                    abort = remote(ssh, config, {**base, "action": "abort"})
                except OSError:
                    print("stage cleanup incomplete; recovery material retained for " + tx, file=sys.stderr)
                else:
                    if abort.returncode:
                        print("stage cleanup incomplete; recovery material retained for " + tx, file=sys.stderr)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--name", required=True)
    parser.add_argument("--cli", default="agent-vm", help="installed CLI path (or candidate client)")
    parser.add_argument("--session-id", help="explicit session identity passed to the CLI")
    args = parser.parse_args()
    try:
        return sync(args.name, args.cli, session_id=args.session_id)
    except SyncFailure as e:
        print(str(e), file=sys.stderr)
        return e.code
    except (OSError, ValueError, KeyError) as e:
        print("sync failed: " + str(e), file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
