# Agent VM Token API

The Token API is the same-origin `/api/v1/tokens` surface used by an authenticated browser session. It issues credentials for the separate [Agent API](agent-api.md); it does not expose protocol-4 actions, Incus operations or arbitrary control requests. The browser session, Token and Agent API are three different credentials and must not be interchangeable.

## Identity and ownership

The authenticated principal is the exact case-sensitive `(issuer, subject)` pair produced by OIDC. Email, display name, group membership and a browser-provided identity header are not ownership keys. Every Token has one owner pair, an explicit set of session IDs, a bounded scope set, creation time, expiry and revocation state.

An ordinary user may list, create or revoke only Tokens owned by the current pair and may bind a new Token only to sessions owned by that pair. A guessed `token_id`, `session_id` or filter never broadens that scope. An administrator may inspect or revoke another user's Token only through an explicit administrator capability; cross-owner revocation requires a reason, a preview-bound confirmation token and an audit event. The administrator cannot turn an ordinary Token into an administrator Token by changing its scope, and `agent:observe_admin` may be requested only by a principal with the matching administrator observation capability.

Historical protocol-4 records or Tokens without a verified owner remain inaccessible to ordinary users. A migration or administrator ownership-mapping operation must bind the exact legacy record to one explicit `(issuer, subject)` pair before it can be used. The service must reject automatic email matching, first-login claims and ambiguous mappings.

## Endpoints

All browser mutations require the opaque browser session cookie, the session-bound `X-CSRF-Token` and an `Origin` exactly matching the configured HTTPS origin. All responses use the same structured, field-whitelisted error envelope as the management API.

| Method and path | Purpose | Ordinary-user rule |
| --- | --- | --- |
| `GET /api/v1/tokens` | List Token metadata with bounded cursor pagination. | Return only Tokens owned by the current `(issuer, subject)` pair. Never return the bearer value. |
| `POST /api/v1/tokens` | Create one Token with explicit session IDs, scopes and expiry; the operation is durably idempotent. | Every session and scope must belong to the current pair; return the plaintext Token once and never again. |
| `POST /api/v1/tokens/{token_id}/revoke` | Revoke a Token idempotently. | The owner may revoke its own Token; an administrator revoking another owner must supply reason, current version and confirmation. |
| `GET /api/v1/tokens/{token_id}` | Read one Token's metadata. | Return only if the current pair owns it, unless the caller has the explicit administrator read capability. |

The create request contains an operation ID for durable idempotency:

```json
{
  "operation_id": "50e8400e-ccf8-4c7b-8d1d-14f66c7fb126",
  "session_ids": ["session_example_01"],
  "scopes": ["agent:read", "agent:operate"],
  "expires_at": "2026-02-01T00:00:00Z"
}
```

The operation identity is the exact `(issuer, subject, operation_id, canonical request digest)` tuple. The authenticated browser session, CSRF token and exact configured `Origin` are checked before looking up or mutating that operation; a different principal cannot use an operation ID to discover whether another principal created a Token. The service must apply a configured maximum lifetime, reject an empty scope or session set, and reject an `agent:observe_admin` request unless the current principal has that administrator capability.

On the first successful create transaction, the service generates the high-entropy opaque value, stores only its versioned validation digest and safe metadata, creates the `token_create` operation result, and atomically consumes a persistent one-time plaintext-disclosure latch before sending the `201` response. That response contains the `operation_id`, `token_id`, owner `(issuer, subject)`, session IDs, scopes, expiry, disclosure state and a `token` field. The bearer is held only long enough to form that response; it is never written to the database, operation record, audit event or log.

An identical request arriving concurrently or after the latch is consumed returns the existing operation outcome and safe Token metadata without a `token` field, with disclosure state `consumed`. A retry with a different canonical request returns `operation_conflict`; a request from another principal is denied without an existence oracle. The latch is consumed before the first response is sent, so a process crash or network loss after the durable commit is treated as a lost response and never authorizes plaintext re-disclosure. `GET /api/v1/operations/{operation_id}` (or the equivalent operation page) is owner-scoped and returns the operation target `token_id`, safe `token_metadata` and disclosure state, but never the bearer.

For a lost create response, the owner keeps the original operation ID, queries its operation status until the authoritative state is available, confirms the returned `token_id` and metadata, and explicitly revokes that Token with a new revoke operation and bounded reason. The owner then creates a replacement Token with a new operation ID. The service never automatically reissues plaintext, creates a guessed replacement, or treats a timeout as proof that no Token exists. Only an authoritative result proving that the original operation never committed permits retrying the same operation ID.

The revoke request contains an operation ID and bounded reason. For cross-owner administration it also contains the current `target_version` and a single-use confirmation token bound to the administrator, exact Token ID and current metadata. Reusing an operation ID with a different canonical request returns `operation_conflict`; repeating the same request returns the existing outcome.

## Storage and request checks

The service stores only a versioned validation digest, owner pair, allowed session IDs, scopes, timestamps, revocation state and the one-time plaintext-disclosure state. It rechecks expiry, revocation, owner and scope on every Agent API request. Token creation and revocation are durable operations in the shared state journal; the Token create operation result contains only the Token ID, safe metadata and disclosure state. A response timeout is unknown and does not authorize plaintext re-disclosure, a new Token under the same operation, or a guessed retry.

Token values never appear in URLs, query strings, browser local storage, static assets, operation reasons, audit payloads, native command arguments or unredacted logs. The Agent API receives the value only in the HTTPS `Authorization` header, and the service redacts that header before logging or forwarding any diagnostic.

## Compatibility

Token API versioning follows the release manifest's request/response/semantic compatibility window. A blue-green candidate may serve the Token API only when both slots can read and write its state capability. A rollback that cannot preserve Token expiry, revocation, owner and scope semantics is rejected before changing the active slot. Token API changes do not change protocol 4; the [Agent API contract](agent-api.md) owns the machine request compatibility window.
