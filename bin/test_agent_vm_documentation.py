"""Contract checks for the public Agent VM topology and recovery documents."""

from __future__ import annotations

import re
import subprocess
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
SPEC = ROOT / "docs/specs/agent-vm/SPEC.md"
RUNBOOK = ROOT / "docs/specs/agent-vm/OPERATIONS.md"
CLI_CONTRACT = ROOT / "docs/specs/agent-vm/contracts/cli.md"
RELEASE_CONTRACT = ROOT / "docs/specs/agent-vm/contracts/release.md"
ADR = ROOT / "docs/adr/0028-agent-vm-topology-recovery-boundary.md"
OPS_SKILL = ROOT / "skills/agent-vm-ops/SKILL.md"
AGENT_SKILL = ROOT / "skills/agent-vm/SKILL.md"
DEPLOYMENT = ROOT / "skills/agent-vm-ops/references/deployment.md"
INITIALIZATION = ROOT / "skills/agent-vm-ops/references/initialization.md"
INITIALIZATION_SCRIPT = ROOT / "skills/agent-vm-ops/scripts/agent_vm_incus_init.ts"
INCUS_INITIALIZATION_ADR = ROOT / "docs/adr/0041-agent-vm-incus-project-initialization.md"
OBSERVABILITY = ROOT / "docs/specs/agent-vm/contracts/observability.md"
OBSERVABILITY_SOLUTION = ROOT / "docs/solutions/agent-vm/observability-dashboard.md"
CPU_PRESSURE_ADR = ROOT / "docs/adr/0032-agent-vm-cpu-startup-pressure-for-overcommit.md"
OBSERVABILITY_ADR = ROOT / "docs/adr/0033-agent-vm-capacity-and-inventory-observability.md"
SUPERSEDED_CPU_ADR = ROOT / "docs/adr/0031-agent-vm-cpu-startup-pressure-boundary.md"


def check_spec(path: Path, root: Path) -> list[str]:
    try:
        result = subprocess.run(
            [
                "bun",
                "run",
                str(root / "bin/spec_contract_check.ts"),
                "--path",
                str(path),
                "--repo-root",
                str(root),
            ],
            cwd=root,
            capture_output=True,
            text=True,
            check=False,
        )
    except OSError as error:
        return [str(error)]
    if result.returncode == 0:
        return []
    return [line for line in (result.stdout + result.stderr).splitlines() if line]


class AgentVmDocumentationTests(unittest.TestCase):
    def test_canonical_spec_is_traceable(self) -> None:
        self.assertEqual(check_spec(SPEC, ROOT), [])

    def test_public_documents_define_the_layered_topology(self) -> None:
        runbook = RUNBOOK.read_text(encoding="utf-8")
        for phrase in (
            "Codex client",
            "Agent VM control endpoint",
            "CT109 LXC",
            "Incus container inside CT109",
            "Underlying hosting source",
            "not proof of the underlying host",
            "Unknown underlying hosting source",
        ):
            self.assertIn(phrase, runbook)

    def test_recovery_contract_preserves_identity_and_stops_on_missing_state(self) -> None:
        runbook = RUNBOOK.read_text(encoding="utf-8")
        for phrase in (
            "state database",
            "WAL/journal",
            "operation_id",
            "generation",
            "outcome: pending",
            "admin reconcile --apply",
            "admin drain exit",
            "Missing state is a hard stop",
            "Do not guess ownership",
            "Do not delete an Incus instance",
        ):
            self.assertIn(phrase, runbook)

    def test_disk_contract_separates_sparse_capacity_from_control_state(self) -> None:
        runbook = RUNBOOK.read_text(encoding="utf-8")
        for phrase in (
            "does not reserve the full requested rootfs allocation",
            "configured CT-local Incus pool filesystem",
            "separate logical failure domains",
            "ENOSPC",
            "existing effective lease must not be evicted",
            "Guest-level ENOSPC is a separate observation",
        ):
            self.assertIn(phrase, runbook)

    def test_contract_links_are_present_and_public_values_stay_redacted(self) -> None:
        spec = SPEC.read_text(encoding="utf-8")
        cli_contract = CLI_CONTRACT.read_text(encoding="utf-8")
        release_contract = RELEASE_CONTRACT.read_text(encoding="utf-8")
        adr = ADR.read_text(encoding="utf-8")
        ops_skill = OPS_SKILL.read_text(encoding="utf-8")
        deployment = DEPLOYMENT.read_text(encoding="utf-8")
        initialization = INITIALIZATION.read_text(encoding="utf-8")
        incus_initialization_adr = INCUS_INITIALIZATION_ADR.read_text(encoding="utf-8")
        self.assertIn("OPERATIONS.md", spec)
        self.assertIn("../OPERATIONS.md", cli_contract)
        self.assertIn("0028-agent-vm-topology-recovery-boundary.md", spec)
        self.assertIn("../../docs/specs/agent-vm/OPERATIONS.md", ops_skill)
        self.assertIn("../../../docs/specs/agent-vm/OPERATIONS.md", deployment)
        self.assertIn("references/initialization.md", ops_skill)
        self.assertIn("initialization reference", deployment)
        self.assertTrue(INITIALIZATION_SCRIPT.is_file())
        for phrase in (
            "limits.processes",
            "limits.instances",
            "read-only preview",
            "--apply",
            "never deletes or restarts an instance",
            "stable JSON API paths",
        ):
            self.assertIn(phrase, initialization)
            self.assertIn(phrase, incus_initialization_adr + initialization)
        self.assertIn("contracts/release.md", spec)
        self.assertIn("release contract", cli_contract)
        for phrase in (
            "release-manifest.json",
            "legacy-protocol-4",
            "client_to_control",
            "control_to_client",
            "stage",
            "verify",
            "activate",
            "rollback",
            "daemon-reload",
            "does not stop or disable",
            "state database",
        ):
            self.assertIn(phrase, release_contract)
        self.assertIn("protected operator records", adr)
        for document in (spec, cli_contract, release_contract, adr, ops_skill, deployment, initialization, incus_initialization_adr):
            self.assertNotRegex(document, re.compile(r"/(?:Users|home)/[A-Za-z0-9._-]+"))
            self.assertNotRegex(document, re.compile(r"\b(?:10|172|192\.168)\.\d+\.\d+\.\d+\b"))

    def test_guest_environment_readiness_is_scoped(self) -> None:
        agent_skill = AGENT_SKILL.read_text(encoding="utf-8")
        ops_skill = OPS_SKILL.read_text(encoding="utf-8")
        for phrase in (
            "## Guest environment readiness",
            "`acquire` confirms guest startup and network readiness, then performs the mandatory bounded guest root-filesystem post-check before exposing SSH",
            "prepare only missing prerequisites",
            "a missing `psql` client does not imply a PostgreSQL server",
            "exact `not_run` items",
            "never claim the full suite ran",
        ):
            self.assertIn(phrase, agent_skill)
        self.assertIn("Keep project-specific toolchains out of the generic image", ops_skill)

    def test_guest_root_growth_contract_is_explicit(self) -> None:
        runbook = RUNBOOK.read_text(encoding="utf-8")
        cli_contract = CLI_CONTRACT.read_text(encoding="utf-8")
        for phrase in (
            "separate fail-closed boundary after the container rootfs mutation",
            "Before changing the guest, record capability and topology evidence",
            "Incus storage/profile path that can enforce and verify the requested rootfs limit",
            "already at least 95% of the requested rootfs capacity is an idempotent success",
            "one bounded reread and settle retry",
            "bounded attempt/retry chain",
            "process-group cleanup evidence on command timeout",
        ):
            self.assertIn(phrase, runbook)
        for phrase in (
            "guest_filesystem",
            "guest_resize_unverified",
            "configured rootfs capacity",
            "storage/topology evidence",
            "no active lease or exposed SSH mapping",
            "inspectable and retryable",
            "process-group cleanup evidence on command timeout",
        ):
            self.assertIn(phrase, cli_contract)

    def test_observability_contract_separates_capacity_layers(self) -> None:
        contract = OBSERVABILITY.read_text(encoding="utf-8")
        solution = OBSERVABILITY_SOLUTION.read_text(encoding="utf-8")
        for phrase in (
            "incus_storage_pool_size_bytes",
            "agent_vm_admission_memory_limit_bytes",
            "agent_vm_filesystem_available_bytes",
            "incus_project_limit",
            "refresh: \"auto\"",
            "next managed container",
            "horizontal dashed line",
            "image-unpack failures",
            "must not multiply a `pps` query by `8`",
            "provisioning source",
            "grafana/dashboards/incus/incus-agent-vm.json",
            "grafana/provisioning/dashboards/incus.yml",
        ):
            self.assertIn(phrase, contract)
        self.assertIn("32 GiB", solution)
        self.assertIn("64 GiB", solution)
        self.assertIn("2x", solution)
        self.assertIn("observability contract", solution)

    def test_cpu_policy_and_split_observability_contract_are_traceable(self) -> None:
        spec = SPEC.read_text(encoding="utf-8")
        operations = RUNBOOK.read_text(encoding="utf-8")
        cli_contract = CLI_CONTRACT.read_text(encoding="utf-8")
        contract = OBSERVABILITY.read_text(encoding="utf-8")
        cpu_adr = CPU_PRESSURE_ADR.read_text(encoding="utf-8")
        observability_adr = OBSERVABILITY_ADR.read_text(encoding="utf-8")
        superseded_cpu_adr = SUPERSEDED_CPU_ADR.read_text(encoding="utf-8")
        for document in (spec, operations, cli_contract, contract, cpu_adr):
            self.assertIn("50", document)
        for phrase in (
            "Agent VM / Capacity & Admission",
            "Agent VM / Inventory & Operations",
            "admin-metrics",
            "admin-status",
            "agent_vm_admission_default_vm_logical_slots",
            "agent_vm_admission_default_vm_startable_slots",
            "agent_vm_task_info",
            "snapshot estimate",
        ):
            self.assertIn(phrase, contract + observability_adr)
        self.assertIn("status: superseded", superseded_cpu_adr)
        self.assertIn("superseded_by: 0032-agent-vm-cpu-startup-pressure-for-overcommit.md", superseded_cpu_adr)

    def test_observability_sampling_lanes_are_independent(self) -> None:
        spec = SPEC.read_text(encoding="utf-8")
        contract = OBSERVABILITY.read_text(encoding="utf-8")
        solution = OBSERVABILITY_SOLUTION.read_text(encoding="utf-8")
        for phrase in (
            "independent observation lanes",
            "pending or failed inventory read",
            "latest valid capacity metric families",
            "retained source snapshot",
            "original observation timestamp",
        ):
            self.assertIn(phrase, spec)
        for phrase in (
            "independent sampling lanes",
            "slow or unavailable `admin-status` call",
            "erase capacity families",
            "remove or invalidate task rows",
            "Source-specific timestamps and health diagnostics",
        ):
            self.assertIn(phrase, contract)
        for phrase in (
            "independent sampling lanes",
            "does not erase the last valid capacity families",
            "removes or invalidates task rows",
            "aggregate `up=0`",
        ):
            self.assertIn(phrase, solution)


if __name__ == "__main__":
    unittest.main()
