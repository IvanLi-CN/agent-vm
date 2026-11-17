import { describe, expect, test } from "bun:test";
import { join } from "node:path";
import { mkdtempSync, writeFileSync, readFileSync, readdirSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";

const root = join(import.meta.dir, "..");
const builder = join(root, "scripts/build_images.ts");

describe("guest image builder", () => {
  test("validates the amd64 Ubuntu 24.04 lock without Incus", () => {
    const result = Bun.spawnSync([process.execPath, builder, "--all", "--check"], { cwd: join(root, ".."), stdout: "pipe", stderr: "pipe" });
    expect(result.exitCode).toBe(0);
    const value = JSON.parse(new TextDecoder().decode(result.stdout));
    expect(value.architecture).toBe("amd64");
    expect(value.profiles).toEqual(["base", "rust", "java", "go"]);
  });

  test("keeps the required language families and runtime versions in the lock", () => {
    const lock = Bun.file(join(root, "lock/ubuntu-24.04-amd64.lock.yaml"));
    return lock.text().then((text) => {
      expect(text).toContain("architecture: amd64");
      expect(text).toContain("require_fingerprint: true");
      expect(text).toContain("zulu: \"25.0.1\"");
      expect(text).toContain("maven: \"3.10.1\"");
      expect(text).toContain("postgresql-client-16");
      expect(text).toContain("mysql-client");
      expect(text).toContain("https://nodejs.org/dist/v24.11.1/");
      expect(text).toContain("https://go.dev/dl/go1.25.4.linux-amd64.tar.gz");
      expect(text).toContain("https://static.rust-lang.org/dist/rust-1.91.1-");
      expect(text).toContain("https://dlcdn.apache.org/maven/maven-3/3.10.1/binaries/apache-maven-3.10.1-bin.tar.gz");
      expect(text).not.toContain("playwright");
    });
  });

  test("installs Maven only in the Java profile and checks its version", async () => {
    const language = await Bun.file(join(root, "provisioning/60-language.sh")).text();
    const readiness = await Bun.file(join(root, "provisioning/50-readiness.sh")).text();
    expect(language).toContain('download_verify "$MAVEN_URL" "$MAVEN_SHA256" "$work/maven.tar.gz"');
    expect(language).toContain("/opt/maven");
    expect(language).toContain("mvn -version");
    expect(readiness).toContain('mvn -version | grep -F "Apache Maven $MAVEN_VERSION "');
  });
});


test("builds both runtime variants with separate commands and artifacts", () => {
  const temp = mkdtempSync(join(tmpdir(), "image-variants-test-"));
  try {
    const log = join(temp, "commands.jsonl");
    writeFileSync(join(temp, "incus"), `#!/usr/bin/env python3
import hashlib,json,os,sys,subprocess,time,tarfile,io
args=sys.argv[1:]
if args and args[0]=="query" and "--project" in args:sys.exit(43)
if "--project" in args:
 i=args.index("--project");del args[i:i+2]
with open(os.environ["IMAGE_TEST_LOG"],"a") as f:f.write(json.dumps(args)+"\\n")
if args[:2]==["image","info"]:sys.exit(1)
if os.environ.get("IMAGE_TEST_FAIL_START")=="1" and args[0]=="start":
 if "unprivileged" in args[1]:time.sleep(0.3)
 sys.exit(42)
if os.environ.get("IMAGE_TEST_FAIL_READY")=="1" and args[0]=="start" and args[1].startswith("avm-ready-"):sys.exit(45)
if args[:2]==["file","push"]:
 with open(args[2]) as f:script=f.read()
 assert script.startswith("set -euo pipefail"+chr(10)), "missing real newline"
 assert "docker build --network=none" in script
 assert "systemctl restart docker" in script
 if "avm-build-common-" not in args[3]:
  assert script.index("timeout 90 bash") < script.index("verify_ssh()"), "sealed guest validation raced first boot"
 subprocess.run(["bash","-n",args[2]],check=True)
if args[:2]==["file","pull"]:
 with open(args[3],"w") as f:f.write('{"ready":true}')
if args[0]=="query" and os.environ.get("IMAGE_TEST_FAIL_QUERY")=="1":sys.exit(44)
if args[0]=="query":print(json.dumps({"target":hashlib.sha256(args[1].encode()).hexdigest()}))
if args[:2]==["image","export"]:
 with tarfile.open(args[3]+".tar.gz","w:gz") as f:
  item=tarfile.TarInfo("metadata.yaml");data=args[2].encode();item.size=len(data);f.addfile(item,io.BytesIO(data))
  if os.environ.get("IMAGE_TEST_DEVICE_NODE")=="1":
   item=tarfile.TarInfo("rootfs/var/lib/docker/volumes/backingFsBlockDev");item.type=tarfile.BLKTYPE;item.devmajor=8;item.devminor=1;f.addfile(item)
`, { mode: 0o755 });
    const output = join(temp, "output");
    const result = Bun.spawnSync([process.execPath, builder, "--all"], {
      env: { ...process.env, PATH: `${temp}:${process.env.PATH}`, IMAGE_TEST_LOG: log, AGENT_VM_IMAGE_OUTPUT: output },
      stdout: "pipe", stderr: "pipe",
    });
    expect(new TextDecoder().decode(result.stderr)).toBe("");
    expect(result.exitCode).toBe(0);
    const commands = readFileSync(log, "utf8").trim().split("\n").map(line => JSON.parse(line) as string[]);
    const init = commands.filter(args => args[0] === "init");
    expect(init).toHaveLength(18);
    expect(init.filter(args => args.includes("security.privileged=true"))).toHaveLength(9);
    expect(init.filter(args => args.includes("security.privileged=false"))).toHaveLength(9);
    for (const args of init) expect(args).toContain("security.nesting=true");
    const manifests = readdirSync(output).filter(name => name.endsWith(".manifest.json"));
    expect(manifests).toHaveLength(8);
    for (const name of manifests) {
      const manifest = JSON.parse(readFileSync(join(output, name), "utf8"));
      expect(manifest.runtime_config["security.privileged"]).toBe(String(manifest.variant === "privileged"));
      expect(name).toContain(`-${manifest.variant}-`);
    }
    expect(readFileSync(join(output, "agent-vm-ubuntu24-SHA256SUMS"), "utf8").trim().split("\n")).toHaveLength(32);
    const commonOutput = join(temp, "common-output");
    const commonLog = join(temp, "common.jsonl");
    const common = Bun.spawnSync([process.execPath, builder, "--base-only", "--variant", "privileged"], {
      env: { ...process.env, PATH: `${temp}:${process.env.PATH}`, IMAGE_TEST_LOG: commonLog, AGENT_VM_IMAGE_OUTPUT: commonOutput },
      stdout: "pipe", stderr: "pipe",
    });
    expect(new TextDecoder().decode(common.stderr)).toBe("");
    expect(common.exitCode).toBe(0);
    const commonArchive = join(commonOutput, "agent-vm-ubuntu24-common-privileged-amd64.tar.gz");
    expect(readdirSync(commonOutput)).toEqual(expect.arrayContaining([
      "agent-vm-ubuntu24-common-privileged-amd64.manifest.json",
      "agent-vm-ubuntu24-common-privileged-SHA256SUMS",
    ]));
    const importedLog = join(temp, "imported.jsonl");
    const importedOutput = join(temp, "imported-output");
    const imported = Bun.spawnSync([process.execPath, builder, "--profile", "go", "--variant", "privileged", "--common-base-archive", commonArchive], {
      env: { ...process.env, PATH: `${temp}:${process.env.PATH}`, IMAGE_TEST_LOG: importedLog, AGENT_VM_IMAGE_OUTPUT: importedOutput },
      stdout: "pipe", stderr: "pipe",
    });
    expect(new TextDecoder().decode(imported.stderr)).toBe("");
    expect(imported.exitCode).toBe(0);
    const importedCommands = readFileSync(importedLog, "utf8").trim().split("\n").map(line => JSON.parse(line) as string[]);
    expect(importedCommands.some(args => args[0] === "image" && args[1] === "import")).toBe(true);
    expect(importedCommands.some(args => args[0] === "publish")).toBe(true);
    const failureLog = join(temp, "failure.jsonl");
    const failed = Bun.spawnSync([process.execPath, builder, "--all", "--variant", "unprivileged"], {
      env: { ...process.env, PATH: `${temp}:${process.env.PATH}`, IMAGE_TEST_LOG: failureLog, IMAGE_TEST_FAIL_START: "1", AGENT_VM_IMAGE_OUTPUT: join(temp, "failure-output") },
      stdout: "pipe", stderr: "pipe",
    });
    expect(failed.exitCode).not.toBe(0);
    const failedCommands = readFileSync(failureLog, "utf8").trim().split("\n").map(line => JSON.parse(line) as string[]);
    expect(failedCommands.filter(args => args[0] === "init")).toHaveLength(1);
    expect(failedCommands.find(args => args[0] === "init")).toContain("security.privileged=false");
    expect(failedCommands.some(args => args[0] === "publish")).toBe(false);
    expect(failedCommands.some(args => args[0] === "delete")).toBe(true);
    expect(readdirSync(join(temp, "failure-output"))).toHaveLength(0);
    const queryLog = join(temp, "query-failure.jsonl");
    const queryFailure = Bun.spawnSync([process.execPath, builder, "--profile", "go", "--variant", "privileged"], {
      env: { ...process.env, PATH: `${temp}:${process.env.PATH}`, IMAGE_TEST_LOG: queryLog, IMAGE_TEST_FAIL_QUERY: "1", AGENT_VM_IMAGE_OUTPUT: join(temp, "query-failure-output") }, stdout: "pipe", stderr: "pipe",
    });
    expect(queryFailure.exitCode).not.toBe(0);
    const queryCommands = readFileSync(queryLog, "utf8").trim().split("\n").map(line => JSON.parse(line) as string[]);
    const ownedAlias = queryCommands.find(args => args[0] === "query")![1]!.split("/aliases/")[1]!.split("?")[0]!;
    expect(queryCommands.filter(args => args.slice(0, 3).join(" ") === "image alias delete").map(args => args[3])).toEqual([ownedAlias]);
    expect(queryCommands.filter(args => args[0] === "delete").map(args => args[1])).toContain(ownedAlias);
    const concurrentLog = join(temp, "concurrent-failure.jsonl");
    const concurrent = Bun.spawnSync([process.execPath, builder, "--all"], {
      env: { ...process.env, PATH: `${temp}:${process.env.PATH}`, IMAGE_TEST_LOG: concurrentLog, IMAGE_TEST_FAIL_START: "1", AGENT_VM_IMAGE_OUTPUT: join(temp, "concurrent-output") }, stdout: "pipe", stderr: "pipe",
    });
    expect(concurrent.exitCode).not.toBe(0);
    const concurrentCommands = readFileSync(concurrentLog, "utf8").trim().split("\n").map(line => JSON.parse(line) as string[]);
    expect(concurrentCommands.filter(args => args[0] === "delete")).toHaveLength(2);
    expect(concurrentCommands.some(args => args[0] === "publish")).toBe(false);
    const readyLog = join(temp, "ready-failure.jsonl");
    const readyOutput = join(temp, "ready-failure-output");
    const readyFailure = Bun.spawnSync([process.execPath, builder, "--profile", "go", "--variant", "privileged"], {
      env: { ...process.env, PATH: `${temp}:${process.env.PATH}`, IMAGE_TEST_LOG: readyLog, IMAGE_TEST_FAIL_READY: "1", AGENT_VM_IMAGE_OUTPUT: readyOutput }, stdout: "pipe", stderr: "pipe",
    });
    expect(readyFailure.exitCode).not.toBe(0);
    const readyCommands = readFileSync(readyLog, "utf8").trim().split("\n").map(line => JSON.parse(line) as string[]);
    const fresh = readyCommands.find(args => args[0] === "init" && args[2]!.startsWith("avm-ready-"))![2]!;
    expect(readyCommands.some(args => args[0] === "delete" && args[1] === fresh)).toBe(true);
    expect(readyCommands.some(args => args.slice(0, 3).join(" ") === "image alias delete")).toBe(true);
    expect(readdirSync(readyOutput).some(name => name.endsWith(".manifest.json") || name.endsWith("SHA256SUMS"))).toBe(false);
    const deviceOutput = join(temp, "device-output");
    const deviceFailure = Bun.spawnSync([process.execPath, builder, "--profile", "go", "--variant", "privileged"], {
      env: { ...process.env, PATH: `${temp}:${process.env.PATH}`, IMAGE_TEST_LOG: join(temp, "device.jsonl"), IMAGE_TEST_DEVICE_NODE: "1", AGENT_VM_IMAGE_OUTPUT: deviceOutput }, stdout: "pipe", stderr: "pipe",
    });
    expect(deviceFailure.exitCode).not.toBe(0);
    expect(new TextDecoder().decode(deviceFailure.stderr)).toContain("build-host device in guest image");
    expect(readdirSync(deviceOutput).some(name => name.endsWith(".manifest.json") || name.endsWith("SHA256SUMS"))).toBe(false);
  } finally { rmSync(temp, { recursive: true, force: true }); }
}, 30_000);

test("supports selecting one variant and rejects invalid modes", () => {
  for (const variant of ["privileged", "unprivileged"]) {
    const result = Bun.spawnSync([process.execPath, builder, "--profile", "go", "--variant", variant, "--check"], { stdout: "pipe", stderr: "pipe" });
    expect(result.exitCode).toBe(0);
    const plan = JSON.parse(new TextDecoder().decode(result.stdout));
    expect(plan.builds).toHaveLength(1);
    expect(plan.builds[0].variant).toBe(variant);
  }
  for (const args of [["--variant"], ["--variant", "invalid"]]) {
    expect(Bun.spawnSync([process.execPath, builder, ...args, "--check"], { stdout: "pipe", stderr: "pipe" }).exitCode).not.toBe(0);
  }
});
