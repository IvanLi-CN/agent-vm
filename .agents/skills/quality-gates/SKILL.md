---
name: quality-gates
description: "Establish and audit explicit repository merge and release quality gates, required checks, waivers, and signatures."
schema_version: 1
kind: policy-skill
slug: quality-gates
primary_topic: quality-gates
policy_dependencies: []
visibility: public
public_url_hosts: []
---

# Quality gates

Use this project policy when a repository has, or is establishing, an explicit contract for merge and release readiness. The repo-local declaration is the source of truth; GitHub branch rules and workflow names are execution surfaces that must align with it.

## Workflow

1. Copy `assets/templates/quality-gates.example.json` to the project's declared quality-gate path and fill the real default branch, required checks, informational checks, signatures, and waivers.
2. Keep PR gates separate from post-merge and release gates. Include every real delivery surface, not only the easiest test suite.
3. Run `assets/scripts/check_quality_gates.py` after each declaration or workflow change. A waiver must identify scope, owner, reason, and expiry.
4. Align default-branch protection and required checks; treat direct pushes and bypasses as explicit exceptions.

If review policy is declared, preserve maintainer exceptions through native PR rules. Do not infer API compatibility or migration safety from a green gate; those belong to their dedicated policies.

## Adoption preview

Before applying this policy to a target project, preview the writes to `.agents/skills/quality-gates/` and `skills-lock.json`, then wait for explicit owner approval. Install it with `npx skills add IvanLi-CN/style-playbook-skills --skill quality-gates --yes`; the CLI detects the Agent and project layout.

## Package resources

- `assets/templates/quality-gates.example.json`
- `assets/templates/waiver-record.example.json`
- `assets/scripts/check_quality_gates.py`
