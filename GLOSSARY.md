# Agent VM 领域术语

项目领域术语使用中文说明，保留准确的英文名称。

## 产品交付

**Agent VM 产品版本（Product version）**：客户端与控制环境共同使用的 SemVer。它表达公开接口的兼容性承诺，独立于协议版本、持久状态格式版本和 guest 镜像身份。

**Agent VM 产品发布（Product release）**：为一个产品版本交付 GitHub Release 及其全部版本化安装包，且完成产物校验。它的完成条件不包含任何现网安装或升级。

**Agent VM 部署（Deployment）**：将已发布产物安装或升级到明确选定的控制环境及客户端的独立操作，需要明确触发。

_Avoid_：用“发布成功”同时指代 GitHub 产物发布与现网部署；用源码 SHA、协议版本或数据库 schema 版本代替产品版本。

## 资源与初始化

**Incus project**：Agent VM 所在的 Incus 隔离与策略边界。它不是 Git 项目，也不等同于 Agent VM 控制服务的持久状态或资源 admission。

**项目级聚合配额（Project aggregate quota）**：由 Incus project 对多个实例合计施加的 CPU、内存、进程或实例数量限制。Agent VM 部署基线不使用它作为控制服务 admission 上限。

**实例级资源限制（Instance resource limit）**：由 Incus profile 或实例配置对单个 Agent VM 容器施加的限制，例如 `limits.processes=512`。

**部署初始化基线（Deployment initialization baseline）**：在受控部署维护期间，对 Incus project/profile 必须检查或设置的最小状态集合；它不包含地址、凭据、库存、状态数据库或实时部署证据。
