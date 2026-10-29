import type { AgentToken, AuditEvent, ImageReferenceCounts, ManagedImage, ManagedVM, ManagementSession, Operation, Overview, SystemStatus } from "./types";

export const session: ManagementSession = {
  issuer: "https://issuer.example.invalid",
  subject: "admin-maintainer",
  role: "administrator",
  capabilities: ["read", "operate", "manage_images", "maintain_system", "manage_tokens", "observe_admin"],
  csrf_token: "synthetic-csrf-token-0123456789abcdef0123456789abcdef",
  expires_at: "2027-01-15T11:20:30Z",
};

export const overview: Overview = {
  resource_version: "overview:synthetic-17",
  observed_at: "2026-01-15T10:20:30Z",
  admission: { ready: true, draining: false, reasons: [] },
  capacity: {
    logical_cpu_remaining_vcpu: { state: "available", value: 6, observed_at: "2026-01-15T10:20:28Z" },
    ct_cpu_capacity_vcpu: { state: "available", value: 16, observed_at: "2026-01-15T10:20:28Z" },
    logical_memory_remaining: { state: "available", value_bytes: "8589934592", observed_at: "2026-01-15T10:20:28Z" },
    ct_memory_available: { state: "available", value_bytes: "17179869184", observed_at: "2026-01-15T10:20:28Z" },
    physical_disk_available: { state: "stale", value_bytes: null, observed_at: "2026-01-15T10:00:00Z" },
  },
  pressure: {
    cpu_some_avg10: { state: "available", value: 0.2, observed_at: "2026-01-15T10:20:28Z" },
    cpu_full_avg10: { state: "unverified", value: null, observed_at: null },
    cpu_startup_pressure_threshold_percent: { state: "available", value: 50, observed_at: "2026-01-15T10:20:28Z" },
    cpu_startup_blocked: { state: "unverified", value: null, observed_at: null },
    memory_pressure: { state: "available", value: false, observed_at: "2026-01-15T10:20:28Z" },
  },
  task_counts: { active: 4, preparing: 0, cached: 2, closing: 0, deleting: 0, failed: 1, absent: 0 },
  sources: [
    { name: "capacity", state: "stale", observed_at: "2026-01-15T10:00:00Z", stale_after_seconds: 600, safe_code: "observation_stale" },
    { name: "inventory", state: "available", observed_at: "2026-01-15T10:20:29Z", stale_after_seconds: 30, safe_code: null },
    { name: "pressure", state: "unverified", observed_at: null, stale_after_seconds: 30, safe_code: "observation_unverified" },
  ],
};

const baseVM = {
  session_id: "session_example_01",
  generation: "42",
  resource_version: "vm:task_example_01:revision-9",
  state: "active" as const,
  native_state: "running" as const,
  guest_availability: "ready" as const,
  image: { fingerprint: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", version_label: "2026.01" },
  resources: { cpu_vcpu: 4, memory_bytes: "8589934592", disk_bytes: "53687091200" },
  lease: { state: "effective" as const, expires_at: "2026-01-15T11:20:00Z" },
  port_mappings: [{ name: "preview", protocol: "tcp" as const, host_port: 24001, guest_port: 3000 }],
  last_error: null,
  updated_at: "2026-01-15T10:19:00Z",
  allowed_actions: ["restart", "resize", "release", "destroy", "port_add", "port_remove"] as ManagedVM["allowed_actions"],
};

export const vms: ManagedVM[] = [
  { ...baseVM, task_id: "task_example_01" },
  { ...baseVM, task_id: "task_aurora_02", session_id: "session_aurora_02", generation: "18", resource_version: "vm:task_aurora_02:revision-4", state: "active", image: { fingerprint: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", version_label: "legacy-default" }, resources: { cpu_vcpu: 2, memory_bytes: "4294967296", disk_bytes: "32212254720" }, lease: { state: "effective", expires_at: "2026-01-15T10:44:00Z" }, port_mappings: [{ name: "preview", protocol: "tcp", host_port: 24002, guest_port: 3000 }] },
  { ...baseVM, task_id: "task_cedar_03", session_id: "session_cedar_03", generation: "7", resource_version: "vm:task_cedar_03:revision-12", state: "cached", native_state: "stopped", guest_availability: "unavailable", lease: { state: "absent", expires_at: null }, resources: { cpu_vcpu: 1, memory_bytes: "2147483648", disk_bytes: "21474836480" }, port_mappings: [] },
  { ...baseVM, task_id: "task_delta_04", session_id: "session_delta_04", generation: "33", resource_version: "vm:task_delta_04:revision-8", state: "failed", native_state: "unknown", guest_availability: "unknown", lease: { state: "expired", expires_at: "2026-01-14T22:10:00Z" }, last_error: { code: "authority_unavailable", phase: "native_action", retryable: true }, port_mappings: [] },
  { ...baseVM, task_id: "task_echo_05", session_id: "session_echo_05", generation: "11", resource_version: "vm:task_echo_05:revision-3", state: "preparing", native_state: "transitional", guest_availability: "unavailable", image: { fingerprint: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", version_label: "2026.01" }, lease: { state: "unknown", expires_at: null }, resources: { cpu_vcpu: 2, memory_bytes: "4294967296", disk_bytes: "21474836480" }, port_mappings: [] },
];

export const images: ManagedImage[] = [
  { fingerprint: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", resource_version: "image:bbbb:revision-3", version_label: "legacy-default", architecture: "x86_64", instance_type: "container", source: { kind: "legacy_config", source_id: null }, state: "available", verification: { integrity: "legacy_unverified", suitability: "not_assessed", findings: [] }, is_default: true, default_compatibility: "legacy_unverified", references: { default: 1, active: 1, cached: 1, releasing: 0, pending_creation: 0, verification: 0 }, created_at: "2025-10-01T00:00:00Z" },
  { fingerprint: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", resource_version: "image:aaaa:revision-1", version_label: "2026.01", architecture: "x86_64", instance_type: "container", source: { kind: "local_upload", source_id: null }, state: "available", verification: { integrity: "passed", suitability: "passed", findings: [] }, is_default: false, default_compatibility: "none", references: { default: 0, active: 0, cached: 0, releasing: 0, pending_creation: 0, verification: 0 }, created_at: "2026-01-15T09:55:00Z" },
  { fingerprint: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", resource_version: "image:cccc:revision-2", version_label: "2025.12-maintenance", architecture: "x86_64", instance_type: "container", source: { kind: "trusted_remote", source_id: "ubuntu-staging" }, state: "retired", verification: { integrity: "passed", suitability: "passed", findings: [] }, is_default: false, default_compatibility: "none", references: { default: 0, active: 0, cached: 0, releasing: 0, pending_creation: 0, verification: 0 }, created_at: "2025-12-16T08:30:00Z" },
];

const administratorActor = { issuer: session.issuer, subject: session.subject, role: "administrator" as const };

export const operations: Operation[] = [
  { operation_id: "50e8400e-ccf8-4c7b-8d1d-14f66c7fb125", action: "image_import", status: "running", phase: "suitability_check", target: { kind: "image_catalog", id: "image-catalog" }, actor: administratorActor, reason: "Evaluate the next approved session image", created_at: "2026-01-15T09:55:00Z", updated_at: "2026-01-15T10:05:00Z", result: null, error: null },
  { operation_id: "50e8400e-ccf8-4c7b-8d1d-14f66c7fb124", action: "restart", status: "succeeded", phase: "completed", target: { kind: "vm", id: "task_example_01" }, actor: administratorActor, reason: "Restore service after a confirmed guest-side fault", created_at: "2026-01-15T09:40:00Z", updated_at: "2026-01-15T09:43:00Z", result: { effect: "completed", affected_session_count: 1 }, error: null },
  { operation_id: "50e8400e-ccf8-4c7b-8d1d-14f66c7fb128", action: "destroy", status: "failed", phase: "reference_check", target: { kind: "vm", id: "task_delta_04" }, actor: administratorActor, reason: "Remove failed lease after stale target review", created_at: "2026-01-15T09:15:00Z", updated_at: "2026-01-15T09:16:00Z", result: null, error: { code: "stale_target", phase: "precondition", retryable: false, safe_fields: { current_target_version: "vm:task_delta_04:revision-9" } } },
  { operation_id: "50e8400e-ccf8-4c7b-8d1d-14f66c7fb129", action: "reconcile", status: "outcome_unknown", phase: "reconciliation", target: { kind: "system", id: "system" }, actor: administratorActor, reason: "Confirm state after authority restart", created_at: "2026-01-15T08:55:00Z", updated_at: "2026-01-15T08:57:00Z", result: null, error: { code: "authority_unavailable", phase: "reconciliation", retryable: true } },
];

export const audits: AuditEvent[] = [
  { event_id: "50e8400e-ccf8-4c7b-8d1d-14f66c7fb126", operation_id: "50e8400e-ccf8-4c7b-8d1d-14f66c7fb124", action: "restart", actor: administratorActor, reason: "Restore service after a confirmed guest-side fault", target: { kind: "vm", id: "task_example_01" }, outcome: "accepted", phase: "queued", occurred_at: "2026-01-15T10:10:00Z", safe_code: null },
  { event_id: "50e8400e-ccf8-4c7b-8d1d-14f66c7fb130", operation_id: "50e8400e-ccf8-4c7b-8d1d-14f66c7fb128", action: "destroy", actor: administratorActor, reason: "Remove failed lease after stale target review", target: { kind: "vm", id: "task_delta_04" }, outcome: "failed", phase: "reference_check", occurred_at: "2026-01-15T09:16:00Z", safe_code: "stale_target" },
  { event_id: "50e8400e-ccf8-4c7b-8d1d-14f66c7fb131", operation_id: "50e8400e-ccf8-4c7b-8d1d-14f66c7fb129", action: "reconcile", actor: administratorActor, reason: "Confirm state after authority restart", target: { kind: "system", id: "system" }, outcome: "outcome_unknown", phase: "reconciliation", occurred_at: "2026-01-15T08:57:00Z", safe_code: "authority_unavailable" },
];

export const system: SystemStatus = {
  product_version: "0.0.0+synthetic",
  resource_version: "system:revision-4",
  state_capabilities: ["legacy-agent-vm-core@1", "managed-image-catalog@1", "management-operation-audit@1", "session-image-binding@1"],
  maintenance: { draining: false, reconciliation_pending: 0, garbage_collection_pending: 0 },
  components: [
    { name: "web", state: "ready", version: "0.0.0+synthetic", observed_at: "2026-01-15T10:20:30Z" },
    { name: "authority", state: "ready", version: "0.0.0+synthetic", observed_at: "2026-01-15T10:20:30Z" },
    { name: "observer", state: "degraded", version: null, observed_at: "2026-01-15T10:00:00Z" },
  ],
  sources: [{ name: "health", state: "available", observed_at: "2026-01-15T10:20:30Z", stale_after_seconds: 30, safe_code: null }],
};

export const emptyReferences: ImageReferenceCounts = { default: 0, active: 0, cached: 0, releasing: 0, pending_creation: 0, verification: 0 };

export const tokens: AgentToken[] = [
  { token_id: "tok_read_01_7d9e22b1", resource_version: "token:read:revision-2", scopes: ["agent:read"], created_at: "2026-10-10T08:00:00Z", expires_at: "2027-02-10T08:00:00Z", revoked_at: null },
  { token_id: "tok_ops_02_4f1c6a77", resource_version: "token:ops:revision-5", scopes: ["agent:read", "agent:operate"], created_at: "2026-09-20T08:00:00Z", expires_at: "2027-01-20T08:00:00Z", revoked_at: "2026-10-14T18:30:00Z" },
];

export const mockState = { overview, vms, images, operations, audits, system, session, tokens };
