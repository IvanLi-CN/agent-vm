# Agent VM

Agent VM is the source repository for the reusable client, Go control service,
release tooling and operational contracts for low-cost nested Incus system
containers. It is the extracted canonical source for Agent VM work; it does not
depend on `agent-toolkit` at runtime.

The initial standalone snapshot is based on the merged Agent VM implementation
at `2651fc8a8d32eaa611027fc04aa7908843639222` from `agent-toolkit`.

## Architecture

The [system architecture](docs/architecture/system.md) describes the selected component boundaries and identifies the parts that are not yet implemented. Agent VM uses Incus system containers and does not require KVM or `/dev/kvm`.

## Layout

- `skills/agent-vm/` contains the local client and source-sync behavior.
- `skills/agent-vm-ops/` contains the Go control service, compatibility bridge,
  release tools, systemd assets and operations Skill.
- `docs/specs/agent-vm/` contains the public behavior and protocol contracts.
- `docs/adr/` contains the accepted lifecycle, transport, recovery and runtime
  decisions.

See the [documentation guide](docs/README.md) for architecture, specifications, decisions and operations.

## Management console design

The built-in Web management console is a product target and has not been implemented. Its scope covers managed images and existing Agent session VMs, with controlled maintenance and audit. The approved first-release contract fixes five pages, `zh-CN`/`en` localization, the authenticated same-origin `/api/v1` boundary, image migration, and package rollback compatibility.

Read [PRODUCT.md](PRODUCT.md) for product boundaries, [DESIGN.md](DESIGN.md) for the selected workbench baseline, and the [Web surface brief](.impeccable/surfaces/web.md) for task flows. The [management contract](docs/specs/agent-vm/contracts/management-api.md), [OpenAPI document](docs/specs/agent-vm/contracts/management-api.yaml), and its synthetic examples define the shared frontend/backend API and state-compatibility boundary. The [decision memo](docs/proposals/agent-vm-web-console.md) records the source investigation and architecture rationale.

## Local validation

```bash
bun install --frozen-lockfile
bun run check
bun run test:repo
bun run test:skills
go -C skills/agent-vm-ops/control test -count=1 ./...
go -C skills/agent-vm-ops/control vet ./...
```

Linux native and nested-Incus acceptance requires a session-owned Agent VM and
an explicitly authorized isolated control environment. GitHub Actions runs
portable Bun/Python checks, Go tests, Go vet, Linux cross-builds and Linux race
tests; it does not claim real Incus or systemd acceptance.

Deployment configuration, credentials, host addresses, Incus inventories,
state databases and rendered observability files remain outside this repository.
