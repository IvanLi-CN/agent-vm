import { describe, expect, it } from "vitest";
import { modelForScene } from "./runtime";

describe("web demo scenarios", () => {
  it("models capacity pressure without changing the normal source model", () => {
    const capacity = modelForScene("capacity");
    const normal = modelForScene("normal");

    expect(capacity.overview.admission.ready).toBe(false);
    expect(capacity.overview.admission.reasons).toContain("physical_disk_staging_low");
    expect(capacity.images[0]?.verification.findings).toContain("staging_capacity_low");
    expect(normal.overview.admission.ready).toBe(true);
  });

  it("models conflict and empty states as explicit, inspectable outcomes", () => {
    const conflict = modelForScene("conflict");
    const empty = modelForScene("empty");

    expect(conflict.operations[0]?.status).toBe("outcome_unknown");
    expect(conflict.operations[0]?.error?.code).toBe("authority_unavailable");
    expect(empty.vms).toHaveLength(0);
    expect(empty.images).toHaveLength(0);
    expect(empty.audits).toHaveLength(0);
  });

  it("supports a minimal data share without mutating the full scenario", () => {
    const full = modelForScene("normal", "full");
    const minimal = modelForScene("normal", "minimal");

    expect(minimal.operations.length).toBeLessThanOrEqual(full.operations.length);
    expect(minimal.audits.length).toBeLessThanOrEqual(full.audits.length);
    expect(full.operations.length).toBeGreaterThan(0);
  });
});
