---
status: accepted
---

# Agent VM 镜像同时支持特权与非特权运行变体

## 背景

用户要求构建脚本同时支持特权与非特权镜像变体。本决策取代 [语言镜像族决策](0050-agent-vm-ubuntu24-language-image-families.md)。保留 Ubuntu 24.04、amd64、Rust stable、Azul Zulu LTS、Go、Bun、Node.js、OpenSSH、Docker/Compose、PG/MySQL 客户端和系统工具要求；不预装浏览器自动化依赖。

## 决策

Rust、Java、Go 每族提供 privileged 与 unprivileged 两种变体，默认构建六个产物。Java 变体包含锁定版本的 Azul Zulu JDK 与 Apache Maven。每种模式有独立公共层缓存、名称和 manifest；构建容器显式设置 security.privileged 与 security.nesting，镜像内保存运行配置记录，创建端必须使用对应参数。

非特权构建需要宿主提供嵌套 UID/GID 映射能力。任一模式启动或 readiness 失败应停止并报告，不得自动转换为另一模式，不得修改外层宿主权限。每种模式都要验证 SSH 与 guest 内 Docker；脚本测试通过不代表真实镜像验收通过。

## 影响

双模式增加构建与验收成本，模式隔离缓存避免错误复用。管理端验收仅读取 lifecycle profile 中与运行模式相关的白名单配置，创建实例仍采用配置的 lifecycle profile；不会因验收而继承生产设备、网络或凭据。每次部署选择一种经过验证的运行模式，不新增镜像管理 API。版本锁记录基础 fingerprint、官方二进制 URL/SHA256 和签名 APT 元数据中的精确包版本/SHA256；下载缺失或摘要变化时失败。Incus 原生构建复用每种模式的公共层，导出 unified tar、manifest、SBOM、readiness 与校验清单，通过 GitHub Release 独立发布；目标服务器下载验证后再通过受管 catalog 导入，已有会话保持原 fingerprint。
