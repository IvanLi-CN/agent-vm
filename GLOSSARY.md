# Agent VM 领域术语

项目领域术语使用中文说明，保留准确的英文名称。

## 产品交付

**Agent VM 产品版本（Product version）**：客户端与控制环境共同使用的 SemVer。它表达公开接口的兼容性承诺，独立于协议版本、持久状态格式版本和 guest 镜像身份。

**Agent VM 产品发布（Product release）**：为一个产品版本交付 GitHub Release 及其全部版本化安装包，且完成产物校验。它的完成条件不包含任何现网安装或升级。

**Agent VM 部署（Deployment）**：将已发布产物安装或升级到明确选定的控制环境及客户端的独立操作，需要明确触发。

_Avoid_：用“发布成功”同时指代 GitHub 产物发布与现网部署；用源码 SHA、协议版本或数据库 schema 版本代替产品版本。

## 受管资源与管理台

**Agent VM 管理台（Management console）**：维护者对 Agent VM 受管资源进行全局观察和授权管理的操作面。

**管理 API（Management API）**：内置管理台使用的同源 `/api/v1` 浏览器合同。它独立于 CLI protocol 4、内部 control API 和部署侧观测接口。

**受管 VM（Managed VM）**：由 Agent VM 根据会话归属、资源和租约规则管理的工作环境。管理台中的 VM 指已有 Agent 会话的工作环境。

**受管镜像（Managed image）**：纳入 Agent VM 管理、具有明确来源和不可变内容身份的基础环境。新导入镜像必须分别通过完整性与适用性校验后才能被选择为新会话默认镜像；迁移保留的旧配置默认镜像是显式标记的兼容例外。

**会话镜像固定（Session image pinning）**：一个会话的 VM 及其缓存持续绑定同一镜像内容身份的规则。切换默认镜像只影响新会话，旧会话换镜像需要明确销毁并重建。

**VM 缓存（VM cache）**：VM 停止后保留、可供同一会话恢复的工作环境及其数据。它可以被回收，不构成持久数据保证。

**资源版本（Resource version）**：管理 API 为目标资源返回的不透明并发令牌。写请求提交该令牌以检测目标变化；它不替代会话身份、generation 或产品版本。

**管理操作（Management operation）**：具有 UUID、操作者、原因、目标、阶段和结果的持久管理动作记录。浏览器断开或 native 结果不确定时，仍可按 operation ID 查询；它不替代现有会话生命周期 journal。

_Avoid_：把受管镜像、VM 缓存与 Agent VM 产品发布包统称为“镜像”；把“释放租约”理解为保留访问权的普通关机。
