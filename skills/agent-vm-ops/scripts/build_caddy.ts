#!/usr/bin/env bun

import { createHash } from "node:crypto";
import { existsSync, mkdirSync, readFileSync, statSync } from "node:fs";
import { dirname, join, resolve } from "node:path";

const repositoryRoot = resolve(import.meta.dir, "../../..");
const metadataPath = join(repositoryRoot, "skills/agent-vm-ops/assets/blue-green/deployment-components.json");

type CaddyMetadata = {
  version_major: number;
  version_policy: string;
  builder: string;
  provider_module: string;
  provider_ref: string;
  provider_module_name: string;
};

type BuildOptions = {
  caddyVersion: string;
  target: "linux/amd64" | "linux/arm64";
  output: string;
  expectedSha512?: string;
};

const buildEnvironmentKeys = [
  "PATH",
  "HOME",
  "USER",
  "LOGNAME",
  "TMPDIR",
  "TMP",
  "TEMP",
  "GOCACHE",
  "GOMODCACHE",
  "GOPATH",
  "GOROOT",
  "GOTOOLCHAIN",
  "GOFLAGS",
  "GOSUMDB",
  "GONOSUMDB",
  "GONOPROXY",
  "GOPRIVATE",
  "CGO_ENABLED",
  "CC",
  "CXX",
  "PKG_CONFIG",
  "SOURCE_DATE_EPOCH",
  "XDG_CACHE_HOME",
  "XDG_CONFIG_HOME",
  "XDG_DATA_HOME",
  "LANG",
  "LC_ALL",
  "LC_CTYPE",
] as const;

function usage(): string {
  return [
    "usage: bun skills/agent-vm-ops/scripts/build_caddy.ts \\",
    "  --caddy-version <v2.x.y> \\",
    "  --target <linux/amd64|linux/arm64> \\",
    "  --output <absolute-path> \\",
    "  [--expected-sha512 <128-character-hex-digest>]",
  ].join("\n");
}

function fail(message: string): never {
  throw new Error(message);
}

function parseArgs(args: string[]): BuildOptions {
  let caddyVersion = "";
  let target: BuildOptions["target"] | "" = "";
  let output = "";
  let expectedSha512 = "";

  for (let index = 0; index < args.length; index += 1) {
    const flag = args[index];
    const value = args[index + 1];
    if (flag === "--help" || flag === "-h") {
      console.log(usage());
      process.exit(0);
    }
    if (flag === "--caddy-version") {
      caddyVersion = value ?? "";
      index += 1;
    } else if (flag === "--target") {
      target = (value ?? "") as BuildOptions["target"];
      index += 1;
    } else if (flag === "--output") {
      output = value ?? "";
      index += 1;
    } else if (flag === "--expected-sha512") {
      expectedSha512 = value ?? "";
      index += 1;
    } else {
      fail(`unknown argument: ${flag}\n${usage()}`);
    }
  }

  if (!caddyVersion) fail(`--caddy-version is required\n${usage()}`);
  if (!/^v?2\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?$/.test(caddyVersion)) {
    fail("--caddy-version must be an explicit Caddy v2 release (for example, v2.x.y)");
  }
  if (target !== "linux/amd64" && target !== "linux/arm64") {
    fail("--target must be linux/amd64 or linux/arm64");
  }
  if (!output || !output.startsWith("/")) fail("--output must be an absolute path");
  if (expectedSha512 && !/^[0-9a-f]{128}$/.test(expectedSha512)) {
    fail("--expected-sha512 must be a 128-character lowercase SHA-512 digest");
  }

  return {
    caddyVersion: caddyVersion.startsWith("v") ? caddyVersion : `v${caddyVersion}`,
    target,
    output,
    expectedSha512: expectedSha512 || undefined,
  };
}

function readCaddyMetadata(): CaddyMetadata {
  let metadata: any;
  try {
    metadata = JSON.parse(readFileSync(metadataPath, "utf8"));
  } catch (error) {
    fail(`cannot read Caddy metadata: ${error instanceof Error ? error.message : String(error)}`);
  }

  const caddy = metadata?.caddy;
  if (
    caddy?.version_major !== 2 ||
    caddy?.version_policy !== "operator-selected-v2-release" ||
    caddy?.builder !== "xcaddy" ||
    typeof caddy?.provider_module !== "string" ||
    caddy?.provider_module_name !== "dns.providers.dnspod" ||
    !/^[0-9a-f]{40}$/.test(caddy?.provider_ref ?? "")
  ) {
    fail("Caddy metadata does not describe the supported xcaddy DNSPod build");
  }
  return caddy as CaddyMetadata;
}

function moduleVersion(buildInfo: string, modulePath: string): string {
  const escapedModulePath = modulePath.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const match = buildInfo.match(new RegExp(`^\\s*dep\\s+${escapedModulePath}\\s+(\\S+)`, "m"));
  return match?.[1] ?? "";
}

function buildInfoValue(buildInfo: string, key: string): string {
  const escapedKey = key.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const match = buildInfo.match(new RegExp(`^\\s*build\\s+${escapedKey}=(\\S+)`, "m"));
  return match?.[1] ?? "";
}

function buildEnvironment(environment: Record<string, string | undefined>): Record<string, string> {
  const sanitized: Record<string, string> = {};
  for (const key of buildEnvironmentKeys) {
    const value = environment[key];
    if (typeof value === "string") sanitized[key] = value;
  }
  return sanitized;
}

export function commandFor(options: BuildOptions, metadata: CaddyMetadata): string[] {
  return [
    "build",
    options.caddyVersion,
    "--output",
    options.output,
    "--with",
    `${metadata.provider_module}@${metadata.provider_ref}`,
  ];
}

export function buildCaddy(args: string[], environment: Record<string, string | undefined> = process.env): number {
  const options = parseArgs(args);
  const metadata = readCaddyMetadata();
  const sanitizedEnvironment = buildEnvironment(environment);
  mkdirSync(dirname(options.output), { recursive: true });

  let result: ReturnType<typeof Bun.spawnSync>;
  try {
    result = Bun.spawnSync([metadata.builder, ...commandFor(options, metadata)], {
      env: {
        ...sanitizedEnvironment,
        GOOS: "linux",
        GOARCH: options.target === "linux/amd64" ? "amd64" : "arm64",
      },
      stdout: "inherit",
      stderr: "inherit",
    });
  } catch (error) {
    fail(`failed to start ${metadata.builder}: ${error instanceof Error ? error.message : String(error)}`);
  }

  if (result.exitCode !== 0) fail(`${metadata.builder} exited with status ${result.exitCode}`);
  if (!existsSync(options.output)) fail(`Caddy build did not create ${options.output}`);
  const mode = statSync(options.output).mode & 0o777;
  if ((mode & 0o111) === 0) fail(`Caddy build output is not executable: ${options.output}`);
  // `go version -m` inspects Go build metadata without executing the target
  // binary, so cross-compiled linux/amd64 and linux/arm64 outputs are both
  // verifiable from the operator's build host.
  const buildInfo = Bun.spawnSync(["go", "version", "-m", options.output], {
    env: sanitizedEnvironment,
    stdout: "pipe",
    stderr: "pipe",
  });
  const buildInfoOutput = new TextDecoder().decode(buildInfo.stdout);
  const observedCaddyVersion = moduleVersion(buildInfoOutput, "github.com/caddyserver/caddy/v2");
  const observedProviderVersion = moduleVersion(buildInfoOutput, metadata.provider_module);
  if (buildInfo.exitCode !== 0 || !observedCaddyVersion.startsWith("v2.")) {
    fail(`Caddy build does not contain the Caddy v2 module metadata: ${buildInfoOutput.trim() || "no build metadata"}`);
  }
  if (observedCaddyVersion !== options.caddyVersion) {
    fail(`Caddy build version ${observedCaddyVersion || "<missing>"} does not match requested ${options.caddyVersion}`);
  }
  if (!observedProviderVersion) {
    fail(`Caddy build does not contain ${metadata.provider_module}: ${buildInfoOutput.trim() || "no build metadata"}`);
  }
  const providerRefSuffix = metadata.provider_ref.slice(0, 12);
  if (!observedProviderVersion.endsWith(`-${providerRefSuffix}`)) {
    fail(`Caddy build DNSPod module ${observedProviderVersion} does not resolve to pinned ref ${metadata.provider_ref}`);
  }
  const sha512 = createHash("sha512").update(readFileSync(options.output)).digest("hex");
  const buildVcsRevision = buildInfoValue(buildInfoOutput, "vcs.revision");
  const buildVcsModified = buildInfoValue(buildInfoOutput, "vcs.modified");
  if (options.expectedSha512 && sha512 !== options.expectedSha512) {
    fail(`Caddy SHA-512 ${sha512} does not match expected ${options.expectedSha512}`);
  }
  console.log(JSON.stringify({
    schema_version: 1,
    caddy_version: options.caddyVersion,
    requested_version: options.caddyVersion,
    observed_caddy_version: observedCaddyVersion,
    provider_module: metadata.provider_module,
    provider_module_name: metadata.provider_module_name,
    provider_ref: metadata.provider_ref,
    observed_provider_version: observedProviderVersion,
    sha512,
    build_vcs_revision: buildVcsRevision || null,
    build_vcs_modified: buildVcsModified || null,
    ...(options.expectedSha512 ? { expected_sha512: options.expectedSha512 } : {}),
    target: options.target,
    output: options.output,
    verification: "go-version-m",
  }));
  return result.exitCode;
}

if (import.meta.main) {
  try {
    buildCaddy(process.argv.slice(2));
  } catch (error) {
    console.error(`Caddy build failed: ${error instanceof Error ? error.message : String(error)}`);
    process.exitCode = 1;
  }
}
