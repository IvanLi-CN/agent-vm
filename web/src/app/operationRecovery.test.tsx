import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { Operation } from "./types";

const { getOperationMock } = vi.hoisted(() => ({ getOperationMock: vi.fn() }));

vi.mock("./api", async () => {
  const actual = await vi.importActual<typeof import("./api")>("./api");
  return { ...actual, getOperation: getOperationMock };
});

import { useOperationRecovery, writeOperationRecovery } from "./operationRecovery";

const operation: Operation = {
  operation_id: "50e8400e-ccf8-4c7b-8d1d-14f66c7fb126",
  action: "restart",
  status: "succeeded",
  phase: "completed",
  target: { kind: "vm", id: "task_example_01" },
  actor: { issuer: "https://issuer.example", subject: "admin", role: "administrator" },
  reason: null,
  created_at: "2026-11-04T08:00:00Z",
  updated_at: "2026-11-04T08:00:00Z",
  result: { effect: "completed" },
  error: null,
};

const pendingOperation: Operation = { ...operation, status: "running", phase: "native_action", result: null, error: null };

function Probe({ path = "/vms" }: { path?: string }) {
  const [operationId, setOperationId, recovery] = useOperationRecovery(path);
  return <div>
    <span data-testid="operation-id">{operationId}</span>
    <span data-testid="recovery-state">{recovery.state}</span>
    <span data-testid="recovery-blocked">{String(recovery.isBlocked)}</span>
    <button type="button" onClick={() => setOperationId("50e8400e-ccf8-4c7b-8d1d-14f66c7fb126")}>start</button>
    <button type="button" onClick={() => setOperationId("50e8400e-ccf8-4c7b-8d1d-14f66c7fb126", false)}>preview</button>
    <button type="button" onClick={() => recovery.block(pendingOperation)}>block</button>
    <button type="button" onClick={recovery.persist}>persist</button>
  </div>;
}

describe("operation recovery", () => {
  beforeEach(() => {
    window.sessionStorage.clear();
    getOperationMock.mockReset();
  });

  it("reconciles an operation persisted before the page remounted", async () => {
    writeOperationRecovery("/vms", operation.operation_id);
    getOperationMock.mockResolvedValue(operation);

    render(<Probe />);

    await waitFor(() => expect(getOperationMock).toHaveBeenCalledWith(operation.operation_id));
    await waitFor(() => expect(screen.getByTestId("recovery-state")).toHaveTextContent("resolved"));
  });

  it("does not query a newly created operation until a later remount", async () => {
    const first = render(<Probe />);
    fireEvent.click(screen.getByRole("button", { name: "start" }));
    expect(getOperationMock).not.toHaveBeenCalled();
    first.unmount();

    getOperationMock.mockResolvedValue(operation);
    render(<Probe />);
    await waitFor(() => expect(getOperationMock).toHaveBeenCalledWith(operation.operation_id));
  });

  it("keeps preview operation ids in memory until submission persists them", () => {
    render(<Probe />);
    fireEvent.click(screen.getByRole("button", { name: "preview" }));
    expect(window.sessionStorage.length).toBe(0);
    fireEvent.click(screen.getByRole("button", { name: "persist" }));
    expect(window.sessionStorage.length).toBe(1);
  });

  it("blocks the current page when a submitted operation is nonterminal", () => {
    render(<Probe />);
    fireEvent.click(screen.getByRole("button", { name: "start" }));
    fireEvent.click(screen.getByRole("button", { name: "block" }));
    expect(screen.getByTestId("recovery-state")).toHaveTextContent("blocked");
    expect(screen.getByTestId("recovery-blocked")).toHaveTextContent("true");
  });
});
