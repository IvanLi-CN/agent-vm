#!/usr/bin/env bun

import { lstatSync } from "node:fs";

import { runBounded } from "./client-io.ts";

const MAX_REQUEST_BYTES = 2 * 1024 * 1024;
const STDOUT_LIMIT = 2 * 1024 * 1024;
const STDERR_LIMIT = 64 * 1024;
const DEFAULT_BINARY = "/opt/agent-vm-native/bin/agent-vm-control";
const DEFAULT_SOCKET = "/run/agent-vm-native/control.sock";
const ACTIVE_SOCKET = "/var/lib/agent-vm-native/active/app.sock";

function resolveSocket(): string {
  const configured = process.env.AGENT_VM_GO_CONTROL_SOCKET;
  if (configured) return configured;
  try {
    // Keep the active path authoritative after the first cutover. lstat also
    // treats a not-yet-ready socket symlink as present, so we fail closed
    // instead of falling back to a retired singleton controller.
    lstatSync(ACTIVE_SOCKET);
    return ACTIVE_SOCKET;
  } catch {
    return DEFAULT_SOCKET;
  }
}

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
const socket = resolveSocket();
const timeout = process.env.AGENT_VM_GO_CONTROL_TIMEOUT ?? "240s";
const timeoutMatch = /^(\d+)(ms|s|m)$/.exec(timeout);
if (!timeoutMatch || Number(timeoutMatch[1]) <= 0) throw new Error("invalid Go control timeout");
const timeoutMs = Number(timeoutMatch[1]) * (timeoutMatch[2] === "ms" ? 1 : timeoutMatch[2] === "s" ? 1000 : 60_000);
if (!Number.isSafeInteger(timeoutMs) || timeoutMs > 600_000) throw new Error("Go control timeout exceeds the action budget");
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
    timeoutMs,
    stdoutLimit: STDOUT_LIMIT,
    stderrLimit: STDERR_LIMIT,
    env: environment,
  },
);

await Bun.write(Bun.stdout, result.stdout);
await Bun.write(Bun.stderr, result.stderr);
process.exitCode = result.timedOut ? 75 : result.code;
