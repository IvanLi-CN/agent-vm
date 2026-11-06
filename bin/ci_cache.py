"""Cache identity and summaries; never substitutes for dependency or test execution."""

import json
import os
from pathlib import Path
import subprocess
import sys
import time


def cache_plan(role, platform, arch, image, go_version, bun_version, dependency_hash, source_hash, lock_hash, paths):
    if role not in ("portable", "heavy"):
        raise ValueError("unknown cache role")
    identity = f"{platform}-{arch}-{image}-{go_version}"
    module_prefix = f"go-mod-v2-{identity}-"
    build_prefix = f"go-build-v2-{identity}-{role}-{dependency_hash}-"
    bun_prefix = f"bun-{bun_version}-{platform}-{arch}-"
    return {
        "module": {"key": module_prefix + dependency_hash, "prefix": module_prefix, "path": paths["GOMODCACHE"]},
        "build": {"key": build_prefix + source_hash, "prefix": build_prefix, "path": paths["GOCACHE"]},
        "bun": {"key": bun_prefix + lock_hash, "prefix": bun_prefix, "path": str(Path.home() / ".bun/install/cache")},
    }


def size_kib(path):
    if not Path(path).exists():
        return 0
    result = subprocess.run(["du", "-sk", path], capture_output=True, text=True, check=True)
    return int(result.stdout.split()[0])


def outputs(values):
    with open(os.environ["GITHUB_OUTPUT"], "a") as stream:
        for key, value in values.items():
            stream.write(f"{key}={value}\n")


def report(state, mode, results, now):
    rows = [f"### CI cache {mode}", "", "| Cache | Result | Elapsed (s) | Size (KiB) | Key |", "| --- | --- | ---: | ---: | --- |"]
    for name, entry in state["caches"].items():
        result = results[name]
        elapsed = max(0, result["end"] - result["start"])
        if mode == "restore":
            matched = result.get("matched", "")
            status = "exact-hit" if matched == entry["key"] else "prefix-hit" if matched else "miss"
            key = matched or entry["key"]
            entry["hit"] = status == "exact-hit"
        else:
            status = result["outcome"]
            key = entry["key"]
        rows.append(f"| {name} | {status} | {elapsed:.2f} | {size_kib(entry['path'])} | `{key}` |")
    rows.extend(["", f"Cache {mode} wall time: {max(0, now - state['started']):.2f}s. Sizes are local directory usage, not compressed transfer bytes."])
    return "\n".join(rows) + "\n"


def main():
    mode = sys.argv[1]
    if mode == "plan":
        paths = json.loads(subprocess.check_output(["go", "env", "-json", "GOMODCACHE", "GOCACHE"], text=True))
        caches = cache_plan(os.environ["CACHE_ROLE"], os.environ["RUNNER_OS"], os.environ["RUNNER_ARCH"], os.environ.get("ImageOS", "native"), subprocess.check_output(["go", "env", "GOVERSION"], text=True).strip(), subprocess.check_output(["bun", "--version"], text=True).strip(), os.environ["DEPENDENCY_HASH"], os.environ["SOURCE_HASH"], os.environ["LOCK_HASH"], paths)
        if not all(os.environ[key] for key in ("DEPENDENCY_HASH", "SOURCE_HASH", "LOCK_HASH")):
            raise ValueError("cache inputs must not be empty")
        state = {"started": time.time(), "caches": caches}
        outputs({"state": json.dumps(state, separators=(",", ":"))})
    else:
        state = json.loads(os.environ["CACHE_STATE"])
        now = time.time()
        results = json.loads(os.environ["CACHE_RESULTS"])
        if mode == "save":
            state["started"] = float(os.environ["SAVE_STARTED"])
        starts = [state["started"], float(os.environ["BUILD_STARTED"]), float(os.environ["BUN_STARTED"]), now]
        for index, name in enumerate(("module", "build", "bun")):
            results[name].update(start=starts[index], end=starts[index + 1])
        with open(os.environ["GITHUB_STEP_SUMMARY"], "a") as stream:
            stream.write(report(state, mode, results, now))
        if mode == "restore":
            outputs({"state": json.dumps(state, separators=(",", ":"))})


if __name__ == "__main__":
    main()
