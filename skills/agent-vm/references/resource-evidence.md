# Agent VM 资源校准记录

本文件定义资源建议的实测记录格式。它是证据模板，不是控制服务的配置源；记录应放在任务或外部运行证据中，避免把部署地址、凭据、状态数据库或消费者生成的观测文件提交到仓库。

## 必填字段

每次校准至少记录以下信息：

| 字段 | 内容 |
| --- | --- |
| `observed_at` | UTC 采样时间 |
| `profile` | 使用的资源档位 |
| `project` | 项目或 workspace 标识，不含凭据 |
| `revision` | 源码 revision |
| `command` | 完整构建或测试命令 |
| `build_jobs` | `CARGO_BUILD_JOBS` 或等价并发数 |
| `requested` | 请求的 CPU、内存和磁盘 |
| `memory.current` | 任务期间观察到的当前内存 |
| `memory.peak` | 任务期间观察到的峰值内存 |
| `memory.events` | 至少包含 `oom`、`oom_kill` 和 `high` |
| `disk` | `df -h` 结果和任务期间可用空间 |
| `swap` | `swapon --show`、`memory.swap.max` 和是否发生 thrashing |
| `duration` | 构建或测试耗时 |
| `outcome` | `success`、`oom`、`enospc`、`swap-thrashing` 或其他明确结果 |

## 记录模板

```yaml
observed_at: "YYYY-MM-DDTHH:MM:SSZ"
profile: rust-build
project: "<project-or-workspace>"
revision: "<git-revision>"
command: "CARGO_BUILD_JOBS=4 cargo build --workspace --all-targets"
build_jobs: 4
requested:
  cpu: 8
  memory: "12GiB"
  disk: "40GiB"
observations:
  memory.current: "<value>"
  memory.peak: "<value>"
  memory.events:
    high: 0
    oom: 0
    oom_kill: 0
  disk:
    filesystem: "<df -h output>"
    available_before: "<value>"
    available_after: "<value>"
  swap:
    swapon: "<output or unavailable>"
    memory.swap.max: "<max, value, or unavailable>"
    thrashing: false
duration: "<seconds>"
outcome: success
notes: "<links to external evidence or concise explanation>"
```

## 校准规则

- 资源档位的 `minimum` 是可尝试下限，`recommended` 是通常应使用的起点；两者都不是物理容量保证。
- 只有连续三次同类任务成功，且 `memory.peak` 低于当前 `recommended` 的 70%，才可以提出下调建议。
- 任意一次 OOM、`oom_kill`、ENOSPC 或明显 swap thrashing 都必须保留失败记录，并在下一次执行时提高内存、扩大磁盘或降低并发。
- 观测缺失必须写明 `unavailable`，不能把缺失值转换成零，也不能从宿主 SWAP 推断 guest 已获得可用 SWAP。
- 记录完成后检查 `status`；任务结束且没有后续工作时按 Skill 的 Finish 规则 release，仍需要缓存时只降到能支撑后续工作的规格。
