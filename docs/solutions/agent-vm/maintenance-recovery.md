---
title: 维护日志不能饿死其他会话的过期清理
module: agent-vm
problem_type: recovery
component: lifecycle
tags: [maintenance, journal, expiry, incus]
status: active
related_specs: [docs/specs/agent-vm/SPEC.md]
---

## 现象

控制请求和已有租约仍可使用，后台维护却持续返回 operation_unverified；过期实例仍运行并保留 proxy。另一些实例已停止并进入 cached，原 release journal 却仍为 pending。单独检查 Incus 健康或启动 ready 不能证明恢复正常。

## 原因

如果 pending 扫描必须等第一条 journal 终结后才能推进，未知历史记录就会永久阻挡后面的 worker 对账和 task expiry。完成原生清理后、提交 release 结果前的中断还会留下一个普通 replay 和 closing 分支都不处理的缓存终态窗口。

## 处理方式

维护扫描应在有界预算内分别给 pending 和 task 工作机会，保留失败对象与围栏，记录失败并在扫描回绕后重试；扫描位置不能成为操作成功或准入安全的证明。健康检查和新准入要读取持久维护故障，scheduler 自身继续修复，避免反复启动全局 recovery。

只读核验缓存释放时，至少同时验证原请求与 digest、operation/session/generation、可选 task binding、worker 及整个进程组已终止、同会话无未解决 worker、无 lease/预留/持久端口、精确原生 ownership、Stopped 和无 proxy。存在后续成功的租约操作时，当前缓存状态不能证明原 release 成功，应保留既有 superseded unknown-outcome 合同。

## 验证与边界

回归必须覆盖超过一个 pending 页的未知记录、无效 binding、有效租约保护、缓存释放的所有拒绝条件和维护健康恢复。真实 Incus 测试开始前必须确认所有对照实际 Running；以已经停止的实例验证 stop 会掩盖 worker 或超时问题。模拟时钟前进不能替代原生进程退出与进程组静止证明。

全局 preview 的 task 清单不包含全部 journal recovery 副作用，不能作为精确 session 授权的替代。测试通过不代表生产已安装，也不能代替真实部署事务、容量和唯一控制器主权验证。

## References

- [维护进展与收尾决定](../../adr/0047-agent-vm-maintenance-progress-and-recovery.md)
- [生命周期合同](../../specs/agent-vm/SPEC.md)
