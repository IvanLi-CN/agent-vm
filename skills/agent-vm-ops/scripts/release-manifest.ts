/**
 * Release-manifest parsing and compatibility checks for Agent VM releases.
 *
 * The Python publisher and wrapper retain release_manifest.py as a support
 * artifact and must declare the same artifact set and canonical unit digests.
 * The controller imports only TypeScript modules. guest-root-growth.ts remains
 * an artifact so the controller can bundle its guest source once with Bun.build.
 */

import { createHash } from "node:crypto";
import {
  closeSync,
  lstatSync,
  openSync,
  readFileSync,
  readSync,
  realpathSync,
  statSync,
} from "node:fs";
import { basename, isAbsolute, join, relative, resolve } from "node:path";

import {
  ControlError,
  parse_json,
  canonical_json,
  is_record,
  require_int,
  require_string,
  narrow,
  integer,
  type Data,
  type Integer,
} from "./control-types.ts";
import { parseSemVer } from "./semver.ts";

export const MANIFEST_FORMAT = 1;
export const PROTOCOL_VERSION = 4;
export const SOURCE_REVISION_RE = /^[0-9a-f]{40}$/;
export const SHA256_RE = /^[0-9a-f]{64}$/;
export const RELEASE_ID_RE = SOURCE_REVISION_RE;
export const LEGACY_RELEASE_ID = "legacy-protocol-4";
export const MANIFEST_NAME = "release-manifest.json";
export const CLIENT_SIDECAR_NAME = "client-release.json";
export const CLIENT_DEPENDENCY_NAMES = new Set([
  "byte-fs.ts", "client-io.ts", "client-release.ts", "client.ts",
  "guest-source.ts", "sync-native.ts", "sync_worktree.ts",
]);

export const ACTION_NAMES: readonly string[] = Object.freeze([
  "acquire",
  "admin-gc",
  "admin-metrics",
  "admin-reconcile",
  "admin-session-list",
  "admin-status",
  "destroy",
  "doctor",
  "drain-enter",
  "drain-exit",
  "drain-status",
  "port-add",
  "port-list",
  "port-remove",
  "release",
  "renew",
  "resize",
  "restart",
  "ssh-info",
  "status",
  "timer-age-gc",
  "timer-expire",
  "timer-reconcile",
]);

const ACTION_SET: ReadonlySet<string> = new Set(ACTION_NAMES);

export const UNIT_EXEC_START = Object.freeze({
  "agent-vm-native-age-gc.service":
    "ExecStart=/usr/local/bin/bun /opt/agent-vm-native/agent_vm_control.ts --timer-age-gc",
  "agent-vm-native-expire@.service":
    "ExecStart=/usr/local/bin/bun /opt/agent-vm-native/agent_vm_control.ts --timer-expire %i",
  "agent-vm-native-reconcile.service":
    "ExecStart=/usr/local/bin/bun /opt/agent-vm-native/agent_vm_control.ts --timer-reconcile",
});

/** Exact unit bytes are deployment policy, independent of a caller manifest. */
export const UNIT_CONTENT_SHA256 = Object.freeze({
  "agent-vm-native-age-gc.service":
    "6a6387459c3f4b5f888fc7d4fe6d49cf571ff4eabd3a96edde72a05d37588460",
  "agent-vm-native-age-gc.timer":
    "4eb1c7b012fc3251ef4427c56b8c1b0236deff28762cc4293278f56d02c4cb4b",
  "agent-vm-native-expire@.service":
    "e0cfb3c7eaf4aaadcfe5e296a0698f0cb235c4bbecd22275ad5c6826d7cbd3e2",
  "agent-vm-native-reconcile.service":
    "76ac0a8e730ffdfa4c0fe0b0c0cd6ad75dd11e627b747e843ded9fae5fd8ddc8",
});

export type ArtifactRole = "client" | "control" | "support" | "unit";

export type ArtifactTuple = readonly [
  name: string,
  path: string,
  role: ArtifactRole,
  mode: number,
];

export const FIXED_ARTIFACTS: readonly ArtifactTuple[] = Object.freeze(
  (
    [
      ["agent_vm.ts", "agent_vm.ts", "client", 0o755],
      ["sync_worktree.ts", "sync_worktree.ts", "support", 0o755],
      ["byte-fs.ts", "byte-fs.ts", "support", 0o644],
      ["client-io.ts", "client-io.ts", "support", 0o644],
      ["client-release.ts", "client-release.ts", "support", 0o644],
      ["client.ts", "client.ts", "support", 0o644],
      ["guest-source.ts", "guest-source.ts", "support", 0o644],
      ["sync-native.ts", "sync-native.ts", "support", 0o644],
      ["agent_vm_control.ts", "agent_vm_control.ts", "control", 0o755],
      ["release_manifest.py", "release_manifest.py", "support", 0o644],
      ["control-types.ts", "control-types.ts", "support", 0o644],
      ["control-store.ts", "control-store.ts", "support", 0o644],
      ["control-native.ts", "control-native.ts", "support", 0o644],
      ["control-core.ts", "control-core.ts", "support", 0o644],
      ["control-observation.ts", "control-observation.ts", "support", 0o644],
      ["control-resources.ts", "control-resources.ts", "support", 0o644],
      ["control-systemd.ts", "control-systemd.ts", "support", 0o644],
      ["control-lifecycle.ts", "control-lifecycle.ts", "support", 0o644],
      ["release-manifest.ts", "release-manifest.ts", "support", 0o644],
      ["guest-root-growth.ts", "guest-root-growth.ts", "support", 0o644],
      [
        "unit:agent-vm-native-age-gc.service",
        "systemd/agent-vm-native-age-gc.service",
        "unit",
        0o644,
      ],
      [
        "unit:agent-vm-native-age-gc.timer",
        "systemd/agent-vm-native-age-gc.timer",
        "unit",
        0o644,
      ],
      [
        "unit:agent-vm-native-expire@.service",
        "systemd/agent-vm-native-expire@.service",
        "unit",
        0o644,
      ],
      [
        "unit:agent-vm-native-reconcile.service",
        "systemd/agent-vm-native-reconcile.service",
        "unit",
        0o644,
      ],
    ] satisfies ArtifactTuple[]
  ).map((item) => Object.freeze(item)),
);

export interface Artifact {
  name: string;
  path: string;
  role: ArtifactRole;
  sha256: string;
  mode: number;
}

export interface Schema {
  schema: Integer;
  semantic_revision: Integer;
}

export interface CompatibilityWindow {
  schema_min: Integer;
  schema_max: Integer;
  semantic_min: Integer;
  semantic_max: Integer;
}

export interface OperationSpec {
  request: Schema;
  response: Schema;
  client_to_control: CompatibilityWindow;
  control_to_client: CompatibilityWindow;
}

export interface ClientView {
  release_id: string;
  source_revision: string;
  protocol: number;
  actions: Record<string, OperationSpec>;
}

export interface ReleaseManifest {
  format: number;
  release_id: string;
  source_revision: string;
  protocol: number;
  artifacts: Artifact[];
  actions: Record<string, OperationSpec>;
  client_view: ClientView;
}

export interface OperationOptions {
  request_schema?: number;
  response_schema?: number;
  request_semantic_revision?: number;
  response_semantic_revision?: number;
  client_to_control?: Readonly<CompatibilityWindow> | null;
  control_to_client?: Readonly<CompatibilityWindow> | null;
}

export class ManifestError extends ControlError {
  constructor(code: string, message: string, detail: any = null) {
    super(code, message, detail);
    this.name = "ManifestError";
  }

  as_dict(): Data {
    const result: Data = { code: this.code, message: this.message };
    if (this.detail !== null && this.detail !== undefined) {
      result.detail = this.detail;
    }
    return result;
  }
}

type Mapping = Record<string, unknown>;

function _error(
  code: string,
  message: string,
  detail: any = null,
): ManifestError {
  return new ManifestError(code, message, detail);
}

function _require_mapping(value: unknown, label: string): Mapping {
  if (!is_record(value)) {
    throw _error("manifest_invalid", `${label} must be an object`);
  }
  return value as Mapping;
}

function _fullmatch(pattern: RegExp, value: string): boolean {
  const match = pattern.exec(value);
  return match !== null && match.index === 0 && match[0].length === value.length;
}

function _require_string(
  value: unknown,
  label: string,
  pattern?: RegExp,
): string {
  if (typeof value !== "string" || value.length === 0) {
    throw _error("manifest_invalid", `${label} must be a non-empty string`);
  }
  if (pattern !== undefined && !_fullmatch(pattern, value)) {
    throw _error("manifest_invalid", `${label} has an invalid value`);
  }
  return require_string(value, label);
}

function _require_int(
  value: unknown,
  label: string,
  minimum?: number,
  maximum?: number,
): Integer {
  if (typeof value !== "bigint" && (typeof value !== "number" || !Number.isSafeInteger(value))) {
    throw _error("manifest_invalid", `${label} must be an integer`);
  }
  if (minimum !== undefined && value < minimum) {
    throw _error("manifest_invalid", `${label} is below its minimum`);
  }
  if (maximum !== undefined && value > maximum) {
    throw _error("manifest_invalid", `${label} is above its maximum`);
  }
  return narrow(integer(require_int(value, label)));
}

function _same_keys(value: Mapping, expected: readonly string[]): boolean {
  return (
    Object.keys(value).length === expected.length &&
    expected.every((key) => Object.hasOwn(value, key))
  );
}

function _posix_parts(path: string): string[] {
  // PurePosixPath removes repeated separators and "." components.
  return path.split("/").filter((part) => part !== "" && part !== ".");
}

function _safe_relative_path(value: unknown, label: string): string {
  const path = _require_string(value, label);
  if (path.includes("\\") || path.includes("\0")) {
    throw _error(
      "manifest_path_invalid",
      `${label} contains an unsupported separator`,
    );
  }
  const parts = _posix_parts(path);
  if (path.startsWith("/") || parts.includes("..")) {
    throw _error(
      "manifest_path_invalid",
      `${label} must be a relative regular-file path`,
    );
  }
  return parts.join("/") || ".";
}

function _window(value: unknown, label: string): CompatibilityWindow {
  const raw = _require_mapping(value, label);
  const expected = [
    "schema_min",
    "schema_max",
    "semantic_min",
    "semantic_max",
  ] as const;
  if (!_same_keys(raw, expected)) {
    throw _error(
      "manifest_invalid",
      `${label} must declare schema and semantic min/max`,
    );
  }
  const result: CompatibilityWindow = {
    schema_min: _require_int(raw.schema_min, `${label}.schema_min`, 1),
    schema_max: _require_int(raw.schema_max, `${label}.schema_max`, 1),
    semantic_min: _require_int(raw.semantic_min, `${label}.semantic_min`, 1),
    semantic_max: _require_int(raw.semantic_max, `${label}.semantic_max`, 1),
  };
  if (
    result.schema_min > result.schema_max ||
    result.semantic_min > result.semantic_max
  ) {
    throw _error(
      "manifest_invalid",
      `${label} has an inverted compatibility window`,
    );
  }
  return result;
}

function _schema(value: unknown, label: string): Schema {
  const raw = _require_mapping(value, label);
  if (!_same_keys(raw, ["schema", "semantic_revision"])) {
    throw _error(
      "manifest_invalid",
      `${label} must declare schema and semantic_revision`,
    );
  }
  return {
    schema: _require_int(raw.schema, `${label}.schema`, 1),
    semantic_revision: _require_int(
      raw.semantic_revision,
      `${label}.semantic_revision`,
      1,
    ),
  };
}

function _sorted_artifacts(
  artifacts: readonly ArtifactTuple[],
): ArtifactTuple[] {
  return [...artifacts].sort((left, right) => {
    for (const index of [0, 1, 2] as const) {
      if (left[index] < right[index]) return -1;
      if (left[index] > right[index]) return 1;
    }
    return left[3] - right[3];
  });
}

const FIXED_ARTIFACT_KEYS: ReadonlySet<string> = new Set(
  FIXED_ARTIFACTS.map((item) => canonical_json(item)),
);

/**
 * JSON-object equality is independent of key order. Preserve Python's numeric
 * boolean equality where the original code compares client views or sidecars.
 * Schema and window validation itself still rejects booleans.
 */
function _json_equal(left: unknown, right: unknown): boolean {
  if (left === right) return true;
  if (typeof left === "boolean" && typeof right === "number") {
    return Number(left) === right;
  }
  if (typeof left === "number" && typeof right === "boolean") {
    return left === Number(right);
  }
  if (Array.isArray(left) || Array.isArray(right)) {
    return (
      Array.isArray(left) &&
      Array.isArray(right) &&
      left.length === right.length &&
      left.every((value, index) => _json_equal(value, right[index]))
    );
  }
  if (is_record(left) && is_record(right)) {
    const left_map = left as Mapping;
    const right_map = right as Mapping;
    const keys = Object.keys(left_map);
    return (
      _same_keys(right_map, keys) &&
      keys.every((key) => _json_equal(left_map[key], right_map[key]))
    );
  }
  return false;
}

export function operation_spec(
  options: OperationOptions = {},
): OperationSpec {
  const {
    request_schema = 1,
    response_schema = 1,
    request_semantic_revision = 1,
    response_semantic_revision = 1,
    client_to_control = null,
    control_to_client = null,
  } = options;

  const c2c =
    client_to_control !== null && Object.keys(client_to_control).length > 0
      ? { ...client_to_control }
      : {
          schema_min: request_schema,
          schema_max: request_schema,
          semantic_min: request_semantic_revision,
          semantic_max: request_semantic_revision,
        };
  const c2l =
    control_to_client !== null && Object.keys(control_to_client).length > 0
      ? { ...control_to_client }
      : {
          schema_min: response_schema,
          schema_max: response_schema,
          semantic_min: response_semantic_revision,
          semantic_max: response_semantic_revision,
        };

  return {
    request: {
      schema: request_schema,
      semantic_revision: request_semantic_revision,
    },
    response: {
      schema: response_schema,
      semantic_revision: response_semantic_revision,
    },
    client_to_control: c2c,
    control_to_client: c2l,
  };
}

export function default_operations(): Record<string, OperationSpec> {
  return Object.fromEntries(
    ACTION_NAMES.map((action) => [action, operation_spec()]),
  );
}

export function legacy_manifest(): ReleaseManifest {
  const actions = default_operations();
  return {
    format: MANIFEST_FORMAT,
    release_id: "0".repeat(40),
    source_revision: "0".repeat(40),
    protocol: PROTOCOL_VERSION,
    artifacts: _sorted_artifacts(FIXED_ARTIFACTS).map(
      ([name, path, role, mode]) => ({
        name,
        path,
        role,
        sha256: "0".repeat(64),
        mode,
      }),
    ),
    actions,
    client_view: {
      release_id: "0".repeat(40),
      source_revision: "0".repeat(40),
      protocol: PROTOCOL_VERSION,
      actions,
    },
  };
}

export function build_manifest(
  source_revision: string,
  artifacts: readonly object[],
  options: {
    operations?: Readonly<Record<string, object>> | null;
  } = {},
): ReleaseManifest {
  const operations =
    options.operations != null && Object.keys(options.operations).length > 0
      ? options.operations
      : default_operations();
  const actions = Object.fromEntries(
    Object.entries(operations).map(([key, value]) => [key, { ...value }]),
  );
  return validate_manifest({
    format: MANIFEST_FORMAT,
    release_id: source_revision,
    source_revision,
    protocol: PROTOCOL_VERSION,
    artifacts: artifacts.map((item) => ({ ...item })),
    actions,
    client_view: {
      release_id: source_revision,
      source_revision,
      protocol: PROTOCOL_VERSION,
      actions,
    },
  });
}

export function validate_manifest(value: unknown): ReleaseManifest {
  const root = _require_mapping(value, "manifest");
  const required = [
    "format",
    "release_id",
    "source_revision",
    "protocol",
    "artifacts",
    "actions",
    "client_view",
  ];
  const missing = required.filter((key) => !Object.hasOwn(root, key)).sort();
  if (missing.length > 0) {
    throw _error("manifest_invalid", "manifest is missing required fields", {
      missing,
    });
  }
  const unknown = Object.keys(root)
    .filter((key) => !required.includes(key))
    .sort();
  if (unknown.length > 0) {
    throw _error("manifest_invalid", "manifest contains unknown fields", {
      fields: unknown,
    });
  }
  if (_require_int(root.format, "format", 1) !== MANIFEST_FORMAT) {
    throw _error(
      "manifest_format_unsupported",
      `manifest format must be ${MANIFEST_FORMAT}`,
    );
  }
  const source_revision = _require_string(
    root.source_revision,
    "source_revision",
    SOURCE_REVISION_RE,
  );
  const release_id = _require_string(
    root.release_id,
    "release_id",
    RELEASE_ID_RE,
  );
  if (release_id !== source_revision) {
    throw _error("manifest_invalid", "release_id must equal source_revision");
  }
  if (_require_int(root.protocol, "protocol", 1) !== PROTOCOL_VERSION) {
    throw _error(
      "protocol_unsupported",
      `manifest protocol must be ${PROTOCOL_VERSION}`,
    );
  }

  const raw_artifacts = root.artifacts;
  if (!Array.isArray(raw_artifacts) || raw_artifacts.length === 0) {
    throw _error("manifest_invalid", "artifacts must be a non-empty list");
  }
  const artifacts: Artifact[] = [];
  const names = new Set<string>();
  const paths = new Set<string>();
  for (const [index, raw] of raw_artifacts.entries()) {
    const item = _require_mapping(raw, `artifacts[${index}]`);
    if (!_same_keys(item, ["name", "path", "role", "sha256", "mode"])) {
      throw _error(
        "manifest_invalid",
        `artifacts[${index}] has an unexpected shape`,
      );
    }
    const name = _require_string(
      item.name,
      `artifacts[${index}].name`,
      /^[A-Za-z0-9_.:@-]{1,120}$/,
    );
    const path = _safe_relative_path(item.path, `artifacts[${index}].path`);
    const role = _require_string(
      item.role,
      `artifacts[${index}].role`,
      /^(client|control|support|unit)$/,
    ) as ArtifactRole;
    const digest = _require_string(
      item.sha256,
      `artifacts[${index}].sha256`,
      SHA256_RE,
    );
    const mode = _require_int(
      item.mode,
      `artifacts[${index}].mode`,
      0,
      0o777,
    );
    if (names.has(name) || paths.has(path)) {
      throw _error(
        "manifest_invalid",
        "artifact names and paths must be unique",
      );
    }
    if (path === MANIFEST_NAME || path === CLIENT_SIDECAR_NAME) {
      throw _error(
        "manifest_self_reference",
        "release metadata cannot checksum itself",
      );
    }
    names.add(name);
    paths.add(path);
    artifacts.push({ name, path, role, sha256: digest, mode: Number(mode) });
  }

  const actual_artifacts: ArtifactTuple[] = artifacts.map((item) => [
    item.name,
    item.path,
    item.role,
    item.mode,
  ]);
  if (
    actual_artifacts.length !== FIXED_ARTIFACT_KEYS.size ||
    actual_artifacts.some(
      (item) => !FIXED_ARTIFACT_KEYS.has(canonical_json(item)),
    )
  ) {
    throw _error(
      "manifest_artifact_set_invalid",
      "manifest artifact set differs from the fixed release allowlist",
      {
        expected: _sorted_artifacts(FIXED_ARTIFACTS),
        actual: _sorted_artifacts(actual_artifacts),
      },
    );
  }

  const raw_actions = _require_mapping(root.actions, "actions");
  const action_keys = Object.keys(raw_actions);
  const unsupported_actions = action_keys
    .filter((action) => !ACTION_SET.has(action))
    .sort();
  if (unsupported_actions.length > 0) {
    throw _error(
      "manifest_action_unsupported",
      "manifest declares an unsupported action",
      { actions: unsupported_actions },
    );
  }
  if (!_same_keys(raw_actions, ACTION_NAMES)) {
    throw _error(
      "manifest_action_set_invalid",
      "manifest action set differs from the fixed control surface",
      { expected: [...ACTION_NAMES], actual: action_keys.sort() },
    );
  }

  const actions: Record<string, OperationSpec> = {};
  for (const [action, raw_spec] of Object.entries(raw_actions)) {
    const action_name = _require_string(
      action,
      "action",
      /^[a-z][a-z0-9-]{0,63}$/,
    );
    if (!ACTION_SET.has(action_name)) {
      throw _error(
        "manifest_action_unsupported",
        `manifest declares an unsupported action: ${action_name}`,
      );
    }
    const spec = _require_mapping(raw_spec, `actions.${action_name}`);
    if (
      !_same_keys(spec, [
        "request",
        "response",
        "client_to_control",
        "control_to_client",
      ])
    ) {
      throw _error(
        "manifest_invalid",
        `actions.${action_name} has an unexpected shape`,
      );
    }
    const request = _schema(
      spec.request,
      `actions.${action_name}.request`,
    );
    const response = _schema(
      spec.response,
      `actions.${action_name}.response`,
    );
    const c2c = _window(
      spec.client_to_control,
      `actions.${action_name}.client_to_control`,
    );
    const c2l = _window(
      spec.control_to_client,
      `actions.${action_name}.control_to_client`,
    );
    if (
      request.schema < c2c.schema_min ||
      request.schema > c2c.schema_max ||
      request.semantic_revision < c2c.semantic_min ||
      request.semantic_revision > c2c.semantic_max
    ) {
      throw _error(
        "manifest_invalid",
        `actions.${action_name} request is outside its declared window`,
      );
    }
    if (
      response.schema < c2l.schema_min ||
      response.schema > c2l.schema_max ||
      response.semantic_revision < c2l.semantic_min ||
      response.semantic_revision > c2l.semantic_max
    ) {
      throw _error(
        "manifest_invalid",
        `actions.${action_name} response is outside its declared window`,
      );
    }
    actions[action_name] = {
      request,
      response,
      client_to_control: c2c,
      control_to_client: c2l,
    };
  }

  const client_view = _require_mapping(root.client_view, "client_view");
  if (
    !_same_keys(client_view, [
      "release_id",
      "source_revision",
      "protocol",
      "actions",
    ])
  ) {
    throw _error("manifest_invalid", "client_view has an unexpected shape");
  }
  if (
    client_view.release_id !== release_id ||
    client_view.source_revision !== source_revision ||
    client_view.protocol !== PROTOCOL_VERSION
  ) {
    throw _error(
      "manifest_invalid",
      "client_view identity does not match manifest",
    );
  }
  const view_actions = _require_mapping(
    client_view.actions,
    "client_view.actions",
  );
  if (!_json_equal(view_actions, actions)) {
    throw _error(
      "manifest_invalid",
      "client_view actions do not match manifest actions",
    );
  }

  return {
    format: MANIFEST_FORMAT,
    release_id,
    source_revision,
    protocol: PROTOCOL_VERSION,
    artifacts,
    actions,
    client_view: {
      release_id,
      source_revision,
      protocol: PROTOCOL_VERSION,
      actions,
    },
  };
}

function _errno(error: unknown): string | undefined {
  if (
    typeof error === "object" &&
    error !== null &&
    "code" in error &&
    typeof error.code === "string"
  ) {
    return error.code;
  }
  return undefined;
}

function _read_json(path: string): unknown {
  const text = new TextDecoder("utf-8", {
    fatal: true,
    ignoreBOM: true,
  }).decode(readFileSync(path));
  // Python's UTF-8 JSON reader rejects a leading BOM.
  if (text.startsWith("\uFEFF")) {
    throw new SyntaxError("Unexpected UTF-8 BOM");
  }
  return parse_json(text);
}

export function load_manifest(path: string): ReleaseManifest {
  let value: unknown;
  try {
    const info = lstatSync(path);
    if (info.isSymbolicLink() || !info.isFile()) {
      throw _error(
        "manifest_unverified",
        "release manifest is not a regular file",
      );
    }
    value = _read_json(path);
  } catch (error) {
    if (error instanceof ManifestError) throw error;
    if (_errno(error) === "ENOENT") {
      throw _error("manifest_missing", "release manifest is missing");
    }
    throw _error(
      "manifest_invalid",
      "release manifest is not valid UTF-8 JSON",
    );
  }
  return validate_manifest(value);
}

function _resolve_root(root: string): string {
  const absolute = resolve(root);
  try {
    return realpathSync(absolute);
  } catch (error) {
    // Path.resolve(strict=False) permits nonexistent trailing components.
    if (_errno(error) === "ENOENT" || _errno(error) === "ENOTDIR") {
      return absolute;
    }
    throw error;
  }
}

function _inspect_artifact(path: string, relative_path: string) {
  try {
    return lstatSync(path);
  } catch (error) {
    if (_errno(error) === "ENOENT") {
      throw _error(
        "artifact_missing",
        `artifact is missing: ${relative_path}`,
      );
    }
    throw _error(
      "artifact_unverified",
      `artifact cannot be inspected: ${relative_path}`,
    );
  }
}

function _resolve_regular(root: string, relative_path: string): string {
  const parts = _posix_parts(relative_path);
  const candidate = join(root, ...parts);
  let cursor = root;
  for (const [index, part] of parts.entries()) {
    cursor = join(cursor, part);
    const info = _inspect_artifact(cursor, relative_path);
    if (info.isSymbolicLink()) {
      throw _error(
        "artifact_unverified",
        `artifact path contains a symlink: ${relative_path}`,
      );
    }
    if (index < parts.length - 1 && !info.isDirectory()) {
      throw _error(
        "artifact_unverified",
        `artifact path contains a non-directory parent: ${relative_path}`,
      );
    }
  }

  let resolved: string;
  try {
    resolved = realpathSync(candidate);
  } catch (error) {
    if (_errno(error) === "ENOENT") {
      throw _error(
        "artifact_missing",
        `artifact is missing: ${relative_path}`,
      );
    }
    throw _error(
      "artifact_unverified",
      `artifact cannot be inspected: ${relative_path}`,
    );
  }
  const suffix = relative(root, resolved);
  if (suffix === ".." || suffix.startsWith("../") || isAbsolute(suffix)) {
    throw _error("artifact_path_invalid", "artifact escapes bundle root");
  }
  const info = _inspect_artifact(candidate, relative_path);
  if (info.isSymbolicLink() || !info.isFile()) {
    throw _error(
      "artifact_unverified",
      `artifact is not a regular file: ${relative_path}`,
    );
  }
  return candidate;
}

export function sha256_file(path: string): string {
  const digest = createHash("sha256");
  try {
    const descriptor = openSync(path, "r");
    try {
      const buffer = Buffer.allocUnsafe(1024 * 1024);
      for (;;) {
        const count = readSync(
          descriptor,
          buffer,
          0,
          buffer.length,
          null,
        );
        if (count === 0) break;
        digest.update(buffer.subarray(0, count));
      }
    } finally {
      closeSync(descriptor);
    }
  } catch {
    throw _error(
      "artifact_unverified",
      `artifact cannot be read: ${basename(path)}`,
    );
  }
  return digest.digest("hex");
}

function _verify_artifact_file(path: string, item: Artifact): void {
  const info = _inspect_artifact(path, item.path);
  if (info.isSymbolicLink() || !info.isFile()) {
    throw _error(
      "artifact_unverified",
      `artifact is not a regular file: ${item.path}`,
    );
  }
  const actual_mode = info.mode & 0o7777;
  if (actual_mode !== item.mode) {
    throw _error(
      "artifact_mode_mismatch",
      `artifact mode differs: ${item.path}`,
      { expected: item.mode, actual: actual_mode },
    );
  }
  const actual_digest = sha256_file(path);
  if (actual_digest !== item.sha256) {
    throw _error(
      "artifact_checksum_mismatch",
      `artifact checksum differs: ${item.path}`,
      { expected: item.sha256, actual: actual_digest },
    );
  }
}

export function verify_bundle(
  root: string,
  manifest: unknown = null,
): ReleaseManifest | ProductManifest {
  let resolved_root: string;
  try {
    resolved_root = _resolve_root(root);
    if (!statSync(resolved_root).isDirectory()) {
      throw _error("bundle_invalid", "bundle root is not a directory");
    }
  } catch (error) {
    if (error instanceof ManifestError) throw error;
    throw _error("bundle_invalid", "bundle root is not a directory");
  }
  const candidate = manifest == null
    ? _read_json(join(resolved_root, MANIFEST_NAME))
    : manifest;
  if (is_product_manifest(candidate)) {
    return verify_product_bundle(resolved_root, candidate);
  }
  const checked = validate_manifest(candidate);
  for (const item of checked.artifacts) {
    _verify_artifact_file(
      _resolve_regular(resolved_root, item.path),
      item,
    );
  }
  return checked;
}

export function verify_runtime_bundle(
  manifest: unknown,
  artifact_paths: Readonly<Record<string, string | null | undefined>>,
): ReleaseManifest | ProductManifest {
  if (is_product_manifest(manifest)) {
    const checked = validate_product_manifest(manifest);
    for (const item of checked.artifacts) {
      const path = Object.hasOwn(artifact_paths, item.path)
        ? artifact_paths[item.path]
        : undefined;
      if (path == null) {
        throw _error(
          "artifact_missing",
          `runtime artifact path is not configured: ${item.path}`,
        );
      }
      _verify_artifact_file(path, item);
    }
    return checked;
  }
  const checked = validate_manifest(manifest);
  for (const item of checked.artifacts) {
    const path = Object.hasOwn(artifact_paths, item.path)
      ? artifact_paths[item.path]
      : undefined;
    if (path == null) {
      throw _error(
        "artifact_missing",
        `runtime artifact path is not configured: ${item.path}`,
      );
    }
    _verify_artifact_file(path, item);
  }
  return checked;
}

export function verify_client_sidecar(
  root: string,
  manifest: unknown = null,
): ReleaseManifest | ProductManifest {
  const resolved_root = _resolve_root(root);
  const candidate = manifest == null ? load_manifest(join(resolved_root, MANIFEST_NAME)) : manifest;
  const checked = is_product_manifest(candidate)
    ? validate_product_manifest(candidate)
    : validate_manifest(candidate);
  const sidecar_path = _resolve_regular(
    resolved_root,
    CLIENT_SIDECAR_NAME,
  );
  let value: unknown;
  try {
    value = _read_json(sidecar_path);
  } catch {
    throw _error(
      "client_sidecar_invalid",
      "client sidecar is not valid UTF-8 JSON",
    );
  }
  if (!is_record(value)) {
    throw _error(
      "client_sidecar_invalid",
      "client sidecar root must be an object",
    );
  }
  const sidecar = value as Mapping;
  // Sidecars bind executable bytes and modes. Do not inherit the historical
  // Python boolean/number equality used by compatibility-view comparisons.
  const same = (left: unknown, right: unknown) => canonical_json(left) === canonical_json(right);
  const productSidecar = is_product_manifest(checked);
  const expectedSidecarKeys = productSidecar
    ? ["client_artifact", "client_dependencies", "client_view", "format", "protocol", "source_revision", "version"]
    : ["client_artifact", "client_dependencies", "client_view", "format", "protocol", "release_id", "source_revision"];
  if (!_same_keys(sidecar, expectedSidecarKeys)) {
    throw _error(
      "client_sidecar_invalid",
      "client sidecar has an unexpected shape",
    );
  }
  if (productSidecar
    ? !same(sidecar.format, checked.format) || !same(sidecar.version, checked.version) || !same(sidecar.source_revision, checked.source_revision) || !same(sidecar.protocol, checked.protocol)
    : !same(sidecar.format, checked.format) || !same(sidecar.release_id, checked.release_id) || !same(sidecar.source_revision, checked.source_revision) || !same(sidecar.protocol, checked.protocol)) {
    throw _error(
      "client_sidecar_invalid",
      "client sidecar identity does not match manifest",
    );
  }
  const client_artifacts = checked.artifacts.filter(
    (item) => item.role === "client",
  );
  if (
    client_artifacts.length !== 1 ||
    !same(sidecar.client_artifact, client_artifacts[0])
  ) {
    throw _error(
      "client_sidecar_invalid",
      "client sidecar artifact does not match manifest",
    );
  }
  if (!same(sidecar.client_view, checked.client_view)) {
    throw _error(
      "client_sidecar_invalid",
      "client sidecar view does not match manifest",
    );
  }
  const dependencies = checked.artifacts.filter(item => CLIENT_DEPENDENCY_NAMES.has(item.path));
  const declared = sidecar.client_dependencies;
  if (!Array.isArray(declared) || declared.length !== dependencies.length ||
      !same([...declared].sort((a, b) => String(a?.path).localeCompare(String(b?.path))),
        [...dependencies].sort((a, b) => a.path.localeCompare(b.path)))) {
    throw _error("client_sidecar_invalid", "client sidecar dependencies do not match the complete package");
  }
  for (const item of [client_artifacts[0], ...dependencies]) {
    _verify_artifact_file(_resolve_regular(resolved_root, item.path), item);
  }
  return checked;
}

export function compatibility_for(
  manifest: unknown,
  action: string,
): {
  protocol: number;
  action: string;
  request: Schema;
  response: Schema;
  client_to_control: CompatibilityWindow;
  control_to_client: CompatibilityWindow;
} {
  const checked = is_product_manifest(manifest)
    ? validate_product_manifest(manifest)
    : validate_manifest(manifest);
  if (!Object.hasOwn(checked.actions, action)) {
    throw _error(
      "unknown_action",
      `action is not declared in the release manifest: ${action}`,
    );
  }
  const spec = checked.actions[action]!;
  return {
    protocol: checked.protocol,
    action,
    request: { ...spec.request },
    response: { ...spec.response },
    client_to_control: { ...spec.client_to_control },
    control_to_client: { ...spec.control_to_client },
  };
}

export function legacy_capability(action: string): Data {
  if (!ACTION_SET.has(action)) {
    throw _error("unknown_action", `unsupported legacy action: ${action}`);
  }
  const spec = operation_spec();
  return {
    release_id: LEGACY_RELEASE_ID,
    protocol: PROTOCOL_VERSION,
    action,
    request: { ...spec.request },
    accepted_response: {
      schema_min: 1,
      schema_max: 1,
      semantic_min: 1,
      semantic_max: 1,
    },
    legacy: true,
  };
}

export function warning(
  code: string,
  action: string | null,
  message: string,
  detail: any = null,
): Data {
  const value: Data = { code, message };
  if (action !== null && action !== undefined) {
    value.action = action;
  }
  if (detail !== null && detail !== undefined) {
    value.detail = detail;
  }
  return value;
}

export function check_compatibility(
  manifest: unknown,
  action: string,
  peer: unknown,
): Data[] {
  const checked = is_product_manifest(manifest)
    ? validate_product_manifest(manifest)
    : validate_manifest(manifest);
  const local_release = is_product_manifest(checked)
    ? checked.source_revision
    : checked.release_id;
  const local = compatibility_for(checked, action);
  if (peer == null) {
    return [
      warning(
        "legacy_compatibility",
        action,
        "peer omitted release capability; using legacy protocol-4 compatibility",
      ),
    ];
  }
  const peer_map = _require_mapping(peer, "compatibility");
  const protocol = _require_int(
    peer_map.protocol,
    "compatibility.protocol",
    1,
  );
  if (protocol !== checked.protocol) {
    throw _error("protocol_mismatch", "peer protocol is not supported", {
      expected: checked.protocol,
      actual: protocol,
    });
  }
  if (peer_map.action !== action) {
    throw _error(
      "compatibility_action_mismatch",
      "peer capability action does not match request action",
    );
  }
  const request = _schema(peer_map.request, "compatibility.request");
  const accepted_response = _window(
    peer_map.accepted_response,
    "compatibility.accepted_response",
  );
  const local_request_window = local.client_to_control;
  const local_response = local.response;
  if (
    request.schema < local_request_window.schema_min ||
    request.schema > local_request_window.schema_max
  ) {
    throw _error(
      "request_schema_incompatible",
      "request schema is outside the control compatibility window",
      { action },
    );
  }
  if (
    request.semantic_revision < local_request_window.semantic_min ||
    request.semantic_revision > local_request_window.semantic_max
  ) {
    throw _error(
      "request_semantic_incompatible",
      "request semantic revision is outside the control compatibility window",
      { action },
    );
  }
  if (
    local_response.schema < accepted_response.schema_min ||
    local_response.schema > accepted_response.schema_max
  ) {
    throw _error(
      "response_schema_incompatible",
      "control response schema is not accepted by the client",
      { action },
    );
  }
  if (
    local_response.semantic_revision < accepted_response.semantic_min ||
    local_response.semantic_revision > accepted_response.semantic_max
  ) {
    throw _error(
      "response_semantic_incompatible",
      "control response semantic revision is not accepted by the client",
      { action },
    );
  }
  if (!Object.hasOwn(peer_map, "release_id")) {
    throw _error(
      "compatibility_invalid",
      "compatibility.release_id is required for a present capability",
    );
  }
  const peer_release = peer_map.release_id;
  if (typeof peer_release !== "string" || peer_release.length === 0) {
    throw _error(
      "compatibility_invalid",
      "compatibility.release_id must be a string",
    );
  }
  if (
    peer_release !== LEGACY_RELEASE_ID &&
    !_fullmatch(RELEASE_ID_RE, peer_release)
  ) {
    throw _error(
      "compatibility_invalid",
      "compatibility.release_id must be a Git revision or legacy-protocol-4",
    );
  }
  if (peer_release === local_release) {
    return [];
  }
  return [
    warning(
      "compatible_release",
      action,
      "peer release differs but the requested operation is explicitly compatible",
      {
        peer_release,
        control_release: local_release,
      },
    ),
  ];
}

export function manifest_metadata(
  manifest: unknown,
  options: {
    present: boolean;
    verified: boolean;
    legacy?: boolean;
  },
): Data {
  const { present, verified, legacy = false } = options;
  if (legacy || manifest == null) {
    return {
      release_id: LEGACY_RELEASE_ID,
      source_revision: null,
      protocol: PROTOCOL_VERSION,
      manifest_present: present,
      artifacts_verified: verified,
    };
  }
  if (is_product_manifest(manifest)) {
    const checked = validate_product_manifest(manifest);
    return {
      release_id: checked.source_revision,
      source_revision: checked.source_revision,
      version: checked.version,
      protocol: checked.protocol,
      manifest_present: present,
      artifacts_verified: verified,
    };
  }
  const checked = validate_manifest(manifest);
  return {
    release_id: checked.release_id,
    source_revision: checked.source_revision,
    protocol: checked.protocol,
    manifest_present: present,
    artifacts_verified: verified,
  };
}

export function load_runtime_manifest(
  path: string,
): [
  manifest: ReleaseManifest | ProductManifest | null,
  present: boolean,
  verified: boolean,
  warnings: Data[],
] {
  let info: ReturnType<typeof lstatSync>;
  try {
    info = lstatSync(path);
  } catch (error) {
    if (_errno(error) === "ENOENT") {
      return [
        null,
        false,
        true,
        [
          warning(
            "legacy_compatibility",
            null,
            "release manifest is absent; using legacy protocol-4 compatibility",
          ),
        ],
      ];
    }
    throw _error(
      "manifest_unverified",
      "release manifest cannot be inspected",
    );
  }
  if (info.isSymbolicLink() || !info.isFile()) {
    throw _error(
      "manifest_unverified",
      "release manifest is not a regular file",
    );
  }
  let candidate: unknown;
  try {
    candidate = _read_json(path);
  } catch (error) {
    if (error instanceof ManifestError) throw error;
    throw _error(
      "manifest_invalid",
      "release manifest is not valid UTF-8 JSON",
    );
  }
  const manifest = is_product_manifest(candidate)
    ? validate_product_manifest(candidate)
    : validate_manifest(candidate);
  return [manifest, true, true, []];
}

// Format 3 is retained as a read-only product format. Format 4 adds the
// management Web package and the state capability declaration.
export const LEGACY_PRODUCT_MANIFEST_FORMAT = 3;
export const PRODUCT_MANIFEST_FORMAT = 4;
export const PRODUCT_TOOLCHAIN = Object.freeze({ bun: "1.4.4", go: "go1.27.3", cgo_enabled: false });
export const STATE_CAPABILITY_MARKERS = Object.freeze([
  "legacy-agent-vm-core@1",
  "managed-image-catalog@1",
  "management-operation-audit@1",
  "session-image-binding@1",
]);

const PRODUCT_CLIENT_ARTIFACTS: readonly ArtifactTuple[] = Object.freeze([
  ["agent_vm.ts", "agent_vm.ts", "client", 0o755],
  ["byte-fs.ts", "byte-fs.ts", "support", 0o644],
  ["client-io.ts", "client-io.ts", "support", 0o644],
  ["client-release.ts", "client-release.ts", "support", 0o644],
  ["client.ts", "client.ts", "support", 0o644],
  ["guest-source.ts", "guest-source.ts", "support", 0o644],
  ["sync-native.ts", "sync-native.ts", "support", 0o644],
  ["sync_worktree.ts", "sync_worktree.ts", "support", 0o755],
]);

export const PRODUCT_LINUX_LEGACY_ARTIFACTS: readonly ArtifactTuple[] = Object.freeze([
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

export const PRODUCT_LINUX_ARTIFACTS: readonly ArtifactTuple[] = Object.freeze([
  ...PRODUCT_LINUX_LEGACY_ARTIFACTS,
  ["unit:agent-vm-web.service", "systemd/agent-vm-web.service", "unit", 0o644],
  ["web-config-example", "config/agent-vm-web.json.example", "support", 0o640],
  ["web-assets-manifest", "web-assets.manifest.json", "support", 0o644],
]);

export type ProductTarget =
  | { os: "source"; arch: "any" }
  | { os: "linux"; arch: "amd64" | "arm64" };

export interface ProductManifest {
  format: 3 | 4;
  version: string;
  source_revision: string;
  target: ProductTarget;
  toolchain: { bun: string; go: string; cgo_enabled: false };
  protocol: 4;
  artifacts: Artifact[];
  actions: Record<string, OperationSpec>;
  client_view: {
    version: string;
    source_revision: string;
    protocol: 4;
    transport: "legacy-bun" | "go-request";
    actions: Record<string, OperationSpec>;
  };
  state_compatibility?: {
    reads: string[];
    writes: string[];
  };
}

export function is_product_manifest(value: unknown): value is ProductManifest {
  return is_record(value) && (value.format === LEGACY_PRODUCT_MANIFEST_FORMAT || value.format === PRODUCT_MANIFEST_FORMAT);
}

function _product_error(code: string, message: string, detail: unknown = null): ManifestError {
  return new ManifestError(code, message, detail);
}

function _product_target(value: unknown): ProductTarget {
  const target = _require_mapping(value, "target");
  if (!_same_keys(target, ["os", "arch"])) throw _product_error("manifest_invalid", "target has an unexpected shape");
  if (target.os === "source" && target.arch === "any") return { os: "source", arch: "any" };
  if (target.os === "linux" && (target.arch === "amd64" || target.arch === "arm64")) return { os: "linux", arch: target.arch };
  throw _product_error("manifest_invalid", "target must be source/any or linux/amd64|arm64");
}

function _product_artifacts(value: unknown, target: ProductTarget, format: 3 | 4): Artifact[] {
  if (!Array.isArray(value) || value.length === 0) throw _product_error("manifest_invalid", "artifacts must be a non-empty list");
  const names = new Set<string>();
  const paths = new Set<string>();
  const artifacts = value.map((raw, index) => {
    const item = _require_mapping(raw, `artifacts[${index}]`);
    if (!_same_keys(item, ["name", "path", "role", "sha256", "mode"])) throw _product_error("manifest_invalid", `artifacts[${index}] has an unexpected shape`);
    const name = _require_string(item.name, `artifacts[${index}].name`, /^[A-Za-z0-9_.:@-]{1,120}$/);
    const path = _safe_relative_path(item.path, `artifacts[${index}].path`);
    const role = _require_string(item.role, `artifacts[${index}].role`, /^(client|control|support|unit)$/) as ArtifactRole;
    const sha256 = _require_string(item.sha256, `artifacts[${index}].sha256`, SHA256_RE);
    const mode = Number(_require_int(item.mode, `artifacts[${index}].mode`, 0, 0o777));
    if (path === MANIFEST_NAME || path === CLIENT_SIDECAR_NAME || names.has(name) || paths.has(path)) throw _product_error("manifest_invalid", "product artifact names and paths must be unique and cannot self-reference");
    names.add(name); paths.add(path);
    return { name, path, role, sha256, mode };
  });
  const expected = target.os === "source"
    ? PRODUCT_CLIENT_ARTIFACTS
    : format === LEGACY_PRODUCT_MANIFEST_FORMAT ? PRODUCT_LINUX_LEGACY_ARTIFACTS : PRODUCT_LINUX_ARTIFACTS;
  const actualShape = artifacts.map(({ name, path, role, mode }) => canonical_json([name, path, role, mode])).sort();
  const expectedShape = expected.map((item) => canonical_json(item)).sort();
  if (actualShape.length !== expectedShape.length || actualShape.some((item, index) => item !== expectedShape[index])) throw _product_error("manifest_artifact_set_invalid", "product artifact set differs from the fixed target allowlist");
  return artifacts;
}

function _product_actions(value: unknown): Record<string, OperationSpec> {
  const raw = _require_mapping(value, "actions");
  if (!_same_keys(raw, ACTION_NAMES)) throw _product_error("manifest_action_set_invalid", "product action set differs from protocol 4");
  const actions: Record<string, OperationSpec> = {};
  for (const action of ACTION_NAMES) {
    const spec = _require_mapping(raw[action], `actions.${action}`);
    if (!_same_keys(spec, ["request", "response", "client_to_control", "control_to_client"])) throw _product_error("manifest_invalid", `actions.${action} has an unexpected shape`);
    const request = _schema(spec.request, `actions.${action}.request`);
    const response = _schema(spec.response, `actions.${action}.response`);
    const client_to_control = _window(spec.client_to_control, `actions.${action}.client_to_control`);
    const control_to_client = _window(spec.control_to_client, `actions.${action}.control_to_client`);
    if (request.schema < client_to_control.schema_min || request.schema > client_to_control.schema_max || request.semantic_revision < client_to_control.semantic_min || request.semantic_revision > client_to_control.semantic_max || response.schema < control_to_client.schema_min || response.schema > control_to_client.schema_max || response.semantic_revision < control_to_client.semantic_min || response.semantic_revision > control_to_client.semantic_max) throw _product_error("manifest_invalid", `actions.${action} is outside its compatibility window`);
    actions[action] = { request, response, client_to_control, control_to_client };
  }
  return actions;
}

export function build_product_manifest(
  version: string,
  source_revision: string,
  artifacts: readonly Artifact[],
  options: { target?: ProductTarget; transport?: "legacy-bun" | "go-request"; operations?: Record<string, OperationSpec> } = {},
): ProductManifest {
  parseSemVer(version);
  if (!_fullmatch(SOURCE_REVISION_RE, source_revision)) throw _product_error("manifest_invalid", "source_revision must be a Git revision");
  const actions = options.operations ?? default_operations();
  const transport = options.transport ?? "go-request";
  return validate_product_manifest({
    format: PRODUCT_MANIFEST_FORMAT,
    version,
    source_revision,
    target: options.target ?? { os: "source", arch: "any" },
    toolchain: PRODUCT_TOOLCHAIN,
    protocol: PROTOCOL_VERSION,
    artifacts: artifacts.map((item) => ({ ...item })),
    actions,
    client_view: { version, source_revision, protocol: PROTOCOL_VERSION, transport, actions },
    state_compatibility: { reads: [...STATE_CAPABILITY_MARKERS], writes: [...STATE_CAPABILITY_MARKERS] },
  });
}

export function validate_product_manifest(value: unknown): ProductManifest {
  const root = _require_mapping(value, "manifest");
  const format = Number(_require_int(root.format, "format", 1)) as 3 | 4;
  if (format !== LEGACY_PRODUCT_MANIFEST_FORMAT && format !== PRODUCT_MANIFEST_FORMAT) throw _product_error("manifest_format_unsupported", "manifest format must be 3 or 4");
  const required = format === PRODUCT_MANIFEST_FORMAT
    ? ["actions", "artifacts", "client_view", "format", "protocol", "source_revision", "state_compatibility", "target", "toolchain", "version"]
    : ["actions", "artifacts", "client_view", "format", "protocol", "source_revision", "target", "toolchain", "version"];
  if (!_same_keys(root, required)) throw _product_error("manifest_invalid", "product manifest has an unexpected shape");
  const version = _require_string(root.version, "version");
  try { parseSemVer(version); } catch { throw _product_error("manifest_invalid", "version must be valid SemVer 2.0.0"); }
  const source_revision = _require_string(root.source_revision, "source_revision", SOURCE_REVISION_RE);
  const target = _product_target(root.target);
  const toolchain = _require_mapping(root.toolchain, "toolchain");
  if (!_same_keys(toolchain, ["bun", "cgo_enabled", "go"]) || toolchain.bun !== PRODUCT_TOOLCHAIN.bun || toolchain.go !== PRODUCT_TOOLCHAIN.go || toolchain.cgo_enabled !== false) throw _product_error("manifest_invalid", "toolchain does not match the fixed release toolchain");
  if (Number(_require_int(root.protocol, "protocol", 1)) !== PROTOCOL_VERSION) throw _product_error("protocol_unsupported", `manifest protocol must be ${PROTOCOL_VERSION}`);
  const artifacts = _product_artifacts(root.artifacts, target, format);
  const actions = _product_actions(root.actions);
  const view = _require_mapping(root.client_view, "client_view");
  if (!_same_keys(view, ["actions", "protocol", "source_revision", "transport", "version"]) || view.version !== version || view.source_revision !== source_revision || Number(view.protocol) !== PROTOCOL_VERSION || (view.transport !== "legacy-bun" && view.transport !== "go-request") || !_json_equal(view.actions, actions)) throw _product_error("manifest_invalid", "client_view does not match product manifest");
  const result: ProductManifest = { format, version, source_revision, target, toolchain: { bun: PRODUCT_TOOLCHAIN.bun, go: PRODUCT_TOOLCHAIN.go, cgo_enabled: false }, protocol: 4, artifacts, actions, client_view: { version, source_revision, protocol: 4, transport: view.transport, actions } };
  if (format === PRODUCT_MANIFEST_FORMAT) {
    const compatibility = _require_mapping(root.state_compatibility, "state_compatibility");
    if (!_same_keys(compatibility, ["reads", "writes"])) throw _product_error("manifest_invalid", "state_compatibility has an unexpected shape");
    for (const key of ["reads", "writes"] as const) {
      const values = compatibility[key];
      if (!Array.isArray(values) || values.length !== STATE_CAPABILITY_MARKERS.length || values.some((item) => typeof item !== "string") || values.join("\0") !== [...STATE_CAPABILITY_MARKERS].join("\0")) throw _product_error("manifest_state_compatibility_invalid", `state_compatibility.${key} must declare all state capability markers in order`);
    }
    result.state_compatibility = { reads: [...STATE_CAPABILITY_MARKERS], writes: [...STATE_CAPABILITY_MARKERS] };
  }
  return result;
}

function _verify_product_elf(path: string, target: ProductTarget): void {
  if (target.os !== "linux") return;
  const bytes = readFileSync(path);
  if (bytes.length < 20 || bytes[0] !== 0x7f || bytes[1] !== 0x45 || bytes[2] !== 0x4c || bytes[3] !== 0x46 || bytes[4] !== 2 || bytes[5] !== 1) throw _product_error("binary_format_mismatch", "Go control artifact must be little-endian ELF64");
  const machine = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength).getUint16(18, true);
  const expected = target.arch === "amd64" ? 62 : 183;
  if (machine !== expected) throw _product_error("binary_arch_mismatch", "Go control artifact architecture does not match the manifest target");
}

export function verify_product_bundle(root: string, manifest: unknown): ProductManifest {
  const checked = validate_product_manifest(manifest);
  for (const item of checked.artifacts) {
    const path = join(root, ...item.path.split("/"));
    let info: ReturnType<typeof lstatSync>;
    try { info = lstatSync(path); } catch { throw _product_error("artifact_missing", `artifact is missing: ${item.path}`); }
    if (!info.isFile() || info.isSymbolicLink()) throw _product_error("artifact_unverified", `artifact is not a regular file: ${item.path}`);
    if ((info.mode & 0o7777) !== item.mode) throw _product_error("artifact_mode_mismatch", `artifact mode differs: ${item.path}`);
    if (sha256_file(path) !== item.sha256) throw _product_error("artifact_checksum_mismatch", `artifact checksum differs: ${item.path}`);
    if (item.path === "bin/agent-vm-control") _verify_product_elf(path, checked.target);
  }
  return checked;
}
