#!/usr/bin/env bun
import { createHash } from "node:crypto";
import { createReadStream, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { join, resolve } from "node:path";
import { parse } from "yaml";

type Lock = {
  schema: number; name: string; architecture: string; apt_lock_sha256: string;
  base: { source: string; fingerprint: string; require_fingerprint: boolean };
  versions: Record<string, string>;
  downloads: Record<string, { url: string; sha256: string }>;
  profiles: Record<string, { packages: string[] } | string[]>;
};
const root = resolve(process.env.AGENT_VM_IMAGE_SOURCE_ROOT ?? resolve(import.meta.dir, "../.."));
const args = Bun.argv.slice(2);
function fail(message: string): never { throw new Error(`image-build-error: ${message}`); }
function option(name: string, fallback: string) {
  const index = args.indexOf(name);
  if (index < 0) return fallback;
  const value = args[index + 1];
  if (!value || value.startsWith("--")) fail(`missing ${name} value`);
  return value;
}
for (let i = 0; i < args.length; i++) {
  if (["--profile", "--variant"].includes(args[i]!)) { i++; continue; }
  if (!["--all", "--check"].includes(args[i]!)) fail(`unknown argument: ${args[i]}`);
}
const profiles = args.includes("--all") ? ["base", "rust", "java", "go"] : [option("--profile", "base")];
const variant = option("--variant", "both");
if (!["both", "privileged", "unprivileged"].includes(variant)) fail("--variant must be both, privileged or unprivileged");
const variants = variant === "both" ? ["privileged", "unprivileged"] : [variant];
const lockPath = join(root, "images/lock/ubuntu-24.04-amd64.lock.yaml");
const lockText = readFileSync(lockPath, "utf8");
const lock = parse(lockText) as Lock;
const aptText = readFileSync(join(root, "images/lock/ubuntu-24.04-amd64.apt.json"), "utf8");
if (createHash("sha256").update(aptText).digest("hex") !== lock.apt_lock_sha256) fail("APT lock checksum mismatch");
const aptLock = JSON.parse(aptText) as Record<string, Record<string, {version: string; sha256: string}>>;
for (const group of ["ubuntu", "docker"]) {
 if (!Object.keys(aptLock[group] ?? {}).length) fail(`missing ${group} package locks`);
 for (const [name, item] of Object.entries(aptLock[group]!)) if (!/^[a-z0-9][a-z0-9+.-]*$/.test(name) || !/^[a-zA-Z0-9.+:~_-]+$/.test(item.version) || !/^[0-9a-f]{64}$/.test(item.sha256)) fail(`invalid APT package lock: ${name}`);
}
if (lock.schema !== 1 || lock.architecture !== "amd64" || lock.base.source !== "images:ubuntu/24.04" || !lock.base.require_fingerprint || !/^[0-9a-f]{64}$/.test(lock.base.fingerprint)) fail("a pinned Ubuntu 24.04 amd64 base is required");
for (const name of ["bun", "node", "go", "rust", "zulu"]) {
  const item = lock.downloads[name];
  if (!item || !item.url.startsWith("https://") || !/^[0-9a-f]{64}$/.test(item.sha256)) fail(`download ${name} requires HTTPS and SHA256`);
}
for (const profile of profiles) if (!lock.profiles[profile]) fail(`unknown profile: ${profile}`);
function runtimeConfig(mode: string): Record<string, string> {
  const config: Record<string, string> = { "security.privileged": String(mode === "privileged"), "security.nesting": "true", "security.syscalls.intercept.mknod": "true", "security.syscalls.intercept.setxattr": "true", "boot.autostart": "false" };
  for (const [key, value] of [["security.idmap.base", process.env.AGENT_VM_IMAGE_IDMAP_BASE], ["security.idmap.size", process.env.AGENT_VM_IMAGE_IDMAP_SIZE]]) {
    if (value && mode === "unprivileged") { if (!/^\d+$/.test(value)) fail("idmap settings must be numeric"); config[key!] = value; }
  }
  return config;
}
const builds = variants.flatMap(mode => profiles.map(profile => ({ profile, variant: mode, runtime_config: runtimeConfig(mode) })));
console.log(JSON.stringify({ action: args.includes("--check") ? "check" : "build", architecture: lock.architecture, base: lock.base, profiles, variants, builds }, null, 2));
if (args.includes("--check")) process.exit(0);
if (process.platform !== "linux" && !process.env.IMAGE_TEST_LOG) fail("real image builds require Linux/Incus");
const project = process.env.AGENT_VM_IMAGE_PROJECT ?? "default";
const pool = process.env.AGENT_VM_IMAGE_POOL ?? "default";
const network = process.env.AGENT_VM_IMAGE_NETWORK ?? "incusbr0";
const output = resolve(process.env.AGENT_VM_IMAGE_OUTPUT ?? join(root, "dist"));
mkdirSync(output, { recursive: true });
async function incus(argv: string[], capture = false, allowFailure = false) {
  console.log(JSON.stringify({ incus: argv }));
  // query takes project in its API URL and rejects the CLI --project flag.
  const scoped = argv[0] === "query" ? argv : [...argv, "--project", project];
  const process = Bun.spawn(["incus", ...scoped], { cwd: root, stdout: capture ? "pipe" : "inherit", stderr: "inherit" });
  const text = capture ? await new Response(process.stdout).text() : "";
  const code = await process.exited;
  if (code && !allowFailure) fail(`incus ${argv[0]} failed (${code})`);
  return { code, text };
}
async function digest(path: string) { const hash = createHash("sha256"); for await (const chunk of createReadStream(path)) hash.update(chunk); return hash.digest("hex"); }
const imageRoot = join(root, "images");
const read = (name: string) => readFileSync(join(imageRoot, "provisioning", name), "utf8");
const base = read("00-base.sh"), docker = read("30-docker.sh"), language = read("60-language.sh"), seal = read("70-seal.sh");
const aptInstaller = read("10-apt-lock.py");
const readiness = read("40-ssh-readiness.sh") + "\n" + read("45-docker-readiness.sh") + "\n" + read("50-readiness.sh");
const header = `set -euo pipefail\nexport DEBIAN_FRONTEND=noninteractive\ncat > /root/.agent-vm-apt-lock.json <<'APTLOCK'\n${aptText}\nAPTLOCK\ncat > /root/.agent-vm-apt-install.py <<'APTINSTALL'\n${aptInstaller}\nAPTINSTALL\n` + Object.entries(lock.versions).map(([key, value]) => `${key.toUpperCase()}_VERSION='${value}'\n`).join("") + Object.entries(lock.downloads).map(([key, item]) => `${key.toUpperCase()}_URL='${item.url}'\n${key.toUpperCase()}_SHA256='${item.sha256}'\n`).join("");
const checksums: string[] = [];
const buildNetwork = `install -d /etc/apt/apt.conf.d
cat > /etc/apt/apt.conf.d/80-agent-vm-build-network <<'APTNETWORK'
Acquire::ForceIPv4 "true";
Acquire::Retries "3";
Acquire::http::Timeout "30";
Acquire::https::Timeout "30";
APT::Update::Error-Mode "any";
APTNETWORK
for host in archive.ubuntu.com security.ubuntu.com; do
  timeout 90 bash -c 'until getent ahostsv4 "$1" >/dev/null; do sleep 2; done' -- "$host"
done
`;
const commonProfile = lock.profiles.base!;
const packages = Array.isArray(commonProfile) ? commonProfile : commonProfile.packages;
async function init(image: string, name: string, mode: string) {
  await incus(["init", image, name, "--no-profiles", "--storage", pool, "--network", network, ...Object.entries(runtimeConfig(mode)).flatMap(([key, value]) => ["--config", `${key}=${value}`]), "--config", "limits.cpu=2", "--config", "limits.memory=2GiB", "--config", `user.agent-vm.image-build=${sourceRevision}`]);
}
async function provision(name: string, script: string) {
  const dir = mkdtempSync("/tmp/agent-vm-image-script-");
  const path = join(dir, "provision.sh");
  writeFileSync(path, script, { mode: 0o700 });
  try {
    await incus(["file", "push", path, `${name}/root/.agent-vm-image-provision.sh`]);
    // exec options must precede --, not become guest command arguments.
    const child = Bun.spawn(["incus", "--project", project, "exec", name, "--", "timeout", "1800", "bash", "/root/.agent-vm-image-provision.sh"], { stdout: "inherit", stderr: "inherit" });
    if (await child.exited) fail(`guest provisioning failed: ${name}`);
  } finally { rmSync(dir, { recursive: true, force: true }); }
}
async function remove(name: string) { await incus(["delete", name, "--force"]); }
const sealedBoot = `timeout 90 bash -c 'until test -s /etc/ssh/ssh_host_ed25519_key.pub && (systemctl is-active --quiet ssh || systemctl is-active --quiet ssh.socket); do sleep 1; done'\n`;
async function verifyPublished(image: string, mode: string, profile: string, report: string) {
  const name = `avm-ready-${profile}-${mode}-${crypto.randomUUID().slice(0, 8)}`;
  let created = false;
  try {
    await init(image, name, mode); created = true;
    await incus(["start", name]);
    // Observe autonomous first-boot key generation before making any SSH request.
    await provision(name, `${header}IMAGE_PROFILE=${profile}\n${sealedBoot}${readiness}\n`);
    await incus(["file", "pull", `${name}/etc/agent-vm-image/readiness.json`, report]);
  } finally { if (created) await remove(name); }
}
const sourceRevision = process.env.AGENT_VM_IMAGE_SOURCE_REVISION ?? Bun.spawnSync(["git", "rev-parse", "HEAD"], { cwd: root }).stdout.toString().trim();
if (!/^[0-9a-f]{40}$/.test(sourceRevision)) fail("build requires a full source revision");
const lockSha = createHash("sha256").update(lockText).digest("hex");
async function buildMode(mode: string) {
  const layerHash = createHash("sha256").update(header + buildNetwork + base + docker + readiness + seal + aptText + aptInstaller + JSON.stringify(packages) + JSON.stringify(runtimeConfig(mode)) + lock.base.fingerprint).digest("hex").slice(0, 20);
  const commonAlias = `${lock.name}-${mode}-common-${layerHash}`;
  const runtimeRecord = `install -d /etc/agent-vm-image\nprintf '%s\\n' '${JSON.stringify({ variant: mode, runtime_config: runtimeConfig(mode) })}' > /etc/agent-vm-image/runtime-config.json\n`;
  if ((await incus(["image", "info", commonAlias], true, true)).code) {
    const name = `avm-build-common-${mode}-${crypto.randomUUID().slice(0, 8)}`;
    let created = false;
    try {
      const local = await incus(["image", "info", lock.base.fingerprint], true, true);
      await init(local.code ? `images:${lock.base.fingerprint}` : lock.base.fingerprint, name, mode); created = true;
      await incus(["start", name]);
      await provision(name, `${header}${buildNetwork}IMAGE_PROFILE=base\napt-get update\ncommand -v python3 >/dev/null || apt-get install -y --no-install-recommends python3=${aptLock.ubuntu!.python3!.version}\npython3 /root/.agent-vm-apt-install.py ubuntu\n${base}\n${docker}\n${runtimeRecord}${readiness}\n${seal}\n`);
      await incus(["stop", name]);
      await incus(["publish", name, "--alias", commonAlias]);
    } finally { if (created) await remove(name); }
  }
  for (const profile of profiles) {
    const name = `avm-build-${profile}-${mode}-${crypto.randomUUID().slice(0, 8)}`;
    const asset = `${lock.name}-${profile}-${mode}-amd64`;
    let created = false;
    let imageFingerprint = "";
    let publishedAlias = false;
    try {
      await init(commonAlias, name, mode); created = true;
      await incus(["start", name]);
      await provision(name, `${header}IMAGE_PROFILE=${profile}\n${sealedBoot}${language}\n${readiness}\n${seal}\n`);
      for (const [guestFile, suffix] of [["readiness.json", "readiness.json"], ["sbom.json", "sbom.json"]]) {
        await incus(["file", "pull", `${name}/etc/agent-vm-image/${guestFile}`, join(output, `${asset}.${suffix}`)]);
      }
      await incus(["stop", name]);
      await incus(["publish", name, "--alias", name, "--format", "unified", "--compression", "gzip"]);
      publishedAlias = true;
      const published = await incus(["query", `/1.0/images/aliases/${name}?project=${encodeURIComponent(project)}`], true);
      imageFingerprint = JSON.parse(published.text).target;
      if (!/^[0-9a-f]{64}$/.test(imageFingerprint)) fail("publish did not return an immutable fingerprint");
      // Incus appends the full archive suffix (.tar.gz) to this prefix.
      await incus(["image", "export", imageFingerprint, join(output, asset)]);
      const tar = join(output, `${asset}.tar.gz`);
      if (!existsSync(tar)) fail("Incus did not export a unified .tar.gz image");
      const portability = Bun.spawn(["python3", join(imageRoot, "scripts/verify_image_archive.py"), tar], { stdout: "inherit", stderr: "inherit" });
      if (await portability.exited) fail("exported image contains non-portable build state");
      await verifyPublished(imageFingerprint, mode, profile, join(output, `${asset}.readiness.json`));
      const manifest = { schema: 1, name: asset, profile, variant: mode, runtime_config: runtimeConfig(mode), architecture: "amd64", source_revision: sourceRevision, lock_sha256: lockSha, image_fingerprint: imageFingerprint, base: lock.base, versions: lock.versions, sha256: await digest(tar) };
      writeFileSync(join(output, `${asset}.manifest.json`), JSON.stringify(manifest, null, 2) + "\n");
      for (const suffix of ["tar.gz", "manifest.json", "sbom.json", "readiness.json"]) { const file = `${asset}.${suffix}`; checksums.push(`${await digest(join(output, file))}  ${file}`); }
    } finally {
      try { if (publishedAlias) await incus(["image", "alias", "delete", name]); }
      finally { if (created) await remove(name); }
    }
  }
}
const outcomes = await Promise.allSettled(variants.map(buildMode));
const failures = outcomes.filter((outcome): outcome is PromiseRejectedResult => outcome.status === "rejected");
if (failures.length) throw new AggregateError(failures.map(outcome => outcome.reason), "image builds failed after all owned workers cleaned up");
writeFileSync(join(output, "agent-vm-ubuntu24-SHA256SUMS"), checksums.sort().join("\n") + "\n");
