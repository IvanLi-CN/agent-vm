--- title: Agent VM 容量与故障看板 module: agent-vm problem_type: observability component: grafana-prometheus tags:
  - grafana
  - prometheus
  - incus
  - capacity
  - admission
status: active related_specs:
  - docs/specs/agent-vm/SPEC.md
  - docs/specs/agent-vm/contracts/observability.md
---

# 背景

`incus-agent-vm` 看板必须解释：为什么 Incus data pool 看起来还有空闲空间，创建容器却仍然可能失败。看板需要把 CT root filesystem、临时 image unpack staging、physical pool usage、guest rootfs allocation、native admission 和 cleanup failure 作为彼此独立的事实展示。

# 症状

- 看板能够报告 guest filesystem quota，却不能展示 `/var/lib/incus/images` 使用的 filesystem。
- `incus_project_limit` 在 CPU、memory、disk 或 instance count 上为 `-1`，所以 project metrics 没有提供 native admission ceiling。
- image creation 报告 `Unable to unpack image, run out of disk space`，但最终 Incus pool 仍报告有 available space。
- 看板已经完成 provisioning，但 runtime copy 无法安全保存，因为 provisioning 会覆盖它。
- auto-refresh 默认关闭，packet-per-second panel 把 count rate 错误地乘以了 `8`。

# 根因

Incus pool metrics 与 guest filesystem metrics 描述的是不同层次。 Image unpacking 会在最终分配 instance storage 之前使用 CT-local staging path，而 native controller 在另一个控制边界维护 admission reservation 和状态。只使用 Incus project 或 guest metrics 的看板无法解释这两个 failure domain，也无法解释 native limit。

本仓库拥有 native controller、公开合同以及 `deploy/observability/` 下的参数化 Grafana 基线。直接修改 Grafana database copy 仍然只是临时操作，公共模板与 renderer 才是可以由各部署实例化的仓库实现。

原看板还没有清晰说明 CPU startup admission：logical CPU reservation、CT-visible CPU capacity 和 CT-local PSI 被看成了同一个 limit。批准的 overcommit policy 保持 logical ceiling 独立，只有 projected CPU 超过 CT-visible capacity 且 `full.avg10 >= 50%` 时才阻止启动。 `some.avg10` 和 `full.avg10` 是 pressure observation，不是 CPU usage 或 allocation count。

部署方的 dashboard source 位于 operator-owned `obs` stack。它的 dashboard path 是 `grafana/dashboards/incus/incus-agent-vm.json`，provisioning provider 是 `grafana/provisioning/dashboards/incus.yml`；Grafana service 以 read-only 方式挂载这两个路径。恢复出的 dashboard snapshot 可以作为部署证据和公共模板的视觉基线，但部署实例必须保留在拥有它的仓库中。如果 snapshot 尚未被跟踪，应用 dashboard 变更前必须先在拥有它的仓库中完成版本化。

# 解决方案

## 仓库合同

使用[观测合同](../../specs/agent-vm/contracts/observability.md)作为 metric name、权威边界、limit line 规则和验证方法的来源。该文件就是本方案的 `observability contract` 引用入口。该合同明确区分以下事实：

- CT root 和 `/var/lib/incus/images` 的 filesystem capacity。
- Physical Incus pool 的 total、used 和 free bytes。
- Guest rootfs allocation 和 quota。
- Native CPU、memory、disk 和 managed-container slot 的 allocation、reservation、remaining 和 limit。
- Admission readiness、reclaim-required、draining、operation failure、image-unpack failure、warning 和 task state。
- Logical default-container slot 与当前可启动 default-container slot 的 snapshot estimate，以及它们的 request/cost 输入和非权威声明。

渲染后的部署看板分为两个：

- `Agent VM / Capacity & Admission`：展示 reservation composition、CT-visible capacity、CPU PSI/startup gate、runtime pressure、disk/image staging 以及 admission/failure state。
- `Agent VM / Inventory & Operations`：展示所有 managed task state、有界的 per-task resource/lease/mapping data、structured error code/phase 以及 pending cleanup/operation state。

第一个看板使用 `admin-metrics`，第二个看板使用 `admin-status`。两个 action 遵循 `independent sampling lanes`；inventory failure `does not erase the last valid capacity families`。 `agent-vm-deployd` 在后台分别采样两个 action，并通过自己的 `GET /metrics` endpoint 向 Prometheus 输出最终指标。慢或失败的 inventory observation 必须 `removes or invalidates task rows`，同时保留最新有效的 capacity families。该 endpoint 不在 scrape 请求中触发新的采样，也不引入独立的 controller bridge、Pushgateway 或 textfile relay。

## 部署实现

1. 在拥有部署配置的仓库中复制[公共观测配置示例](../../../deploy/observability/observability.config.json.example)，填写部署方负责的值，把 `dashboard_path` 设置为 Grafana 容器内稳定的 `current` 目录，然后使用 `render_observability_dashboard.ts` 或部署方记录的 split-render wrapper 渲染两个 dashboard 和 provisioning 输出。Renderer 通过原子 `current` symlink 发布 dashboard pair，provisioning YAML 通过一次临时文件加 rename 安装。
2. 把 Prometheus scrape target 指向部署控制服务的内部 `agent-vm-deployd /metrics` endpoint，并把 `controller_job` 设置为 `agent-vm-deployd`。不要把槽位 textfile、snapshot 文件或独立 bridge 作为第二个 scrape target。
3. 把 CT root 和 `/var/lib/incus/images` filesystem series 纳入 deployd 的最终输出。不得从 `incus_project_limit`、guest rootfs size 或 pool free space 推导这些 series。
4. 使用 `validate_observability_dashboard.ts` 检查渲染文件，然后审查生成 diff。部署方可以在拥有部署配置的仓库中增加专用 panel，但必须记录每个 panel 的数据权威。
5. 只根据 authoritative metric 或明确的 policy threshold 配置横向虚线 limit line。对于缺失或为 `-1` 的 Incus project limit 不得绘制 line。
6. Reload provisioning 后通过 Grafana 验证看板。必须确认 dashboard metadata 仍被正确 provisioning、每个 panel 都能返回 fresh data，并且可以区分 image-unpack failure、pool exhaustion、CT-root exhaustion 和 guest-level ENOSPC。

## 当前 memory line

当前部署的 CT-visible memory capacity 是 `32768 MiB`（`32 GiB`），配置的 logical memory budget 同样是 `32768 MiB`，`memory_overcommit_ratio` 为 `2x`，因此唯一权威的 logical limit 是 `65536 MiB`（`64 GiB`）。公共模板将这些 series 分别标记为 `CT capacity` 和 `logical limit`，并从 authoritative metric 读取值；不能把该部署策略写死在可复用资源中。看板不得从 PVE UI working-set value、值为 `-1` 的 project quota 或 pressure sample 推导这些值。

容量视图必须把实际 CPU、memory wall 与 policy target 分开，并把 physical pool available bytes 与 guest rootfs allocation 分开。 “下一个 managed container 能有多大”必须由 CPU、memory 和 disk 的 native `allocated`、`reserved`、`remaining`、`limit` metrics，以及 controller readiness 和 reclaim state 共同回答。 Pool free bytes 只是 physical headroom，不是完整的 admission result。

容量视图还必须展示 policy default-container request/cost、观测到的 task-state count、logical default-container slot 和当前可启动的 default-container slot。这两个 slot value 都是有界的 snapshot estimate：先根据已验证的 CPU、memory 和 physical-disk headroom ratio 的最小值计算 logical estimate，再对 startable estimate 应用当前 CT-visible CPU capacity 和 50 percent startup-pressure policy。输入未验证时省略估算，不能把它们称为 native maximum 或 safe maximum。

清单视图必须展示 active、preparing、closing、deleting、failed、cached 和 absent record。排序时先展示 active/transitional row，再展示 failed cleanup，cached/absent 默认折叠。保留 task ID、instance/session identity、state、native status、guest availability、requested resource、effective lease、expiry、SSH mapping 和 structured last-error code/phase。 Requested CPU 以 vCPU 展示，requested RAM 和 disk 以 GiB 展示，lease expiry 使用 Unix seconds，避免表格把原始 byte count 当成含义不明确的短值。不得暴露 raw error message、credentials 或 deployment path。

## 验收

公共 dashboard template 能够完成渲染并通过 validation 时，可复用看板路径才算完成。部署实现只有在渲染源已经版本化到拥有 provisioning 的仓库，并且无需访问 host manager 即可观察以下事实时才算完成：

- CT root 和 image-staging 的 used/available bytes。
- Physical pool 的 total/used/free bytes。
- 与 physical usage 分开显示的 guest rootfs allocation。
- CPU、memory、disk 和 managed-container slot 的 allocated、reserved、remaining 和 limit。
- Admission ready、reclaim-required 和 draining state。
- 按 phase 区分的 operation success/failure、image-unpack failure、warning、uptime、scrape freshness 和 task-state count。
- 每个可用 authoritative limit 都有 horizontal dashed line，缺失或为 `-1` 的值不生成虚构 line。
- CT-visible memory capacity 与 logical memory reservation limit 是两条由 metric 支撑的独立 line，pressure 和 OOM 只是 observation，不改变 logical line。
- CPU PSI `some.avg10`、`full.avg10`、50 percent startup threshold 和 `startup_blocked` 与 logical CPU remaining 分开显示，pressure line 不改变 logical line。
- 两个 dashboard 使用自动刷新，reload 后仍然彼此独立；成功 observation 后 inventory table 不保留 stale task row。
- 两个 read-only action 使用独立的 sampling lane；慢或失败的 inventory action 不会擦除最新有效的 capacity family，失败的 inventory observation 会移除或标记 task row 失效，而不是静默保留 stale series。Source-specific health 和 observation timestamp 必须能与 aggregate `up=0` 区分。
- Prometheus 只抓取 `agent-vm-deployd` 的 `/metrics`，Grafana 只通过 Prometheus datasource 查询，双槽位同时存活时所有槽位级 series 都带 `slot` label。

# 防护规则与复用说明

- 不要修改 provisioned Grafana database 后把结果当作持久实现。
- 当值为 `-1` 时，不要使用 Incus project quota metric 作为 native admission limit。
- 不要把 guest rootfs allocation 标记为 physical disk usage。
- 不要从 Incus pool capacity 推断 CT root 或 image-staging capacity。
- 不要把 CPU PSI 当作 CPU utilization、logical reservation usage 或 native managed-container-slot limit。
- 不要把 derived default-container slot estimate 称为 safe maximum 或 authoritative Incus capacity。
- 不要把 packet-per-second query 乘以 `8`。
- Deployment address、credential、dashboard datasource identity、controller job、渲染后的 dashboard 和 metrics listener 配置必须保存在部署仓库或受保护的运维记录中；公共资源包只保留占位符和通用默认值。

# 参考

- [Agent VM 观测合同](../../specs/agent-vm/contracts/observability.md)
- [Agent VM 拓扑与恢复运维手册](../../specs/agent-vm/OPERATIONS.md)
- [Native 部署边界](../../../skills/agent-vm-ops/references/deployment.md)
- [Agent VM overcommit CPU startup pressure ADR](../../adr/0032-agent-vm-cpu-startup-pressure-for-overcommit.md)
- [Agent VM capacity and inventory observability ADR](../../adr/0033-agent-vm-capacity-and-inventory-observability.md)
