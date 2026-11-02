---
status: accepted
---

# Agent VM Caddy 采用 Caddy-owned Tencent DNSPod DNS-01

## Context

Agent VM 的蓝绿服务需要一个稳定的 HTTPS 入口。现有资源把 Caddy 配置为加载部署方生成的静态叶证书，这会把证书生命周期放到 Caddy 之外，也不能表达目标部署要求的 Tencent DNSPod DNS-01。Caddy 仍必须保持唯一入口，只连接受保护的 active Unix socket，不承担登录、授权或蓝绿槽位选择以外的业务职责。

Caddy 的 DNS challenge provider 不是 stock binary 的默认模块。官方 `caddy-dns/dnspod` 模块通过 `dns.providers.dnspod` 接入 Caddy，Caddyfile 使用 `tls force_automate { dns dnspod {env.DNSPOD_TOKEN} }`。仓库需要保留 provider 构建集成，同时避免把某个 Caddy patch release 变成项目的长期版本边界。

## Decision

部署使用由 `xcaddy` 构建的自定义 Caddy v2 binary，并编译官方 `github.com/caddy-dns/dnspod` provider。仓库只约束 Caddy major version 2；构建 helper 要求操作者显式提供一个 v2 release，拒绝 `latest`、分支和其他隐式版本解析。DNSPod provider source 使用仓库 metadata 中的官方 commit ref，以便自定义 binary 的模块输入可复核。

Caddyfile 由 Caddy 自己通过 DNS-01 申请、续期并持久化证书和私钥，同时关闭自动 HTTP listener。域名和 `DNSPOD_TOKEN` 只通过 root-owned mode-0600 的 deployment environment file 提供；Caddy 以非 root 用户运行，并将 ACME storage 写入专用持久目录。Caddy 启动不打印环境变量。仓库不保存真实域名、DNSPod 凭据、证书私钥、部署地址或 inventory。

部署不引入独立 ACME client、静态 Caddy leaf certificate、EdgeOne、Forward Auth、身份注入头或公开 blue/green slot 域名。Caddy 继续只做 TLS、固定路径转发、脱敏 access log 和 active Unix socket 路由；rqlite 的本地 CA/leaf 仍由独立的 rqlite TLS 边界使用。

## Consequences

- 证书申请和续期的 owner 是 Caddy，部署不需要额外的证书服务或证书同步任务。
- Caddy 的 data directory 成为必须持久化、由 `caddy` 用户可写的部署路径；丢失它会丢失 Caddy 的 ACME state，需要重新建立证书管理状态。
- Caddy patch release 的选择属于每次构建的显式部署输入，不是项目 metadata 的固定值；构建产物必须由 operator 记录并验证 checksum。
- 静态部署 CA 只为本地 rqlite HTTPS 保留，不能被解释为 Caddy 已经取得生产证书，也不能作为 CT109 部署成功证据。

## Rejected alternatives

- **继续加载静态 Caddy leaf**：证书 lifecycle 不由 Caddy 管理，且不能满足 Caddy-owned DNS-01。
- **每次部署自动解析最新 Caddy**：会把未审查的 patch movement 混入部署策略；显式 v2 release input 保留 operator 的版本选择和验证责任。
- **使用独立 lego/certbot/helper**：增加第二个 ACME authority 和证书交接边界，违反 Caddy-owned certificate contract。
