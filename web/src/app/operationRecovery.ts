import { useCallback, useEffect, useRef, useState } from "react";
import { getOperation, ManagementRequestError } from "./api";
import type { Operation } from "./types";

const recoveryPrefix = "agent-vm:operation-recovery:";
const recoveryLifetimeMs = 24 * 60 * 60 * 1000;

type StoredRecovery = {
  operationId: string;
  storedAt: number;
};

export type OperationRecoveryState = "idle" | "checking" | "resolved" | "blocked" | "not_found" | "error";

export type OperationRecovery = {
  operationId: string | null;
  state: OperationRecoveryState;
  operation: Operation | null;
  errorCode: string | null;
  isBlocked: boolean;
  retry: () => void;
  block: (operation?: Operation | null) => void;
  persist: () => void;
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

export function useOperationRecovery(path: string, enabled = true): [string | null, (operationId: string | null, persist?: boolean) => void, OperationRecovery] {
  const [operationId, setOperationIdState] = useState<string | null>(() => readOperationRecovery(path));
  const shouldReconcile = useRef<boolean | null>(null);
  const wasEnabled = useRef(enabled);
  const [retryCount, setRetryCount] = useState(0);
  const [state, setState] = useState<OperationRecoveryState>(() => operationId ? "checking" : "idle");
  const [operation, setOperation] = useState<Operation | null>(null);
  const [errorCode, setErrorCode] = useState<string | null>(null);

  const setOperationId = useCallback((next: string | null, persist = true) => {
    shouldReconcile.current = false;
    setOperationIdState(next);
    setState("idle");
    setOperation(null);
    setErrorCode(null);
    if (persist) writeOperationRecovery(path, next);
  }, [path]);

  const persist = useCallback(() => {
    if (operationId) writeOperationRecovery(path, operationId);
  }, [operationId, path]);

  const retry = useCallback(() => {
    if (!operationId) return;
    shouldReconcile.current = true;
    setRetryCount((count) => count + 1);
  }, [operationId]);

  const block = useCallback((nextOperation: Operation | null = null) => {
    if (!operationId) return;
    shouldReconcile.current = false;
    setOperation(nextOperation);
    setErrorCode(null);
    setState("blocked");
  }, [operationId]);

  useEffect(() => {
    if (!wasEnabled.current && enabled && operationId) shouldReconcile.current = true;
    wasEnabled.current = enabled;
  }, [enabled, operationId]);

  useEffect(() => {
    if (shouldReconcile.current === null) shouldReconcile.current = Boolean(operationId);
    if (!enabled || !operationId || !shouldReconcile.current) return;
    shouldReconcile.current = false;
    let active = true;
    setState("checking");
    setOperation(null);
    setErrorCode(null);
    void getOperation(operationId).then((nextOperation) => {
      if (!active) return;
      setOperation(nextOperation);
      setState(nextOperation.status === "succeeded" || nextOperation.status === "failed" ? "resolved" : "blocked");
    }).catch((error: unknown) => {
      if (!active) return;
      const code = error instanceof ManagementRequestError ? error.code : "generic";
      setErrorCode(code);
      setState(code === "not_found" ? "not_found" : "error");
    });
    return () => {
      active = false;
    };
  }, [enabled, operationId, retryCount]);

  const isBlocked = state === "checking" || state === "blocked" || state === "not_found" || state === "error";
  return [operationId, setOperationId, { operationId, state, operation, errorCode, isBlocked, retry, block, persist }];
}
