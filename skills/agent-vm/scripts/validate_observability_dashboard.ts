#!/usr/bin/env bun
export * from "../../../deploy/scripts/validate_observability_dashboard.ts";
if (import.meta.main) {
 const result=Bun.spawnSync([process.execPath,new URL("../../../deploy/scripts/validate_observability_dashboard.ts",import.meta.url).pathname,...process.argv.slice(2)],{stdin:"inherit",stdout:"inherit",stderr:"inherit"});
 process.exit(result.exitCode);
}
