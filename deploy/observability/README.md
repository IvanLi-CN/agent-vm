# Agent VM 观测资源包

本目录包含 Agent VM 观测合同对应的可移植 Grafana 源文件。它是带参数的公共基线，不是某个部署的导出文件。

## 渲染

复制[示例配置](observability.config.json.example)，替换部署方负责的所有占位符，然后把资源包渲染到输出目录：

```bash
bun deploy/scripts/render_observability_dashboard.ts \
  --config /path/to/agent-vm-observability.json \
  --output-dir /path/to/deployment/grafana/generated \
  --force
```

渲染器会生成容量看板、清单看板、Deployd 看板和 Grafana file-provider 配置。看板 JSON 写入 `generated/current`，该路径通过原子符号链接整体替换；旧版本保留在 `generated/.releases` 下。部署实现必须把 `dashboard_path` 设置为 Grafana 容器内可见的 `current` 目录，并通过一次临时文件加 rename 原子替换 provisioning YAML。这样 Grafana 不会读到只发布了一半的看板资源包。

部署仓库应当把渲染后的文件与 Prometheus scrape 配置一并审查和提交。Prometheus 的 target 必须指向槽位外 `agent-vm-deployd` 服务的内部 `GET /metrics` endpoint；不再部署独立的 controller bridge、Pushgateway 或 textfile relay 作为最终出口。公共仓库不接收这些渲染文件、datasource 凭据、主机名、metrics 监听地址或实时清单。

如果 provisioning 激活失败且无法恢复旧的 `current` 链接，渲染器会移除稳定链接，并保留 staged release 供恢复，而不是继续提供混合版本的看板和 provisioning 配置。

## 配置字段

| 字段 | 含义 |
| --- | --- |
| `dashboard_uid` | 本部署稳定的 Grafana dashboard UID。 |
| `dashboard_title` | Grafana 中显示的 dashboard 标题。 |
| `grafana_org_id` | 拥有该 dashboard 的 Grafana organization ID。 |
| `provider_name` | Grafana provisioning provider 名称。 |
| `dashboard_folder` | dashboard 所在的 Grafana folder。 |
| `prometheus_datasource_uid` | 已存在的 Prometheus datasource UID。 |
| `controller_job` | Prometheus 的 `job` label；默认应为 `agent-vm-deployd`，指向 Deployd 的 `/metrics` target。 |
| `dashboard_path` | Grafana 扫描 JSON dashboard 的容器内 `current` 目录；必须指向稳定的活动资源包路径，不能指向 release 父目录。 |
| `dashboard_filename` | 可选的渲染文件名，默认是 `agent-vm-observability.json`。 |
| `inventory_dashboard_uid` | 可选的清单 dashboard UID，默认是 `<dashboard_uid>-inventory`。 |
| `inventory_dashboard_title` | 可选的清单 dashboard 标题，默认是 `<dashboard_title> / 清单与操作`。 |
| `inventory_dashboard_filename` | 可选的清单文件名，默认是 `agent-vm-observability-inventory.json`。 |

模板使用隐藏的 `controller_job` 变量，使 controller 查询保持可移植；部署方提供实际的 job label。业务查询在聚合前通过 `agent_vm_deployd_slot_selected == 1`，按 `job, instance, slot` 选择活动槽位，避免两个槽位同时存活时重复统计容量和任务；部署控制看板仍可分别展示两个槽位。清单新鲜度使用 `agent_vm_inventory_scrape_timestamp_seconds`，失败操作识别 `error` 和历史 `failure` 标签。活动槽位或有效观测缺失时保留无数据状态，不能用零值伪装健康。CPU capacity 从 `agent_vm_admission_cpu_capacity_vcpu` 读取，memory 和 disk limit 只从权威 metric family 渲染。

容量看板把逻辑 admission 与运行时压力分开：摘要显示逻辑 RAM 余量、CT-local `MemAvailable`、pressure/OOM 状态以及紧凑的 CPU/disk readiness；policy snapshot 显示默认请求、slot estimate 和 task-state 计数。清单看板通过 state filter 展示所有生命周期状态，默认显示 active、transitional 和 failed 行，并把 cached/absent 历史折叠到筛选结果中。清单字段使用 `task_id|state` snapshot key join，因此生命周期切换不会把不同状态的资源数据拼接在一起。

## 验证

渲染器在写入前会验证三个 dashboard。已经渲染的资源包也可以直接检查：

```bash
bun deploy/scripts/validate_observability_dashboard.ts \
  --dashboard /path/to/deployment/grafana/generated/current/agent-vm-observability.json \
  --provisioning /path/to/deployment/grafana/generated/agent-vm-observability.yml
```

验证 `agent-vm-observability-inventory.json` 时使用 inventory profile：

```bash
bun deploy/scripts/validate_observability_dashboard.ts \
  --dashboard /path/to/deployment/grafana/generated/current/agent-vm-observability-inventory.json \
  --provisioning /path/to/deployment/grafana/generated/agent-vm-observability.yml \
  --profile inventory
```

验证器会检查所选 dashboard profile 的 panel/metric 合同、自动刷新、datasource 一致性、controller 变量，以及 packet-rate PromQL 不能乘以 `8` 的规则。

容量与业务清单查询必须先筛选 `agent_vm_deployd_slot_selected == 1`，再以 `sum without (slot)` 合并活动槽位序列。这样时间范围跨越蓝绿切换时，同一业务指标仍只有一条序列和一个图例；不能先汇总两个槽位，也不能补零掩盖缺失观测。Deployd 角色看板保留 `slot` 标签，用于直接比较双槽位。

可使用 Prometheus 自带的 `promtool` 执行[蓝绿查询回归](selected-slot.test.yml)：

```bash
promtool test rules deploy/observability/selected-slot.test.yml
```

该测试同时提供双槽位样本，验证切换前后业务值始终为一份且标签稳定；没有可确认的活动槽位时必须返回空结果，不能伪造零值。

Deployd 看板固定写入 `agent-vm-deployd.json`，UID 为 `<dashboard_uid>-deployd`，展示抓取状态、双槽位角色、快照新鲜度、观测来源、事务阶段和恢复阻塞。该文件随容量与清单看板一并原子发布，角色查询保留两个槽位。

验证 Deployd 看板时，使用相同验证命令并指定 `--dashboard <current目录>/agent-vm-deployd.json --profile deployd`。
