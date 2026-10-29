import { describe, expect, it } from "vitest";
import { formatActor, formatBytes, formatDateTime, shortFingerprint, stateTone } from "./format";

describe("management formatting", () => {
  it("keeps byte values readable without losing unknown state", () => {
    expect(formatBytes("8589934592", "en")).toBe("8 GiB");
    expect(formatBytes(null, "zh-CN")).toBe("—");
  });

  it("formats timestamps with a timezone marker", () => {
    expect(formatDateTime("2026-01-15T10:20:30Z", "en")).toMatch(/2026/);
  });

  it("shortens fingerprints without changing the identity source", () => {
    expect(shortFingerprint("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")).toBe("aaaaaaaa…aaaaaa");
  });

  it("uses semantic tones for state", () => {
    expect(stateTone("stale")).toBe("warning");
    expect(stateTone("failed")).toBe("danger");
    expect(stateTone("passed")).toBe("success");
  });

  it("preserves actor provenance in audit labels", () => {
    expect(formatActor({ issuer: "https://issuer.example.invalid", subject: "admin-maintainer", role: "administrator" })).toBe("admin-maintainer · https://issuer.example.invalid");
    expect(formatActor({ kind: "legacy_unknown", raw_actor: "legacy-admin" })).toBe("legacy-admin · legacy_unknown");
  });
});
