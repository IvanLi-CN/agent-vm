import { lstatSync } from "node:fs";
import { dirname, isAbsolute, join } from "node:path";
import { Data, fail, record, integer, readJson, fileHash } from "./client-io.ts";

export const PROTOCOL = 4;
export const VERSION = "dev";
export const PRODUCT_FORMAT = 4;
export const CLIENT_PRODUCT_FORMAT = 3;
export const ACTIONS = ["acquire", "admin-gc", "admin-metrics", "admin-reconcile", "admin-reconcile-session", "admin-reconcile-history", "admin-session-list", "admin-status", "destroy", "doctor", "drain-enter", "drain-exit", "drain-status", "port-add", "port-list", "port-remove", "release", "renew", "resize", "restart", "ssh-info", "status", "image-list", "timer-age-gc", "timer-expire", "timer-reconcile"];
// @agent-vm-build-identity: replaced only in staged release artifacts.
export const BUILD_IDENTITY: { version: string; source_revision: string | null } = { version: "dev", source_revision: null };
export function runtimeActions(): Record<string, Data> {
  return Object.fromEntries(ACTIONS.map(action => [action, {
    request: { schema: 1, semantic_revision: 1 }, response: { schema: 1, semantic_revision: 1 },
    client_to_control: { schema_min: 1, schema_max: 1, semantic_min: 1, semantic_max: action === "acquire" ? 2 : 1 },
    control_to_client: { schema_min: 1, schema_max: 1, semantic_min: 1, semantic_max: 1 },
  }]));
}
export function runtimeRelease(): Data {
  return { version: BUILD_IDENTITY.version, source_revision: BUILD_IDENTITY.source_revision, release_id: BUILD_IDENTITY.source_revision ?? "legacy-protocol-4", protocol: PROTOCOL, manifest_present: false, artifacts_verified: false, transport: "go-request", actions: runtimeActions() };
}
export function assertInstallationReady(path: string): void {
  for (const directory of new Set([dirname(path), import.meta.dir])) {
    try { lstatSync(join(directory, ".agent-vm-client-install.json")); }
    catch (e: any) { if (e.code === "ENOENT") continue; throw e; }
    fail("local_client_install_pending", "complete-package installation or recovery is pending; retain its recovery material");
  }
}
export const CLIENT_DEPENDENCIES: ReadonlyArray<readonly [string, number]> = [["byte-fs.ts", 0o644], ["client-io.ts", 0o644], ["client-release.ts", 0o644], ["client.ts", 0o644], ["guest-source.ts", 0o644], ["host-config.ts", 0o644], ["sync-native.ts", 0o644], ["sync_worktree.ts", 0o755]];
export const warning = (code: string, message: string, action?: string): Data => ({ code, message, ...(action === undefined ? {} : { action }) });
export const shape = (v: any, keys: string[]): boolean => record(v) && Object.keys(v).sort().join(",") === [...keys].sort().join(",");
function schema(v: any): Data {
  if (!shape(v, ["schema", "semantic_revision"]) || !integer(v.schema) || v.schema < 1 || !integer(v.semantic_revision) || v.semantic_revision < 1) fail("release_metadata_invalid", "invalid schema and semantic revision");
  return v;
}
export function window(v: any): Data {
  if (!shape(v, ["schema_min", "schema_max", "semantic_min", "semantic_max"]) || Object.values(v).some(n => !integer(n) || (n as number) < 1) || v.schema_min > v.schema_max || v.semantic_min > v.semantic_max) fail("release_metadata_invalid", "invalid schema and semantic window");
  return v;
}
const within = (s: Data, w: Data): boolean => s.schema >= w.schema_min && s.schema <= w.schema_max && s.semantic_revision >= w.semantic_min && s.semantic_revision <= w.semantic_max;
const SEMVER = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$/;
const productVersion = (value: unknown): string => {
  if (typeof value !== "string" || !SEMVER.test(value)) fail("release_metadata_invalid", "product version must be valid SemVer 2.0.0");
  return value;
};
export function loadRelease(path: string): [Data, Data[]] {
  if (!isAbsolute(path) || path.split("/").some(p => p === "." || p === "..") || path.includes("\0")) fail("release_metadata_invalid", "release sidecar path must be absolute and canonical");
  for (const directory of new Set([dirname(path), import.meta.dir])) {
    try {
      lstatSync(join(directory, ".agent-vm-client-install.json"));
      fail("local_client_install_pending", "complete-package installation or recovery is pending; retain its recovery material");
    } catch (e: any) { if (e.code !== "ENOENT") throw e; }
  }
  let info;
  try { info = lstatSync(path); } catch (e: any) {
    if (e.code === "ENOENT") fail("manifest_missing", "release sidecar is required; reinstall the current Agent VM client package");
    fail("release_metadata_invalid", "release sidecar cannot be inspected");
  }
  if (!info.isFile() || info.isSymbolicLink()) fail("release_metadata_invalid", "release sidecar is not a regular file");
  let raw;
  try { raw = readJson(path); } catch { fail("release_metadata_invalid", "release sidecar is not valid UTF-8 JSON"); }
  if (record(raw) && (raw.format === CLIENT_PRODUCT_FORMAT || raw.format === PRODUCT_FORMAT)) {
    if (!shape(raw, ["format", "version", "source_revision", "protocol", "client_artifact", "client_dependencies", "client_view"]) || typeof raw.source_revision !== "string" || !/^[0-9a-f]{40}$/.test(raw.source_revision)) fail("release_metadata_invalid", "product sidecar has an invalid shape or identity");
    const version = productVersion(raw.version);
    if (raw.protocol !== PROTOCOL) fail("protocol_unsupported", "release sidecar protocol must be 4");
    const v = raw.client_view;
    if (!shape(v, ["version", "source_revision", "protocol", "transport", "actions"]) || v.version !== version || v.source_revision !== raw.source_revision || v.protocol !== PROTOCOL || !["go-request", "https"].includes(v.transport) || !shape(v.actions, ACTIONS)) fail("release_metadata_invalid", "product client view identity, transport or fixed action set differs");
    for (const action of ACTIONS) {
      const s = v.actions[action];
      if (!shape(s, ["request", "response", "client_to_control", "control_to_client"])) fail("release_metadata_invalid", "client_view action has an invalid shape");
      if (!within(schema(s.request), window(s.client_to_control)) || !within(schema(s.response), window(s.control_to_client))) fail("release_metadata_invalid", "schema is outside its compatibility window");
    }
    verifyArtifact(path, raw.client_artifact, "agent_vm.ts", "client", 0o755);
    if (!Array.isArray(raw.client_dependencies) || raw.client_dependencies.length !== CLIENT_DEPENDENCIES.length) fail("release_metadata_invalid", "client sidecar must declare the complete fixed dependency set");
    for (const [name, mode] of CLIENT_DEPENDENCIES) {
      const matches = raw.client_dependencies.filter((a: any) => record(a) && a.name === name);
      if (matches.length !== 1) fail("release_metadata_invalid", "client dependency is missing or duplicated");
      verifyArtifact(path, matches[0], name, "support", mode);
    }
    return [{ version, release_id: raw.source_revision, source_revision: raw.source_revision, protocol: PROTOCOL, manifest_present: true, artifacts_verified: true, transport: v.transport, actions: v.actions }, []];
  }
  fail("manifest_format_unsupported", "only product release manifest format 4 is supported");
}
function verifyArtifact(path: string, a: any, name: string, role: string, mode: number): void {
  if (!shape(a, ["name", "path", "role", "sha256", "mode"]) || a.role !== role || a.name !== name || a.path !== name || typeof a.sha256 !== "string" || !/^[0-9a-f]{64}$/.test(a.sha256) || a.mode !== mode) fail("release_metadata_invalid", `artifact must identify ${name} with its fixed role and mode`);
  const artifact = join(dirname(path), a.path);
  let stat;
  try { stat = lstatSync(artifact); } catch { fail("artifact_unverified", "declared client artifact is missing"); }
  if (!stat.isFile() || stat.isSymbolicLink()) fail("artifact_unverified", "declared client artifact is not a regular file");
  if ((stat.mode & 0o7777) !== a.mode) fail("artifact_mode_mismatch", "declared client artifact mode differs");
  let actual;
  try { actual = fileHash(artifact); } catch { fail("artifact_unverified", "declared client artifact cannot be read"); }
  if (actual !== a.sha256) fail("artifact_checksum_mismatch", "declared client artifact checksum differs");
}
export function capability(action: string, metadata: Data): Data {
  const spec = Object.hasOwn(metadata.actions, action) ? metadata.actions[action] : undefined;
  if (!spec && metadata.manifest_present) fail("unknown_action", `action is not declared in the local release sidecar: ${action}`);
  if (!spec) fail("unknown_action", `action is not declared in the local release sidecar: ${action}`);
  return { release_id: metadata.release_id, protocol: PROTOCOL, action, request: spec.request, accepted_response: spec.control_to_client, legacy: false };
}
export function responseMetadata(response: Data, request: Data, localWarnings: Data[]): void {
  const action = request.action, cap = request.compatibility;
  const warnings = response.warnings ?? [];
  if (!Array.isArray(warnings) || warnings.some(v => !record(v) || typeof v.code !== "string" || !v.code || typeof v.message !== "string" || !v.message || "action" in v && v.action !== action)) fail("invalid_remote_response", "control warnings must contain code, message and matching action");
  if (typeof response.outcome !== "string" || !["success", "replayed"].includes(response.outcome)) fail("invalid_remote_response", "control response outcome is missing or invalid");
  if (response.ok !== true || !record(response.result) || typeof response.server_time !== "string") fail("invalid_remote_response", "invalid control response envelope");
  const w = window(cap.accepted_response), s = response.response;
  if (s == null) fail("invalid_remote_response", "control response compatibility metadata is missing");
  else {
    if (!shape(s, ["schema", "semantic_revision"]) || !integer(s.schema) || s.schema < 1 || !integer(s.semantic_revision) || s.semantic_revision < 1) fail("invalid_remote_response", "control response compatibility metadata is invalid");
    if (s.schema < w.schema_min || s.schema > w.schema_max) fail("response_schema_incompatible", "control response schema is outside the accepted window", { action });
    if (s.semantic_revision < w.semantic_min || s.semantic_revision > w.semantic_max) fail("response_semantic_incompatible", "control response semantic revision is outside the accepted window", { action });
  }
  if ("release" in response) {
    const r = response.release;
    if (!record(r) || typeof r.release_id !== "string" || !/^[0-9a-f]{40}$/.test(r.release_id) || r.source_revision !== r.release_id || "version" in r && !SEMVER.test(r.version) || ["manifest_present", "artifacts_verified"].some(k => k in r && typeof r[k] !== "boolean")) fail("invalid_remote_response", "control release metadata is invalid");
    if (r.protocol !== PROTOCOL) fail("protocol_mismatch", "control release metadata protocol differs");
  } else fail("invalid_remote_response", "control response did not identify a release", { action });
  warnings.push(...localWarnings.map(v => ({ ...v, action })));
  response.warnings = warnings.filter((w, i) => warnings.findIndex(v => v.code === w.code && v.message === w.message && v.action === w.action) === i);
}
