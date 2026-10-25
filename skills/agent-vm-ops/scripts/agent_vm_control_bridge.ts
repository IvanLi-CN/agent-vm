#!/usr/bin/env bun

import { runBounded } from "../../agent-vm/scripts/client-io.ts";

const MAX_REQUEST_BYTES = 2 * 1024 * 1024;
const STDOUT_LIMIT = 2 * 1024 * 1024;
const STDERR_LIMIT = 64 * 1024;
const DEFAULT_BINARY = "/opt/agent-vm-native/bin/agent-vm-control";
const DEFAULT_SOCKET = "/run/agent-vm-native/control.sock";

async function readRequest(): Promise<Buffer> {
  const chunks: Buffer[] = [];
  let length = 0;
  for await (const chunk of Bun.stdin.stream()) {
    const bytes = Buffer.from(chunk);
    length += bytes.length;
    if (length > MAX_REQUEST_BYTES) {
      throw new Error("control request body exceeds the limit");
    }
    chunks.push(bytes);
  }
  return Buffer.concat(chunks);
}

const input = await readRequest();
const binary = process.env.AGENT_VM_GO_CONTROL_BINARY ?? DEFAULT_BINARY;
const socket = process.env.AGENT_VM_GO_CONTROL_SOCKET ?? DEFAULT_SOCKET;
const timeout = process.env.AGENT_VM_GO_CONTROL_TIMEOUT ?? "240s";
const environment = {
  PATH: "/usr/local/bin:/usr/bin:/bin",
  AGENT_VM_GO_CONTROL_BINARY: binary,
  AGENT_VM_GO_CONTROL_SOCKET: socket,
  AGENT_VM_GO_CONTROL_TIMEOUT: timeout,
};
const result = await runBounded(
  [binary, "request", "--socket", socket, "--timeout", timeout],
  {
    input,
    timeoutMs: 240_000,
    stdoutLimit: STDOUT_LIMIT,
    stderrLimit: STDERR_LIMIT,
    env: environment,
  },
);

await Bun.write(Bun.stdout, result.stdout);
await Bun.write(Bun.stderr, result.stderr);
process.exitCode = result.timedOut ? 75 : result.code;
