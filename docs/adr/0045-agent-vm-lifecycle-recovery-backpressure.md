# ADR 0045: Lifecycle Recovery and Maintenance Backpressure

## Status

Accepted for the protocol-4 Go control runtime.

## Context

Lifecycle recovery needs to distinguish a durable intent, a native effect, and
an observation whose outcome is not yet proven. The resident scheduler also
needs to remain useful after restart without producing an unbounded journal of
timer operations or allowing a large task inventory to monopolize the control
service. Administrative inventory must expose uncertainty instead of
collapsing a slow or unavailable native probe into a healthy result.

## Decision

The lifecycle state machine remains the existing seven-state set:
`absent`, `cached`, `preparing`, `active`, `closing`, `deleting`, and
`failed`. `native_missing` is an observation detail only. It is set only when a
bounded native query returns the exact instance-and-project contextual
not-found diagnostic; transport errors, timeouts, truncated output and
unrelated not-found responses remain unverified.

Every native-effect operation keeps its durable intent, operation identity,
generation, request digest and worker identity. Finalization cannot change the
operation identity or replace an established PID/start-time/process-group
identity. An uncertain native or SQLite result remains pending or explicitly
unknown and is never replayed with a new operation ID. Failed acquire cleanup
may clear only an explicitly pre-native, same-generation, worker-free intent.
Terminal absence additionally requires the destroy journal, generation
tombstone, worker quiescence and exact native absence.

The scheduler owns two single-flight maintenance lanes, `reconcile` and
`age-gc`. Their additive JSON cursor is stored under the `maintenance_cursor`
meta key and contains `next_attempt_at`, `attempt`, `scan_after_task_id`,
`last_error_code`, `last_error_phase` and `updated_at` per lane. Each round
processes at most 16 tasks or 2 seconds. A successful durable task/journal
commit permits the keyset cursor to advance; a failed task does not advance it.
Failures use the bounded 2/4/8/16/32/60-second backoff. A scheduler tick or
wake calls the lane directly, so it creates no timer journal operation. Legacy
`timer-reconcile` and `timer-age-gc` rows remain readable and recoverable.

`admin-status` is a read-only keyset-paginated observation. It defaults to 25
tasks and accepts at most 100. It uses at most 8 concurrent probes, with a
1-second item deadline and a 5-second page deadline. The response includes
`partial`, `unknown`, `observation_consistent`, `probed`, `verified` and
`timed_out`; an unknown observation is never reported as healthy.

No protocol, SQLite schema, task state, distributed scheduler, remote control
path or new worker pool is introduced. An absent cursor is treated as due, so
older databases remain compatible and initialize the additive key on the
first durable maintenance commit.

## Consequences

Restart recovery is bounded and restartable, while older timer intents retain
their original operation identity and evidence. A maintenance failure may
remain visible for a backoff interval, but it cannot silently skip the failed
task. Inventory pages are bounded and useful under partial native availability;
callers must handle `partial` and `unknown` rather than infer readiness from
the presence of task rows.

The cursor is metadata, not a replacement for the operation journal. It may be
rewound by restoring the database and journal together, but it must not be
advanced independently of the durable work it describes.

## Rejected alternatives

- Adding `reconciling`, `unknown` or `native_missing` as task states would mix
  observation and lifecycle authority and break the existing protocol-4 state
  contract.
- Creating one timer operation per tick would preserve an audit-looking trace
  at the cost of unbounded journal growth and unnecessary operation identity
  churn.
- A distributed scheduler or a new worker pool would expand the authority and
  shutdown boundaries without improving the single-process recovery contract.
- Treating any Incus 404, transport failure or timeout as absence would permit
  destructive cleanup without an ownership proof.
