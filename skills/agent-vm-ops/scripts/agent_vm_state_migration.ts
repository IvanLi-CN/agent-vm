#!/usr/bin/env bun
export * from "../../../deploy/scripts/agent_vm_state_migration.ts";
if (import.meta.main) {
 const result=Bun.spawnSync([process.execPath,new URL("../../../deploy/scripts/agent_vm_state_migration.ts",import.meta.url).pathname,...process.argv.slice(2)],{stdin:"inherit",stdout:"inherit",stderr:"inherit"});
 process.exit(result.exitCode);
}
