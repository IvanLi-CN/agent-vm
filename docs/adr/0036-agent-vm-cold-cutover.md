---
status: accepted
---

# Agent VM 父级服务首次迁移采用冷切换

首次从 TS 控制实现切换到 Go 父级服务时采用冷切换：进入 drain 后拒绝新的 lease acquisition、restart admission 和 renew，继续允许查询、释放、已有租约到期清理和必要恢复，直到有效租约、进行中操作、任务状态转换及 native worker 全部收敛。
切换不强制终止 guest，也不把等待长租约的时间塞进既有 publisher 默认收敛预算；在收敛前保留 TS 为唯一控制权威，避免两个实现同时管理 live state。

这个选择牺牲首次切换期间的可用性换取明确的 ownership 边界和可回退性；切换超时保持旧 owner 与 drain 状态，不越权清理或强制接管运行中的任务。
详细顺序见[Go 父级服务方案](../proposals/agent-vm-go-parent-service.md#发布切换与恢复)。
