---
name: release-failure-notification
description: "Define actionable, privacy-safe context for notifying maintainers when an expected-success workflow fails."
schema_version: 1
kind: policy-skill
slug: release-failure-notification
primary_topic: release-failure-notification
policy_dependencies: []
visibility: public
public_url_hosts: []
---

# Release failure notification

Use this project policy when an expected-success workflow failure needs owner-facing incident context. It defines what a notification must contain and how delivery is selected; it does not silently choose a provider, secret, destination, or paging policy. Ordinary CI, Docs Pages, deploy, and gate failures use metadata-only context; a Release failure may additionally carry verified release identity and recovery context.

## Workflow

1. Copy `assets/templates/release-failure-notification-transport-gate.example.json` and explicitly select a project-approved transport.
2. Trigger from a completed expected-success workflow with a failed conclusion. Include repository identity, workflow run, head SHA, actor, and a link or durable reference that the maintainer can inspect. Do not invent release identity for ordinary CI, Docs Pages, deploy, or gate failures; preserve verified release context when the failed workflow is `Release`.
3. Keep notification failure distinct from the failed workflow, avoid leaking secrets or internal paths, and make retries idempotent.
4. Validate the selected transport and required credentials before enabling delivery. Without checked-in evidence or owner approval, stop at the selection gate.

## Adoption preview

Before applying this policy to a target project, preview the writes to `.agents/skills/release-failure-notification/` and `skills-lock.json`, then wait for explicit owner approval. Install it with `npx skills add IvanLi-CN/style-playbook-skills --skill release-failure-notification --yes`; the CLI detects the Agent and project layout.

## Package resources

- `assets/templates/release-failure-notification-transport-gate.example.json`
