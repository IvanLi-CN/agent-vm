---
status: accepted
---

# Agent VM 容量与清单观测

## 背景

一个 dashboard 无法同时清晰表达聚合准入、CT 运行压力和每个 task 的生命周期状态。
聚合指标回答下一个请求是否满足逻辑和物理门槛；task 状态回答哪些 managed-container 记录消耗了这些预留，以及故障或状态转换是否阻塞进展。
把它们混在一起只会产生看似已满、却无法解释组成的图表。

Incus 不会为每个部署提供权威的 managed-container slot limit。
Dashboard 不得从未设置或为 `-1` 的 project quota 臆造 slot limit。
它可以针对配置的默认容器请求提供有界快照估算，但必须标记为派生值，不能展示为 native slot limit。

## 决策

部署提供两个由同一个 `agent-vm-deployd /metrics` endpoint 支持的 dashboard：

1. `Agent VM / Capacity & Admission` 使用 `admin-metrics` 提供聚合资源、CT-local pressure、准入门槛、故障和新鲜度。
2. `Agent VM / Inventory & Operations` 使用 `admin-status` 提供有界的 task 清单、生命周期状态、lease/mapping 状态和最近一次结构化错误信息。

容量 dashboard 分别展示 CPU、memory 和 disk 的 `allocated`、`reserved`、`allocated_and_reserved`、逻辑 `remaining` 以及有权威来源时的逻辑 `limit`。
它还分别展示 CT-visible capacity、当前运行时观测、pressure、OOM 和 startup blocking。
CPU 视图包含 `some.avg10`、`full.avg10`、配置的 startup threshold 和 `startup_blocked` 状态；pressure 不折算进逻辑 remaining。

容量 dashboard 展示已观测的 task-state 数量和两个派生的默认容器估算值：

- `logical default-container slots` 是已验证的逻辑 CPU、逻辑 memory 和物理 disk 余量分别除以默认 request/cost 后取最小值并向下取整。
- `current startable default-container slots` 在同一快照上应用当前 CT-visible CPU capacity 和 startup-pressure gate。

两个值都是快照估算。
所需输入未验证时必须省略，不能称为 `safe max`、`available VM limit` 或 Incus slot limit。
默认 request 和物理准入成本从 deployd 的只读观测输入读取，不得硬编码在公共 dashboard 中。

清单 dashboard 包含 active、preparing、closing、deleting、failed、cached 和 absent 的 managed record，并提供筛选器；默认顺序为 active、preparing、failed、closing/deleting、cached、absent。
Cached 行默认折叠。
表格字段有界地包含 task ID、instance name、session ID、state、native status、guest availability、请求的 CPU/memory/disk、effective-lease state、lease expiry、SSH mapping 以及最近一次结构化 error code/phase。
它绝不暴露原始错误文本、凭据或部署文件系统路径。

公共仓库拥有 metric 名称、来源 schema、通用 dashboard 模板和 validator。
运维者拥有的 `obs` 仓库拥有渲染后的 JSON、Prometheus scrape 配置、datasource、controller job、provisioning path 和线上 rollout。
槽位外的 `agent-vm-deployd` 是部署控制服务，也是 Prometheus 的唯一 scrape target；不得再为 dashboard 引入独立的 HTTP broker、Pushgateway 或 textfile relay。

这里的“one-shot controller”指 Agent VM 业务观测/操作的容量和清单来源，不是[ADR 0045](0045-agent-vm-deployment-controller.md)定义的槽位外部署事务控制器。

## 后果

运维者可以区分逻辑 reservation 余量、物理 disk 余量、当前运行压力和 task-state 组成，而不会把其中任何一个误认为另一个。
即使逻辑 CPU 仍有余量，current-startable 估算也可能在 CPU startup-pressure 事件期间降为零；这只是 startup gate 的明确快照，不是逻辑策略发生变化。

两个 dashboard 要求 deployd 调用两个只读 action，并在每次成功 observation 时删除过期的 per-task series。
action 失败时必须暴露 `up=0` 和 freshness failure，不能伪造为零。
拆分会增加少量 provisioning 工作，但能让运维问题彼此分离且可测试。
