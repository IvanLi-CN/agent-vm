---
name: semver-change-governance
description: "Apply optional SemVer compatibility classification when a change may alter a public API or persistent state; it does not select manual release targets."
schema_version: 1
kind: policy-skill
slug: semver-change-governance
primary_topic: semver-change-governance
policy_dependencies: []
visibility: public
public_url_hosts: []
---

# SemVer change governance

Use this optional project policy when an explicitly selected compatibility scope may change a public API or durable state. It records planned, current, and verified compatibility impact; it does not choose exact version numbers, manual release targets, labels, tags, migrations, release eligibility, or release orchestration. It is not part of the target-only `Manual version release delivery` route.

## Workflow

1. Define the compatibility scope and, when the project chooses to keep a durable assessment, record separate `public_api` and `persistent_state` impacts in `assets/templates/version-impact-record.example.json`. The assessment does not require candidate `base_sha` or `source_sha` fields, one file per PR, or a release-target consumer.
2. Classify each contract as `patch`, `minor`, or `major` using compatibility evidence. Reclassify when scope or supported versions change.
3. Before the compatibility-sensitive change is accepted, verify the declared scope and evidence. A separate adopter may define release-unit aggregation, but this policy does not create a manual-release state or target from it.

## Compatibility rules

- `patch`: public API is nonbreaking; newly written state is readable by all supported earlier versions in the same Minor.
- `minor`: public API stays compatible within the Major; earlier Minor versions need not read newly migrated state, but the candidate must read supported earlier-Minor state.
- `major`: public API may break; state support is limited to the immediately preceding Major. Direct multi-Major jumps require explicit intermediate upgrades.

Keep API and state classifications separate. Migration mechanics belong to `persistent-state-migrations`; version identity and label projection belong to `pr-label-release`.

## Manual release boundary

The target-only `Manual version release delivery` route is intentionally excluded from this policy. It receives one owner-supplied `target` and resolves it from the trusted final baseline; it does not read this assessment, require a repository-local candidate binding, aggregate PR impacts, or persist `maximum_verified_impact` as release state. Projects that explicitly adopt a different compatibility-to-release mapping must document that integration separately and must not infer it from installing this policy.

## Adoption preview

Before applying this policy to a target project, preview the writes to `.agents/skills/semver-change-governance/` and `skills-lock.json`, then wait for explicit owner approval. Install it with `npx skills add IvanLi-CN/style-playbook-skills --skill semver-change-governance --yes`; the CLI detects the Agent and project layout.

## Package resources

- `assets/templates/version-impact-record.example.json`
