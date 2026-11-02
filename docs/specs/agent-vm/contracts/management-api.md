# Agent VM 管理合同

内置 management console 使用[management-api.yaml](management-api.yaml)及其[synthetic shared examples](management/examples/)定义的 same-origin `/api/v1` surface。
API 由 active blue 或 green Agent VM service 提供。
它管理 existing Agent VM resources，并独立于 protocol-4 [Agent API](agent-api.md)、[Token API](token-api.md)和 deployment observation bridge。

## 认证、身份与服务边界

项目 Caddy 入口终止 TLS，只向 active service 转发固定路径。
它不执行 Forward Auth，不注入 `X-Forwarded-User`，不接受浏览器提供的身份标头，也不决定资源授权。
系统没有独立 Web 进程、公共管理 UDS、公共 TCP 绕过路径、任意路由器或浏览器到控制服务的代理。
同一个 Go/Gin 服务提供静态资源、OIDC 回调、`/api/v1`、`/agent/v1/control`、生命周期规则和 native worker；运维者专用的 deployd Unix socket 是独立的部署边界，Caddy 和浏览器都不可访问。
高权限浏览器边界是[ADR 0043](../../../adr/0043-agent-vm-unified-service-security-boundary.md) 明确记录的设计决定。

首个 release 使用 external OIDC provider 作为 relying-party identity service。
`GET /api/v1/auth/oidc/start` 创建一次性的 Authorization Code + PKCE transaction，仅允许 `code_challenge_method=S256`，并设置短时 `Secure`、`HttpOnly`、`SameSite=Lax` login-binding cookie；`plain` 或其他 PKCE method 必须拒绝。
Transaction 在 shared storage 中绑定 random `state`、`nonce`、protected code verifier、browser-binding cookie 的 digest、exact redirect URI、client ID、creation/expiry time 和 consumed state。
`GET /api/v1/auth/oidc/callback` 接受 successful response（`code` + matching `state`）或 provider denial/error（`error` + matching `state` 且无 `code`）；两种形式都只消费一次 transaction，并清除 binding cookie。
Provider error 重定向到 generic localized login-failure state，不创建 session，也不回显 `error_description`/`error_uri`；malformed、missing-state、mismatch、cross-browser transfer 或 replay 必须 fail closed。
Service 验证 TLS、authorization code exchange、redirect URI、discovery issuer、ID Token signature/JWKS、exact `iss`、`aud`/`azp`、`exp`、`iat` policy 和 `nonce`；provider outage 必须 fail closed。
Service 与 Caddy 从 access logs 中删除 callback query strings 和 `Referer`；application logs 绝不保留 OIDC code、state、nonce、verifier、error description 或 error URI values。

Service 将 sole administrator 表示为精确且区分大小写的 `(issuer, subject)` pair。
Issuer 是 configured discovery issuer，subject 是 OIDC `sub`；email、display name、groups 和 forwarded headers 不是 identity keys。
只有该 pair 匹配 configured administrator principal 时，successful login 才接受，并为 session 授予 configured administrator capabilities。
不存在 ordinary-user role、user-resource relation 或 historical principal mapping；legacy task 和 native records 根据 task identity、generation、lease、journal 和 native identity reconcile。

Service 创建 opaque browser session，使用 `Secure`、`HttpOnly`、`SameSite=Strict`、`Path=/` 和 bounded TTL。
Session metadata 在 shared state 中存储 principal pair、role、capabilities、creation/expiry/revocation state 以及 session identifier 的 digest。
`GET /api/v1/session` 返回 principal、role、capabilities 和 session-bound CSRF token。
每个 `POST` 和 `PUT` 携带 cookie、`X-CSRF-Token` 以及与 configured HTTPS origin 完全相等的 `Origin`；CORS disabled。
Browser session 不是 Agent Token，`/agent/v1/control` 永不接受它。

Required deployment inputs 必须显式提供；configuration 不完整时，management listener disabled：

| 字段 | 含义与校验 |
| --- | --- |
| `management.enabled` | 默认值为 `false`；只有所有 required OIDC、origin、session、Token 和 staging limits 验证通过后才启用。 |
| `management.external_origin` | 一个 normalized HTTPS origin；使用 non-default port 时必须包含该 port；不允许 path、wildcard 或 list。 |
| `management.oidc.issuer` | Exact discovery issuer URL；discovery metadata 和 ID Token `iss` 必须匹配。 |
| `management.oidc.client_id` | 绑定 callback 和 ID Token audience check 的 registered client ID。 |
| `management.oidc.redirect_uri` | stable Caddy origin 下的一个 exact callback URI；存储在每个 login transaction 中。 |
| `management.oidc.client_secret` | 仅用于 authorization-code exchange 的 protected deployment secret；不得存储在 repository files 或 logs 中。 |
| `management.admin_principal` | 一个带 configured capabilities 的 exact `(issuer, subject)` administrator allowlist entry；不接受仅 email 或 group 的 entry。 |
| `management.session_ttl_seconds` | Browser session lifetime 为 60 至 3600 秒；默认 900 秒。 |
| `management.token_ttl_seconds` | Maximum Agent Token lifetime；每个 Token 还携带自己的 expiry 和 revocation state。 |
| `management.upload.*` | 由 image contract 定义的 bounded image upload、private staging 和 minimum free-space limits。 |

## 授权与敏感操作

Sole administrator 使用 browser session 和[Token API](token-api.md)。
API 没有 ordinary-user 或 cross-principal resource boundary：administrator 可以 inspect/manage configured capability 允许的每个 target。
Token creation 要求 UUID operation ID 和绑定 exact administrator `(issuer, subject)` pair 的 canonical retry identity。
Requested `agent:operate` scope 必须同时显式带有 `agent:read` scope；这是 co-requirement，不是 implicit inheritance，因此 response 丢失后同一个 Token 仍可 query status/reconciliation。
Succeeded durable operation status 暴露 `token_id`、safe metadata 和 one-time disclosure state；successful `token_create` result 不能省略这些 fields，并且 concurrent 或 repeated request 绝不重新签发 plaintext。
Creation response 丢失时，administrator query original operation，使用 new operation ID 按 ID 确认并 revoke Token，然后用另一个 operation ID 创建 replacement。
`agent:observe_admin` 是 explicit machine scope，不是 user role。
下方 capability matrix 是 normative：administrator role 不代表拥有每个 action capability，每个 endpoint 都拒绝缺少对应 mapped capability 的 request。

| Browser capability | Authorized management surface | Additional authorization rule |
| --- | --- | --- |
| `read` | `GET /api/v1/session`；`GET /api/v1/operations` 和 `GET /api/v1/operations/{operation_id}`。 | 只有 exact administrator principal 可以访问 browser API；operation metadata 仍受 field whitelist 约束。 |
| `operate` | VM action preview 和 `restart`、`resize`、`release`、`destroy`、`port_add`、`port_remove` 的 submission。 | 重新检查 target `target_version`、generation、lease 和 state；sensitive actions 仍要求 preview 和 confirmation。可以提供 audit reason，但不是必需项。 |
| `manage_tokens` | Token list、read、create、revoke 和 revoke-preview endpoints。 | Token revocation 要求 current target version 和 preview-bound confirmation token。 |
| `manage_images` | Image list、import、upload、default selection、retirement、deletion 及其 previews。 | Integrity、suitability、reference、target-version 和 confirmation checks 仍是必需项。 |
| `maintain_system` | System maintenance preview 和 submission，包括 drain、reconcile 和 garbage collection。 | Release transaction 和 state-write fence 仍是权威；browser request 不能 reopen admission。 |
| `observe_admin` | Overview、VM inventory、image/system/audit views 和 administrator observation pages。 | 该 capability 本身不授予 mutation、Token issuance 或 machine `agent:observe_admin` scope。 |

Capabilities 是 administrator-internal least-privilege gates，不是 ordinary-user roles、Token-to-user grants 或 session bindings。
Capabilities 绝不从 email、group 或 unverified header 推断；unknown action/capability combinations 必须在任何 durable 或 native effect 前失败。

Every administrator mutation 携带 UUID `operation_id`；existing resource 的 mutation 携带 current `target_version`，VM actions 还携带 exact expected generation。
Durable creates（包括 Token issuance）使用 operation ID 和 canonical request identity，不要求 target version 或 preview。
任何 mutation MAY 包含 audit `reason`；提供时必须 non-empty 且不超过 512 characters，省略合法，不会拒绝 request。
Effective lease、image default/reference、deletion、maintenance 或 existing Token 的 change，首先返回 impact preview。
Token creation 是 durable-create exception，不需要 separate preview。
Confirmation token 是 opaque、short-lived（不超过 five minutes）、single-use，并绑定 authenticated `(issuer, subject)`、exact target、action、parameters、target version 和 generation。
Submission 在 service 内重新执行 authorization、reference、generation、lease、capacity 和 state checks。

Identical operation retry 返回相同 durable outcome；Token creation 在 one-time plaintext disclosure latch 被消费后，outcome 只包含 safe metadata。
使用不同 canonical request 重用 operation ID 时返回 `operation_conflict`。
Browser disconnect、timeout 或 `outcome_unknown` state 不授权创建 new operation ID。
Administrator 查询 `GET /operations/{operation_id}` 和 audit record，直到 service 建立 outcome。
Token revoke 在发送 `202` response 前，在 shared state 中原子 commit `revoked_at` 并推进 Token `resource_version`；Agent authorization 以 linearizable consistency 读取 revocation，因此 commit 后尚未越过 authorization read 的 request 会被拒绝。
未验证的 native result 保持 pending，绝不自动 replay。

Audit records 覆盖 rejected/accepted requests，以及 successful、failed 和 unknown outcomes。
每条 record 存储以下三者之一：current administrator actor 的 `(issuer, subject)` 和 role/capability；没有 role/capability 的 typed `unauthenticated` 或 `unauthorized` rejected-request actor；或明确的 `legacy_unknown` actor，并逐字保留 original historical value；绝不猜测 OIDC identity。
`legacy_unknown` 仅用于 historical records；rejected-request actors 不携带 inferred OIDC identity。
提供 optional reason 时存储，省略时明确保持 absent，并同时存储 operation ID、target、phase、safe error code 和 timestamp。
绝不存储 OIDC codes、Token values、credentials、raw command output、SQL 或 private deployment paths。
Responses 使用 field-whitelisted structured errors，不暴露 internal stack traces。

## Token 与 Agent API 的关系

Token API 创建 `POST /agent/v1/control` 使用的 opaque Bearer credentials；完整的 machine-Token、scope、one-time plaintext 和 revocation rules 见[token-api.md](token-api.md)。
Agent API 保留 protocol 4 及其 operation/generation/journal semantics；authentication 和 compatibility contract 见[agent-api.md](agent-api.md)。
Management API 不得包装 arbitrary protocol-4 request，也不得将 Agent Token 作为 browser session 接受。

## Wire 行为

`management-api.yaml` 是 same-origin `/api/v1` browser API 的 wire contract。
Backend 和 frontend 消费相同的 request、response 和 error shapes。
Unknown fields、unsupported enum values、extra properties、stale target versions 以及不在 enumerated allowlist 中的 actions 都必须拒绝。

List endpoints 使用 opaque keyset cursors。
Default limit 为 25，maximum 为 100。
Timestamps 是 UTC RFC 3339 values。
Byte counts 和 generation values 使用 decimal strings，避免 TypeScript clients 丢失 precision。
Missing、stale、failed 或 unverified observation 返回其 state 和 null value，绝不转换为 zero、false 或 healthy。

Every resource 暴露 opaque `resource_version`。
Write requests 将该 version 作为 `target_version` 携带；VM actions 还携带 exact `expected_generation`。
Version 是 concurrency guard，不是 user-editable label。
当 version、generation、lease、native identity 或 image references 变化时，API 返回 `stale_target`。
Client 必须 reload resource 并 request new preview；不得静默 retarget 或 replay dangerous action。

Image import 以 durable operation 开始。
Local upload 在 upload 前声明 exact byte counts 和 SHA-256 values；service 在 staging 前及 staging 期间应用 configured size ceiling 和 physical-space reserve。
Trusted-source import 先解析 exact fingerprint 和 image size，然后在 native copy 前针对 configured Incus pool 检查同一 reserve。
两条路径都必须在 image 可被 selected 前运行 integrity 和 suitability checks。

API 使用 stable machine error codes、phases 和小范围 safe structured fields allowlist。
Frontend 将 codes 映射为 English 和 Simplified Chinese copy。
Unknown codes 使用 generic localized failure message。
Responses 绝不包含 raw command output、credentials、private paths、database content 或 internal stack traces。
Admission drained 时，System page 显示 recovery 由 release 管理，不提供 browser `drain_exit` control；只有 active release transaction 可以 reopen admission。

## 产品与语言行为

首个 release 只有两个 interface locales：`zh-CN` 和 `en`。
Selection priority 依次是 saved user choice、browser language、English。
每个 navigation label、field label、accessible name、confirmation、validation message、status explanation 和 error 都有两种翻译。
Technical identifiers、fingerprints、error codes 和 user-authored content 不翻译。

First viewport 打开 Overview。
Navigation 包含 Overview、VMs、Images、Operations & Audit 和 System。
VM page 同时保持 selected resource identity、status evidence 和 allowed action context 可见。
不提供 manual VM creation flow。
Overview、VM、image、operation 和 system data 保留 source freshness 与 unknown state。
Design direction 和 responsive behavior 见[DESIGN.md](../../../../DESIGN.md)及[Web surface brief](../../../../.impeccable/surfaces/web.md)。

## Image 与 VM 不变量

Unified local uploads 使用一个 `application/octet-stream` payload，携带一个 expected size 和 SHA-256。
Split local uploads 使用 `multipart/form-data`，且恰好包含一个 `metadata` part 和一个 `rootfs` part；import request 为每个 part 声明自己的 expected size 和 SHA-256，upload ceiling 作用于两个 payload sizes 之和。
Original filenames 不作为 Incus aliases 或 filesystem paths。

Image 由 immutable Incus fingerprint 标识。
Version labels 和 configured source IDs 是 metadata，不是 identity。
Trusted-remote import 只接受 configured `source_id` 和 expected fingerprint，不接受 arbitrary URL、alias 或 credential。
Local upload 在 stream 开始前声明 package format、exact byte count 和 SHA-256。
Upload 必须符合 configured maximum 和 staging reserve，只接收一次且不可 resumable。
Short、oversized、mismatched 或 interrupted stream 会成为 queryable failed operation，其 incomplete staging data 会清理。

Integrity 和 Agent VM suitability 是独立 results。
Verification 使用有界、隔离的 temporary system container，不带 production credentials、host mounts 或 production network access。
只有 integrity 和 suitability 都 passed 的 image 才能作为 new default 被 selected。
Selection changes 只影响 new sessions。
Existing tasks 和 caches 继续 pin 到 stored fingerprint。
Retiring image 会阻止 new selection，但不使 existing bindings 失效。
Image 为 default，或存在 active、cached、releasing、pending-creation 或 verification reference 时，deletion 被拒绝；reference checks 与 image operations 串行化。

## 状态迁移与包回滚

Management state 是 additive。
`state_capabilities` registry 包含 versioned markers，API 将每个 marker 渲染为 `name@version`。
Initial capability set 保留既有 protocol-4 task、generation、lease、reservation、port 和 journal markers，并新增 management catalog、session-image binding、operation-audit、principal-identity 和 Token-revocation markers。
Migration 保留 task IDs、session IDs、image fingerprints、generations、leases、ports、reservations、operation journals 和 existing audit evidence。
除非 configured administrator identity 有直接证据，legacy operation/audit actor strings 保留为 `legacy_unknown` values；service 绝不猜测或伪造 OIDC actor。
不创建 user-to-session 或 user-to-Token ownership mappings。

Pre-rqlite phase 中 SQLite 继续作为 single writer。
[ADR 0044](../../../adr/0044-agent-vm-state-migration-to-rqlite.md)描述的 conservative migration 进入 state-write fence，拒绝新的 browser/API/Agent durable mutations，暂停 scheduler/reconciliation/timer durable writes，并要求每个 in-flight native worker 在 original operation ID 下以 durable result 完成，同时验证 process-group、PID-identity 和 Incus-operation quiescence。
Unresolved pending/unknown result、active worker 或 unverified quiescence 会阻止 final export；不能只记录 outcome 然后继续。
只有所有 transactions 和 workers converge 后，service 才关闭 SQLite writers 并导出 consistent backup。
它验证 schema/capability/row-count 和 identity summaries，通过一个 rqlite transaction import，并在激活 shared backend 前完成 validation。
绝不 dual-write、从两个 slots 打开 shared SQLite file、downgrade schema 或 recreate state 来强制 rollback。
Cutover 后 rqlite 是唯一 live authority；critical reads 和 observation epoch boundaries 使用 linearizable consistency。
如果 rqlite unavailable，service fence durable mutations 和 native admission，并在不猜测的情况下保留 active leases。
Verified rqlite backup/snapshot 是第一恢复来源，必须记录不早于 last trusted commit 的 coverage boundary。
Recovery 必须使用 trusted append-only journal 和 native-effect evidence reconcile 该 boundary 之后的 backup tail；仅 schema、digest、epoch 或 journal checks 不能证明 coverage。
只有证据证明没有 post-cutover durable write、epoch advance、lease change 或 native effect 时，pre-cutover SQLite snapshot 才可 re-import。
如果无法证明 backup tail 或任何 post-cutover write，recovery 保持 fenced，等待 administrator-authorized journal/native reconciliation；不得打开 stale SQLite 作为 competing authority，也不得通过猜测重建 state。

Package rollback 期间绝不 downgrade 或 recreate database。
Installed package 只有声明支持 read/write 每个 present capability marker 时才能 activation。
无法保留 administrator issuer/subject actor、没有 administrator authority 的 typed rejected-request actors、explicit unknown historical actor values、Token revocation、journal、generation 和 unknown-outcome semantics 的 target，必须在改变 active slot 前拒绝。
Current package 保持 selected，admission 保持 drained，等待 operator recovery。

## 兼容窗口

Blue-green release contract 至少保留 current 和 previous verified packages 及其 fixed static resource manifests 一小时。
每个 package 声明 protocol、request/response/semantic windows、Token 和 management API versions，以及 state capability read/write sets。
Convergence 期间，只有 operation 位于 shared compatibility set 时，两个 slots 才能接收 requests；Caddy 绝不 retry writes。
超过窗口的 browser asset 收到明确的 refresh response。
SSH `legacy-bun`/`go-request` 在声明窗口结束前继续作为 compatibility transport；direct HTTPS 使用相同的 protocol-4 body，不改变 lifecycle semantics。

## 共享合同示例

本合同旁的 JSON files 是 synthetic fixtures，不包含 deployment facts，供 backend 和 frontend contract tests 共享：

- `overview.json` 覆盖 separate freshness 和 unverified pressure observation。
- `vms-page.json` 覆盖 pinned image、generation、effective lease 和 bounded port data。
- `images-page.json` 覆盖 verified candidate 和 preserved legacy compatibility default。
- `image-import-request.json` 覆盖 exact-size local upload declaration。
- `image-import-split-request.json` 覆盖 separate metadata 和 rootfs payload declarations。
- `vm-action-request.json` 覆盖 preview-bound restart request。
- `operation.json` 覆盖 durable asynchronous progress 和 current issuer/subject actor。
- `audit-page.json` 覆盖 current、typed rejected-request 或 explicitly unknown historical actor，以及 optional reason、target 和 outcome。
- `system.json` 覆盖 unified service、Caddy、rqlite 和 Incus readiness。
- `error.json` 覆盖不带 internal diagnostics 的 stale-target conflict。
