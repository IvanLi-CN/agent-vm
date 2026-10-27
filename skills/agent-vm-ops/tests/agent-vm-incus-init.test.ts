import { test } from "bun:test";

import {
  DEFAULT_PROCESS_LIMIT,
  buildInitializationPlan,
  initialize,
  projectQueryCommand,
  profileQueryCommand,
  readSnapshot,
  type CommandResult,
  type IncusSnapshot,
  type InitializationOptions,
} from "../scripts/agent_vm_incus_init.ts";

const options: InitializationOptions = {
  remote: "control-alias",
  project: "agents",
  profile: "agent-vm",
  processLimit: DEFAULT_PROCESS_LIMIT,
  apply: false,
};

const alignedSnapshot = (): IncusSnapshot => ({
  project: { "restricted": "true" },
  profile: { "limits.processes": "512", "limits.cpu": "2" },
});

test("the canonical baseline leaves aggregate project limits unset", () => {
  const plan = buildInitializationPlan(alignedSnapshot());
  if (!plan.aligned || plan.actions.length !== 0) throw new Error("aligned Incus policy produced changes");
});

test("the plan removes every aggregate project quota and repairs the profile process limit", () => {
  const plan = buildInitializationPlan({
    project: {
      "limits.processes": "4096",
      "limits.cpu": "16",
      "limits.memory": "32768MiB",
      "limits.instances": "8",
      restricted: "true",
    },
    profile: { "limits.processes": "256" },
  });
  if (plan.actions.length !== 5) throw new Error(`expected five policy actions, got ${plan.actions.length}`);
  if (plan.actions.filter(action => action.kind === "unset-project-config").length !== 4) throw new Error("aggregate quota actions are incomplete");
  const profileAction = plan.actions.find(action => action.kind === "set-profile-config");
  if (!profileAction || profileAction.desired !== "512") throw new Error("profile process limit was not normalized");
});

test("Incus reads use the project-scoped JSON API", () => {
  const project = projectQueryCommand("control-alias", "agents");
  const profile = profileQueryCommand("control-alias", "agents", "agent-vm");
  if (project.join(" ") !== "ssh -o BatchMode=yes control-alias -- incus query /1.0/projects/agents") throw new Error("project query command changed");
  if (profile.join(" ") !== "ssh -o BatchMode=yes control-alias -- incus query /1.0/profiles/agent-vm?project=agents") throw new Error("profile query command changed");
});

test("the parser accepts the unwrapped object emitted by incus query", () => {
  const runner = (command: string[]): CommandResult => ({
    exitCode: 0,
    stdout: JSON.stringify({ config: command.some(item => item.includes("/projects/")) ? { restricted: "true" } : { "limits.processes": "512" } }),
    stderr: "",
  });
  const snapshot = readSnapshot(options, runner);
  if (snapshot.project.restricted !== "true" || snapshot.profile["limits.processes"] !== "512") throw new Error("unwrapped Incus response was not parsed");
});

test("default mode is read-only and apply mode verifies the final policy", () => {
  const state: IncusSnapshot = {
    project: { "limits.processes": "4096", restricted: "true" },
    profile: { "limits.processes": "512" },
  };
  const calls: string[][] = [];
  const runner = (command: string[]): CommandResult => {
    calls.push(command);
    const queryIndex = command.indexOf("query");
    if (queryIndex >= 0) {
      const path = command[queryIndex + 1];
      const metadata = path.includes("/projects/") ? { config: state.project } : { config: state.profile };
      return { exitCode: 0, stdout: JSON.stringify({ metadata }), stderr: "" };
    }
    if (command.includes("project") && command.includes("unset")) {
      delete state.project[command.at(-1)!];
    } else {
      const assignment = command.find(item => item.startsWith("limits.processes="));
      if (assignment) state.profile["limits.processes"] = assignment.split("=", 2)[1];
    }
    return { exitCode: 0, stdout: "", stderr: "" };
  };

  const preview = initialize(options, runner);
  if (preview.status !== "needs_action" || !preview.dry_run || preview.actions.length !== 1 || calls.length !== 2) throw new Error("preview performed an unexpected operation");

  const applied = initialize({ ...options, apply: true }, runner);
  if (applied.dry_run || applied.actions.length !== 1) throw new Error("apply result was not verified");
  const observedCalls: number = Number(calls.length);
  if (observedCalls !== 7) throw new Error(`expected preview reads plus apply reads and one mutation, got ${observedCalls}`);
});
