#!/usr/bin/env bun

import { fileURLToPath } from "node:url";

// Repository compatibility entrypoint. The public implementation lives in
// the agent-vm-ops Skill; deployment materials project it back to this path.
export * from "../../skills/agent-vm-ops/scripts/validate_observability_dashboard.ts";

if (import.meta.main) {
  const result = Bun.spawnSync([
    process.execPath,
    fileURLToPath(new URL("../../skills/agent-vm-ops/scripts/validate_observability_dashboard.ts", import.meta.url)),
    ...process.argv.slice(2),
  ], { stdin: "inherit", stdout: "inherit", stderr: "inherit" });
  process.exit(result.exitCode);
}
