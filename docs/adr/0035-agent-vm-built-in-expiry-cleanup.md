---
status: accepted
---

# Agent VM 到期清理由控制程序内置执行

Go 父级服务内置租约到期调度、租约回收和 stopped-cache age-GC，直接调用同一生命周期服务；持久租约状态和缓存年龄是恢复依据，调度不经过 SSH 或 HTTP，也不依赖外部 cron 或按租约生成的 systemd timer。
这个选择把到期回收与租约决策归于同一 Control authority，减少外部业务调度和重复控制路径；systemd 负责启动和重启程序。

内存计划丢失时从当前数据库重建；程序恢复后补处理已到期记录和错过周期的缓存 age-GC，未完成恢复前保持新 admission 关闭，不自动延长租约。
程序整体停止期间不能执行回收，因此需要重启恢复、补偿与健康观测，不能承诺停机期间准点清理；不另设独立 expiry 控制器。
租约回收仍遵守 generation、ownership、journal 与既有 lease-end 合同，关闭访问并停止 VM 后留下的磁盘按 stopped-cache 策略处理；age-GC 继续使用既有 24 小时阈值，容量压力或明确授权可提前回收，重复 release 不刷新年龄。

本 ADR 记录已确认的目标职责边界，详细设计见[Go 父级服务方案](../proposals/agent-vm-go-parent-service.md#内置到期调度与重启补偿)。
现行 TS/systemd 实现继续有效，直到迁移经验证完成；旧调度仅在桥接阶段保留，并随控制权交接退出，程序回退时依据当前状态恢复。
