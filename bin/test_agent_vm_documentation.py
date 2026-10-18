"""Contract checks for the public Agent VM topology and recovery documents."""

from __future__ import annotations

import re
import unittest
from pathlib import Path

from spec_contract_check import check as check_spec


ROOT = Path(__file__).resolve().parents[1]
SPEC = ROOT / "docs/specs/agent-vm/SPEC.md"
RUNBOOK = ROOT / "docs/specs/agent-vm/OPERATIONS.md"
CLI_CONTRACT = ROOT / "docs/specs/agent-vm/contracts/cli.md"
ADR = ROOT / "docs/adr/0028-agent-vm-topology-recovery-boundary.md"
OPS_SKILL = ROOT / "skills/agent-vm-ops/SKILL.md"
DEPLOYMENT = ROOT / "skills/agent-vm-ops/references/deployment.md"


class AgentVmDocumentationTests(unittest.TestCase):
    def test_canonical_spec_is_traceable(self) -> None:
        self.assertEqual(check_spec(SPEC, ROOT), [])

    def test_public_documents_define_the_layered_topology(self) -> None:
        runbook = RUNBOOK.read_text(encoding="utf-8")
        for phrase in (
            "Codex client",
            "Agent VM control endpoint",
            "CT109 LXC",
            "Incus VM inside CT109",
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
            "does not reserve the full requested virtual disk",
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
        adr = ADR.read_text(encoding="utf-8")
        ops_skill = OPS_SKILL.read_text(encoding="utf-8")
        deployment = DEPLOYMENT.read_text(encoding="utf-8")
        self.assertIn("OPERATIONS.md", spec)
        self.assertIn("../OPERATIONS.md", cli_contract)
        self.assertIn("0028-agent-vm-topology-recovery-boundary.md", spec)
        self.assertIn("../../docs/specs/agent-vm/OPERATIONS.md", ops_skill)
        self.assertIn("../../../docs/specs/agent-vm/OPERATIONS.md", deployment)
        self.assertIn("protected operator records", adr)
        for document in (spec, cli_contract, adr, ops_skill, deployment):
            self.assertNotRegex(document, re.compile(r"/(?:Users|home)/[A-Za-z0-9._-]+"))
            self.assertNotRegex(document, re.compile(r"\b(?:10|172|192\.168)\.\d+\.\d+\.\d+\b"))


if __name__ == "__main__":
    unittest.main()
