import { expect, test } from "bun:test";
import { createHash } from "node:crypto";
import { chmodSync, existsSync, lstatSync, mkdirSync, mkdtempSync, readFileSync, rmSync, statSync, symlinkSync, writeFileSync } from "node:fs";
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
  expect(caddy).toContain("@public path / /index.html /assets/* /api/v1 /api/v1/* /agent/v1/control");
  expect(caddy).toContain("handle @public");
  expect(caddy).toContain("respond 404");
  expect(caddy).not.toContain("auto_https disable_redirects");
  expect(caddy).not.toContain("/etc/agent-vm-native/tls/caddy");
  expect(caddy).not.toContain("forward_auth");
  expect(caddy).not.toContain("edgeone");
  expect(read("docs/deployment/blue-green.md")).toContain("install -o root -g root -m 0600");
  expect(read("docs/deployment/blue-green.md")).toContain("install -o agent-vm-rqlite -g agent-vm-rqlite -m 0600");

  const rqlite = read("skills/agent-vm-ops/assets/systemd/agent-vm-rqlite.service");
  expect(rqlite).toContain("User=agent-vm-rqlite");
  expect(rqlite).toContain("SupplementaryGroups=agent-vm-web");
  expect(rqlite).toContain("-http-addr 127.0.0.1:4001");
  expect(rqlite).toContain("-auth /etc/agent-vm-native/rqlite/auth.json");
  expect(rqlite).toContain("ExecStartPre=/usr/bin/test -f /etc/agent-vm-native/rqlite/auth.json");
  expect(rqlite).toContain("agent-vm-rqlite:600");
  expect(rqlite).toContain("/usr/bin/stat");
  expect(rqlite).not.toContain("0.0.0.0");

  const slot = read("skills/agent-vm-ops/assets/systemd/agent-vm-native@.service");
  expect(slot).toContain("BindReadOnlyPaths=/opt/agent-vm-native/slots/%i/release-manifest.json:/opt/agent-vm-native/release-manifest.json");
  expect(slot).toContain("--slot %i");
  expect(slot).toContain("--socket /run/agent-vm-native/%i/app.sock");
  expect(slot).toContain("--manifest /opt/agent-vm-native/release-manifest.json");
  const caddyUnit = read("skills/agent-vm-ops/assets/systemd/agent-vm-caddy.service");
  expect(caddyUnit).toContain("User=caddy");
  expect(caddyUnit).toContain("After=network-online.target");
  expect(caddyUnit).toContain("ConditionPathExists=/etc/agent-vm-native/caddy.env");
  expect(caddyUnit).toContain("EnvironmentFile=/etc/agent-vm-native/caddy.env");
  expect(caddyUnit).toContain("ExecStartPre=/usr/bin/test -S /var/lib/agent-vm-native/active/app.sock");
  expect(caddyUnit).toContain("ExecStartPre=+/bin/sh -c");
  expect(caddyUnit).toContain('while [ "$$attempt" -lt 30 ]');
  expect(caddyUnit).toContain("attempt=$$(($$attempt + 1))");
  expect(caddyUnit).toContain("--unix-socket /var/lib/agent-vm-native/active/app.sock http://localhost/internal/v1/health");
  expect(caddyUnit).toContain("/usr/bin/stat -c");
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
    const xcaddy = join(binDir, "xcaddy");
    writeFileSync(xcaddy, `#!/bin/sh
set -eu
output=""
previous=""
for argument in "$@"; do
  if [ "$previous" = "--output" ]; then
    output="$argument"
  fi
  previous="$argument"
done
build_root="$(dirname "$(dirname "$output")")"
printf '%s\\n' "$@" > "$build_root/xcaddy.args"
if env | grep -qE '^(DNSPOD_TOKEN|AGENT_VM_DOMAIN)='; then
  printf '%s\\n' 'deployment environment leaked to xcaddy' >&2
  exit 23
fi
if env | grep -qE '^GOPROXY='; then
  printf '%s\\n' 'credential-bearing Go proxy environment leaked to xcaddy' >&2
  exit 24
fi
if [ -f "$build_root/fake-xcaddy-mode" ] && [ "$(cat "$build_root/fake-xcaddy-mode")" = symlink ]; then
  printf '%s\\n' '#!/bin/sh' > "$output.target"
  ln -s "$output.target" "$output"
  exit 0
fi
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
  mode=normal
  if [ -f "${join(temporaryRoot, "fake-go-mode")}" ]; then
    mode="\$(cat "${join(temporaryRoot, "fake-go-mode")}")"
  fi
  case "\$mode" in
    version) caddy_version=v2.10.1; provider_version=v0.0.5-0.20260325061251-fb7cc31cc04c ;;
    provider) caddy_version=v2.10.0; provider_version=v0.0.5-0.20260325061251-000000000000 ;;
    *) caddy_version=v2.10.0; provider_version=v0.0.5-0.20260325061251-fb7cc31cc04c ;;
  esac
  printf '%s\\n' 'path caddy' "mod github.com/caddyserver/caddy/v2 \$caddy_version" "dep github.com/caddy-dns/dnspod \$provider_version"
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
        DNSPOD_TOKEN: "test-secret",
        AGENT_VM_DOMAIN: "test.example",
        GOPROXY: "https://user:pass@proxy.example.invalid",
      },
    });
    expect(built.exitCode).toBe(0);
    expect(statSync(output).mode & 0o111).not.toBe(0);
    const args = readFileSync(join(temporaryRoot, "xcaddy.args"), "utf8").trim().split("\n");
    expect(args.slice(0, 3)).toEqual(["build", "v2.10.0", "--output"]);
    expect(args[3]).toMatch(/\/\.caddy-build-[^/]+\/caddy$/);
    expect(args.slice(4)).toEqual([
      "--with",
      "github.com/caddy-dns/dnspod@fb7cc31cc04c68a304b8d2672c3e5d9f2ad3d7ba",
    ]);
    const buildReceipt = JSON.parse(new TextDecoder().decode(built.stdout));
    expect(buildReceipt.observed_caddy_version).toBe("v2.10.0");
    expect(buildReceipt.observed_provider_version).toContain("-fb7cc31cc04c");
    expect(buildReceipt.sha512).toMatch(/^[0-9a-f]{128}$/);

    const existingOutput = join(temporaryRoot, "existing-caddy");
    writeFileSync(existingOutput, "previous-caddy\n");
    const failedReplacement = run([
      process.execPath,
      resolve(root, "skills/agent-vm-ops/scripts/build_caddy.ts"),
      "--caddy-version",
      "v2.10.0",
      "--target",
      "linux/amd64",
      "--output",
      existingOutput,
      "--expected-sha512",
      "0".repeat(128),
    ], { env: { ...process.env, PATH: `${binDir}:${process.env.PATH ?? ""}` } });
    expect(failedReplacement.exitCode).not.toBe(0);
    expect(readFileSync(existingOutput, "utf8")).toBe("previous-caddy\n");

    const symlinkTarget = join(temporaryRoot, "caddy-target");
    const symlinkOutput = join(temporaryRoot, "symlink-caddy");
    writeFileSync(symlinkTarget, "protected-caddy\n");
    symlinkSync(symlinkTarget, symlinkOutput);
    const rejectedSymlink = run([
      process.execPath,
      resolve(root, "skills/agent-vm-ops/scripts/build_caddy.ts"),
      "--caddy-version",
      "v2.10.0",
      "--target",
      "linux/amd64",
      "--output",
      symlinkOutput,
    ], { env: { ...process.env, PATH: `${binDir}:${process.env.PATH ?? ""}` } });
    expect(rejectedSymlink.exitCode).not.toBe(0);
    expect(lstatSync(symlinkOutput).isSymbolicLink()).toBe(true);
    expect(readFileSync(symlinkTarget, "utf8")).toBe("protected-caddy\n");

    const stagingSymlinkOutput = join(temporaryRoot, "staging-symlink-caddy");
    writeFileSync(join(temporaryRoot, "fake-xcaddy-mode"), "symlink\n");
    const rejectedStagingSymlink = run([
      process.execPath,
      resolve(root, "skills/agent-vm-ops/scripts/build_caddy.ts"),
      "--caddy-version",
      "v2.10.0",
      "--target",
      "linux/amd64",
      "--output",
      stagingSymlinkOutput,
    ], { env: { ...process.env, PATH: `${binDir}:${process.env.PATH ?? ""}` } });
    expect(rejectedStagingSymlink.exitCode).not.toBe(0);
    expect(existsSync(stagingSymlinkOutput)).toBe(false);
    rmSync(join(temporaryRoot, "fake-xcaddy-mode"));

    writeFileSync(join(temporaryRoot, "fake-go-mode"), "version\n");
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
      },
    });
    expect(mismatchedVersion.exitCode).not.toBe(0);

    writeFileSync(join(temporaryRoot, "fake-go-mode"), "provider\n");
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
      },
    });
    expect(rejected.exitCode).not.toBe(0);

    const acceptance = read("skills/agent-vm-ops/scripts/agent_vm_deployment_acceptance.sh");
    expect(acceptance).toContain('rqlite_auth_file="$proof_dir/auth.json"');
    expect(acceptance).toContain('chmod 0600 "$rqlite_auth_file"');
    expect(acceptance).toContain('rm -f -- "$rqlite_auth_file"');
    expect(acceptance).toContain('"$proof_dir/ca/ca.key"');
    expect(acceptance).toContain("Caddy build receipt SHA-512 does not match the installed binary");
    expect(acceptance).toContain("not-allowed");
  const caGenerator = read("skills/agent-vm-ops/scripts/generate_deployment_ca.sh");
  expect(caGenerator).toContain('[[ -L "$output_dir" ]]');
  expect(caGenerator).toContain('"$output_dir/ca.key"');
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

    const symlinkTarget = join(temporaryRoot, "ca-target");
    const symlinkDir = join(temporaryRoot, "ca-link");
    mkdirSync(symlinkTarget);
    symlinkSync(symlinkTarget, symlinkDir);
    const rejectedSymlinkDir = run(["bash", resolve(root, "skills/agent-vm-ops/scripts/generate_deployment_ca.sh"), "--output-dir", symlinkDir, "--hostname", "localhost"]);
    expect(rejectedSymlinkDir.exitCode).not.toBe(0);
    expect(existsSync(join(symlinkTarget, "ca.key"))).toBe(false);

    const opensslPath = new TextDecoder().decode(run(["sh", "-c", "command -v openssl"]).stdout).trim();
    const fakeBin = join(temporaryRoot, "fake-bin");
    mkdirSync(fakeBin);
    writeFileSync(join(fakeBin, "openssl"), `#!/bin/sh
set -eu
if [ "\$1" = req ] && [ "\$2" = -new ]; then
  exit 42
fi
exec "${opensslPath}" "\$@"
`, { mode: 0o755 });
    const failedCaDir = join(temporaryRoot, "failed-ca");
    const failedCa = run(["bash", resolve(root, "skills/agent-vm-ops/scripts/generate_deployment_ca.sh"), "--output-dir", failedCaDir, "--hostname", "localhost"], {
      env: { ...process.env, PATH: `${fakeBin}:${process.env.PATH ?? ""}` },
    });
    expect(failedCa.exitCode).not.toBe(0);
    expect(existsSync(join(failedCaDir, "ca.key"))).toBe(false);
    expect(existsSync(join(failedCaDir, "ca.crt"))).toBe(false);
    expect(existsSync(join(failedCaDir, "rqlite/server.key"))).toBe(false);

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
  const cutover = read("skills/agent-vm-ops/scripts/agent_vm_cutover.sh");
  expect(cutover).toContain("mv -Tf --");
  expect(cutover).toContain('health_check "$active/app.sock"');
  expect(cutover).toContain("candidate failed post-cutover health");
  expect(cutover).toContain("rollback_link");
  expect(cutover).toContain("pending_file");
  expect(cutover).toContain("recover_pending_cutover");
  expect(cutover).toContain("validate_candidate_layout");
  expect(cutover).toContain('! -L "$slot_dir"');
  expect(cutover).toContain('! -L "$slot_dir/release-manifest.json"');
  expect(cutover).toContain('! -L "$root_dir/slots"');
  expect(cutover).toContain('[[ -e "$pending_file" || -L "$pending_file" ]]');
  expect(cutover).toContain('validate_slot_layout "$current_slot"');
  expect(cutover).toContain('validate_slot_layout "$candidate_record"');
  expect(cutover).toContain("sync -f");
  expect(cutover).toContain("candidate_record/app.sock");
  expect(cutover).toContain("read_current_slot");
  expect(cutover).toContain("active_matches_current || die");
  expect(cutover).toContain("active target slot is missing");
  expect(cutover).toContain("slot_manifest_digest");
  expect(cutover).toContain("previous_manifest=");
  expect(cutover).toContain("candidate_manifest=");
  expect(cutover).toContain('assert_manifest_digest "$previous_record" "$previous_manifest_record"');
  expect(cutover).toContain('assert_manifest_digest "$current_slot" "$pending_previous_manifest"');
  expect(cutover).toContain('assert_manifest_digest "$slot" "$pending_candidate_manifest"');
  expect(cutover).toContain('health_check "$root_dir/slots/$previous_record/app.sock"');
  expect(cutover).toContain("flock --wait 30 9");
  expect(cutover).toContain('exec 8<"$root_dir"');
  expect(cutover).toContain("flock --wait 30 8");
  expect(cutover).toContain("mktemp -d -- \"$root_dir/.active-stage.XXXXXX\"");
  expect(cutover).toContain('[[ -L "$lock_path" ]]');
  expect(cutover).toContain('ln -- "$lock_seed" "$lock_path"');
  expect(cutover).toContain("if ! ln --");
  expect(cutover).toContain('mktemp "$stage_dir/pending.XXXXXX"');
  expect(cutover).not.toContain(".active.cutover.pending.$$.tmp");
  expect(cutover).not.toContain(".active.$$.tmp");
  expect(cutover).not.toContain('mkdir -- "$lock_dir"');
  expect(cutover).toContain("Every writer of the protected active link must hold this kernel lock");
  const acceptance = read("skills/agent-vm-ops/scripts/agent_vm_deployment_acceptance.sh");
  expect(acceptance).toContain('"$systemd_root/bin/sh"');
  expect(acceptance).toContain('"$systemd_root/usr/bin/curl"');
  expect(acceptance).toContain('"$systemd_root/usr/bin/sleep"');
  expect(acceptance).toContain('"$systemd_root/usr/bin/stat"');
  expect(acceptance).toContain("env -i");
  expect(acceptance).toContain("stop_process");
  expect(acceptance).toContain("process_alive");
  expect(acceptance).toContain("capture_process_group");
  expect(acceptance).toContain("kill -KILL");
  expect(acceptance).toContain("setsid --wait");
  expect(acceptance).toContain('kill -TERM -- "-$group_id"');
  expect(acceptance).toContain("process_group_for");
  expect(acceptance).toContain('wait "$pid"');
  expect(acceptance).toContain('rm -f -- "$rqlite_auth_file" || cleanup_failed=1');
  expect(acceptance).toContain('rm -f -- "$rqlite_auth_config" || cleanup_failed=1');
  expect(acceptance).toContain("exit 1");
  expect(acceptance).toContain("chown 994:994");
  expect(acceptance).toContain("staged rqlite auth metadata does not satisfy ExecStartPre");
  expect(acceptance).toContain("staged Caddy environment metadata does not satisfy ExecStartPre");
  expect(acceptance).toContain('"$systemd_root/bin/sh" -c');
  expect(acceptance).toContain('"$2" -c "%U:%a"');
  expect(acceptance).toContain('failed post-swap health');
  expect(acceptance).toContain('previous=blue\\ncandidate=green\\nprevious_manifest=%s\\ncandidate_manifest=%s');
  expect(acceptance).toContain('pending recovery did not restore blue');
  expect(acceptance).toContain('candidate whose manifest changed after swap');
  const ci = read(".github/workflows/ci.yml");
  expect(ci).toContain("github.com/caddyserver/xcaddy/cmd/xcaddy@v0.4.4");
  expect(ci).toContain("skills/agent-vm-ops/scripts/agent_vm_deployment_acceptance.sh");
  expect(ci).toContain("repos/caddyserver/caddy/releases");
  expect(ci).toContain("CADDY_VERSION=$caddy_version");
  expect(ci).not.toContain("CADDY_VERSION: v2.");
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

    const symlinkedSlotsLayout = join(temporaryRoot, "symlinked-slots-layout");
    const symlinkedSlots = join(temporaryRoot, "outside-slots");
    mkdirSync(join(symlinkedSlots, "blue"), { recursive: true });
    mkdirSync(symlinkedSlotsLayout, { recursive: true });
    writeFileSync(join(symlinkedSlots, "blue/release-manifest.json"), "{}\n");
    symlinkSync(join(runtime, "blue/app.sock"), join(symlinkedSlots, "blue/app.sock"));
    symlinkSync(symlinkedSlots, join(symlinkedSlotsLayout, "slots"));
    const symlinkedSlotsResult = run(["bash", resolve(root, "skills/agent-vm-ops/scripts/agent_vm_cutover.sh"), "--root", symlinkedSlotsLayout, "--runtime-root", runtime, "--slot", "blue", "--dry-run"]);
    expect(symlinkedSlotsResult.exitCode).not.toBe(0);

    const symlinkedManifestLayout = join(temporaryRoot, "symlinked-manifest-layout");
    mkdirSync(join(symlinkedManifestLayout, "slots/blue"), { recursive: true });
    writeFileSync(join(symlinkedManifestLayout, "slots/blue/manifest-target.json"), "{}\n");
    symlinkSync(join(symlinkedManifestLayout, "slots/blue/manifest-target.json"), join(symlinkedManifestLayout, "slots/blue/release-manifest.json"));
    symlinkSync(join(runtime, "blue/app.sock"), join(symlinkedManifestLayout, "slots/blue/app.sock"));
    const symlinkedManifestResult = run(["bash", resolve(root, "skills/agent-vm-ops/scripts/agent_vm_cutover.sh"), "--root", symlinkedManifestLayout, "--runtime-root", runtime, "--slot", "blue", "--dry-run"]);
    expect(symlinkedManifestResult.exitCode).not.toBe(0);

    const malformedActiveLayout = join(temporaryRoot, "malformed-active-layout");
    const malformedActiveRuntime = join(temporaryRoot, "malformed-active-runtime");
    mkdirSync(join(malformedActiveLayout, "slots/blue"), { recursive: true });
    mkdirSync(join(malformedActiveLayout, "slots/green"), { recursive: true });
    mkdirSync(join(malformedActiveRuntime, "blue"), { recursive: true });
    mkdirSync(join(malformedActiveRuntime, "green"), { recursive: true });
    writeFileSync(join(malformedActiveLayout, "slots/green/release-manifest.json"), "{}\n");
    symlinkSync(join(malformedActiveRuntime, "green/app.sock"), join(malformedActiveLayout, "slots/green/app.sock"));
    symlinkSync(join(malformedActiveRuntime, "blue/app.sock"), join(malformedActiveLayout, "slots/blue/app.sock"));
    symlinkSync(join(malformedActiveLayout, "slots/blue"), join(malformedActiveLayout, "active"));
    const malformedActiveResult = run(["bash", resolve(root, "skills/agent-vm-ops/scripts/agent_vm_cutover.sh"), "--root", malformedActiveLayout, "--runtime-root", malformedActiveRuntime, "--slot", "green", "--dry-run"]);
    expect(malformedActiveResult.exitCode).not.toBe(0);
  } finally {
    rmSync(temporaryRoot, { recursive: true, force: true });
  }
});
