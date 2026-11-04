import { useState } from "react";

const recoveryPrefix = "agent-vm:operation-recovery:";
const recoveryLifetimeMs = 24 * 60 * 60 * 1000;

type StoredRecovery = {
  operationId: string;
  storedAt: number;
};

function storage(): Storage | null {
  if (typeof window === "undefined") return null;
  try {
    return window.sessionStorage;
  } catch {
    return null;
  }
}

function key(path: string): string {
  return `${recoveryPrefix}${path}`;
}

function isOperationId(value: unknown): value is string {
  return typeof value === "string" && /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(value);
}

export function readOperationRecovery(path: string): string | null {
  const store = storage();
  if (!store) return null;
  try {
    const raw = store.getItem(key(path));
    if (!raw) return null;
    const parsed = JSON.parse(raw) as Partial<StoredRecovery>;
    if (!isOperationId(parsed.operationId) || typeof parsed.storedAt !== "number" || Date.now() - parsed.storedAt > recoveryLifetimeMs) {
      store.removeItem(key(path));
      return null;
    }
    return parsed.operationId;
  } catch {
    return null;
  }
}

export function writeOperationRecovery(path: string, operationId: string | null): void {
  const store = storage();
  if (!store) return;
  try {
    if (!operationId) {
      store.removeItem(key(path));
      return;
    }
    store.setItem(key(path), JSON.stringify({ operationId, storedAt: Date.now() } satisfies StoredRecovery));
  } catch {
    // Session storage is an enhancement; in-memory operation behavior remains valid when unavailable.
  }
}

export function useOperationRecovery(path: string): [string | null, (operationId: string | null) => void] {
  const [operationId, setOperationIdState] = useState<string | null>(() => readOperationRecovery(path));
  const setOperationId = (next: string | null) => {
    setOperationIdState(next);
    writeOperationRecovery(path, next);
  };
  return [operationId, setOperationId];
}
