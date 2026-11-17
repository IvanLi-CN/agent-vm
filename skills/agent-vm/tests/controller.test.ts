import { expect, test } from "bun:test";
import { Client } from "../scripts/client.ts";
import { dispatch, parseArgs } from "../scripts/agent_vm.ts";
import { Data, json, parseJson, readJson } from "../scripts/client-io.ts";
import { Fixture as ClientFixture, result } from "./support.ts";
import { Fixture as ControlFixture } from "../../agent-vm-ops/tests/control-fixture.ts";
import { canonical_json, parse_json } from "../../agent-vm-ops/scripts/control-types.ts";

test("Bun client and canonical controller interoperate across native request serialization and journal replay", async () => {
  const local = new ClientFixture(), remote = new ControlFixture();
  const sent: Data[] = [];
  const client = new Client(local.env, async (argv, opts) => {
    expect(argv.slice(0, -1)).toEqual(["ssh", "-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "StrictHostKeyChecking=yes", "--", "fixture-host"]);
    expect(argv.at(-1)).toMatch(/^\/opt\/agent-vm-native\/bin\/agent-vm-server request --socket \/run\/agent-vm-native\/control\.sock --timeout \d+s$/);
    const request = parse_json(String(opts!.input)); sent.push(request);
    try {
      const response = await remote.control.dispatch(request);
      response.release = { release_id: "a".repeat(40), source_revision: "a".repeat(40), protocol: 4, version: "1.0.0", manifest_present: true, artifacts_verified: true };
      return result(canonical_json(response));
    }
    catch (error: any) { return result("", 2, canonical_json({ ok: false, ...Object.fromEntries(["protocol", "action", "session_id", "generation", "operation_id"].filter(k => k in request).map(k => [k, request[k]])), error: { code: error.code, message: error.message, detail: error.detail ?? {} } })); }
  }, () => {});
  // Only the real public-key generation process is substituted; no private material is logged.
  client.key = async () => [local.key(), "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest fixture"];
  try {
    const args = (argv: string[]) => parseArgs(["--session-id", "integration", ...argv]);
    const absent = await dispatch(args(["status"]), client) as Data; expect(absent.result.state).toBe("absent");
    const first = await dispatch(args(["acquire", "--operation-id", "allocate"]), client) as Data;
    expect(first.result.effective_lease).toBe(true); const generation = client.generation("integration");
    const journal = readJson(client.operationPath("integration", "allocate")); expect(journal.request.generation).toBe(0);
    const replayed = await dispatch(args(["acquire", "--operation-id", "allocate"]), client) as Data;
    expect(replayed.outcome).toBe("replayed"); expect(sent.filter(r => r.action === "acquire").map(r => r.generation)).toEqual([0, 0]); expect(client.generation("integration")).toBe(generation);
    const before = remote.store.get_task({ session_id: "integration" })!.lease_expires_at;
    await dispatch(args(["status"]), client); await dispatch(args(["port", "list"]), client);
    expect(remote.store.get_task({ session_id: "integration" })!.lease_expires_at).toBe(before);
    await dispatch(args(["renew", "--operation-id", "renew"]), client);
    await dispatch(args(["release", "--operation-id", "release"]), client);
    expect(remote.store.get_task({ session_id: "integration" })!.state).toBe("cached");
    await expect(client.request("unknown", { session: "integration" })).rejects.toMatchObject({ code: "unknown_action" });
  } finally { local.close(); remote.close(); }
});
