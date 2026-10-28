# Agent VM Management Contract

The built-in management console uses the same-origin [Management API](management-api.yaml) and its [synthetic shared examples](management/examples/). The API is a browser-facing surface for existing Agent VM resources; it is independent of the client protocol, internal control API, and deployment observation bridge.

## Authentication and process boundary

The first release uses Forward Auth at an operator-managed HTTPS reverse proxy. The proxy connects to the Web process through a dedicated Unix socket. The Web process accepts forwarded identity only when the socket peer UID matches its configured proxy UID and the stable subject is in the explicit administrator allowlist. The proxy must remove identity headers received from the browser before setting its own `X-Forwarded-User` header. Group headers, browser-supplied identity and an unlisted subject never grant access.

The Web process runs as its dedicated non-root system user. It has no direct SQLite access, control-service private configuration, Incus credentials, or membership in an Incus administration group. It calls the sole Go control authority through a separate management UDS. The authority verifies the Web peer UID, administrator subject and fixed action before dispatch. It rechecks ownership, generation, lease, image-reference and journal rules inside the authority.

The Web listener has no public TCP mode or authentication bypass. The reverse proxy owns TLS and login. The authenticated external origin, trusted proxy UID, Web UID, authority socket and authority peer UID, forwarded-identity header, administrator subject allowlist, allowed trusted image source IDs, upload maximum, private staging directory and minimum free-space reserve are explicit deployment inputs. Configuration is incomplete when a required value is missing, empty or inconsistent; in that case the management listener remains disabled. No deployment address, subject, credential, filesystem path or live inventory belongs in this repository.

The immutable Web and authority configuration has these required names and meanings:

| Field | Meaning and validation |
| --- | --- |
| `management.enabled` | Defaults to `false`; becomes `true` only after every enabled boundary below validates. |
| `management.external_origin` | One normalized HTTPS origin, including a non-default port when used; no path, wildcard or list. |
| `management.forward_auth.socket_path` | Private Unix socket on which the trusted reverse proxy reaches Web. |
| `management.forward_auth.peer_uid` | Exact non-root UID required on the accepted proxy socket peer. |
| `management.forward_auth.identity_header` | Fixed to `X-Forwarded-User`; the proxy strips browser-supplied values before writing it. |
| `management.allowed_admin_subjects` | Non-empty, unique stable-subject allowlist shared by Web and authority; group claims do not grant access. |
| `management.web_uid` | Dedicated service UID used by Web and presented on the authority UDS. |
| `management.authority.socket_path` | Dedicated local management UDS; it is distinct from the client/control UDS. |
| `management.authority.expected_web_uid` | Must equal `management.web_uid`; authority rejects every other peer UID. |
| `management.session_ttl_seconds` | Opaque in-memory Web session lifetime, from 60 through 3600 seconds; default is 900 seconds. A Web restart invalidates all sessions. |
| `management.trusted_image_source_ids` | Unique IDs resolved only through preconfigured Incus remotes; URLs, aliases and credentials are not accepted from callers. |
| `management.upload.enabled` | Enables the local upload route; false disables it without disabling trusted-source imports. |
| `management.upload.max_bytes` | Positive decimal byte ceiling required when upload is enabled. |
| `management.upload.staging_path` | Absolute, private, service-owned staging directory outside the repository and guest data. |
| `management.upload.minimum_free_bytes` | Positive physical-free-space reserve required before a local upload begins and while staging; when trusted image sources are configured, the same reserve is checked against the configured Incus pool before remote import. |

Web and authority consume separate read-only configuration files. The package configuration validator must require the administrator subject sets and Web peer UID to agree before enabling the listener. All socket paths are deployment-local and must not be published in examples.

The Web process issues a short-lived opaque browser session cookie with `Secure`, `HttpOnly`, `SameSite=Strict`, `Path=/` and `Max-Age` equal to the configured session TTL. `GET /session` returns a random session-bound CSRF token. Every `POST` and `PUT` must carry that cookie, `X-CSRF-Token`, and an `Origin` exactly equal to the configured scheme, host and port. CORS is disabled. The cookie and CSRF token do not replace Forward Auth or the administrator allowlist.

## Wire behavior

`management-api.yaml` is the wire contract for `/api/v1`. The backend and frontend consume the same request, response and error shapes. This API does not wrap arbitrary CLI commands or forward arbitrary internal-control request bodies. Unknown fields, unsupported enum values, extra properties, stale target versions and actions outside the enumerated allowlist are rejected.

List endpoints use opaque keyset cursors. The default limit is 25 and the maximum is 100. Timestamps are UTC RFC 3339 values. Byte counts and generation values are decimal strings so TypeScript clients do not lose precision. A missing, stale, failed or unverified observation is returned with its state and a null value; it is never converted to zero, false or healthy.

Every resource exposes an opaque `resource_version`. Write requests carry that version as `target_version`; VM actions also carry the exact `expected_generation`. The version is a concurrency guard, not a user-editable label. The API returns `stale_target` when the version, generation, lease, ownership or image references changed. The client must reload the resource and request a new preview; it must not silently retarget or replay a dangerous action.

All mutations use a client-generated UUID `operation_id`, a non-empty reason of at most 512 characters, and the current `target_version`. The authenticated actor is derived from Forward Auth and is never accepted from the request body. The same operation ID and identical canonical request returns the same durable operation. Reusing it with a different request returns `operation_conflict`. A disconnected browser, timeout or `outcome_unknown` state does not mean failure and does not authorize a new operation ID. The caller queries `GET /operations/{operation_id}` and the audit record until the authority establishes an outcome.

`PUT /images/imports/{operation_id}/content` continues the already-created upload operation. Its path binds the operation ID, and the reason, actor and target version remain those stored by the creation request; the stream cannot create a second operation or change its metadata.

Actions that affect existing VMs or images, change the default image, or perform system maintenance first use their matching preview endpoint. A preview reports affected sessions, image-reference counts and a localized-safe summary code. Its opaque confirmation token is bound to the authenticated subject, exact action, target, parameters and `target_version`, and expires promptly. Submission consumes that token and repeats all authorization, generation, lease, reference and capacity checks. A mismatch returns a conflict and performs no native effect. The token is a confirmation of the displayed impact, not an authorization credential.

Image import starts directly as a durable operation because it creates a new immutable candidate without changing an existing VM, image, default selection or maintenance state. A local upload must declare exact byte counts and SHA-256 values before upload; the authority applies the configured size ceiling and physical-space reserve before and during staging. A trusted-source import resolves the exact fingerprint and image size first, then checks the same reserve against the configured Incus pool before native copy. Both paths run integrity and suitability checks before the image can be selected.

The API uses stable machine error codes, phases and a small allowlist of safe structured fields. The frontend maps codes to English and Simplified Chinese copy. Unknown codes use a generic localized failure message. Responses never include raw command output, credentials, private paths, database content or internal stack traces. Audit events record the authenticated actor, reason, target, action, phase and outcome; the UI keeps user-entered reasons unchanged.

Confirmation tokens expire no later than five minutes after issue and cannot be reused.

## Product and language behavior

The first release has exactly two interface locales: `zh-CN` and `en`. Selection priority is a saved user choice, then the browser language, then English. Every navigation label, field label, accessible name, confirmation, validation message, status explanation and error has both translations. Technical identifiers, fingerprints, error codes and user-authored content are not translated.

The API returns locale-neutral values and stable codes. The frontend formats numbers and dates with `Intl` using the selected locale and the browser's local time zone; it displays the time zone with timestamps. No server-side translation catalog or third-party translation service is required.

The first viewport opens on Overview. Navigation covers Overview, VMs, Images, Operations & Audit, and System. The VM page keeps the selected resource identity, status evidence and allowed action context visible together. It has no manual VM creation flow. Overview, VM, image, operation and system data each retain source freshness and unknown state. The design direction and responsive behavior are specified in [DESIGN.md](../../../../DESIGN.md) and the [Web surface brief](../../../../.impeccable/surfaces/web.md); viewport widths in the delivery plan are evidence cases, not a general device-support promise.

## Image and VM invariants

Unified local uploads use one `application/octet-stream` payload with one expected size and SHA-256. Split local uploads use `multipart/form-data` with exactly one `metadata` and one `rootfs` part; each part has its own expected size and SHA-256 in the import request, and the upload ceiling applies to the sum of their payload sizes. Original filenames are not used as Incus aliases or filesystem paths.

An image is identified by its immutable Incus fingerprint. Version labels and configured source IDs are metadata, not identity. A trusted-remote import accepts only a configured `source_id` and an expected fingerprint; it does not accept an arbitrary URL, alias or credential. A local upload declares its package format, exact byte count and SHA-256 before the stream begins. The upload must fit the configured maximum and staging reserve, is received once, and is not resumable. A short, oversized, mismatched or interrupted stream becomes a queryable failed operation and its incomplete staging data is cleaned up.

Integrity and Agent VM suitability are separate results. Verification uses a bounded, isolated temporary system container without production credentials, host mounts or production network access. Only an image with passed integrity and suitability can be selected as a new default. Selection changes apply to new sessions only. Existing tasks and caches remain pinned to their stored fingerprint; changing the default never rewrites them. A cached VM resumes with its recorded fingerprint, while a missing, retired or otherwise unverified image is rejected until the exact session is explicitly destroyed.

The initial migration preserves the configured legacy default and each task's existing fingerprint. The legacy default remains usable as the compatibility default until an administrator selects a fully verified replacement, but it is displayed as `legacy_unverified` and cannot be selected again through the management API. Retiring an image prevents new selection but does not invalidate existing bindings. Deletion is rejected while the image is default or has any active, cached, releasing (closing, deleting or failed cleanup), pending-creation or verification reference. The authority rechecks references while holding the image operation lock before native deletion.

## State migration and package rollback

The management state is additive. The SQLite `state_capabilities` registry contains versioned capability markers; the API renders each marker as `name@version`. The first management release declares:

| Capability | Meaning |
| --- | --- |
| `legacy-agent-vm-core@1` | Existing protocol-4 tasks, generations, leases, reservations, ports and operation journal. |
| `managed-image-catalog@1` | Immutable image records, labels, verification, references and default selection. |
| `session-image-binding@1` | Session and cache identity remains bound to the exact image fingerprint. |
| `management-operation-audit@1` | Durable administrator operation identity, actor, reason, target, phase and outcome. |

The migration inserts each marker in the same transaction as its schema changes. It does not rewrite task IDs, session IDs, image fingerprints, generations, leases, ports, reservations, operation journals or existing audit data. It seeds the catalog from the current configured image alias and fingerprint without asserting new verification evidence. A failed or interrupted migration leaves the previous schema usable and does not partially publish a capability marker.

Default selection and new-session creation use one serialized authority boundary: creation resolves the current selection and records its pending image reference atomically, while default changes use the same boundary. This prevents a new task from observing a mixed selection or losing its image reference during a concurrent change.

The CLI remains on protocol 4. Its request shapes, command set, session identity rules, `status`, `admin status`, `admin session list`, and release-manifest action compatibility remain unchanged. Existing status data's `image_fingerprint` is the session pin. A default-image change never invalidates a cached session; lifecycle rejection is reserved for a missing or unverified pinned image. The management API does not add a public `agent-vm` command and does not change the session lifecycle protocol.

Product manifest format 4 adds `state_compatibility.reads` and `state_compatibility.writes`, each a sorted list of supported `name@version` markers. Formats 1, 2 and 3 have no such declaration and are treated as supporting only the pre-management state. An installer or rollback preflight reads the live marker set and permits activation only if the target release declares support to both read and write every present marker. Unknown or unreadable markers fail closed before changing the active package pointer or starting the target service.

The database is never downgraded or recreated during package rollback. A compatible target receives a complete package rollback and uses the same state database. An incompatible target is rejected before activation; the current package remains selected, admission stays drained, and the operation requires operator recovery. A post-migration health failure must not automatically launch a target that cannot read the new state. The product version is selected by the release process; it is independent of API version 1 and CLI protocol 4.

## Shared contract examples

The JSON files beside this contract are synthetic fixtures, contain no deployment facts, and are intended to be shared by backend and frontend contract tests:

- `overview.json` covers separate freshness and an unverified pressure value.
- `vms-page.json` covers a pinned image, generation, effective lease and bounded port data.
- `images-page.json` covers a verified candidate and a preserved legacy compatibility default.
- `image-import-request.json` covers an exact-size local upload declaration.
- `image-import-split-request.json` covers separate metadata and rootfs payload declarations.
- `vm-action-request.json` covers a preview-bound restart request.
- `operation.json` covers durable asynchronous progress.
- `audit-page.json` covers actor, reason, target and outcome.
- `system.json` covers product state capabilities and component readiness.
- `error.json` covers a stale-target conflict without internal diagnostics.
