# Agent VM Token API

The Token API is the same-origin `/api/v1/tokens` surface used by an authenticated browser session. It issues credentials for the separate [Agent API](agent-api.md); it does not expose protocol-4 actions, Incus operations or arbitrary control requests. The browser session, Token and Agent API are three different credentials and must not be interchangeable.

## Identity and ownership

The authenticated principal is the exact case-sensitive `(issuer, subject)` pair produced by OIDC. Email, display name, group membership and a browser-provided identity header are not ownership keys. Every Token has one owner pair, an explicit set of session IDs, a bounded scope set, creation time, expiry and revocation state.

An ordinary user may list, create, rotate or revoke only Tokens owned by the current pair and may bind a new Token only to sessions owned by that pair. A guessed `token_id`, `session_id` or filter never broadens that scope. An administrator may inspect or revoke another user's Token only through an explicit administrator capability; cross-owner revocation requires a reason, a preview-bound confirmation token and an audit event. The administrator cannot turn an ordinary Token into an administrator Token by changing its scope.

Historical protocol-4 records or Tokens without a verified owner remain inaccessible to ordinary users. A migration or administrator ownership-mapping operation must bind the exact legacy record to one explicit `(issuer, subject)` pair before it can be used. The service must reject automatic email matching, first-login claims and ambiguous mappings.

## Endpoints

All browser mutations require the opaque browser session cookie, the session-bound `X-CSRF-Token` and an `Origin` exactly matching the configured HTTPS origin. All responses use the same structured, field-whitelisted error envelope as the management API.

| Method and path | Purpose | Ordinary-user rule |
| --- | --- | --- |
| `GET /api/v1/tokens` | List Token metadata with bounded cursor pagination. | Return only Tokens owned by the current `(issuer, subject)` pair. Never return the bearer value. |
| `POST /api/v1/tokens` | Create one Token with explicit session IDs, scopes and expiry. | Every session and scope must belong to the current pair; return the plaintext Token once and never again. |
| `POST /api/v1/tokens/{token_id}/revoke` | Revoke a Token idempotently. | The owner may revoke its own Token; an administrator revoking another owner must supply reason, current version and confirmation. |
| `GET /api/v1/tokens/{token_id}` | Read one Token's metadata. | Return only if the current pair owns it, unless the caller has the explicit administrator read capability. |

The create request contains:

```json
{
  "session_ids": ["session_example_01"],
  "scopes": ["agent:read", "agent:operate"],
  "expires_at": "2026-02-01T00:00:00Z"
}
```

The response contains a `token_id`, owner `(issuer, subject)`, session IDs, scopes, expiry and a `token` field only on successful creation. The plaintext value is high entropy, opaque and shown once; a later read, audit event, operation record or log contains only the Token ID and safe metadata. The service must apply a configured maximum lifetime and reject an empty scope or session set.

The revoke request contains an operation ID and bounded reason. For cross-owner administration it also contains the current `target_version` and a single-use confirmation token bound to the administrator, exact Token ID and current metadata. Reusing an operation ID with a different canonical request returns `operation_conflict`; repeating the same request returns the existing outcome.

## Storage and request checks

The service stores only a versioned validation digest, owner pair, allowed session IDs, scopes, timestamps and revocation state. It rechecks expiry, revocation, owner and scope on every Agent API request. Token creation, rotation and revocation are durable operations in the shared state journal; a response timeout is unknown and does not authorize a new Token or a guessed retry.

Token values never appear in URLs, query strings, browser local storage, static assets, operation reasons, audit payloads, native command arguments or unredacted logs. The Agent API receives the value only in the HTTPS `Authorization` header, and the service redacts that header before logging or forwarding any diagnostic.

## Compatibility

Token API versioning follows the release manifest's request/response/semantic compatibility window. A blue-green candidate may serve the Token API only when both slots can read and write its state capability. A rollback that cannot preserve Token expiry, revocation, owner and scope semantics is rejected before changing the active slot. Token API changes do not change protocol 4; the [Agent API contract](agent-api.md) owns the machine request compatibility window.
