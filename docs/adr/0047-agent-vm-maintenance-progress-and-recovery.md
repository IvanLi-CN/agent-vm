# ADR 0047：维护扫描进展与缓存释放收尾

## Status

Accepted.

## Context

已停止缓存的 release 日志可能在最后一次数据库提交前中断。它既不属于可重放的维护操作，也不会再次进入 closing 清理；让 pending 游标永久停在这条记录会阻止其他会话的过期清理。单纯放开未知操作的围栏或将记录直接标记成功都不能证明安全。

## Decision

本决定仅替代 [ADR 0045](0045-agent-vm-lifecycle-recovery-backpressure.md) 中 reconcile 游标必须停在失败对象之前的约束；保留其状态机、原生身份、单一权威、有界工作量和 age-GC 失败游标规则。

Reconcile 游标表示已尝试扫描的位置，不表示所有此前操作已经成功。每页最多扫描四条 pending 和十六个 task，总预算仍为两秒；pending 使用其中至多一秒，保留 task 清理机会。失败对象保持原 task/journal 及原生围栏，扫描继续处理其他对象，并在键集扫描回绕后重试。不能删除 intent、伪造终态或把游标用作准入安全证明。进程取消或控制器失权不授予任何额外执行权。

维护失败必须持久暴露，并阻止新 acquire/restart；正常状态读取、取回、续租和释放仍按既有合同运行。scheduler 继续执行有界维护，不因维护故障反复执行全局 startup。只有 pending 与 task 两个扫描均完成一个无失败周期后，才能清除故障健康状态。

同代 cached release 可以只核验、不重放原生操作而收尾，但必须同时满足保存请求与 digest、operation/session/generation、可选 task binding、worker 已终止且进程组不存在、同会话无未解决 worker、无 lease/资源预留、受管原生归属、Stopped、持久端口及原生 proxy 均不存在。必须保留原 request、digest、worker 和 recovery evidence。缺失或矛盾证据保持 pending。

手工全局 reconcile 可以在某个 orphan acquire 校验失败后继续处理其他会话的已到期任务，最终仍返回失败；这不是原子事务，也不允许绕过目标会话自身的 worker/identity 围栏。

## Consequences

未知历史记录会持续影响健康和准入，却不会永久饿死其他会话的清理。游标仍在既有 JSON metadata 内兼容增加周期状态，不变更协议版本或数据库 schema。拒绝直接清空 pending、放宽 worker 身份、由 Deployd 修改业务状态或增加第二个恢复权威。
