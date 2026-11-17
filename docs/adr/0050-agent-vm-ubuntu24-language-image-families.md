---
status: superseded
superseded_by: 0051-agent-vm-image-runtime-variants.md
---

# Agent VM Ubuntu 24.04 语言镜像族与可重复构建

## 背景

Agent VM 目前可以登记和验证 Incus 镜像，但生产环境曾直接使用裸 `ubuntu/24.04`。该镜像没有 OpenSSH，导致来宾已运行而 `$agent-vm` 无法取得 ed25519 host key。项目需要可重复、代码优先的 guest 镜像交付路径，并让新建 VM 开箱即用。

## 决策

- 以 Ubuntu 24.04 为唯一基础，当前只构建 `amd64`。
- 构建三套镜像族成员：Rust、Java、Go；共用基础系统工具层。
- 基础层包含 OpenSSH server、Bun、Node.js、Git、rsync、Python、Docker CLI、Compose plugin、PostgreSQL/MySQL 客户端、构建和磁盘诊断工具。浏览器自动化依赖由项目按需安装。
- Docker daemon 在 guest 内运行，镜像使用明确的 privileged nested Incus profile；不依赖宿主 Docker socket，不要求 KVM。
- Java 使用 Azul Zulu LTS；Rust 使用最新 stable（Rust 没有官方 LTS）；Node.js、Go、Bun 和数据库客户端遵循最新受支持 LTS/stable 线。
- 使用版本锁文件固定基础 fingerprint、软件版本、下载 URL、架构和 SHA256。版本更新先生成 lock 变更，再经审查和显式 release。
- 使用 Incus 原生临时容器构建器，不引入 Packer。构建器执行 provisioning、guest readiness 验证并导出 unified image tar、manifest、SBOM 和 SHA256SUMS。
- 构建器先发布公共层为本地 Incus image，再从该 image 派生 Rust、Java、Go profile；Node.js、Go、Bun 和 rustup-init 使用官方二进制及 SHA256 锁定。
- GitHub Release 发布镜像资产；105 和 109 在目标服务器下载、校验并导入受管镜像 catalog。镜像默认选择只影响新 session，已有 VM/cache 继续绑定原 fingerprint。
- 旧裸 Ubuntu fingerprint 保留为 `retired/compatibility-only`，禁止新 session 选择。

## 取舍

privileged nested profile 最大化 Docker/Compose、overlay2、网络和设备映射兼容性，但扩大 guest 对宿主内核边界的访问，因此它必须单独命名、单独验收和审计。只支持 amd64 降低了首期构建与验证成本，但暂不覆盖 ARM 宿主。Incus 原生构建路径与生产运行时一致，代价是构建需要 Linux/Incus 环境；该成本通过 Agent VM 和 GitHub Actions 承担。

## 失败边界

镜像若缺少 SSH host key、SSH 服务、Docker daemon、数据库客户端或任一必需工具，构建和导入均失败，不能仅因为 Incus instance 能启动就标记为可用。运行时 `acquire` 必须使用通过适用性验证的 fingerprint。
