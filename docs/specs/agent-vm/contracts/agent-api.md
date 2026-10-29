# Agent VM Agent API

The Agent API is the direct HTTPS transport for the existing Agent VM control contract. Its stable endpoint is `POST /agent/v1/control`, routed by the project Caddy entry point to the active blue or green Go/Gin service. It is separate from the browser management API and does not add a second lifecycle protocol.

## Request boundary

Each request must include:

- `Authorization: Bearer <opaque-token>` from the [Token API](token-api.md).
- `Content-Type: application/json` and one protocol-4 request body.
- The existing `action`, `session_id` (when the action is session-scoped), `operation_id` (for mutations) and `generation` (for mutations) fields required by the [CLI contract](cli.md).

The endpoint accepts only the fixed method, path, request size, JSON shape and action set. It does not accept browser cookies, OIDC codes, forwarded identity headers, arbitrary CLI commands, a file path, an Incus operation, or a request that asks the server to select a different slot. Caddy may route the request to either slot, but the service is responsible for all authentication and authorization.

## Authorization

The service resolves the Token's validation digest to its exact `(issuer, subject)` owner, checks expiry and revocation, and verifies that the requested scope covers the action. For a session-scoped action, the request `session_id` must be one of the Token's bound sessions and owned by that same pair. The service then checks the current generation, lease, resource ownership, admission state and operation journal before any native effect.

An ordinary user Token cannot list or mutate another user's session, use a sessionless administrator action, or elevate its scope by changing JSON. Sessionless read-only observation such as `admin-metrics` requires a separately granted administrator observation scope. Administrator Token use remains subject to the administrator capability and audit policy; a machine Token does not bypass reason, confirmation or state checks for sensitive management operations.

## Response and recovery

The response keeps the existing protocol-4 success and error envelope, including protocol, action, session identity, generation, operation ID, outcome and structured compatibility warnings. The Agent API does not expose raw SQL, OIDC, Token or native diagnostics. A successful request, a verified rejection and an uncertain native outcome remain distinguishable.

If the HTTP response is lost or the state backend reports an unknown commit, the client retains the original operation ID and generation and queries status/reconciliation through the same Token. It must not create a new operation ID to retry an unknown native effect. The service keeps pending journal evidence until it can verify the native outcome; a new intent needs a new operation ID only after the original outcome is resolved as safe.

The Agent API returns a structured state-backend failure during migration. The legacy protocol-4 error label `sqlite_unavailable` remains accepted as a compatibility label for a state backend outage, while new browser and Agent API documentation uses the backend-neutral `state_unavailable` meaning. No error response includes Token values, credentials, private paths or raw command output.

## Compatibility window and transport transition

The release manifest declares per-action request, response and semantic compatibility for `legacy-bun`, `go-request` and direct HTTPS transport. During the blue-green window, both slots must accept their shared protocol-4 compatibility set and the active release must retain the previous verified package for at least one hour of static-resource/API compatibility. SSH remains a compatibility transport while the declared window is open; it is not a second control authority.

The direct HTTPS transport does not alter session ownership, generation, operation identity, journal ordering, image binding, lease or native-worker recovery rules. An incompatible transport or action is rejected before durable or native effects, and a release that cannot read/write the shared state capabilities is not eligible for activation or rollback.
