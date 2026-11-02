import { expect, test } from "bun:test";
import { createHash } from "node:crypto";
import { chmodSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, statSync, symlinkSync, writeFileSync } from "node:fs";
import { join, resolve } from "node:path";
import { tmpdir } from "node:os";

const root = resolve(import.meta.dir, "../../..");
const read = (path: string) => readFileSync(resolve(root, path), "utf8");
const json = (path: string) => JSON.parse(read(path));
const run = (command: string[], options: Record<string, unknown> = {}) => Bun.spawnSync(command, {
  cwd: root,
  stdout: "pipe",
  stderr: "pipe",
  ...options,
});

const canonicalJson = (value: unknown): string => {
  if (value === null) return "null";
  if (typeof value === "string" || typeof value === "boolean") return JSON.stringify(value);
  if (typeof value === "number") return JSON.stringify(value);
  if (Array.isArray(value)) return `[${value.map(canonicalJson).join(",")}]`;
  if (typeof value === "object") {
    const object = value as Record<string, unknown>;
    return `{${Object.keys(object).sort().map((key) => `${JSON.stringify(key)}:${canonicalJson(object[key])}`).join(",")}}`;
  }
  throw new Error("unsupported canonical JSON value");
};

function createEvidenceSigner(directory: string): { privateKey: string; publicKey: string } {
  const privateKey = join(directory, "evidence-private.pem");
  const publicKey = join(directory, "evidence-public.pem");
  expect(run(["openssl", "genpkey", "-algorithm", "RSA", "-pkeyopt", "rsa_keygen_bits:2048", "-out", privateKey]).exitCode).toBe(0);
  expect(run(["openssl", "pkey", "-in", privateKey, "-pubout", "-out", publicKey]).exitCode).toBe(0);
  chmodSync(privateKey, 0o600);
  chmodSync(publicKey, 0o600);
  return { privateKey, publicKey };
}

function signedEvidence(path: string, payload: Record<string, unknown>, privateKey: string): void {
  const canonicalPath = `${path}.canonical`;
  const signaturePath = `${path}.sig`;
  writeFileSync(canonicalPath, canonicalJson(payload), { mode: 0o600 });
  const result = run(["openssl", "dgst", "-sha256", "-sign", privateKey, "-out", signaturePath, canonicalPath]);
  expect(result.exitCode).toBe(0);
  writeFileSync(path, JSON.stringify({ payload, signature: readFileSync(signaturePath).toString("base64") }), { mode: 0o600 });
}

function evidencePayload(source: string, evidenceId: string, overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    schema_version: 1,
    producer: "agent-vm-control",
    evidence_id: evidenceId,
    state_write_fenced: true,
    workers_quiescent: true,
    sqlite_writers_closed: true,
    release_fence_path: "/run/agent-vm-native/release-activation.fence",
    fence_generation: 1,
    state_epoch: "epoch-1",
    state_generation: "6728b2001bc842af236929770c506c5dca42faca834859e693d7b9ff7d969cf6",
    trusted_commit: "commit-1",
    observed_at: new Date(Date.now() - 1000).toISOString(),
    expires_at: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
    source_sha256: createHash("sha256").update(readFileSync(source)).digest("hex"),
    source_size: statSync(source).size,
    ...overrides,
  };
}

test("blue-green deployment metadata and static validator enforce the Caddy v2 DNSPod boundary", () => {
  const metadata = json("skills/agent-vm-ops/assets/blue-green/deployment-components.json");
  expect(metadata.schema_version).toBe(2);
  expect(metadata.caddy.version_major).toBe(2);
  expect(metadata.caddy.version_policy).toBe("operator-selected-v2-release");
  expect(metadata.caddy.builder).toBe("xcaddy");
  expect(metadata.caddy.provider_module).toBe("github.com/caddy-dns/dnspod");
  expect(metadata.caddy.provider_module_name).toBe("dns.providers.dnspod");
  expect(metadata.caddy.provider_ref).toMatch(/^[0-9a-f]{40}$/);
  expect(metadata.caddy.version).toBeUndefined();
  expect(metadata.rqlite.version).toBe("10.5.2");
  expect(metadata.rqlite.supported_architectures).toEqual(["amd64", "arm64"]);
  expect(metadata.policy.automatic_install).toBe(false);
  expect(metadata.caddy.artifacts).toBeUndefined();
  expect(metadata.rqlite.artifacts).toHaveLength(2);
  expect(metadata.rqlite.artifacts[1].arch).toBe("arm64");
  expect(metadata.rqlite.artifacts[1].sha256).toHaveLength(64);
  const releaseComponents = json(".github/release-contract.json").deployment_artifacts.components;
  expect(releaseComponents).toHaveLength(2);
  expect(releaseComponents.find((component: any) => component.name === "caddy")).toMatchObject({
    version_major: 2,
    version_policy: "operator-selected-v2-release",
    builder: "xcaddy",
    provider_module: "github.com/caddy-dns/dnspod",
    provider_module_name: "dns.providers.dnspod",
  });

  const result = run([process.execPath, resolve(root, "skills/agent-vm-ops/scripts/validate_blue_green_deployment.ts")]);
  expect(result.exitCode).toBe(0);
  const output = JSON.parse(new TextDecoder().decode(result.stdout));
  expect(output.verdict).toBe("pass");
  expect(output.isolated_runtime_required).toBe(true);
});

test("deployment examples keep the edge and state boundaries fail-closed", () => {
  const caddy = read("skills/agent-vm-ops/assets/blue-green/Caddyfile.example");
  expect(caddy).toContain("admin off");
  expect(caddy).toContain("auto_https off");
  expect(caddy).toContain("storage file_system /var/lib/agent-vm-native/caddy");
  expect(caddy).toContain("dns dnspod {env.DNSPOD_TOKEN}");
  expect(caddy).toContain("force_automate");
  expect(caddy).toContain("\tlog default");
  expect(caddy).toContain("reverse_proxy unix//var/lib/agent-vm-native/active/app.sock");
  expect(caddy).toContain("header_up -X-Forwarded-User");
  expect(caddy).not.toContain("header_up X-Forwarded-User");
  expect(caddy).toContain("request>uri regexp");
  expect(caddy).toContain("request>headers>X-Csrf-Token delete");
  expect(caddy).not.toContain("tls_insecure_skip_verify");
  expect(caddy).toContain("@internal path /internal/v1/*");
  expect(caddy).toContain("respond 404");
  expect(caddy).not.toContain("auto_https disable_redirects");
  expect(caddy).not.toContain("/etc/agent-vm-native/tls/caddy");
  expect(caddy).not.toContain("forward_auth");
  expect(caddy).not.toContain("edgeone");
  expect(read("docs/deployment/blue-green.md")).toContain("install -o root -g root -m 0600");

  const rqlite = read("skills/agent-vm-ops/assets/systemd/agent-vm-rqlite.service");
  expect(rqlite).toContain("User=agent-vm-rqlite");
  expect(rqlite).toContain("SupplementaryGroups=agent-vm-web");
  expect(rqlite).toContain("-http-addr 127.0.0.1:4001");
  expect(rqlite).toContain("-auth /etc/agent-vm-native/rqlite/auth.json");
  expect(rqlite).not.toContain("0.0.0.0");

  const slot = read("skills/agent-vm-ops/assets/systemd/agent-vm-native@.service");
  expect(slot).toContain("BindReadOnlyPaths=/opt/agent-vm-native/slots/%i/release-manifest.json:/opt/agent-vm-native/release-manifest.json");
  expect(slot).toContain("--manifest /opt/agent-vm-native/release-manifest.json");
  const caddyUnit = read("skills/agent-vm-ops/assets/systemd/agent-vm-caddy.service");
  expect(caddyUnit).toContain("User=caddy");
  expect(caddyUnit).toContain("After=network-online.target");
  expect(caddyUnit).toContain("ConditionPathExists=/etc/agent-vm-native/caddy.env");
  expect(caddyUnit).toContain("EnvironmentFile=/etc/agent-vm-native/caddy.env");
  expect(caddyUnit).toContain("ExecStartPre=/usr/bin/test -S /var/lib/agent-vm-native/active/app.sock");
  expect(caddyUnit).toContain("stat -c");
  expect(caddyUnit).toContain("0:0:600");
  expect(caddyUnit).not.toContain("--environ");
  expect(caddyUnit).toContain("ReadWritePaths=/var/lib/agent-vm-native/caddy");
  expect(caddyUnit).not.toContain("/etc/agent-vm-native/tls/caddy");
  const sysusers = read("skills/agent-vm-ops/assets/sysusers.d/agent-vm-native.conf");
  expect(sysusers).toContain("u caddy");
  expect(read("skills/agent-vm-ops/assets/tmpfiles.d/agent-vm-native.conf")).toContain("z /etc/agent-vm-native/caddy.env 0600 root root");
});

test("Caddy build integration requires an explicit v2 release and embeds DNSPod", () => {
  const temporaryRoot = mkdtempSync(join(tmpdir(), "agent-vm-caddy-build-test-"));
  try {
    const binDir = join(temporaryRoot, "bin");
    mkdirSync(binDir, { recursive: true });
    const argsPath = join(temporaryRoot, "xcaddy.args");
    const xcaddy = join(binDir, "xcaddy");
    writeFileSync(xcaddy, `#!/bin/sh
set -eu
printf '%s\\n' "$@" > "$XCADDY_ARGS_FILE"
output=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--output" ]; then
    shift
    output="$1"
  fi
  shift
done
cat > "$output" <<'EOF'
#!/bin/sh
case "\${1:-}" in
  list-modules) printf '%s\\n' 'dns.providers.dnspod' ;;
  version) printf '%s\\n' 'v2.99.1' ;;
  *) exit 1 ;;
esac
EOF
chmod 0755 "$output"
`, { mode: 0o755 });
    const go = join(binDir, "go");
    writeFileSync(go, `#!/bin/sh
set -eu
if [ "\$1" = "version" ] && [ "\$2" = "-m" ]; then
  printf '%s\\n' 'path caddy' "dep github.com/caddyserver/caddy/v2 \${CADDY_BUILD_VERSION:-v2.10.0}" "dep github.com/caddy-dns/dnspod \${DNSPOD_BUILD_VERSION:-v0.0.5-0.20260325061251-fb7cc31cc04c}"
  exit 0
fi
exit 1
`, { mode: 0o755 });
    const output = join(temporaryRoot, "caddy");
    const built = run([
      process.execPath,
      resolve(root, "skills/agent-vm-ops/scripts/build_caddy.ts"),
      "--caddy-version",
      "v2.10.0",
      "--target",
      "linux/amd64",
      "--output",
      output,
    ], {
      env: {
        ...process.env,
        PATH: `${binDir}:${process.env.PATH ?? ""}`,
        XCADDY_ARGS_FILE: argsPath,
      },
    });
    expect(built.exitCode).toBe(0);
    expect(statSync(output).mode & 0o111).not.toBe(0);
    const args = readFileSync(argsPath, "utf8").trim().split("\n");
    expect(args).toEqual([
      "build",
      "v2.10.0",
      "--output",
      output,
      "--with",
      "github.com/caddy-dns/dnspod@fb7cc31cc04c68a304b8d2672c3e5d9f2ad3d7ba",
    ]);
    const buildReceipt = JSON.parse(new TextDecoder().decode(built.stdout));
    expect(buildReceipt.observed_caddy_version).toBe("v2.10.0");
    expect(buildReceipt.observed_provider_version).toContain("-fb7cc31cc04c");
    expect(buildReceipt.sha512).toMatch(/^[0-9a-f]{128}$/);

    const mismatchedVersion = run([
      process.execPath,
      resolve(root, "skills/agent-vm-ops/scripts/build_caddy.ts"),
      "--caddy-version",
      "v2.10.0",
      "--target",
      "linux/amd64",
      "--output",
      join(temporaryRoot, "mismatched-version-caddy"),
    ], {
      env: {
        ...process.env,
        PATH: `${binDir}:${process.env.PATH ?? ""}`,
        XCADDY_ARGS_FILE: argsPath,
        CADDY_BUILD_VERSION: "v2.10.1",
      },
    });
    expect(mismatchedVersion.exitCode).not.toBe(0);

    const mismatchedProvider = run([
      process.execPath,
      resolve(root, "skills/agent-vm-ops/scripts/build_caddy.ts"),
      "--caddy-version",
      "v2.10.0",
      "--target",
      "linux/amd64",
      "--output",
      join(temporaryRoot, "mismatched-provider-caddy"),
    ], {
      env: {
        ...process.env,
        PATH: `${binDir}:${process.env.PATH ?? ""}`,
        XCADDY_ARGS_FILE: argsPath,
        DNSPOD_BUILD_VERSION: "v0.0.5-0.20260325061251-000000000000",
      },
    });
    expect(mismatchedProvider.exitCode).not.toBe(0);

    const rejected = run([
      process.execPath,
      resolve(root, "skills/agent-vm-ops/scripts/build_caddy.ts"),
      "--caddy-version",
      "latest",
      "--target",
      "linux/amd64",
      "--output",
      join(temporaryRoot, "rejected-caddy"),
    ], {
      env: {
        ...process.env,
        PATH: `${binDir}:${process.env.PATH ?? ""}`,
        XCADDY_ARGS_FILE: argsPath,
      },
    });
    expect(rejected.exitCode).not.toBe(0);
  } finally {
    rmSync(temporaryRoot, { recursive: true, force: true });
  }
});

test("CA generation and state preflight require operator evidence", () => {
  const temporaryRoot = mkdtempSync(join(tmpdir(), "agent-vm-blue-green-test-"));
  try {
    const caDir = join(temporaryRoot, "ca");
    const ca = run(["bash", resolve(root, "skills/agent-vm-ops/scripts/generate_deployment_ca.sh"), "--output-dir", caDir, "--hostname", "localhost"]);
    expect(ca.exitCode).toBe(0);
    expect(existsSync(join(caDir, "ca.crt"))).toBe(true);
    expect(statSync(join(caDir, "ca.key")).mode & 0o777).toBe(0o600);
    expect(existsSync(join(caDir, "caddy/server.key"))).toBe(false);

    const source = join(temporaryRoot, "state.sqlite3");
    const database = run(["python3", "-c", "import sqlite3,sys; c=sqlite3.connect(sys.argv[1]); c.execute(\"CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)\"); c.execute(\"CREATE TABLE tasks (session_id TEXT NOT NULL, generation INTEGER NOT NULL)\"); c.execute(\"INSERT INTO meta VALUES ('state_epoch','epoch-1')\"); c.execute(\"INSERT INTO tasks VALUES ('session-1',7)\"); c.commit(); c.close()", source]);
    expect(database.exitCode).toBe(0);
    const signer = createEvidenceSigner(temporaryRoot);
    const invalidEvidence = join(temporaryRoot, "invalid.json");
    signedEvidence(invalidEvidence, evidencePayload(source, "invalid-0001", { workers_quiescent: false }), signer.privateKey);
    const rejected = run([process.execPath, resolve(root, "skills/agent-vm-ops/scripts/agent_vm_state_migration.ts"), "preflight", "--source", source, "--evidence", invalidEvidence, "--evidence-public-key", signer.publicKey, "--output-dir", join(temporaryRoot, "rejected")]);
    expect(rejected.exitCode).not.toBe(0);

    const evidence = join(temporaryRoot, "evidence.json");
    signedEvidence(evidence, evidencePayload(source, "valid-0001"), signer.privateKey);
    const tamperedEvidence = join(temporaryRoot, "tampered-evidence.json");
    const tampered = JSON.parse(readFileSync(evidence, "utf8"));
    tampered.payload.trusted_commit = "forged-commit";
    writeFileSync(tamperedEvidence, JSON.stringify(tampered), { mode: 0o600 });
    const tamperedResult = run([process.execPath, resolve(root, "skills/agent-vm-ops/scripts/agent_vm_state_migration.ts"), "preflight", "--source", source, "--evidence", tamperedEvidence, "--evidence-public-key", signer.publicKey, "--output-dir", join(temporaryRoot, "tampered-output")]);
    expect(tamperedResult.exitCode).not.toBe(0);
    const migrationDir = join(temporaryRoot, "migration");
    const preflight = run([process.execPath, resolve(root, "skills/agent-vm-ops/scripts/agent_vm_state_migration.ts"), "preflight", "--source", source, "--evidence", evidence, "--evidence-public-key", signer.publicKey, "--output-dir", migrationDir]);
    expect(preflight.exitCode).toBe(0);
    expect(existsSync(join(migrationDir, "state.sqlite3"))).toBe(true);
    const manifest = JSON.parse(readFileSync(join(migrationDir, "snapshot-manifest.json"), "utf8"));
    expect(manifest.fence.sqlite_writers_closed).toBe(true);
    expect(manifest.state_epoch).toBe("epoch-1");

    const blockedSource = join(temporaryRoot, "blocked.sqlite3");
    const blockedDatabase = run(["python3", "-c", "import sqlite3,sys; c=sqlite3.connect(sys.argv[1]); c.executescript(\"CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL); INSERT INTO meta VALUES ('state_epoch','epoch-1'); CREATE TABLE tasks (session_id TEXT NOT NULL, generation INTEGER NOT NULL); INSERT INTO tasks VALUES ('session-1',7); CREATE TABLE operations (operation_id TEXT, response_json TEXT); INSERT INTO operations VALUES ('lifecycle-pending', '{\\\"outcome\\\":\\\"pending\\\"}'); CREATE TABLE management_operations (operation_id TEXT, status TEXT); INSERT INTO management_operations VALUES ('blocked-upload','awaiting_upload');\"); c.commit(); c.close()", blockedSource]);
    expect(blockedDatabase.exitCode).toBe(0);
    const blockedEvidence = join(temporaryRoot, "blocked-evidence.json");
    signedEvidence(blockedEvidence, evidencePayload(blockedSource, "blocked-0001"), signer.privateKey);
    const blocked = run([process.execPath, resolve(root, "skills/agent-vm-ops/scripts/agent_vm_state_migration.ts"), "preflight", "--source", blockedSource, "--evidence", blockedEvidence, "--evidence-public-key", signer.publicKey, "--output-dir", join(temporaryRoot, "blocked-output")]);
    expect(blocked.exitCode).not.toBe(0);
  } finally {
    rmSync(temporaryRoot, { recursive: true, force: true });
  }
});

test("cutover dry-run accepts only a verified blue or green layout", () => {
  expect(read("skills/agent-vm-ops/scripts/agent_vm_cutover.sh")).toContain("mv -Tf --");
  const temporaryRoot = mkdtempSync(join(tmpdir(), "agent-vm-cutover-test-"));
  try {
    const layout = join(temporaryRoot, "layout");
    const runtime = join(temporaryRoot, "runtime");
    mkdirSync(join(layout, "slots/blue"), { recursive: true });
    mkdirSync(join(runtime, "blue"), { recursive: true });
    writeFileSync(join(layout, "slots/blue/release-manifest.json"), "{}\n");
    symlinkSync(join(runtime, "blue/app.sock"), join(layout, "slots/blue/app.sock"));
    const accepted = run(["bash", resolve(root, "skills/agent-vm-ops/scripts/agent_vm_cutover.sh"), "--root", layout, "--runtime-root", runtime, "--slot", "blue", "--dry-run"]);
    expect(accepted.exitCode).toBe(0);
    expect(JSON.parse(new TextDecoder().decode(accepted.stdout)).candidate).toBe("blue");
    const rejected = run(["bash", resolve(root, "skills/agent-vm-ops/scripts/agent_vm_cutover.sh"), "--root", layout, "--runtime-root", runtime, "--slot", "red", "--dry-run"]);
    expect(rejected.exitCode).not.toBe(0);
  } finally {
    rmSync(temporaryRoot, { recursive: true, force: true });
  }
});
