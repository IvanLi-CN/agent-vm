import { useSyncExternalStore, type ReactNode } from "react";
import { mockState, type MockState } from "../app/mockData";

export type DemoScene = "normal" | "capacity" | "conflict" | "empty";
export type DemoPersona = "admin" | "viewer";
export type DemoNetwork = "online" | "offline";
export type DemoDataMode = "full" | "minimal";
export type DemoForcedOutcome = "succeeded" | "failed" | "outcome_unknown";

export interface DemoRuntimeState {
  scene: DemoScene;
  persona: DemoPersona;
  network: DemoNetwork;
  dataMode: DemoDataMode;
  forcedOutcome: DemoForcedOutcome;
  model: MockState;
  revision: number;
  bootstrapError: string | null;
}

const listeners = new Set<() => void>();
let runtime: DemoRuntimeState | null = null;

const isDemoBuild = import.meta.env.MODE === "demo";

function cloneModel(): MockState {
  return structuredClone(mockState) as MockState;
}

function readParam(name: string): string | null {
  if (typeof window === "undefined") return null;
  return new URLSearchParams(window.location.search).get(name);
}

function validScene(value: string | null): DemoScene {
  return value === "capacity" || value === "conflict" || value === "empty" ? value : "normal";
}

function validPersona(value: string | null): DemoPersona {
  return value === "viewer" ? "viewer" : "admin";
}

function validNetwork(value: string | null): DemoNetwork {
  return value === "offline" ? "offline" : "online";
}

function validDataMode(value: string | null): DemoDataMode {
  return value === "minimal" ? "minimal" : "full";
}

export function modelForScene(scene: DemoScene, dataMode: DemoDataMode = "full"): MockState {
  const model = cloneModel();
  if (scene === "capacity") {
    model.overview.admission = { ready: false, draining: false, reasons: ["physical_disk_staging_low", "cpu_startup_pressure"] };
    model.overview.capacity.logical_cpu_remaining_vcpu = { state: "failed", value: null, observed_at: null };
    model.overview.capacity.physical_disk_available = { state: "stale", value_bytes: null, observed_at: null };
    model.overview.pressure.cpu_startup_blocked = { state: "available", value: true, observed_at: model.overview.observed_at };
    model.overview.task_counts.failed = 3;
    model.overview.task_counts.preparing = 2;
    model.images[0].verification.findings = ["staging_capacity_low", "physical_pool_observation_stale"];
  }
  if (scene === "conflict") {
    model.operations.unshift({
      operation_id: "50e8400e-ccf8-4c7b-8d1d-14f66c7fb199",
      action: "destroy",
      status: "outcome_unknown",
      phase: "native_action",
      target: { kind: "vm", id: "task_delta_04" },
      actor: "admin@example.invalid",
      reason: "Simulate an interrupted native action",
      created_at: model.overview.observed_at,
      updated_at: model.overview.observed_at,
      result: null,
      error: { code: "authority_unavailable", phase: "native_action", retryable: true },
    });
  }
  if (scene === "empty") {
    model.vms = [];
    model.images = [];
    model.operations = [];
    model.audits = [];
    model.overview.task_counts = { active: 0, preparing: 0, cached: 0, closing: 0, deleting: 0, failed: 0, absent: 0 };
    model.overview.admission = { ready: true, draining: false, reasons: [] };
  }
  if (dataMode === "minimal" && scene !== "empty") {
    model.operations = model.operations.slice(0, 2);
    model.audits = model.audits.slice(0, 2);
  }
  return model;
}

function createRuntime(): DemoRuntimeState {
  const scene = validScene(readParam("demo"));
  const persona = validPersona(readParam("d_persona"));
  const network = validNetwork(readParam("d_network"));
  const dataMode = validDataMode(readParam("d_data"));
  return { scene, persona, network, dataMode, forcedOutcome: scene === "conflict" ? "outcome_unknown" : "succeeded", model: modelForScene(scene, dataMode), revision: 0, bootstrapError: readParam("d_bootstrap") === "fail" ? "demo_bootstrap_failed" : null };
}

export function isDemoRuntime(): boolean {
  return isDemoBuild;
}

export function getDemoRuntime(): DemoRuntimeState {
  if (!runtime) runtime = createRuntime();
  return runtime;
}

export function getMockState(): MockState {
  return isDemoBuild ? getDemoRuntime().model : mockState;
}

export function emitDemoChange(): void {
  if (!isDemoBuild || !runtime) return;
  runtime = { ...runtime, revision: runtime.revision + 1 };
  listeners.forEach((listener) => listener());
  if (typeof window !== "undefined") window.dispatchEvent(new CustomEvent("agent-vm-demo-change"));
}

export function setDemoControl(control: Partial<Pick<DemoRuntimeState, "scene" | "persona" | "network" | "dataMode">>): void {
  const current = getDemoRuntime();
  const next = {
    scene: control.scene ?? current.scene,
    persona: control.persona ?? current.persona,
    network: control.network ?? current.network,
    dataMode: control.dataMode ?? current.dataMode,
  };
  runtime = { ...current, ...next, forcedOutcome: next.scene === "conflict" ? "outcome_unknown" : "succeeded", model: modelForScene(next.scene, next.dataMode), revision: current.revision + 1, bootstrapError: null };
  syncShareParams(runtime);
  listeners.forEach((listener) => listener());
  if (typeof window !== "undefined") window.dispatchEvent(new CustomEvent("agent-vm-demo-change"));
}

export function setDemoBootstrapError(message: string | null): void {
  const current = getDemoRuntime();
  runtime = { ...current, bootstrapError: message };
  listeners.forEach((listener) => listener());
}

export function canMutateDemo(): boolean {
  const current = getDemoRuntime();
  return current.persona === "admin" && current.network === "online";
}

export function demoMutationError(): string | null {
  const current = getDemoRuntime();
  if (current.network === "offline") return "network_unavailable";
  if (current.persona !== "admin") return "permission_denied";
  return null;
}

export function demoForcedOutcome(): DemoForcedOutcome {
  return getDemoRuntime().forcedOutcome;
}

function syncShareParams(current: DemoRuntimeState): void {
  if (typeof window === "undefined" || !isDemoBuild) return;
  const url = new URL(window.location.href);
  url.searchParams.set("demo", current.scene);
  url.searchParams.set("d_persona", current.persona);
  url.searchParams.set("d_network", current.network);
  url.searchParams.set("d_data", current.dataMode);
  window.history.replaceState({}, "", url);
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

function getSnapshot(): DemoRuntimeState {
  return getDemoRuntime();
}

export function useDemoRuntime(): DemoRuntimeState {
  return useSyncExternalStore(subscribe, getSnapshot, getSnapshot);
}

export function DemoProvider({ children }: { children: ReactNode }): ReactNode {
  return children;
}

export function demoShareUrl(): string {
  return typeof window === "undefined" ? "" : window.location.href;
}
