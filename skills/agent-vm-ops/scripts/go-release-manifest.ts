/** Format-2 release contract for the Go control service and compatibility client. */

import { createHash } from "node:crypto";
import { lstatSync, readFileSync } from "node:fs";
import { join, posix } from "node:path";

import { ACTION_NAMES, CLIENT_TRANSPORTS, default_operations, type ArtifactRole, type ClientTransport, type OperationSpec } from "./release-manifest.ts";
import { ControlError, canonical_json, type Data } from "./control-types.ts";

export const GO_MANIFEST_FORMAT = 2;
export const GO_TOOLCHAIN = "go1.27.3";
export const GO_TARGETS = Object.freeze(["linux/amd64", "linux/arm64"]);
export { CLIENT_TRANSPORTS };
export type { ClientTransport };
export const RETIRED_UNITS = Object.freeze([
  "agent-vm-native-age-gc.service",
  "agent-vm-native-age-gc.timer",
  "agent-vm-native-expire@.service",
  "agent-vm-native-reconcile.service",
]);

type ArtifactTuple = readonly [string, string, ArtifactRole, number];
export const GO_FIXED_ARTIFACTS: readonly ArtifactTuple[] = Object.freeze([
  ["agent_vm.ts", "agent_vm.ts", "client", 0o755],
  ["byte-fs.ts", "byte-fs.ts", "support", 0o644],
  ["client-io.ts", "client-io.ts", "support", 0o644],
  ["client-release.ts", "client-release.ts", "support", 0o644],
  ["client.ts", "client.ts", "support", 0o644],
  ["guest-source.ts", "guest-source.ts", "support", 0o644],
  ["sync-native.ts", "sync-native.ts", "support", 0o644],
  ["sync_worktree.ts", "sync_worktree.ts", "support", 0o755],
  ["agent_vm_control_bridge.ts", "agent_vm_control_bridge.ts", "control", 0o755],
  ["agent-vm-control", "bin/agent-vm-control", "control", 0o755],
  ["unit:agent-vm-native.service", "systemd/agent-vm-native.service", "unit", 0o644],
]);

export interface GoArtifact {
  name: string;
  path: string;
  role: ArtifactRole;
  sha256: string;
  mode: number;
}

export interface GoReleaseManifest {
  format: 2;
  release_id: string;
  source_revision: string;
  protocol: 4;
  target: { os: "linux"; arch: "amd64" | "arm64" };
  toolchain: { go: typeof GO_TOOLCHAIN; cgo_enabled: false };
  artifacts: GoArtifact[];
  actions: Record<string, OperationSpec>;
  client_view: {
    release_id: string;
    source_revision: string;
    protocol: 4;
    transport: ClientTransport;
    actions: Record<string, OperationSpec>;
  };
  retired_units: string[];
}

export class GoManifestError extends ControlError {}

const error = (message: string, detail?: unknown): never => {
  throw new GoManifestError("manifest_invalid", message, detail);
};

function revision(value: unknown, label: string): string {
  if (typeof value !== "string" || !/^[0-9a-f]{40}$/.test(value)) error(`${label} must be a Git revision`);
  return value as string;
}

function artifact(value: unknown, index: number): GoArtifact {
  if (!value || typeof value !== "object" || Array.isArray(value)) error(`artifacts[${index}] must be an object`);
  const item = value as Data;
  if (Object.keys(item).sort().join(",") !== "mode,name,path,role,sha256") error(`artifacts[${index}] has an unexpected shape`);
  if (typeof item.name !== "string" || !/^[A-Za-z0-9_.:@-]{1,120}$/.test(item.name)) error(`artifacts[${index}].name is invalid`);
  if (typeof item.path !== "string" || item.path.startsWith("/") || item.path.includes("\\") || item.path.split("/").some((part: string) => !part || part === "." || part === "..")) error(`artifacts[${index}].path is unsafe`);
  if (!/^(client|control|support|unit)$/.test(item.role) || typeof item.sha256 !== "string" || !/^[0-9a-f]{64}$/.test(item.sha256) || !Number.isInteger(item.mode) || item.mode < 0 || item.mode > 0o777) error(`artifacts[${index}] is invalid`);
  return { name: item.name, path: item.path, role: item.role, sha256: item.sha256, mode: item.mode };
}

function schema(value: unknown, label: string): { schema: number; semantic_revision: number } {
  if (!value || typeof value !== "object" || Array.isArray(value)) error(`${label} must be an object`);
  const item = value as Data;
  if (Object.keys(item).sort().join(",") !== "schema,semantic_revision") error(`${label} has an unexpected shape`);
  if (!Number.isInteger(item.schema) || item.schema < 1 || !Number.isInteger(item.semantic_revision) || item.semantic_revision < 1) error(`${label} is invalid`);
  return { schema: item.schema as number, semantic_revision: item.semantic_revision as number };
}

function window(value: unknown, label: string): { schema_min: number; schema_max: number; semantic_min: number; semantic_max: number } {
  if (!value || typeof value !== "object" || Array.isArray(value)) error(`${label} must be an object`);
  const item = value as Data;
  if (Object.keys(item).sort().join(",") !== "schema_max,schema_min,semantic_max,semantic_min") error(`${label} has an unexpected shape`);
  if (!["schema_min", "schema_max", "semantic_min", "semantic_max"].every((key) => Number.isInteger(item[key]) && (item[key] as number) >= 1)) error(`${label} is invalid`);
  const result = { schema_min: item.schema_min as number, schema_max: item.schema_max as number, semantic_min: item.semantic_min as number, semantic_max: item.semantic_max as number };
  if (result.schema_min > result.schema_max || result.semantic_min > result.semantic_max) error(`${label} has an inverted compatibility window`);
  return result;
}

function operationMap(value: unknown): Record<string, OperationSpec> {
  if (!value || typeof value !== "object" || Array.isArray(value)) error("actions must be an object");
  const object = value as Record<string, unknown>;
  if (Object.keys(object).sort().join(",") !== [...ACTION_NAMES].sort().join(",")) error("actions differ from the protocol-4 control surface");
  const actions: Record<string, OperationSpec> = {};
  for (const action of ACTION_NAMES) {
    const raw = object[action];
    if (!raw || typeof raw !== "object" || Array.isArray(raw)) error(`actions.${action} must be an object`);
    const item = raw as Data;
    if (Object.keys(item).sort().join(",") !== "client_to_control,control_to_client,request,response") error(`actions.${action} has an unexpected shape`);
    const request = schema(item.request, `actions.${action}.request`);
    const response = schema(item.response, `actions.${action}.response`);
    const clientToControl = window(item.client_to_control, `actions.${action}.client_to_control`);
    const controlToClient = window(item.control_to_client, `actions.${action}.control_to_client`);
    if (request.schema < clientToControl.schema_min || request.schema > clientToControl.schema_max || request.semantic_revision < clientToControl.semantic_min || request.semantic_revision > clientToControl.semantic_max) error(`actions.${action} request is outside its declared window`);
    if (response.schema < controlToClient.schema_min || response.schema > controlToClient.schema_max || response.semantic_revision < controlToClient.semantic_min || response.semantic_revision > controlToClient.semantic_max) error(`actions.${action} response is outside its declared window`);
    actions[action] = { request, response, client_to_control: clientToControl, control_to_client: controlToClient };
  }
  return actions;
}

function target(value: unknown): GoReleaseManifest["target"] {
  if (!value || typeof value !== "object" || Array.isArray(value)) error("target must be an object");
  const item = value as Data;
  if (Object.keys(item).sort().join(",") !== "arch,os" || item.os !== "linux" || (item.arch !== "amd64" && item.arch !== "arm64")) error("target must be linux/amd64 or linux/arm64");
  return { os: "linux", arch: item.arch };
}

function verifyElfTarget(path: string, expected: GoReleaseManifest["target"]): void {
  const bytes = readFileSync(path);
  if (bytes.length < 20 || bytes[0] !== 0x7f || bytes[1] !== 0x45 || bytes[2] !== 0x4c || bytes[3] !== 0x46 || bytes[4] !== 2 || bytes[5] !== 1)
    throw new GoManifestError("binary_format_mismatch", "Go control artifact must be little-endian ELF64");
  const machine = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength).getUint16(18, true);
  const expectedMachine = expected.arch === "amd64" ? 62 : 183;
  if (machine !== expectedMachine)
    throw new GoManifestError("binary_arch_mismatch", "Go control artifact architecture does not match the manifest target");
}

export function build_go_manifest(sourceRevision: string, artifacts: readonly GoArtifact[], options: { target?: GoReleaseManifest["target"]; transport?: GoReleaseManifest["client_view"]["transport"]; operations?: Record<string, OperationSpec> } = {}): GoReleaseManifest {
  const revisionValue = revision(sourceRevision, "source_revision");
  const actions = options.operations ?? default_operations();
  const transport = options.transport ?? "go-request";
  if (!CLIENT_TRANSPORTS.includes(transport)) error("client transport is unsupported");
  if (transport === "legacy-bun") error("native Linux releases require go-request or https transport");
  return validate_go_manifest({
    format: GO_MANIFEST_FORMAT,
    release_id: revisionValue,
    source_revision: revisionValue,
    protocol: 4,
    target: options.target ?? { os: "linux", arch: "amd64" },
    toolchain: { go: GO_TOOLCHAIN, cgo_enabled: false },
    artifacts: artifacts.map((item) => ({ ...item })),
    actions,
    client_view: { release_id: revisionValue, source_revision: revisionValue, protocol: 4, transport, actions },
    retired_units: [...RETIRED_UNITS],
  });
}

export function validate_go_manifest(value: unknown): GoReleaseManifest {
  if (!value || typeof value !== "object" || Array.isArray(value)) error("manifest must be an object");
  const root = value as Data;
  const required = ["actions", "artifacts", "client_view", "format", "protocol", "release_id", "retired_units", "source_revision", "target", "toolchain"];
  if (Object.keys(root).sort().join(",") !== required.sort().join(",")) error("manifest has an unexpected shape");
  if (root.format !== GO_MANIFEST_FORMAT) throw new GoManifestError("manifest_format_unsupported", "manifest format must be 2");
  const sourceRevision = revision(root.source_revision, "source_revision");
  if (root.release_id !== sourceRevision || root.protocol !== 4) error("manifest identity or protocol is invalid");
  const checkedTarget = target(root.target);
  if (!root.toolchain || typeof root.toolchain !== "object" || root.toolchain.go !== GO_TOOLCHAIN || root.toolchain.cgo_enabled !== false) error("Go toolchain declaration is invalid");
  if (!Array.isArray(root.retired_units) || canonical_json(root.retired_units) !== canonical_json(RETIRED_UNITS)) error("retired unit set differs from the fixed transition set");
  if (!Array.isArray(root.artifacts) || root.artifacts.length !== GO_FIXED_ARTIFACTS.length) error("artifact set is incomplete");
  const artifacts = (root.artifacts as unknown[]).map((item, index) => artifact(item, index));
  const actual = artifacts.map((item): ArtifactTuple => [item.name, item.path, item.role, item.mode]).sort((a: ArtifactTuple, b: ArtifactTuple) => canonical_json(a).localeCompare(canonical_json(b)));
  const expected = [...GO_FIXED_ARTIFACTS].sort((a, b) => canonical_json(a).localeCompare(canonical_json(b)));
  if (canonical_json(actual) !== canonical_json(expected)) error("artifact set differs from the fixed Go release allowlist");
  const actions = operationMap(root.actions);
  const view = root.client_view;
  if (!view || typeof view !== "object" || Array.isArray(view) || Object.keys(view).sort().join(",") !== "actions,protocol,release_id,source_revision,transport") error("client_view has an unexpected shape");
  if (view.release_id !== sourceRevision || view.source_revision !== sourceRevision || view.protocol !== 4 || !CLIENT_TRANSPORTS.includes(view.transport)) error("client_view identity or transport is invalid");
  if (canonical_json(view.actions) !== canonical_json(actions)) error("client_view actions differ from manifest actions");
  return { format: 2, release_id: sourceRevision, source_revision: sourceRevision, protocol: 4, target: checkedTarget, toolchain: { go: GO_TOOLCHAIN, cgo_enabled: false }, artifacts, actions, client_view: { release_id: sourceRevision, source_revision: sourceRevision, protocol: 4, transport: view.transport, actions }, retired_units: [...RETIRED_UNITS] };
}

export function verify_go_bundle(root: string, manifest: unknown): GoReleaseManifest {
  const checked = validate_go_manifest(manifest);
  for (const item of checked.artifacts) {
    const path = join(root, ...posix.normalize(item.path).split("/"));
    let info;
    try { info = lstatSync(path); } catch { throw new GoManifestError("artifact_missing", `artifact is missing: ${item.path}`); }
    if (!info.isFile() || info.isSymbolicLink()) throw new GoManifestError("artifact_unverified", `artifact is not a regular file: ${item.path}`);
    if ((info.mode & 0o7777) !== item.mode) throw new GoManifestError("artifact_mode_mismatch", `artifact mode differs: ${item.path}`);
    const digest = createHash("sha256").update(readFileSync(path)).digest("hex");
    if (digest !== item.sha256) throw new GoManifestError("artifact_checksum_mismatch", `artifact checksum differs: ${item.path}`);
    if (item.path === "bin/agent-vm-control") verifyElfTarget(path, checked.target);
  }
  return checked;
}
