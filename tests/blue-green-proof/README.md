# rqlite 10.5.2 blue-green state proof

This directory contains a bounded feasibility experiment for the shared-state
contract in `docs/architecture/system.md`. It does not deploy rqlite, Caddy,
the Agent VM service, or any production data.

The experiment starts one loopback-only rqlite 10.5.2 process inside the
session-owned Agent VM. It uses the non-queued
`POST /db/execute?transaction` API and `GET /db/query?level=strong` for
verification. The SQL model is deliberately small: an operation journal, one
conditional slot row, and a trigger-backed database-visible effect row.
The runner chooses free loopback ports when none are supplied and verifies the
rqlite status PID, node ID, and bound HTTP address before it writes any schema.

Run it from the repository root in an acquired Agent VM session:

```sh
bun skills/agent-vm/scripts/agent_vm.ts doctor
bun skills/agent-vm/scripts/agent_vm.ts --session-id <session-id> status
bun skills/agent-vm/scripts/agent_vm.ts --session-id <session-id> exec -- \
  sh -lc 'cd /workspace/worktrees/<name>/src && tests/blue-green-proof/run.sh'
```

`run.sh` downloads the exact official amd64 release asset, verifies the pinned
SHA-256, starts rqlite on loopback, and emits one JSON evidence document. Set
`KEEP_PROOF_DIR=1` to retain the guest-side `evidence.json` and `rqlite.log`
under the reported temporary directory for inspection.

The evidence has three deliberately separate sections:

- `actual_observations` records the version, returned operation outcomes,
  final durable rows, the injected transaction error, and the response-drop
  transport observation.
- `inferences` states what those observations support for this SQL shape.
- `not_proven` records the boundaries that the experiment cannot establish.

The expected successful verdict is
`db_level_contract_observed_with_external_effect_scope_limit`. It means the
database-level proof ran and the three requested behaviors were observed for
the model. It is not a claim that an external Incus/native-worker effect is
transactionally atomic, that a cluster survives failover, or that every Agent
VM transition is already expressed safely in rqlite.

The response-loss case uses a local one-shot TCP proxy. The proxy forwards the
request, waits until the upstream HTTP response is complete, discards that
response, and closes the client connection. The client then queries the
original operation ID and retries the same transaction. A passing run requires
the original outcome to remain `success`, the slot generation to remain `1`,
and the operation-keyed durable effect count to remain `1` after the retry. The
script also retries that operation ID with a different request digest and
requires the transaction to fail while preserving the original journal row and
effect.

The pinned asset is
`rqlite-v10.5.2-linux-amd64.tar.gz` from the
[rqlite v10.5.2 release](https://github.com/rqlite/rqlite/releases/tag/v10.5.2)
with SHA-256
`1a04a7c2c542e8e1c1347bd31af0fa9ef7043febb3c4d1806d36bc1702c6fa0f`.
