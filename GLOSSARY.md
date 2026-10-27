# Agent VM 领域术语

项目领域术语使用中文说明，保留准确的英文名称。

## 产品交付

**Agent VM 产品版本（Product version）**：客户端与控制环境共同使用的 SemVer。它表达公开接口的兼容性承诺，独立于协议版本、持久状态格式版本和 guest 镜像身份。

**Agent VM 产品发布（Product release）**：为一个产品版本交付 GitHub Release 及其全部版本化安装包，且完成产物校验。它的完成条件不包含任何现网安装或升级。

**Agent VM 部署（Deployment）**：将已发布产物安装或升级到明确选定的控制环境及客户端的独立操作，需要明确触发。

_Avoid_：用“发布成功”同时指代 GitHub 产物发布与现网部署；用源码 SHA、协议版本或数据库 schema 版本代替产品版本。
