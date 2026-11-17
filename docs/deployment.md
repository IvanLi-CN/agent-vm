# 部署指南

Agent VM 在已具备 Incus 运行条件的 Linux 控制环境中运行。Caddy 提供入口，两个控制服务槽位共享 rqlite 业务状态，槽位外 Deployd 协调部署与唯一控制器角色，并向外部 Prometheus 提供一个指标出口；Grafana 只查询 Prometheus。

## 前置条件与输入

部署方提供 Incus 项目、网络隔离、存储池、已验证容器镜像、域名、OIDC 注册、TLS 证书和受保护配置。安装不接管 PVE、CT 权限、挂载或外部观测服务。组件必须使用[固定清单](../deploy/assets/blue-green/deployment-components.json)及其校验和。

控制环境本身是容器时，先完成[嵌套 Incus 权限准备与排查](../deploy/docs/nested-incus.md)：核对外层有效 UID/GID 范围、内层 subordinate ID 授权与显式权限模式，并实际验证声明支持的非特权 guest 和 Docker。安装预检通过、Incus 资源存在或特权 guest 成功均不能替代这项验收。维护时按[维护 drain 与恢复](../deploy/docs/maintenance.md)保存同一 owner 会话，避免普通会话进入后无法退出暂停。

## 操作入口

从[首次安装步骤](../deploy/docs/install.md)准备受保护输入，运行 `agent-vm-deploy install --config <输入> --bundle <产品包> --materials <资料目录>` 进行只读预检，确认无阻塞后以相同输入加 `--apply` 执行安装。已有安装的线上升级由目标服务器上的 `agent-vm-deploy update --version <semver> --source-revision <sha>` 完成；该命令在服务器本地选择架构、下载并校验正式 release，再把候选交给 deployd 执行 blue-green 事务。Agent 不下载或上传产品 bundle。部署资料布局与配置示例见 [deploy/README.md](../deploy/README.md)。安装路径、双槽位角色、旧状态迁移、升级、有限回滚及恢复细节见[蓝绿部署手册](../deploy/docs/blue-green.md)。全新安装和旧环境迁移必须区别处理，普通蓝绿升级不能替换业务数据库。

## 固定安装布局

当前服务端包仅提供 `/opt/agent-vm-native/bin/agent-vm-server` 与 `/opt/agent-vm-native/bin/agent-vm-deploy`；后者以 `serve` 子命令运行槽位外部署服务。既有 `agent-vm-deployd.service`、配置和 socket 名称不变，新包没有旧名称别名。历史完整包保留读取能力，槽位按已验证 manifest 选择自己的可执行路径。槽位外的 server request adapter 与槽位业务服务使用各自的 manifest，不可只替换其中一个路径。

## 验收

检查服务入口、唯一 controller lease、两个槽位观测、新鲜度及 Prometheus 单一 target；还必须在隔离环境验证 VM 创建、执行、续租、释放与销毁。安装、重启和切换记录应能查询原部署事务。缺失数据或结果未知不能视为验收通过，不得通过中断正在使用的生产 VM 来替代隔离验收。

## 命令名称迁移

`agent-vm-control` 的命令改用 `agent-vm-server`；独立 `agent-vm-deployd --config <文件>` 改用 `agent-vm-deploy serve --config <文件>`。本次按维护者明确指定发布 Minor，此命令路径硬切换是兼容性例外，已有脚本及客户端配置中的显式 `remote_request` 必须同步调整。

旧部署管理程序的固定 artifact 校验不会自动认识新布局。因此，首次采用新布局时，运维者须在显式宿主维护中，从同一已验证 Release 更新槽位外部署二进制、固定 request adapter、对应 manifest、unit 和 tmpfiles（创建两个槽位的持久 drop-in 目录），再使用蓝绿更新命令；不能把新包直接交给旧 reader 并期待它自行升级。该步骤使用同一正式版本，不要求额外过渡发布。升级前保留原程序与配置副本；旧管理程序尚在运行或恢复路径尚未准备好时，不删除其文件。生产环境操作必须另行授权。
