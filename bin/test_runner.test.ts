import { expect, test } from "bun:test";
import { chmodSync, existsSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { commandForTest, discoverTests, ensureTestSelection, ensureTestSources, executedTestCount, testKind } from "./test_runner.ts";

function isProcessAlive(pid: number): boolean {
  try {
    if (process.platform !== "win32") {
      try {
        const stat = readFileSync(`/proc/${pid}/stat`, "utf8");
        const state = stat.slice(stat.lastIndexOf(")") + 2, stat.lastIndexOf(")") + 3);
        if (state === "Z") return false;
      } catch {
        // macOS does not expose Linux's /proc process state.
      }
    }
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

function signalProcessGroup(pid: number, signal: NodeJS.Signals): void {
  if (process.platform === "win32") {
    try {
      process.kill(pid, signal);
    } catch {
      // The child may have exited before test cleanup.
    }
    return;
  }
  try {
    process.kill(-pid, signal);
  } catch {
    try {
      process.kill(pid, signal);
    } catch {
      // The child may have exited before test cleanup.
    }
  }
}

async function waitForProcessExit(pid: number, timeoutMs = 1500): Promise<boolean> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (!isProcessAlive(pid)) return true;
    await Bun.sleep(10);
  }
  return !isProcessAlive(pid);
}

async function waitForPath(path: string, timeoutMs = 1500): Promise<boolean> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (existsSync(path)) return true;
    await Bun.sleep(10);
  }
  return existsSync(path);
}

async function exitWithin(child: Bun.Subprocess, timeoutMs = 2000): Promise<number> {
  const result = await Promise.race([
    child.exited.then((code) => ({ code, timedOut: false as const })),
    Bun.sleep(timeoutMs).then(() => ({ code: -1, timedOut: true as const })),
  ]);
  if (result.timedOut) {
    signalProcessGroup(child.pid, "SIGKILL");
    await child.exited;
    throw new Error(`child did not exit within ${timeoutMs}ms`);
  }
  return result.code;
}

test("discovers maintained TS, Python, and Shell tests automatically", () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-"));
  try {
    mkdirSync(join(root, "bin"), { recursive: true });
    mkdirSync(join(root, "skills", "alpha"), { recursive: true });
    mkdirSync(join(root, "skills", "build"), { recursive: true });
    mkdirSync(join(root, "build"), { recursive: true });
    mkdirSync(join(root, "alpha.egg-info"), { recursive: true });
    mkdirSync(join(root, "skills", "alpha", ".pytest_cache"), { recursive: true });
    writeFileSync(join(root, "bin", "example.test.ts"), "");
    writeFileSync(join(root, "bin", "test_runner.ts"), "");
    writeFileSync(join(root, "skills", "alpha", "test_python.py"), "");
    writeFileSync(join(root, "skills", "alpha", "test-shell.sh"), "");
    writeFileSync(join(root, "skills", "alpha", "test_runner.ts"), "");
    writeFileSync(join(root, "skills", "build", "test_direct.py"), "");
    mkdirSync(join(root, "skills", "alpha", "tmp"), { recursive: true });
    writeFileSync(join(root, "skills", "alpha", "tmp", "test_nested.py"), "");
    writeFileSync(join(root, "build", "test_generated.py"), "");
    writeFileSync(join(root, "alpha.egg-info", "test_generated.py"), "");
    writeFileSync(join(root, "skills", "alpha", ".pytest_cache", "test_generated.py"), "");
    expect(testKind("example.test.ts")).toBe("bun");
    expect(testKind("test_python.py")).toBe("python");
    expect(testKind("test-shell.sh")).toBe("shell");
    expect(testKind("test_runner.ts")).toBe("bun");
    expect(discoverTests(root, "repo").map((testFile) => testFile.kind)).toEqual(["bun"]);
    expect(discoverTests(root, "skills").map((testFile) => testFile.kind).sort()).toEqual(["bun", "python", "python", "shell"]);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("fails closed when deliberate test selection omits every file", () => {
  expect(() => ensureTestSelection([], "repo")).toThrow("refusing to pass an empty test selection");
});

test("runs Python files by path so files without a main guard or with hyphens are executed", () => {
  expect(commandForTest({ path: "/repo/skills/example/tests/test_reclamation.py", kind: "python" })).toEqual([
    "python3", "-c", expect.any(String), "/repo/skills/example/tests/test_reclamation.py",
  ]);
  const command = commandForTest({ path: "/repo/skills/example/tests/test_reclamation.py", kind: "python" });
  expect(command[2]).toContain("spec_from_file_location");
});

test("fails closed for empty and non-test source files", () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-invalid-"));
  try {
    const empty = join(root, "test_empty.py");
    const nonTest = join(root, "test_non_test.py");
    writeFileSync(empty, "");
    writeFileSync(nonTest, "print('not a unittest suite')\n");
    expect(() => ensureTestSources([
      { path: empty, kind: "python" },
      { path: nonTest, kind: "python" },
    ])).toThrow("without maintained test declarations");
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("requires shell tests to contain a test command", () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-shell-"));
  try {
    const noOp = join(root, "test_noop.sh");
    const valid = join(root, "test_valid.sh");
    const grouped = join(root, "test_grouped.sh");
    const builtin = join(root, "test_builtin.sh");
    writeFileSync(noOp, "#!/usr/bin/env bash\necho 'test TESTS_RUN=1'\n");
    writeFileSync(valid, "#!/usr/bin/env bash\ntest -e /tmp\n");
    writeFileSync(grouped, "#!/usr/bin/env bash\n( test -e /tmp )\n");
    writeFileSync(builtin, "#!/usr/bin/env bash\nbuiltin test -e /tmp\n");
    expect(() => ensureTestSources([{ path: noOp, kind: "shell" }])).toThrow("without maintained test declarations");
    expect(() => ensureTestSources([{ path: valid, kind: "shell" }])).not.toThrow();
    expect(() => ensureTestSources([{ path: grouped, kind: "shell" }])).not.toThrow();
    expect(() => ensureTestSources([{ path: builtin, kind: "shell" }])).not.toThrow();
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("accepts Bun parameterized test declarations", () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-bun-each-"));
  try {
    const path = join(root, "test_each.test.ts");
    writeFileSync(path, "test.each([1, 2])('value', (value) => expect(value).toBeTruthy())\ntest.concurrent('concurrent', () => {})\ntest.skipIf(false).each([1])('skipped condition', () => {})\n");
    expect(() => ensureTestSources([{ path, kind: "bun" }])).not.toThrow();
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("fails shell suites when no test command executes", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-shell-runtime-"));
  try {
    const path = join(root, "test_dead_branch.sh");
    writeFileSync(path, "#!/usr/bin/env bash\ncount=1\nif false; then test -e /missing; fi\n");
    const child = Bun.spawn(commandForTest({ path, kind: "shell" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    const stdout = await new Response(child.stdout).text();
    const stderr = await new Response(child.stderr).text();
    expect(await child.exited).toBe(1);
    expect(executedTestCount("shell", `${stdout}\n${stderr}`)).toBeUndefined();
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("preserves shell assertion status in conditional contexts", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-shell-status-"));
  try {
    const path = join(root, "test_failed_assertion.sh");
    writeFileSync(path, "test 1 -eq 2 || exit 1\n");
    const child = Bun.spawn(commandForTest({ path, kind: "shell" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("reports shell counts before an explicit successful exit", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-shell-explicit-exit-"));
  try {
    const path = join(root, "test_explicit_exit.sh");
    writeFileSync(path, "trap ':' EXIT\ntest 1 -eq 1\nexit 0\n");
    const child = Bun.spawn(commandForTest({ path, kind: "shell" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    const stdout = await new Response(child.stdout).text();
    const stderr = await new Response(child.stderr).text();
    expect(await child.exited).toBe(0);
    expect(executedTestCount("shell", `${stdout}\n${stderr}`)).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("counts explicit builtin and command shell tests", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-shell-builtin-test-"));
  try {
    const path = join(root, "test_builtin.sh");
    writeFileSync(path, "builtin test -e /tmp\ncommand test -e /tmp\n");
    const child = Bun.spawn(commandForTest({ path, kind: "shell" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    const stdout = await new Response(child.stdout).text();
    const stderr = await new Response(child.stderr).text();
    expect(await child.exited).toBe(0);
    expect(executedTestCount("shell", `${stdout}\n${stderr}`)).toBe(2);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("counts command test options and rejects command-name prefixes", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-shell-command-options-"));
  try {
    const validPath = join(root, "test_command_options.sh");
    writeFileSync(validPath, "X=1 command -- test -e /tmp\ncommand -p -- \"test\" -e /tmp\ncommand 'test' -e /tmp\n");
    const validChild = Bun.spawn(commandForTest({ path: validPath, kind: "shell" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    const validStdout = await new Response(validChild.stdout).text();
    const validStderr = await new Response(validChild.stderr).text();
    expect(await validChild.exited).toBe(0);
    expect(executedTestCount("shell", `${validStdout}\n${validStderr}`)).toBe(3);

    const prefixPath = join(root, "test_command_prefix.sh");
    writeFileSync(prefixPath, "if false; then test -e /missing; fi\ncommand test_not_a_builtin || true\ncommand test -e /tmp < /definitely-missing-agent-toolkit-input || true\n");
    const prefixChild = Bun.spawn(commandForTest({ path: prefixPath, kind: "shell" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await prefixChild.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("counts shell assertions with comparison characters and valid redirection", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-shell-builtin-redirection-"));
  try {
    const path = join(root, "test_builtin_redirection.sh");
    writeFileSync(path, "builtin test 'a>b' = 'a>b'\nbuiltin test 'a<b' = 'a<b'\nbuiltin test 1 -eq 1 > /dev/null\nbuiltin test 1 -eq 1 < /dev/null\nbuiltin test 1 -eq 1 > '/dev/null'\nbuiltin test 1 -eq 1 > \"/dev/null\"\ncommand test 'a>b' = 'a>b'\ncommand test 'a<b' = 'a<b'\ncommand test 1 -eq 1 > /dev/null\ncommand test 1 -eq 1 < /dev/null\ncommand test 1 -eq 1 > '/dev/null'\ncommand test 1 -eq 1 < \"/dev/null\"\nbuiltin test -d / < /definitely-missing-agent-toolkit-input || true\ncommand test -d / > /definitely-missing-agent-toolkit-output/out || true\nbuiltin test -d / > . || true\ncommand test -d / > . || true\nbuiltin test 1 -eq 1 > /dev/null > . || true\ncommand test 1 -eq 1 < /dev/null < /definitely-missing-agent-toolkit-input || true\n");
    const child = Bun.spawn(commandForTest({ path, kind: "shell" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    const stdout = await new Response(child.stdout).text();
    const stderr = await new Response(child.stderr).text();
    expect(await child.exited).toBe(0);
    expect(executedTestCount("shell", `${stdout}\n${stderr}`)).toBe(12);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("counts valid file-descriptor, force, and here-string redirections", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-shell-valid-redirections-"));
  try {
    const path = join(root, "test_valid_redirections.sh");
    writeFileSync(path, "builtin test 1 -eq 1 >&1\nbuiltin test 1 -eq 1 >&2\nbuiltin test 1 -eq 1 >| README.md\nbuiltin test 1 -eq 1 <<< \"hello\"\nbuiltin test 1 -eq 1 > /dev/fd/1\ntarget=/dev/null\nbuiltin test 1 -eq 1 > \"$target\"\nexec 3>&1\nbuiltin test 1 -eq 1 3>&1 1>&3\nbuiltin test 1 -eq 1 3>/dev/null >&3\n");
    writeFileSync(join(root, "README.md"), "existing\n");
    const child = Bun.spawn(commandForTest({ path, kind: "shell" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    const stdout = await new Response(child.stdout).text();
    const stderr = await new Response(child.stderr).text();
    expect(await child.exited).toBe(0);
    expect(executedTestCount("shell", `${stdout}\n${stderr}`)).toBe(8);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("counts redirections with resolved targets, descriptor moves, and heredocs", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-shell-redirection-state-"));
  try {
    const path = join(root, "test_redirection_state.sh");
    writeFileSync(path, "builtin test 1 -eq 1 >> README.md\nbuiltin test 1 -eq 1 > /dev/fd/./1\nbuiltin test 1 -eq 1 > /dev/./fd/1\ninput_target=/dev/null\nbuiltin test 1 -eq 1 < \"$input_target\"\nfd=1\nbuiltin test 1 -eq 1 >&\"$fd\"\noutput_target=/dev/null\nbuiltin test 1 -eq 1 > \"$output_target\"\nexec 3<&0\nbuiltin test 1 -eq 1 3<&0 0<&3\nexec 4>&1\nbuiltin test 1 -eq 1 4>&1- 1>&4\nbuiltin test 1 -eq 1 <<EOF\nbody\nEOF\n");
    writeFileSync(join(root, "README.md"), "existing\n");
    const child = Bun.spawn(commandForTest({ path, kind: "shell" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    const stdout = await new Response(child.stdout).text();
    const stderr = await new Response(child.stderr).text();
    expect(await child.exited).toBe(0);
    expect(executedTestCount("shell", `${stdout}\n${stderr}`)).toBe(9);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("replays shell stdout and stderr after regularizing descriptors", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-shell-output-replay-"));
  try {
    const path = join(root, "test_output_replay.sh");
    writeFileSync(path, "printf 'shell stdout\n'\nprintf 'shell stderr\n' >&2\nbuiltin test 1 -eq 1 > /dev/fd/1\n");
    const child = Bun.spawn(commandForTest({ path, kind: "shell" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    const stdout = await new Response(child.stdout).text();
    const stderr = await new Response(child.stderr).text();
    expect(await child.exited).toBe(0);
    expect(stdout).toContain("shell stdout\n");
    expect(stdout).toMatch(/TESTS_RUN=\s*1\s*$/);
    expect(stderr).toBe("shell stderr\n");
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("does not wait for background descendants holding shell output FIFOs", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-shell-background-descendant-"));
  let child: Bun.Subprocess | undefined;
  let descendantPid: number | undefined;
  try {
    const path = join(root, "test_background_descendant.sh");
    const pidFile = join(root, "background.pid");
    const quotedPidFile = JSON.stringify(pidFile);
    writeFileSync(path, [
      `sh -c 'printf "%s\\n" "$$" > ${quotedPidFile}; while :; do sleep 1; done' &`,
      "printf 'shell stdout\\n'",
      "printf 'shell stderr\\n' >&2",
      "builtin test 1 -eq 1 > /dev/fd/1",
    ].join("\n"));
    const spawnedChild = Bun.spawn(commandForTest({ path, kind: "shell" }), { cwd: root, stdout: "pipe", stderr: "pipe", detached: true });
    child = spawnedChild;
    const stdout = new Response(spawnedChild.stdout).text();
    const stderr = new Response(spawnedChild.stderr).text();
    expect(await exitWithin(spawnedChild)).toBe(0);
    expect(await stdout).toContain("shell stdout\n");
    expect(await stdout).toMatch(/TESTS_RUN=\s*1\s*$/);
    expect(await stderr).toBe("shell stderr\n");
    expect(await waitForPath(pidFile)).toBe(true);
    descendantPid = Number(readFileSync(pidFile, "utf8").trim());
    expect(Number.isInteger(descendantPid)).toBe(true);
    expect(await waitForProcessExit(descendantPid)).toBe(true);
  } finally {
    if (descendantPid !== undefined && isProcessAlive(descendantPid)) signalProcessGroup(descendantPid, "SIGKILL");
    if (child !== undefined && isProcessAlive(child.pid)) signalProcessGroup(child.pid, "SIGKILL");
    rmSync(root, { recursive: true, force: true });
  }
});

test("forwards SIGINT and SIGTERM to shell test process groups", async () => {
  const cases: Array<[NodeJS.Signals, number]> = [["SIGINT", 130], ["SIGTERM", 143]];
  for (const [signal, expectedExitCode] of cases) {
    const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", `agent-toolkit-discovery-shell-signal-${signal.toLowerCase()}-`));
    let child: Bun.Subprocess | undefined;
    let sourcePid: number | undefined;
    try {
      const path = join(root, "test_signal.sh");
      const pidFile = join(root, "source.pid");
      const quotedPidFile = JSON.stringify(pidFile);
      writeFileSync(path, [
        `sh -c 'printf "%s\\n" "$$" > ${quotedPidFile}; while :; do sleep 1; done' &`,
        "wait",
      ].join("\n"));
      const spawnedChild = Bun.spawn(commandForTest({ path, kind: "shell" }), { cwd: root, stdout: "pipe", stderr: "pipe", detached: true });
      child = spawnedChild;
      const stdout = new Response(spawnedChild.stdout).text();
      const stderr = new Response(spawnedChild.stderr).text();
      expect(await waitForPath(pidFile)).toBe(true);
      sourcePid = Number(readFileSync(pidFile, "utf8").trim());
      process.kill(spawnedChild.pid, signal);
      expect(await exitWithin(spawnedChild)).toBe(expectedExitCode);
      expect(await stdout).toBe("");
      expect(await stderr).toBe("");
      expect(await waitForProcessExit(sourcePid)).toBe(true);
    } finally {
      if (sourcePid !== undefined && isProcessAlive(sourcePid)) signalProcessGroup(sourcePid, "SIGKILL");
      if (child !== undefined && isProcessAlive(child.pid)) signalProcessGroup(child.pid, "SIGKILL");
      rmSync(root, { recursive: true, force: true });
    }
  }
});

test("cleans shell relay processes and paths across setup failures", async () => {
  const failureCases = [
    { name: "first-mktemp", failMktempAt: 1, failMkfifo: false, expectedCatCount: 0 },
    { name: "second-mktemp", failMktempAt: 2, failMkfifo: false, expectedCatCount: 0 },
    { name: "mkfifo", failMktempAt: 0, failMkfifo: true, expectedCatCount: 0 },
    { name: "marker-mktemp", failMktempAt: 3, failMkfifo: false, expectedCatCount: 2 },
  ];
  for (const failureCase of failureCases) {
    const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", `agent-toolkit-discovery-shell-setup-${failureCase.name}-`));
    let child: Bun.Subprocess | undefined;
    try {
      const tools = join(root, "tools");
      mkdirSync(tools);
      const stateFile = join(root, "mktemp.state");
      const catPidFile = join(root, "cat.pids");
      const testPath = join(root, "test_setup_failure.sh");
      const mktempPath = join(tools, "mktemp");
      const mkfifoPath = join(tools, "mkfifo");
      const catPath = join(tools, "cat");
      writeFileSync(mktempPath, [
        "#!/usr/bin/env bash",
        "state=\"$AGENT_TOOLKIT_MKTEMP_STATE\"",
        "count=0",
        "if [[ -f \"$state\" ]]; then IFS= read -r count < \"$state\"; fi",
        "next=$((count + 1))",
        "if [[ \"$AGENT_TOOLKIT_FAIL_MKTEMP_AT\" != 0 && \"$next\" == \"$AGENT_TOOLKIT_FAIL_MKTEMP_AT\" ]]; then",
        "  if [[ \"$next\" == 3 ]]; then",
        "    attempt=0",
        "    while (( attempt < 100 )); do",
        "      if [[ -f \"$AGENT_TOOLKIT_CAT_PID_FILE\" ]] && (( $(/usr/bin/wc -l < \"$AGENT_TOOLKIT_CAT_PID_FILE\") >= 2 )); then break; fi",
        "      sleep 0.01",
        "      ((attempt += 1))",
        "    done",
        "  fi",
        "  exit 1",
        "fi",
        "printf '%s\\n' \"$next\" > \"$state\"",
        "path=\"$AGENT_TOOLKIT_TMP_ROOT/tmp.agent-toolkit-$next\"",
        "printf '%s\\n' \"$path\"",
        ": > \"$path\"",
        "",
      ].join("\n"));
      writeFileSync(mkfifoPath, [
        "#!/usr/bin/env bash",
        "if [[ \"$AGENT_TOOLKIT_FAIL_MKFIFO\" == 1 ]]; then exit 1; fi",
        "exec /usr/bin/mkfifo \"$@\"",
        "",
      ].join("\n"));
      writeFileSync(catPath, [
        "#!/usr/bin/env bash",
        "printf '%s\\n' \"$$\" >> \"$AGENT_TOOLKIT_CAT_PID_FILE\"",
        "exec /bin/cat \"$@\"",
        "",
      ].join("\n"));
      chmodSync(mktempPath, 0o755);
      chmodSync(mkfifoPath, 0o755);
      chmodSync(catPath, 0o755);
      writeFileSync(testPath, "builtin test 1 -eq 1\\n");
      const spawnedChild = Bun.spawn(commandForTest({ path: testPath, kind: "shell" }), {
        cwd: root,
        stdout: "pipe",
        stderr: "pipe",
        detached: true,
        env: {
          ...Bun.env,
          AGENT_TOOLKIT_FAIL_MKFIFO: failureCase.failMkfifo ? "1" : "0",
          AGENT_TOOLKIT_FAIL_MKTEMP_AT: String(failureCase.failMktempAt),
          AGENT_TOOLKIT_CAT_PID_FILE: catPidFile,
          AGENT_TOOLKIT_MKTEMP_STATE: stateFile,
          AGENT_TOOLKIT_TMP_ROOT: root,
          PATH: `${tools}:/usr/bin:/bin:/usr/sbin:/sbin`,
          TMPDIR: root,
        },
      });
      child = spawnedChild;
      const stdout = new Response(spawnedChild.stdout).text();
      const stderr = new Response(spawnedChild.stderr).text();
      expect(await exitWithin(spawnedChild)).toBe(1);
      expect(await stdout).toBe("");
      expect(await stderr).toBe("");
      const catPids = existsSync(catPidFile)
        ? readFileSync(catPidFile, "utf8").trim().split(/\s+/).filter(Boolean).map(Number)
        : [];
      expect(catPids).toHaveLength(failureCase.expectedCatCount);
      for (const pid of catPids) expect(await waitForProcessExit(pid)).toBe(true);
      expect(readdirSync(root).filter((entry) => entry.startsWith("tmp.agent-toolkit-"))).toEqual([]);
    } finally {
      if (child !== undefined && isProcessAlive(child.pid)) signalProcessGroup(child.pid, "SIGKILL");
      rmSync(root, { recursive: true, force: true });
    }
  }
});

test("rejects empty, split, and traversal redirection targets", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-shell-invalid-targets-"));
  try {
    const path = join(root, "test_invalid_targets.sh");
    writeFileSync(path, "empty=\"\"\nbuiltin test 1 -eq 1 > \"$empty\" || true\nspaced=\"bad target\"\nbuiltin test 1 -eq 1 > $spaced || true\nbuiltin test 1 -eq 1 > /dev/fd/../fd/999 || true\n");
    const child = Bun.spawn(commandForTest({ path, kind: "shell" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("tracks closed input descriptors and literal quoted redirection variables", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-shell-redirection-closed-input-"));
  try {
    const path = join(root, "test_closed_input.sh");
    writeFileSync(path, "exec 3>/dev/null\nbuiltin test 1 -eq 1 3>&- 0<&3 || true\nx=README.md\nbuiltin test 1 -eq 1 < '$x' || true\n");
    writeFileSync(join(root, "README.md"), "existing\n");
    const child = Bun.spawn(commandForTest({ path, kind: "shell" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("accepts embedded variable expansion in quoted output targets", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-shell-redirection-variable-"));
  try {
    const path = join(root, "test_embedded_target.sh");
    writeFileSync(path, "dest=null\nbuiltin test 1 -eq 1 >\"/dev/$dest\"\n");
    const child = Bun.spawn(commandForTest({ path, kind: "shell" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    const stdout = await new Response(child.stdout).text();
    const stderr = await new Response(child.stderr).text();
    expect(await child.exited).toBe(0);
    expect(executedTestCount("shell", `${stdout}\n${stderr}`)).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("rejects shell assertions whose redirections cannot execute", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-shell-invalid-redirections-"));
  try {
    const path = join(root, "test_invalid_redirections.sh");
    writeFileSync(path, "set -C\ntarget=.; builtin test 1 -eq 1 > \"$target\" || true\nbuiltin test 1 -eq 1 > /dev/fd/999 || true\nbuiltin test 1 -eq 1 >&999 || true\nbuiltin test 1 -eq 1 > /dev/./fd/999 || true\nbuiltin test 1 -eq 1 > README.md || true\nexec 3>&1\nbuiltin test 1 -eq 1 3>&- >&3 || true\nbuiltin test 1 -eq 1 1>&- 2>&1 || true\n");
    writeFileSync(join(root, "README.md"), "existing\n");
    const child = Bun.spawn(commandForTest({ path, kind: "shell" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("rejects output redirection through a read-only standard input descriptor", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-shell-readonly-fd-"));
  try {
    const path = join(root, "test_readonly_fd.sh");
    writeFileSync(path, "builtin test 1 -eq 1 > /dev/fd/0 || true\n");
    const child = Bun.spawn(commandForTest({ path, kind: "shell" }), { cwd: root, stdin: "pipe", stdout: "pipe", stderr: "pipe" });
    child.stdin?.end();
    expect(await child.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("preserves shell test counts when a suite replaces the DEBUG trap", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-shell-debug-trap-"));
  try {
    const path = join(root, "test_debug_trap.sh");
    writeFileSync(path, "trap ':' DEBUG\nbuiltin test -e /tmp\ntrap - DEBUG\ncommand test -e /tmp\n");
    const child = Bun.spawn(commandForTest({ path, kind: "shell" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    const stdout = await new Response(child.stdout).text();
    const stderr = await new Response(child.stderr).text();
    expect(await child.exited).toBe(0);
    expect(executedTestCount("shell", `${stdout}\n${stderr}`)).toBe(2);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("requires a nonzero runtime test count for Bun and Python suites", () => {
  expect(executedTestCount("bun", "Ran 0 tests across 1 files.\nRan 2 tests across 1 files.")).toBeUndefined();
  expect(executedTestCount("bun", "Ran 0 tests in a comment")).toBeUndefined();
  expect(executedTestCount("python", "Ran 21 tests in 0.1s")).toBe(21);
  expect(executedTestCount("shell", "TESTS_RUN=3\n")).toBe(3);
  expect(executedTestCount("shell", "shell completed")).toBeUndefined();
});

test("rejects conflicting test summaries that could be appended by a child process", () => {
  expect(executedTestCount("python", "Ran 1 test in 0.1s\nRan 999 tests in 0.1s")).toBeUndefined();
  expect(executedTestCount("shell", "TESTS_RUN=1\nTESTS_RUN=999\n")).toBeUndefined();
});

test("rejects a test module that exits during import after printing a fake summary", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-python-import-exit-"));
  try {
    const path = join(root, "test_import_exit.py");
    writeFileSync(path, "import sys\nimport unittest\nprint('Ran 1 test in 0.001s', file=sys.stderr)\nsys.exit(0)\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("fails closed for class-level expected-failure test replacements during setup", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-class-test-replacement-"));
  try {
    const path = join(root, "test_class_test_replacement.py");
    writeFileSync(path, "import unittest\n\nclass Tests(unittest.TestCase):\n    def setUp(self):\n        type(self).test_runs = lambda self: self.fail('replacement must not be hidden')\n    @unittest.expectedFailure\n    def test_runs(self):\n        pass\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("does not copy expected-failure metadata onto instance replacements", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-instance-test-replacement-"));
  try {
    const path = join(root, "test_instance_test_replacement.py");
    writeFileSync(path, "import unittest\n\nclass Tests(unittest.TestCase):\n    @unittest.expectedFailure\n    def test_runs(self):\n        pass\n\ndef load_tests(loader, tests, pattern):\n    case = Tests('test_runs')\n    case.test_runs = lambda: case.fail('instance replacement must not be hidden')\n    return unittest.TestSuite([case])\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("guards module setup replacements across loaded test modules", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-cross-module-setup-replacement-"));
  try {
    const path = join(root, "test_cross_module_setup.py");
    writeFileSync(path, "import sys\nimport types\nimport unittest\n\nmodule_a = types.ModuleType('module_a')\nmodule_b = types.ModuleType('module_b')\nsys.modules[module_a.__name__] = module_a\nsys.modules[module_b.__name__] = module_b\n\ndef setup_b():\n    pass\nmodule_b.setUpModule = setup_b\n\nclass First(unittest.TestCase):\n    def test_replace(self):\n        async def hidden():\n            raise AssertionError('module setup replacement must not run unguarded')\n        module_b.setUpModule = hidden\nFirst.__module__ = 'module_a'\n\nclass Second(unittest.TestCase):\n    def test_runs(self):\n        pass\nSecond.__module__ = 'module_b'\n\ndef load_tests(loader, tests, pattern):\n    return unittest.TestSuite([First('test_replace'), Second('test_runs')])\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("preserves instance method replacements installed by the method itself", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-self-replacing-instance-method-"));
  try {
    const path = join(root, "test_self_replacing_instance_method.py");
    writeFileSync(path, "import unittest\n\nclass Tests(unittest.TestCase):\n    def setUp(self):\n        self.setUp = lambda: self.fail('replacement setup must run')\n    def test_runs(self):\n        pass\n\ndef load_tests(loader, tests, pattern):\n    case = Tests('test_runs')\n    return unittest.TestSuite([case, case])\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("rejects load_tests hooks that exit successfully before returning a suite", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-load-tests-exit-"));
  try {
    const path = join(root, "test_load_tests_exit.py");
    writeFileSync(path, "import unittest\n\nclass Tests(unittest.TestCase):\n    def test_should_not_run(self):\n        raise AssertionError('load_tests must not skip the discovered suite')\n\ndef load_tests(loader, tests, pattern):\n    raise SystemExit(0)\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("guards FunctionTestCase callbacks replaced during execution", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-function-case-replacement-"));
  try {
    const setupPath = join(root, "test_function_case_setup_replacement.py");
    writeFileSync(setupPath, "import unittest\n\nasync def hidden():\n    raise AssertionError('async replacement must not run')\n\ndef load_tests(loader, tests, pattern):\n    case = unittest.FunctionTestCase(lambda: None)\n    def setup():\n        case._testFunc = hidden\n    case._setUpFunc = setup\n    return unittest.TestSuite([case])\n");
    const setupChild = Bun.spawn(commandForTest({ path: setupPath, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await setupChild.exited).toBe(1);

    const teardownPath = join(root, "test_function_case_teardown_replacement.py");
    writeFileSync(teardownPath, "import unittest\n\nasync def hidden():\n    return None\n\ndef load_tests(loader, tests, pattern):\n    case = unittest.FunctionTestCase(lambda: setattr(case, '_tearDownFunc', hidden))\n    case._tearDownFunc = lambda: None\n    return unittest.TestSuite([case])\n");
    const teardownChild = Bun.spawn(commandForTest({ path: teardownPath, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await teardownChild.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("guards FunctionTestCase callbacks replaced by another case", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-function-case-cross-replacement-"));
  try {
    const path = join(root, "test_function_case_cross_replacement.py");
    writeFileSync(path, "import unittest\n\nasync def hidden():\n    raise AssertionError('async replacement must not run')\n\nsecond = unittest.FunctionTestCase(lambda: None)\ndef replace():\n    second._testFunc = hidden\n\ndef load_tests(loader, tests, pattern):\n    return unittest.TestSuite([unittest.FunctionTestCase(replace), second])\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("supports unhashable Python TestCase instances", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-unhashable-testcase-"));
  try {
    const path = join(root, "test_unhashable.py");
    writeFileSync(path, "import unittest\n\nclass Tests(unittest.TestCase):\n    __hash__ = None\n    def test_runs(self):\n        pass\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(0);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("executes a Python unittest suite without a main guard", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-no-main-"));
  try {
    const marker = join(root, "executed.txt");
    const path = join(root, "test_no_main.py");
    writeFileSync(path, `import unittest\nfrom pathlib import Path\n\nclass NoMainTests(unittest.TestCase):\n    def test_runs(self):\n        Path(${JSON.stringify(marker)}).write_text("ok")\n`);
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(0);
    expect(existsSync(marker)).toBe(true);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("executes a hyphenated Python unittest file", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-hyphenated-"));
  try {
    const path = join(root, "test-hyphenated.py");
    writeFileSync(path, "import unittest\n\nclass HyphenatedTests(unittest.TestCase):\n    def test_runs(self):\n        self.assertTrue(True)\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    const stdout = await new Response(child.stdout).text();
    const stderr = await new Response(child.stderr).text();
    expect(await child.exited).toBe(0);
    expect(executedTestCount("python", `${stdout}\n${stderr}`)).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("executes a module-level Python test function", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-function-"));
  try {
    const path = join(root, "test_function.py");
    writeFileSync(path, "def test_runs():\n    assert True\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    const stdout = await new Response(child.stdout).text();
    const stderr = await new Response(child.stderr).text();
    expect(await child.exited).toBe(0);
    expect(executedTestCount("python", `${stdout}\n${stderr}`)).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("fails closed for unsupported async Python tests", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-async-function-"));
  try {
    const path = join(root, "test_async.py");
    writeFileSync(path, "async def test_not_run():\n    raise AssertionError('must be awaited')\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(1);

    const generator = join(root, "test_generator.py");
    writeFileSync(generator, "def test_generator():\n    yield 1\n    raise AssertionError('must not be skipped')\n");
    const generatorChild = Bun.spawn(commandForTest({ path: generator, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await generatorChild.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("fails closed for inherited async and async-generator Python tests", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-inherited-async-"));
  try {
    const inherited = join(root, "test_inherited_async.py");
    writeFileSync(inherited, "import unittest\n\nclass Base(unittest.TestCase):\n    async def test_inherited(self):\n        raise AssertionError('must be awaited')\n\nclass Derived(Base):\n    pass\n");
    const inheritedChild = Bun.spawn(commandForTest({ path: inherited, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await inheritedChild.exited).toBe(1);

    const generator = join(root, "test_async_generator.py");
    writeFileSync(generator, "async def test_generator():\n    yield 1\n    raise AssertionError('must not run')\n");
    const generatorChild = Bun.spawn(commandForTest({ path: generator, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await generatorChild.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("fails closed for decorated async Python tests", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-decorated-async-"));
  try {
    const path = join(root, "test_decorated_async.py");
    writeFileSync(path, "import functools\n\n@functools.lru_cache()\nasync def test_not_run():\n    raise AssertionError('must be awaited')\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("fails closed for TestCase methods that return awaitables", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-testcase-awaitable-"));
  try {
    const path = join(root, "test_testcase_awaitable.py");
    writeFileSync(path, "import unittest\n\nclass Tests(unittest.TestCase):\n    def test_not_run(self):\n        async def hidden():\n            raise AssertionError('must be awaited')\n        return hidden()\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("fails closed for TestCase lifecycle hooks that return awaitables", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-testcase-hooks-"));
  try {
    const setupPath = join(root, "test_setup_hook.py");
    writeFileSync(setupPath, "import unittest\n\nclass Tests(unittest.TestCase):\n    def setUp(self):\n        async def hidden():\n            return None\n        return hidden()\n    def test_not_run(self):\n        pass\n");
    const setupChild = Bun.spawn(commandForTest({ path: setupPath, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await setupChild.exited).toBe(1);

    const teardownPath = join(root, "test_teardown_hook.py");
    writeFileSync(teardownPath, "import unittest\n\nclass Tests(unittest.TestCase):\n    def tearDown(self):\n        def hidden():\n            yield None\n        return hidden()\n    def test_runs(self):\n        pass\n");
    const teardownChild = Bun.spawn(commandForTest({ path: teardownPath, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await teardownChild.exited).toBe(1);

    const classPath = join(root, "test_class_hook.py");
    writeFileSync(classPath, "import unittest\n\nclass Tests(unittest.TestCase):\n    @classmethod\n    def setUpClass(cls):\n        async def hidden():\n            return None\n        return hidden()\n    def test_not_run(self):\n        pass\n");
    const classChild = Bun.spawn(commandForTest({ path: classPath, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await classChild.exited).toBe(1);

    const replacementPath = join(root, "test_cleanup_lifecycle_replacement.py");
    writeFileSync(replacementPath, "import unittest\n\nasync def hidden():\n    raise AssertionError('replaced class lifecycle must not run')\n\nclass Tests(unittest.TestCase):\n    @classmethod\n    def tearDownClass(cls):\n        pass\n    def test_runs(self):\n        self.addCleanup(lambda: setattr(type(self), 'tearDownClass', staticmethod(hidden)))\n");
    const replacementChild = Bun.spawn(commandForTest({ path: replacementPath, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await replacementChild.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("fails closed for Python cleanup callbacks that return unsupported values", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-testcase-cleanup-"));
  try {
    const instancePath = join(root, "test_instance_cleanup.py");
    writeFileSync(instancePath, "import unittest\n\nclass Tests(unittest.TestCase):\n    def test_runs(self):\n        async def hidden():\n            return None\n        self.addCleanup(lambda: hidden())\n");
    const instanceChild = Bun.spawn(commandForTest({ path: instancePath, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await instanceChild.exited).toBe(1);

    const classPath = join(root, "test_class_cleanup.py");
    writeFileSync(classPath, "import unittest\n\nclass Tests(unittest.TestCase):\n    @classmethod\n    def setUpClass(cls):\n        def hidden():\n            yield None\n        cls.addClassCleanup(hidden)\n    def test_runs(self):\n        pass\n");
    const classChild = Bun.spawn(commandForTest({ path: classPath, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await classChild.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("preserves unittest decorators and inherited class lifecycle receivers", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-testcase-metadata-"));
  try {
    const path = join(root, "test_metadata.py");
    writeFileSync(path, "import unittest\n\nclass Base(unittest.TestCase):\n    marker = []\n    @classmethod\n    def setUpClass(cls):\n        cls.marker = [cls.__name__]\n    @classmethod\n    def tearDownClass(cls):\n        if cls.marker != [cls.__name__]:\n            raise AssertionError(cls.marker)\n    def test_base(self):\n        self.assertEqual(self.marker, [self.__class__.__name__])\n\nclass Child(Base):\n    @unittest.skip('decorator metadata')\n    def test_skipped(self):\n        raise AssertionError('skip metadata was lost')\n    @unittest.expectedFailure\n    def test_expected_failure(self):\n        self.fail('expected failure metadata was lost')\n    def test_child(self):\n        self.assertEqual(self.marker, [self.__class__.__name__])\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(0);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("fails closed for unsupported Python module lifecycle and import-time class cleanups", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-module-lifecycle-"));
  try {
    const setupPath = join(root, "test_module_setup.py");
    writeFileSync(setupPath, "import unittest\n\nasync def setUpModule():\n    raise AssertionError('module setup must not be skipped')\n\nclass Tests(unittest.TestCase):\n    def test_runs(self):\n        pass\n");
    const setupChild = Bun.spawn(commandForTest({ path: setupPath, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await setupChild.exited).toBe(1);

    const moduleCleanupPath = join(root, "test_module_cleanup.py");
    writeFileSync(moduleCleanupPath, "import unittest\n\nasync def hidden():\n    raise AssertionError('module cleanup must not be skipped')\nunittest.addModuleCleanup(hidden)\n\nclass Tests(unittest.TestCase):\n    def test_runs(self):\n        pass\n");
    const moduleCleanupChild = Bun.spawn(commandForTest({ path: moduleCleanupPath, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await moduleCleanupChild.exited).toBe(1);

    const classCleanupPath = join(root, "test_import_class_cleanup.py");
    writeFileSync(classCleanupPath, "import unittest\n\nclass Tests(unittest.TestCase):\n    async def hidden():\n        raise AssertionError('class cleanup must not be skipped')\n    def test_runs(self):\n        pass\n\nTests.addClassCleanup(Tests.hidden)\n");
    const classCleanupChild = Bun.spawn(commandForTest({ path: classCleanupPath, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await classCleanupChild.exited).toBe(1);

    const replacementPath = join(root, "test_runtime_module_cleanup_replacement.py");
    writeFileSync(replacementPath, "import unittest\n\ndef setUpModule():\n    async def replacement(function, /, *args, **kwargs):\n        return None\n    unittest.addModuleCleanup = replacement\n\nclass Tests(unittest.TestCase):\n    def test_register(self):\n        unittest.addModuleCleanup(lambda: None)\n");
    const replacementChild = Bun.spawn(commandForTest({ path: replacementPath, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await replacementChild.exited).toBe(1);

    const importReplacementPath = join(root, "test_import_module_cleanup_replacement.py");
    writeFileSync(importReplacementPath, "import unittest\n\nasync def replacement(function, /, *args, **kwargs):\n    return None\nunittest.addModuleCleanup = replacement\n\nclass Tests(unittest.TestCase):\n    def test_register(self):\n        unittest.addModuleCleanup(lambda: None)\n");
    const importReplacementChild = Bun.spawn(commandForTest({ path: importReplacementPath, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await importReplacementChild.exited).toBe(1);

    const doCleanupsPath = join(root, "test_async_do_cleanups.py");
    writeFileSync(doCleanupsPath, "import unittest\n\nclass Tests(unittest.TestCase):\n    async def doCleanups(self):\n        return None\n    def test_runs(self):\n        pass\n");
    const doCleanupsChild = Bun.spawn(commandForTest({ path: doCleanupsPath, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await doCleanupsChild.exited).toBe(1);

    const doModuleCleanupsPath = join(root, "test_async_do_module_cleanups.py");
    writeFileSync(doModuleCleanupsPath, "import unittest\n\ndef setUpModule():\n    async def replacement(*args, **kwargs):\n        return None\n    unittest.doModuleCleanups = replacement\n    unittest.case.doModuleCleanups = replacement\n\nunittest.addModuleCleanup(lambda: None)\n\nclass Tests(unittest.TestCase):\n    def test_runs(self):\n        pass\n");
    const doModuleCleanupsChild = Bun.spawn(commandForTest({ path: doModuleCleanupsPath, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await doModuleCleanupsChild.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("executes synchronous Python module cleanups registered during import", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-module-cleanup-registration-"));
  try {
    const path = join(root, "test_module_cleanup_registration.py");
    writeFileSync(path, "import unittest\n\ndef cleanup():\n    pass\n\nunittest.addModuleCleanup(cleanup)\n\nclass Tests(unittest.TestCase):\n    def test_runs(self):\n        pass\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(0);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("guards Python module cleanups already present in the registry", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-module-cleanup-registry-"));
  try {
    const path = join(root, "test_module_cleanup_registry.py");
    writeFileSync(path, "import unittest\n\nasync def hidden():\n    raise AssertionError('module cleanup registry must be guarded')\n\nunittest._module_cleanups.append((hidden, (), {}))\n\nclass Tests(unittest.TestCase):\n    def test_runs(self):\n        pass\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("applies module lifecycle to module-level function tests", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-module-function-lifecycle-"));
  try {
    const path = join(root, "test_module_function_lifecycle.py");
    writeFileSync(path, "def setUpModule():\n    raise AssertionError('module setup must run')\n\ndef test_runs():\n    pass\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("guards constructor-registered Python cleanups before suite loading", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-constructor-cleanup-"));
  try {
    const path = join(root, "test_constructor_cleanup.py");
    writeFileSync(path, "import unittest\n\nclass Tests(unittest.TestCase):\n    def __init__(self, methodName='runTest'):\n        super().__init__(methodName)\n        async def hidden():\n            return None\n        self.addCleanup(hidden)\n    def test_runs(self):\n        pass\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("forwards cleanup callback keyword arguments without shadowing wrappers", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-cleanup-keyword-arguments-"));
  try {
    const path = join(root, "test_cleanup_keyword_arguments.py");
    writeFileSync(path, "import unittest\n\nclass Tests(unittest.TestCase):\n    def test_runs(self):\n        def restore(original=None):\n            if original != 42:\n                raise AssertionError(original)\n        self.addCleanup(restore, original=42)\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(0);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("preserves positional-only cleanup registration parameters", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-cleanup-positional-only-"));
  try {
    const path = join(root, "test_cleanup_positional_only.py");
    writeFileSync(path, "import unittest\n\ndef restore(function=None, case=None, test_class=None):\n    values = [value for value in (function, case, test_class) if value is not None]\n    if len(values) != 1 or values[0] not in (42, 43, 44):\n        raise AssertionError((function, case, test_class))\n\nunittest.addModuleCleanup(restore, function=42)\n\nclass Tests(unittest.TestCase):\n    @classmethod\n    def setUpClass(cls):\n        cls.addClassCleanup(restore, test_class=44)\n    def setUp(self):\n        self.addCleanup(restore, case=43)\n    def test_runs(self):\n        pass\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(0);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("preserves valid static Python class lifecycle hooks", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-static-class-hooks-"));
  try {
    const path = join(root, "test_static_class_hooks.py");
    writeFileSync(path, "import unittest\n\nclass Tests(unittest.TestCase):\n    @staticmethod\n    def setUpClass():\n        pass\n    @staticmethod\n    def tearDownClass():\n        pass\n    def test_runs(self):\n        pass\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(0);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("uses class lifecycle replacements installed after suite construction", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-dynamic-instance-hook-"));
  try {
    const path = join(root, "test_dynamic_instance_hook.py");
    writeFileSync(path, "import unittest\n\nclass Tests(unittest.TestCase):\n    @classmethod\n    def setUpClass(cls):\n        def failing_setup(instance):\n            raise AssertionError('replacement setup must run')\n        cls.setUp = failing_setup\n    def setUp(self):\n        pass\n    def test_runs(self):\n        pass\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("preserves instance-replaced test methods selected by load_tests", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-instance-replaced-test-"));
  try {
    const path = join(root, "test_instance_replaced_test.py");
    writeFileSync(path, "import unittest\n\nclass Tests(unittest.TestCase):\n    def test_runs(self):\n        pass\n\ndef load_tests(loader, tests, pattern):\n    case = Tests('test_runs')\n    def failing():\n        raise AssertionError('replacement test must run')\n    case.test_runs = failing\n    return unittest.TestSuite([case])\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("preserves skipped Python test metadata on guarded methods", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-skipped-test-metadata-"));
  try {
    const path = join(root, "test_skipped_method_metadata.py");
    writeFileSync(path, "import unittest\n\nclass Tests(unittest.TestCase):\n    def setUp(self):\n        raise AssertionError('skipped setup must not run')\n    @unittest.skip('covered elsewhere')\n    def test_runs(self):\n        pass\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(0);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("guards lifecycle replacements installed during setup", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-runtime-lifecycle-replacements-"));
  try {
    const instancePath = join(root, "test_instance_lifecycle_replacement.py");
    writeFileSync(instancePath, "import unittest\n\nclass Tests(unittest.TestCase):\n    def setUp(self):\n        async def replacement():\n            return None\n        self.tearDown = replacement\n    def tearDown(self):\n        pass\n    def test_runs(self):\n        pass\n");
    const instanceChild = Bun.spawn(commandForTest({ path: instancePath, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await instanceChild.exited).toBe(1);

    const classPath = join(root, "test_class_lifecycle_replacement.py");
    writeFileSync(classPath, "import unittest\n\nclass Tests(unittest.TestCase):\n    @classmethod\n    def setUpClass(cls):\n        async def replacement():\n            return None\n        cls.tearDownClass = replacement\n    @classmethod\n    def tearDownClass(cls):\n        pass\n    def test_runs(self):\n        pass\n");
    const classChild = Bun.spawn(commandForTest({ path: classPath, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await classChild.exited).toBe(1);

    const modulePath = join(root, "test_module_lifecycle_replacement.py");
    writeFileSync(modulePath, "import unittest\n\ndef setUpModule():\n    async def replacement():\n        return None\n    globals()['tearDownModule'] = replacement\n\ndef tearDownModule():\n    pass\n\nclass Tests(unittest.TestCase):\n    def test_runs(self):\n        pass\n");
    const moduleChild = Bun.spawn(commandForTest({ path: modulePath, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await moduleChild.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("does not let runtime test replacements retain expected-failure metadata", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-runtime-test-metadata-"));
  try {
    const path = join(root, "test_runtime_test_metadata.py");
    writeFileSync(path, "import unittest\n\nclass Tests(unittest.TestCase):\n    @classmethod\n    def setUpClass(cls):\n        cls.test_runs = lambda self: self.fail('runtime replacement must fail')\n    @unittest.expectedFailure\n    def test_runs(self):\n        pass\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("guards lifecycle replacements installed by a test body", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-runtime-body-replacements-"));
  try {
    const instancePath = join(root, "test_body_instance_replacement.py");
    writeFileSync(instancePath, "import unittest\n\nclass Tests(unittest.TestCase):\n    def tearDown(self):\n        pass\n    def test_runs(self):\n        async def replacement():\n            return None\n        self.tearDown = replacement\n");
    const instanceChild = Bun.spawn(commandForTest({ path: instancePath, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await instanceChild.exited).toBe(1);

    const classPath = join(root, "test_body_class_replacement.py");
    writeFileSync(classPath, "import unittest\n\nclass Tests(unittest.TestCase):\n    @classmethod\n    def tearDownClass(cls):\n        pass\n    def test_runs(self):\n        async def replacement():\n            return None\n        type(self).tearDownClass = replacement\n");
    const classChild = Bun.spawn(commandForTest({ path: classPath, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await classChild.exited).toBe(1);

    const modulePath = join(root, "test_body_module_replacement.py");
    writeFileSync(modulePath, "import unittest\n\ndef tearDownModule():\n    pass\n\nclass Tests(unittest.TestCase):\n    def test_runs(self):\n        async def replacement():\n            return None\n        globals()['tearDownModule'] = replacement\n");
    const moduleChild = Bun.spawn(commandForTest({ path: modulePath, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await moduleChild.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("fails closed when setUp replaces a test method with stale expected-failure metadata", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-setup-test-replacement-"));
  try {
    const path = join(root, "test_setup_test_replacement.py");
    writeFileSync(path, "import unittest\n\nclass Tests(unittest.TestCase):\n    def setUp(self):\n        self.test_runs = lambda: self.fail('replacement must not be hidden')\n    @unittest.expectedFailure\n    def test_runs(self):\n        pass\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("guards class lifecycle replacements installed by module setup", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-module-class-replacement-"));
  try {
    const path = join(root, "test_module_class_replacement.py");
    writeFileSync(path, "import unittest\n\ndef setUpModule():\n    async def replacement():\n        return None\n    Tests.setUpClass = replacement\n\nclass Tests(unittest.TestCase):\n    @classmethod\n    def setUpClass(cls):\n        pass\n    def test_runs(self):\n        pass\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("preserves bound Python class lifecycle receivers", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-bound-class-lifecycle-"));
  try {
    const path = join(root, "test_bound_class_lifecycle.py");
    writeFileSync(path, "import unittest\n\nclass Hooks:\n    def setup(self):\n        pass\n\nhooks = Hooks()\n\nclass Tests(unittest.TestCase):\n    setUpClass = hooks.setup\n    tearDownClass = hooks.setup\n    def test_runs(self):\n        pass\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(0);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("preserves static Python class cleanup registration", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-static-class-cleanup-"));
  try {
    const path = join(root, "test_static_class_cleanup.py");
    writeFileSync(path, "import unittest\n\nclass Tests(unittest.TestCase):\n    @staticmethod\n    def addClassCleanup(function, /, *args, **kwargs):\n        function(*args, **kwargs)\n    @classmethod\n    def setUpClass(cls):\n        cls.addClassCleanup(lambda: None)\n    def test_runs(self):\n        pass\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(0);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("preserves plain Python class cleanup registration", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-plain-class-cleanup-"));
  try {
    const path = join(root, "test_plain_class_cleanup.py");
    writeFileSync(path, "import unittest\n\nclass Tests(unittest.TestCase):\n    def addClassCleanup(self, function, /, *args, **kwargs):\n        self.assertTrue(callable(function))\n    def test_runs(self):\n        self.addClassCleanup(lambda: None)\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(0);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("guards class cleanup replacements installed after suite setup", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-class-cleanup-replacement-"));
  try {
    const path = join(root, "test_class_cleanup_replacement.py");
    writeFileSync(path, "import unittest\n\nclass Tests(unittest.TestCase):\n    def test_a_replace(self):\n        def replacement(function, /, *args, **kwargs):\n            type(self)._class_cleanups.append((function, args, kwargs))\n        type(self).addClassCleanup = staticmethod(replacement)\n    def test_b_register(self):\n        async def hidden():\n            return None\n        self.addClassCleanup(hidden)\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("rejects unsupported results from runtime cleanup registrations", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-runtime-cleanup-registration-result-"));
  try {
    const instancePath = join(root, "test_instance_cleanup_registration_result.py");
    writeFileSync(instancePath, "import unittest\n\nclass Tests(unittest.TestCase):\n    def setUp(self):\n        async def replacement(function, /, *args, **kwargs):\n            return None\n        self.addCleanup = replacement\n    def test_runs(self):\n        self.addCleanup(lambda: None)\n");
    const instanceChild = Bun.spawn(commandForTest({ path: instancePath, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await instanceChild.exited).toBe(1);

    const classPath = join(root, "test_class_cleanup_registration_result.py");
    writeFileSync(classPath, "import unittest\n\nclass Tests(unittest.TestCase):\n    @classmethod\n    def setUpClass(cls):\n        async def replacement(receiver, function, /, *args, **kwargs):\n            return None\n        cls.addClassCleanup = classmethod(replacement)\n    def test_runs(self):\n        self.addClassCleanup(lambda: None)\n");
    const classChild = Bun.spawn(commandForTest({ path: classPath, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await classChild.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("guards bound and subclass-specific Python class cleanup registries", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-class-cleanup-registries-"));
  try {
    const boundPath = join(root, "test_bound_class_cleanup.py");
    writeFileSync(boundPath, "import unittest\n\nclass Hooks:\n    def register(self, function, /, *args, **kwargs):\n        function(*args, **kwargs)\n\nhooks = Hooks()\n\nclass Tests(unittest.TestCase):\n    addClassCleanup = hooks.register\n    @classmethod\n    def setUpClass(cls):\n        async def hidden():\n            return None\n        cls.addClassCleanup(hidden)\n    def test_runs(self):\n        pass\n");
    const boundChild = Bun.spawn(commandForTest({ path: boundPath, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await boundChild.exited).toBe(1);

    const subclassPath = join(root, "test_subclass_class_cleanup.py");
    writeFileSync(subclassPath, "import unittest\n\nclass Base(unittest.TestCase):\n    def test_base(self):\n        pass\n\nclass Child(Base):\n    @staticmethod\n    def addClassCleanup(function, /, *args, **kwargs):\n        function(*args, **kwargs)\n    @classmethod\n    def setUpClass(cls):\n        async def hidden():\n            return None\n        cls.addClassCleanup(hidden)\n    def test_child(self):\n        pass\n");
    const subclassChild = Bun.spawn(commandForTest({ path: subclassPath, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await subclassChild.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("fails closed for module context cleanups and loaded FunctionTestCases", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-python-extension-cases-"));
  try {
    const contextPath = join(root, "test_module_context.py");
    writeFileSync(contextPath, "import unittest\n\nclass Context:\n    def __enter__(self):\n        return self\n    async def __exit__(self, exc_type, exc_value, traceback):\n        return False\n\nunittest.enterModuleContext(Context())\ndef test_runs():\n    pass\n");
    const contextChild = Bun.spawn(commandForTest({ path: contextPath, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await contextChild.exited).toBe(1);

    const functionCasePath = join(root, "test_loaded_function_case.py");
    writeFileSync(functionCasePath, "import unittest\n\nasync def hidden():\n    return None\n\ndef load_tests(loader, tests, pattern):\n    return unittest.TestSuite([unittest.FunctionTestCase(hidden)])\n");
    const functionCaseChild = Bun.spawn(commandForTest({ path: functionCasePath, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await functionCaseChild.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("fails closed for async tests nested in BaseTestSuite", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-base-test-suite-"));
  try {
    const path = join(root, "test_base_test_suite.py");
    writeFileSync(path, "import unittest\n\nasync def hidden():\n    return None\n\ndef load_tests(loader, tests, pattern):\n    return unittest.BaseTestSuite([unittest.FunctionTestCase(hidden)])\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("guards async lifecycle hooks from modules returned by load_tests", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-loaded-module-lifecycle-"));
  try {
    writeFileSync(join(root, "helper.py"), "import unittest\n\nasync def setUpModule():\n    return None\n\nclass Tests(unittest.TestCase):\n    def test_runs(self):\n        pass\n");
    const path = join(root, "test_loaded_module_lifecycle.py");
    writeFileSync(path, "import unittest\nfrom helper import Tests\n\ndef load_tests(loader, tests, pattern):\n    return loader.loadTestsFromTestCase(Tests)\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(1);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("does not duplicate module functions when load_tests supplies the suite", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-load-tests-deduplication-"));
  try {
    const path = join(root, "test_load_tests_deduplication.py");
    writeFileSync(path, "import unittest\n\ncalls = []\ndef test_runs():\n    calls.append(1)\n\ndef load_tests(loader, tests, pattern):\n    return unittest.TestSuite([unittest.FunctionTestCase(test_runs)])\n\ndef tearDownModule():\n    if calls != [1]:\n        raise AssertionError(calls)\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(0);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("only rejects async tests selected by load_tests", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-load-tests-selection-"));
  try {
    const path = join(root, "test_load_tests_selection.py");
    writeFileSync(path, "import unittest\n\nclass Hidden(unittest.TestCase):\n    async def test_hidden(self):\n        return None\n\nclass Visible(unittest.TestCase):\n    def test_visible(self):\n        pass\n\ndef load_tests(loader, tests, pattern):\n    return loader.loadTestsFromTestCase(Visible)\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(0);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("does not let expectedFailure hide unsupported Python results", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-python-expected-failure-"));
  try {
    const casePath = join(root, "test_expected_failure_case.py");
    writeFileSync(casePath, "import unittest\n\nclass Tests(unittest.TestCase):\n    @unittest.expectedFailure\n    def test_runs(self):\n        async def hidden():\n            return None\n        return hidden()\n");
    const caseChild = Bun.spawn(commandForTest({ path: casePath, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await caseChild.exited).toBe(1);

    const modulePath = join(root, "test_expected_failure_module.py");
    writeFileSync(modulePath, "import unittest\n\n@unittest.expectedFailure\ndef test_expected_failure():\n    raise AssertionError('expected')\n");
    const moduleChild = Bun.spawn(commandForTest({ path: modulePath, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await moduleChild.exited).toBe(0);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("preserves zero-argument Python class lifecycle hooks", async () => {
  const root = mkdtempSync(join(Bun.env.TMPDIR ?? "/tmp", "agent-toolkit-discovery-zero-argument-class-hooks-"));
  try {
    const path = join(root, "test_zero_argument_class_hooks.py");
    writeFileSync(path, "import unittest\n\nclass Tests(unittest.TestCase):\n    def setUpClass():\n        pass\n    def tearDownClass():\n        pass\n    def test_runs(self):\n        pass\n");
    const child = Bun.spawn(commandForTest({ path, kind: "python" }), { cwd: root, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(0);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});
