# 部署指南

Agent VM 在已具备 Incus 运行条件的 Linux 控制环境中运行。Caddy 提供入口，两个控制服务槽位共享 rqlite 业务状态，槽位外 Deployd 协调部署与唯一控制器角色，并向外部 Prometheus 提供一个指标出口；Grafana 只查询 Prometheus。

## 前置条件与输入

部署方提供 Incus 项目、网络隔离、存储池、已验证容器镜像、域名、OIDC 注册、TLS 证书和受保护配置。安装不接管 PVE、CT 权限、挂载或外部观测服务。组件必须使用[固定清单](../deploy/assets/blue-green/deployment-components.json)及其校验和。

## 操作入口

从[首次安装步骤](../deploy/docs/install.md)准备受保护输入，运行 `agent-vm-deploy install --config <输入> --bundle <产品包> --materials <资料目录>` 进行只读预检，确认无阻塞后以相同输入加 `--apply` 执行安装。部署资料布局与配置示例见 [deploy/README.md](../deploy/README.md)。安装路径、双槽位角色、旧状态迁移、升级、有限回滚及恢复细节见[蓝绿部署手册](../deploy/docs/blue-green.md)。全新安装和旧环境迁移必须区别处理，普通蓝绿升级不能替换业务数据库。

## 验收

检查服务入口、唯一 controller lease、两个槽位观测、新鲜度及 Prometheus 单一 target；还必须在隔离环境验证 VM 创建、执行、续租、释放与销毁。安装、重启和切换记录应能查询原部署事务。缺失数据或结果未知不能视为验收通过，不得通过中断正在使用的生产 VM 来替代隔离验收。
