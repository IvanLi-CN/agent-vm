# Contributing

Agent VM changes must keep implementation, tests and the matching contract or
ADR together. The repository is standalone: do not add imports or runtime
dependencies on `agent-toolkit`.

Run the portable checks before opening a change:

```bash
bun install --frozen-lockfile
bun run check
bun run test:repo
bun run test:skills
go -C skills/agent-vm-ops/control test -count=1 ./...
```

Changes to the Go control service should also be checked on Linux with:

```bash
go -C skills/agent-vm-ops/control test -race -count=1 ./...
```

Use English conventional commit messages with a signoff. Do not commit
credentials, deployment inventories, state databases, rendered consumer
configuration or host-specific addresses. Real Incus and systemd evidence
belongs to an explicitly authorized isolated validation environment and must be
reported separately from fixture and unit-test evidence.
