export type Locale = "zh-CN" | "en";
export type PageKey = "overview" | "vms" | "images" | "operations" | "system";

export type ObservationState = "available" | "stale" | "missing" | "failed" | "unverified";
export type VMState = "preparing" | "active" | "cached" | "closing" | "deleting" | "failed" | "absent";
export type LeaseState = "effective" | "expired" | "absent" | "unknown";

export interface ObservedNumber {
  state: ObservationState;
  value: number | null;
  observed_at: string | null;
}

export interface ObservedBytes {
  state: ObservationState;
  value_bytes: string | null;
  observed_at: string | null;
}

export interface ObservedBoolean {
  state: ObservationState;
  value: boolean | null;
  observed_at: string | null;
}

export interface SourceFreshness {
  name: string;
  state: ObservationState;
  observed_at: string | null;
  stale_after_seconds: number;
  safe_code: string | null;
}

export interface Overview {
  resource_version: string;
  observed_at: string;
  admission: { ready: boolean | null; draining: boolean | null; reasons: string[] };
  capacity: {
    logical_cpu_remaining_vcpu: ObservedNumber;
    ct_cpu_capacity_vcpu: ObservedNumber;
    logical_memory_remaining: ObservedBytes;
    ct_memory_available: ObservedBytes;
    physical_disk_available: ObservedBytes;
  };
  pressure: {
    cpu_some_avg10: ObservedNumber;
    cpu_full_avg10: ObservedNumber;
    cpu_startup_pressure_threshold_percent: ObservedNumber;
    cpu_startup_blocked: ObservedBoolean;
    memory_pressure: ObservedBoolean;
  };
  task_counts: Record<"active" | "preparing" | "cached" | "closing" | "deleting" | "failed" | "absent", number>;
  sources: SourceFreshness[];
}

export interface ManagedVM {
  task_id: string;
  session_id: string;
  generation: string;
  resource_version: string;
  state: VMState;
  native_state: "running" | "stopped" | "absent" | "transitional" | "unknown";
  guest_availability: "ready" | "unavailable" | "unknown";
  image: { fingerprint: string; version_label: string | null };
  resources: { cpu_vcpu: number | null; memory_bytes: string | null; disk_bytes: string | null };
  lease: { state: LeaseState; expires_at: string | null };
  port_mappings: Array<{ name: string; protocol: "tcp" | "udp"; host_port: number; guest_port: number }>;
  last_error: SafeError | null;
  updated_at: string;
  allowed_actions: Array<"restart" | "resize" | "release" | "destroy" | "port_add" | "port_remove">;
}

export interface ImageReferenceCounts {
  default: number;
  active: number;
  cached: number;
  pending_creation: number;
  verification: number;
}

export interface ManagedImage {
  fingerprint: string;
  resource_version: string;
  version_label: string | null;
  architecture: string;
  instance_type: "container";
  source: { kind: "trusted_remote" | "local_upload" | "legacy_config"; source_id: string | null };
  state: "available" | "verifying" | "retired" | "failed";
  verification: {
    integrity: "not_checked" | "running" | "passed" | "failed" | "legacy_unverified";
    suitability: "not_assessed" | "running" | "passed" | "failed";
    findings: string[];
  };
  is_default: boolean;
  default_compatibility: "none" | "legacy_unverified";
  references: ImageReferenceCounts;
  created_at: string;
}

export type OperationStatus = "awaiting_upload" | "queued" | "running" | "reconciling" | "succeeded" | "failed" | "outcome_unknown";
export type OperationPhase = "queued" | "awaiting_upload" | "upload" | "integrity_check" | "image_import" | "suitability_check" | "reference_check" | "native_action" | "reconciliation" | "cleanup" | "completed";
export type ManagementAction = "restart" | "resize" | "release" | "destroy" | "port_add" | "port_remove" | "image_import" | "default_image_select" | "image_retire" | "image_delete" | "drain_enter" | "drain_exit" | "reconcile" | "gc";

export interface SafeError {
  code: string;
  phase: string | null;
  retryable: boolean;
  operation_id?: string | null;
  safe_fields?: {
    current_target_version?: string | null;
    maximum_upload_bytes?: string | null;
    affected_session_count?: number | null;
    retry_after_seconds?: number | null;
  };
}

export interface Operation {
  operation_id: string;
  action: ManagementAction;
  status: OperationStatus;
  phase: OperationPhase;
  target: { kind: "vm" | "image" | "image_catalog" | "system"; id: string };
  actor: string;
  reason: string;
  created_at: string;
  updated_at: string;
  result: { image_fingerprint?: string | null; default_fingerprint?: string | null; affected_session_count?: number | null; effect?: string | null } | null;
  error: SafeError | null;
}

export interface AuditEvent {
  event_id: string;
  operation_id: string;
  action: ManagementAction;
  actor: string;
  reason: string;
  target: { kind: "vm" | "image" | "image_catalog" | "system"; id: string };
  outcome: "accepted" | "rejected" | "succeeded" | "failed" | "outcome_unknown";
  phase: OperationPhase;
  occurred_at: string;
  safe_code: string | null;
}

export interface SystemStatus {
  product_version: string;
  resource_version: string;
  state_capabilities: string[];
  maintenance: { draining: boolean | null; reconciliation_pending: number | null; garbage_collection_pending: number | null };
  components: Array<{ name: "web" | "authority" | "observer"; state: "ready" | "degraded" | "unavailable" | "unknown"; version: string | null; observed_at: string | null }>;
  sources: SourceFreshness[];
}

export interface ActionPreview {
  target_version: string;
  confirmation_token: string;
  expires_at: string;
  impact: {
    summary_code: string;
    affected_session_count: number;
    affected_image_references: ImageReferenceCounts;
    changes: Array<{ code: string; count: number | null; value: string | null }>;
  };
}

export interface ImageImportRequest {
  operation_id: string;
  reason: string;
  target_version: string;
  version_label?: string;
  source:
    | { kind: "trusted_remote"; source_id: string; expected_fingerprint: string }
    | { kind: "local_upload"; format: "unified"; expected_sha256: string; expected_size_bytes: string }
    | { kind: "local_upload"; format: "split"; metadata: { expected_sha256: string; expected_size_bytes: string }; rootfs: { expected_sha256: string; expected_size_bytes: string } };
}
