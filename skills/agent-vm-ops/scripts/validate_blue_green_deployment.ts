#!/usr/bin/env bun
export * from "../../../deploy/scripts/validate_blue_green_deployment.ts";
if (import.meta.main) {
 const result=Bun.spawnSync([process.execPath,new URL("../../../deploy/scripts/validate_blue_green_deployment.ts",import.meta.url).pathname,...process.argv.slice(2)],{stdin:"inherit",stdout:"inherit",stderr:"inherit"});
 process.exit(result.exitCode);
}
