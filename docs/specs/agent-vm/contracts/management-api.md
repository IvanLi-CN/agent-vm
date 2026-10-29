# Agent VM Management Contract

The built-in management console uses the same-origin `/api/v1` surface defined by [management-api.yaml](management-api.yaml) and its [synthetic shared examples](management/examples/). The API is served by the active blue or green Agent VM service. It manages existing Agent VM resources and is independent from the protocol-4 [Agent API](agent-api.md), the [Token API](token-api.md), and the deployment observation bridge.

## Authentication, identity and service boundary

The project Caddy entry point terminates TLS and forwards only fixed paths to the active service. It does not perform Forward Auth, inject `X-Forwarded-User`, accept browser-supplied identity headers or decide resource authorization. There is no independent Web process, management UDS, public TCP bypass, arbitrary router or browser-to-control proxy. The same Go/Gin service serves static assets, OIDC callbacks, `/api/v1`, `/agent/v1/control`, lifecycle rules and native workers; the high-privilege browser boundary is an explicit decision recorded in [ADR 0043](../../../adr/0043-agent-vm-unified-service-security-boundary.md).

The first release uses an external OIDC provider as a relying-party identity service. `GET /api/v1/auth/oidc/start` creates a one-time Authorization Code + PKCE transaction with `code_challenge_method=S256` only and sets a short-lived `Secure`, `HttpOnly`, `SameSite=Lax` login-binding cookie; `plain` or another PKCE method is rejected. The transaction binds random `state`, `nonce`, a protected code verifier, a digest of that browser-binding cookie, the exact redirect URI, client ID, creation/expiry time and consumed state in shared storage. `GET /api/v1/auth/oidc/callback` accepts either a successful response (`code` + matching `state`) or a provider denial/error (`error` + matching `state` and no `code`); both forms consume the transaction once and clear the binding cookie. A provider error redirects to a generic localized login-failure state without creating a session or echoing `error_description`/`error_uri`; malformed, missing-state, mismatch, cross-browser transfer or replay fails closed. The service validates TLS, the authorization code exchange, redirect URI, discovery issuer, ID Token signature/JWKS, exact `iss`, `aud`/`azp`, `exp`, `iat` policy and `nonce`; a provider outage fails closed. The service and Caddy omit callback query strings and `Referer` from access logs, and application logs never retain OIDC code, state, nonce, verifier, error description or error URI values.

The service represents the sole administrator as the exact case-sensitive `(issuer, subject)` pair. The issuer is the configured discovery issuer and the subject is the OIDC `sub`; email, display name, groups and forwarded headers are not identity keys. A successful login is accepted only when that pair matches the configured administrator principal, and the session receives the configured administrator capabilities. No ordinary-user role, user-resource relation or historical principal mapping exists; legacy task and native records are reconciled by task identity, generation, lease, journal and native identity.

The service creates an opaque browser session with `Secure`, `HttpOnly`, `SameSite=Strict`, `Path=/` and a bounded TTL. Session metadata stores the principal pair, role, capabilities, creation/expiry/revocation state and a digest of the session identifier in shared state. `GET /api/v1/session` returns the principal, role, capabilities and a session-bound CSRF token. Every `POST` and `PUT` carries the cookie, `X-CSRF-Token` and an `Origin` exactly equal to the configured HTTPS origin; CORS is disabled. The browser session is not an Agent Token and is never accepted by `/agent/v1/control`.

Required deployment inputs are explicit and incomplete configuration disables the management listener:

| Field | Meaning and validation |
| --- | --- |
| `management.enabled` | Defaults to `false`; enabled only after all required OIDC, origin, session, Token and staging limits validate. |
| `management.external_origin` | One normalized HTTPS origin, including a non-default port when used; no path, wildcard or list. |
| `management.oidc.issuer` | Exact discovery issuer URL; discovery metadata and ID Token `iss` must match it. |
| `management.oidc.client_id` | Registered client ID bound to the callback and ID Token audience check. |
| `management.oidc.redirect_uri` | One exact callback URI under the stable Caddy origin; it is stored in each login transaction. |
| `management.oidc.client_secret` | Protected deployment secret used only by the authorization-code exchange; never stored in repository files or logs. |
| `management.admin_principal` | One exact `(issuer, subject)` administrator allowlist entry with configured capabilities; no email or group-only entry. |
| `management.session_ttl_seconds` | Browser session lifetime from 60 through 3600 seconds; default 900 seconds. |
| `management.token_ttl_seconds` | Maximum Agent Token lifetime; each Token also carries its own expiry and revocation state. |
| `management.upload.*` | Bounded image upload, private staging and minimum free-space limits from the image contract. |

## Authorization and sensitive actions

The sole administrator uses the browser session and the [Token API](token-api.md). The API has no ordinary-user or cross-principal resource boundary: the administrator may inspect and manage every target permitted by the configured capability. Token creation requires a UUID operation ID and canonical retry identity bound to the exact administrator `(issuer, subject)` pair. Its durable operation status exposes only `token_id`, safe metadata and one-time disclosure state; a concurrent or repeated request never reissues plaintext. If the creation response is lost, the administrator queries the original operation, confirms and revokes the Token by ID with a new operation ID, then creates a replacement with another operation ID. `agent:observe_admin` is an explicit machine scope, not a user role. The capability matrix below is normative: the administrator role does not imply every action capability, and every endpoint rejects a request missing its mapped capability.

| Browser capability | Authorized management surface | Additional authorization rule |
| --- | --- | --- |
| `read` | `GET /api/v1/session`; `GET /api/v1/operations` and `GET /api/v1/operations/{operation_id}`. | Only the exact administrator principal may access the browser API; operation metadata remains field-whitelisted. |
| `operate` | VM action preview and submission for `restart`, `resize`, `release`, `destroy`, `port_add` and `port_remove`. | The target `target_version`, generation, lease and state are rechecked; sensitive actions still require reason, preview and confirmation. |
| `manage_tokens` | Token list, read, create, revoke and revoke-preview endpoints. | Token revocation requires the current target version and a preview-bound confirmation token. |
| `manage_images` | Image list, import, upload, default selection, retirement, deletion and their previews. | Integrity, suitability, reference, target-version and confirmation checks remain mandatory. |
| `maintain_system` | System maintenance preview and submission, including drain, reconcile and garbage collection. | The release transaction and state-write fence remain authoritative; a browser request cannot reopen admission. |
| `observe_admin` | Overview, VM inventory, image/system/audit views and administrator observation pages. | This capability does not grant mutation, Token issuance or the machine `agent:observe_admin` scope by itself. |

Capabilities are administrator-internal least-privilege gates. They are not ordinary-user roles, Token-to-user grants or session bindings. Capabilities are never inferred from email, group or an unverified header, and unknown action/capability combinations fail before any durable or native effect.

Every administrator mutation carries a UUID `operation_id`, a non-empty reason of at most 512 characters and the current `target_version`; VM actions also carry the exact expected generation. Actions that can affect an effective lease, image default/reference, deletion, maintenance or Token state first return an impact preview. Its confirmation token is opaque, short-lived (no more than five minutes), single-use and bound to the authenticated `(issuer, subject)`, exact target, action, parameters, target version and generation. Submission repeats authorization, reference, generation, lease, capacity and state checks inside the service.

An identical operation retry returns the same durable outcome; for Token creation, that outcome includes only safe metadata after the one-time plaintext disclosure latch is consumed. Reusing an operation ID with a different canonical request returns `operation_conflict`. A browser disconnect, timeout or `outcome_unknown` state does not authorize a new operation ID. The administrator queries `GET /operations/{operation_id}` and the audit record until the service establishes an outcome. A Token revoke commits `revoked_at` and advances the Token `resource_version` atomically in shared state before the `202` response is sent, and Agent authorization reads revocation with linearizable consistency; a request that has not crossed the authorization read after that commit is rejected. A native result that is not verified remains pending and is never replayed automatically.

Audit records cover rejected and accepted requests as well as successful, failed and unknown outcomes. Each record stores the actor's `(issuer, subject)`, role/capability, reason, operation ID, target, phase, safe error code and timestamp. It never stores OIDC codes, Token values, credentials, raw command output, SQL or private deployment paths. Responses use field-whitelisted structured errors and do not expose internal stack traces.

## Token and Agent API relationship

The Token API creates the opaque Bearer credentials used by `POST /agent/v1/control`; its complete machine-Token, scope, one-time plaintext and revocation rules are in [token-api.md](token-api.md). The Agent API retains protocol 4 and its operation/generation/journal semantics; its authentication and compatibility contract is in [agent-api.md](agent-api.md). The management API must not wrap an arbitrary protocol-4 request or accept an Agent Token as a browser session.

## Wire behavior

`management-api.yaml` is the wire contract for the same-origin `/api/v1` browser API. The backend and frontend consume the same request, response and error shapes. Unknown fields, unsupported enum values, extra properties, stale target versions and actions outside the enumerated allowlist are rejected.

List endpoints use opaque keyset cursors. The default limit is 25 and the maximum is 100. Timestamps are UTC RFC 3339 values. Byte counts and generation values are decimal strings so TypeScript clients do not lose precision. A missing, stale, failed or unverified observation is returned with its state and a null value; it is never converted to zero, false or healthy.

Every resource exposes an opaque `resource_version`. Write requests carry that version as `target_version`; VM actions also carry the exact `expected_generation`. The version is a concurrency guard, not a user-editable label. The API returns `stale_target` when the version, generation, lease, native identity or image references changed. The client must reload the resource and request a new preview; it must not silently retarget or replay a dangerous action.

Image import starts as a durable operation. A local upload declares exact byte counts and SHA-256 values before upload; the service applies the configured size ceiling and physical-space reserve before and during staging. A trusted-source import resolves the exact fingerprint and image size first, then checks the same reserve against the configured Incus pool before native copy. Both paths run integrity and suitability checks before an image can be selected.

The API uses stable machine error codes, phases and a small allowlist of safe structured fields. The frontend maps codes to English and Simplified Chinese copy. Unknown codes use a generic localized failure message. Responses never include raw command output, credentials, private paths, database content or internal stack traces. While admission is drained, the System page shows that recovery is release-managed and does not offer a browser `drain_exit` control; only the active release transaction may reopen admission.

## Product and language behavior

The first release has exactly two interface locales: `zh-CN` and `en`. Selection priority is a saved user choice, then the browser language, then English. Every navigation label, field label, accessible name, confirmation, validation message, status explanation and error has both translations. Technical identifiers, fingerprints, error codes and user-authored content are not translated.

The first viewport opens on Overview. Navigation covers Overview, VMs, Images, Operations & Audit, and System. The VM page keeps the selected resource identity, status evidence and allowed action context visible together. It has no manual VM creation flow. Overview, VM, image, operation and system data retain source freshness and unknown state. The design direction and responsive behavior are specified in [DESIGN.md](../../../../DESIGN.md) and the [Web surface brief](../../../../.impeccable/surfaces/web.md).

## Image and VM invariants

Unified local uploads use one `application/octet-stream` payload with one expected size and SHA-256. Split local uploads use `multipart/form-data` with exactly one `metadata` and one `rootfs` part; each part has its own expected size and SHA-256 in the import request, and the upload ceiling applies to the sum of their payload sizes. Original filenames are not used as Incus aliases or filesystem paths.

An image is identified by its immutable Incus fingerprint. Version labels and configured source IDs are metadata, not identity. A trusted-remote import accepts only a configured `source_id` and an expected fingerprint; it does not accept an arbitrary URL, alias or credential. A local upload declares its package format, exact byte count and SHA-256 before the stream begins. The upload must fit the configured maximum and staging reserve, is received once, and is not resumable. A short, oversized, mismatched or interrupted stream becomes a queryable failed operation and its incomplete staging data is cleaned up.

Integrity and Agent VM suitability are separate results. Verification uses a bounded, isolated temporary system container without production credentials, host mounts or production network access. Only an image with passed integrity and suitability can be selected as a new default. Selection changes apply to new sessions only. Existing tasks and caches remain pinned to their stored fingerprint. Retiring an image prevents new selection but does not invalidate existing bindings. Deletion is rejected while the image is default or has any active, cached, releasing, pending-creation or verification reference; reference checks are serialized with image operations.

## State migration and package rollback

The management state is additive. The `state_capabilities` registry contains versioned markers and the API renders each as `name@version`. The initial capability set retains the existing protocol-4 task, generation, lease, reservation, port and journal markers and adds the management catalog, session-image binding, operation-audit, principal-identity and Token-revocation markers. Migration preserves task IDs, session IDs, image fingerprints, generations, leases, ports, reservations, operation journals and existing audit evidence; it does not create user-to-session or user-to-Token ownership mappings.

SQLite remains the single writer during the pre-rqlite phase. The conservative migration described by [ADR 0044](../../../adr/0044-agent-vm-state-migration-to-rqlite.md) enters a state-write fence, rejects new browser/API/Agent durable mutations, pauses scheduler/reconciliation/timer durable writes, and requires every in-flight native worker to finish with a durable result under its original operation ID plus verified process-group, PID-identity and Incus-operation quiescence. An unresolved pending/unknown result, active worker or unverified quiescence blocks final export; it is not enough to record the outcome and continue. Only after all transactions and workers converge does the service close SQLite writers and export a consistent backup. It verifies schema/capability/row-count and identity summaries, imports through one rqlite transaction and validates before activating the shared backend. It never dual-writes, opens a shared SQLite file from both slots, downgrades a schema or recreates state to force rollback. After cutover, rqlite is the only live authority; critical reads and observation epoch boundaries use linearizable consistency. If rqlite becomes unavailable, the service fences durable mutations and native admission and preserves active leases without guessing. A verified rqlite backup/snapshot is the first recovery source, and it must record a coverage boundary at or after the last trusted commit. Recovery must reconcile the backup tail after that boundary using the trusted append-only journal and native-effect evidence; schema, digest, epoch or journal checks alone do not prove coverage. The pre-cutover SQLite snapshot may be re-imported only when evidence proves that no post-cutover durable write, epoch advance, lease change or native effect occurred. If the backup tail or any post-cutover write cannot be proven, recovery remains fenced for administrator-authorized journal/native reconciliation; it must not open stale SQLite as a competing authority or reconstruct state by guesswork.

The database is never downgraded or recreated during package rollback. An installed package may activate only when it declares support to read and write every present capability marker. A target that cannot preserve the administrator issuer/subject actor, Token revocation, journal, generation and unknown-outcome semantics is rejected before changing the active slot. The current package remains selected and admission stays drained for operator recovery.

## Compatibility window

The blue-green release contract retains the current and previous verified package and their fixed static resource manifests for at least one hour. Each package declares protocol, request/response/semantic windows, Token and management API versions, and state capability read/write sets. Both slots may receive requests during convergence only when the operation is in the shared compatibility set; Caddy never retries writes. A browser asset past the window receives a clear refresh response. SSH `legacy-bun`/`go-request` remains a compatibility transport until the declared window ends, while direct HTTPS uses the same protocol-4 body and does not change lifecycle semantics.

## Shared contract examples

The JSON files beside this contract are synthetic fixtures, contain no deployment facts, and are intended to be shared by backend and frontend contract tests:

- `overview.json` covers separate freshness and an unverified pressure observation.
- `vms-page.json` covers a pinned image, generation, effective lease and bounded port data.
- `images-page.json` covers a verified candidate and a preserved legacy compatibility default.
- `image-import-request.json` covers an exact-size local upload declaration.
- `image-import-split-request.json` covers separate metadata and rootfs payload declarations.
- `vm-action-request.json` covers a preview-bound restart request.
- `operation.json` covers durable asynchronous progress and an issuer/subject actor.
- `audit-page.json` covers an issuer/subject actor, reason, target and outcome.
- `system.json` covers the unified service, Caddy, rqlite and Incus readiness.
- `error.json` covers a stale-target conflict without internal diagnostics.
