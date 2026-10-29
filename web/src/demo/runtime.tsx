import { useSyncExternalStore, type ReactNode } from "react";
import { mockState, type MockState } from "../app/mockData";

export type DemoScene = "normal" | "capacity" | "conflict" | "empty";
export type DemoPersona = "admin" | "viewer";
export type DemoNetwork = "online" | "offline";
export type DemoDataMode = "full" | "minimal";
export type DemoForcedOutcome = "succeeded" | "failed" | "outcome_unknown";
export type DemoInspectorLayout = "left" | "right" | "floating" | "minimized";

export interface DemoInspectorPosition {
  left: number;
  top: number;
}

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

const inspectorLayoutStorageKey = "agent-vm:demo-inspector-layout";
const inspectorPositionStorageKey = "agent-vm:demo-inspector-position";

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

type DemoUrlSettings = Pick<DemoRuntimeState, "scene" | "persona" | "network" | "dataMode">;

function settingsFromLocation(): DemoUrlSettings {
  return {
    scene: validScene(readParam("demo")),
    persona: validPersona(readParam("d_persona")),
    network: validNetwork(readParam("d_network")),
    dataMode: validDataMode(readParam("d_data")),
  };
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
  const settings = settingsFromLocation();
  return { ...settings, forcedOutcome: settings.scene === "conflict" ? "outcome_unknown" : "succeeded", model: modelForScene(settings.scene, settings.dataMode), revision: 0, bootstrapError: readParam("d_bootstrap") === "fail" ? "demo_bootstrap_failed" : null };
}

export function isDemoRuntime(): boolean {
  return isDemoBuild;
}

export function getDemoInspectorLayout(): DemoInspectorLayout {
  if (typeof window === "undefined") return "floating";
  try {
    const stored = window.localStorage.getItem(inspectorLayoutStorageKey);
    if (stored === "docked") return "right";
    if (stored === "left" || stored === "right" || stored === "floating" || stored === "minimized") return stored;
  } catch {
    // The demo still works when storage is unavailable.
  }
  return (window.matchMedia?.("(min-width: 1280px)")?.matches ?? false) ? "right" : "floating";
}

export function saveDemoInspectorLayout(layout: DemoInspectorLayout): void {
  if (typeof window === "undefined") return;
  try {
    window.localStorage.setItem(inspectorLayoutStorageKey, layout);
  } catch {
    // Local layout persistence is best-effort and must not affect the demo runtime.
  }
}

export function getDemoInspectorPosition(): DemoInspectorPosition | null {
  if (typeof window === "undefined") return null;
  try {
    const stored = window.localStorage.getItem(inspectorPositionStorageKey);
    if (!stored) return null;
    const parsed = JSON.parse(stored) as { left?: unknown; top?: unknown };
    if (typeof parsed.left !== "number" || !Number.isFinite(parsed.left) || typeof parsed.top !== "number" || !Number.isFinite(parsed.top)) return null;
    return { left: parsed.left, top: parsed.top };
  } catch {
    return null;
  }
}

export function saveDemoInspectorPosition(position: DemoInspectorPosition): void {
  if (typeof window === "undefined") return;
  try {
    window.localStorage.setItem(inspectorPositionStorageKey, JSON.stringify(position));
  } catch {
    // Local position persistence is best-effort and must not affect the demo runtime.
  }
}

export function getDemoRuntime(): DemoRuntimeState {
  if (!runtime) {
    runtime = createRuntime();
    syncShareParams(runtime, "replace");
  }
  return runtime;
}

export function getMockState(): MockState {
  return isDemoBuild ? getDemoRuntime().model : mockState;
}

export function emitDemoChange(): void {
  if (!isDemoBuild || !runtime) return;
  runtime = { ...runtime, revision: runtime.revision + 1 };
  notifyRuntimeChange();
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
  notifyRuntimeChange();
}

export function setDemoBootstrapError(message: string | null): void {
  const current = getDemoRuntime();
  runtime = { ...current, bootstrapError: message };
  notifyRuntimeChange();
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

function syncShareParams(current: DemoRuntimeState, historyMode: "push" | "replace" = "push"): void {
  if (typeof window === "undefined" || !isDemoBuild) return;
  const url = new URL(window.location.href);
  url.search = "";
  url.hash = "";
  url.searchParams.set("demo", current.scene);
  url.searchParams.set("d_persona", current.persona);
  url.searchParams.set("d_network", current.network);
  url.searchParams.set("d_data", current.dataMode);
  if (url.toString() !== window.location.href) {
    if (historyMode === "replace") window.history.replaceState({}, "", url);
    else window.history.pushState({}, "", url);
  }
}

function notifyRuntimeChange(): void {
  listeners.forEach((listener) => listener());
  if (typeof window !== "undefined") window.dispatchEvent(new CustomEvent("agent-vm-demo-change"));
}

export function syncDemoRuntimeFromLocation(): void {
  if (!isDemoBuild || !runtime) return;
  const next = settingsFromLocation();
  if (next.scene === runtime.scene && next.persona === runtime.persona && next.network === runtime.network && next.dataMode === runtime.dataMode) return;
  runtime = { ...runtime, ...next, forcedOutcome: next.scene === "conflict" ? "outcome_unknown" : "succeeded", model: modelForScene(next.scene, next.dataMode), revision: runtime.revision + 1, bootstrapError: null };
  notifyRuntimeChange();
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
  if (typeof window === "undefined" || !isDemoBuild) return "";
  const current = getDemoRuntime();
  const url = new URL(window.location.href);
  url.search = "";
  url.hash = "";
  url.searchParams.set("demo", current.scene);
  url.searchParams.set("d_persona", current.persona);
  url.searchParams.set("d_network", current.network);
  url.searchParams.set("d_data", current.dataMode);
  return url.toString();
}

if (typeof window !== "undefined") window.addEventListener("popstate", syncDemoRuntimeFromLocation);
