# Agent VM Token API

The Token API is the same-origin `/api/v1/tokens` surface used by the sole authenticated administrator. It issues machine credentials for the separate [Agent API](agent-api.md); it does not expose protocol-4 actions, Incus operations or arbitrary control requests. The browser session, machine Token and Agent API are three different credentials and must not be interchangeable.

## Identity and target model

The authenticated administrator is the exact case-sensitive `(issuer, subject)` pair produced by OIDC. Email, display name, group membership and browser-provided identity headers are not identity keys. This pair is the actor recorded for authenticated issuance, revocation and their audit entries; rejected unauthenticated or unauthorized requests use the typed actor defined by the management contract and are not assigned administrator identity. The pair is not a user-resource relation.

Every machine Token has an opaque `token_id`, an opaque `resource_version`, a bounded scope set, creation time, expiry and revocation state. It has no owner field and no pre-bound session set. A valid, unexpired, non-revoked Token may target any `session_id` carried by a protocol-4 request when its scope permits the action; the service still checks the target session's generation, lease, native identity and state for every request.

## Endpoints

All browser mutations require the opaque browser session cookie, the session-bound `X-CSRF-Token` and an `Origin` exactly matching the configured HTTPS origin. The administrator must have `manage_tokens` for Token endpoints. All responses use the same structured, field-whitelisted error envelope as the management API.

| Method and path | Purpose | Additional rule |
| --- | --- | --- |
| `GET /api/v1/tokens` | List Token metadata with bounded cursor pagination. | Return safe metadata only; never return a bearer value. |
| `POST /api/v1/tokens` | Create one machine Token with scopes and expiry; the operation is durably idempotent. | Return the plaintext Token once and never again. |
| `POST /api/v1/tokens/{token_id}/revoke/preview` | Preview Token revocation and issue a short-lived confirmation token. | The preview binds the exact Token metadata and current `target_version`. |
| `POST /api/v1/tokens/{token_id}/revoke` | Revoke a Token idempotently. | The request must carry the current preview version and single-use confirmation token. |
| `GET /api/v1/tokens/{token_id}` | Read one Token's metadata. | Return safe metadata only, including `resource_version`. |

The create request contains an operation ID for durable idempotency. It may also contain an audit `reason`; omission is valid. If `scopes` includes `agent:operate`, it must also explicitly include `agent:read` so the same Token can query status/reconciliation after a lost response; the service never infers one scope from another.

```json
{
  "operation_id": "50e8400e-ccf8-4c7b-8d1d-14f66c7fb126",
  "scopes": ["agent:read", "agent:operate"],
  "expires_at": "2026-02-01T00:00:00Z"
}
```

The operation identity is the exact `(issuer, subject, operation_id, canonical request digest)` tuple. The authenticated browser session, CSRF token and exact configured `Origin` are checked before looking up or mutating that operation. The service must apply a configured maximum lifetime, reject an empty scope set, and accept `agent:observe_admin` only as an explicit machine scope for read-only administrator observations.

On the first successful create transaction, the service generates the high-entropy opaque value, stores only its versioned validation digest and safe metadata, creates the `token_create` operation result, and atomically consumes a persistent one-time plaintext-disclosure latch before sending the `201` response. The succeeded operation result is mandatory and contains the `token_id` through safe `token_metadata` plus the durable `plaintext_disclosure` state; it never contains the bearer value. That response contains the `operation_id`, `token_id`, `resource_version`, scopes, expiry, revocation state, disclosure state and a `token` field. The bearer is held only long enough to form that response; it is never written to the database, operation record, audit event or log.

An identical request arriving concurrently or after the latch is consumed returns the existing operation outcome and safe Token metadata without a `token` field, with disclosure state `consumed`. A retry with a different canonical request returns `operation_conflict`; the administrator actor is part of the operation identity. The latch is consumed before the first response is sent, so a process crash or network loss after the durable commit is treated as a lost response and never authorizes plaintext re-disclosure. `GET /api/v1/operations/{operation_id}` returns the operation target `token_id`, safe `token_metadata` and disclosure state, but never the bearer.

For a lost create response, the administrator keeps the original operation ID, queries its operation status until the authoritative state is available, confirms the returned `token_id` and metadata, and explicitly revokes that Token with a new revoke operation and an optional bounded reason. The administrator then creates a replacement Token with a new operation ID. The service never automatically reissues plaintext, creates a guessed replacement, or treats a timeout as proof that no Token exists. Only an authoritative result proving that the original operation never committed permits retrying the same operation ID.

The revoke preview is available at `POST /api/v1/tokens/{token_id}/revoke/preview` with the current `target_version`. It returns the exact Token ID, safe current metadata, a short-lived (no more than five minutes), single-use `confirmation_token` bound to the administrator's `(issuer, subject)`, action, Token ID, target version and metadata. The revoke request contains an operation ID, the same `target_version` and that confirmation token; an optional reason is retained when supplied and is not required. Reusing an operation ID with a different canonical request returns `operation_conflict`; repeating the same request returns the existing outcome.

## Storage and request checks

The service stores only a versioned validation digest, opaque Token resource version, scopes, timestamps, revocation state and the one-time plaintext-disclosure state. It rechecks expiry, revocation, scope, target session, generation, lease and state on every Agent API request. Token creation and revocation are durable operations in the shared state journal. Revocation sets `revoked_at` and advances `resource_version` atomically before the revoke response is acknowledged, and Agent authorization observes that state with a linearizable read; an already-authorized in-flight operation is not retroactively rolled back, but no later authorization succeeds. A stale target version is rejected before mutation. A response timeout is unknown and does not authorize plaintext re-disclosure, a new Token under the same operation, or a guessed retry.

Token values never appear in URLs, query strings, browser local storage, static assets, operation reasons, audit payloads, native command arguments or unredacted logs. The Agent API receives the value only in the HTTPS `Authorization` header, and the service redacts that header before logging or forwarding any diagnostic.

## Compatibility

Token API versioning follows the release manifest's request/response/semantic compatibility window. A blue-green candidate may serve the Token API only when both slots can read and write its state capability, including `resource_version` and revocation semantics. A rollback that cannot preserve Token expiry, revocation, scope, target-session checks and one-time disclosure semantics is rejected before changing the active slot. Token API changes do not change protocol 4; the [Agent API contract](agent-api.md) owns the machine request compatibility window.
