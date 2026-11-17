# 嵌套 Incus 权限准备与排查

本手册用于外层控制环境本身是 LXC/Incus 容器、内层由 Incus 创建 guest 的部署。安装器只检查已有 Incus 资源是否可查询，初始化器只调整项目聚合配额与 profile 进程上限；两者均不证明非特权 guest 可以启动，也不配置 UID/GID 映射。声明支持非特权 guest 的部署，必须单独完成本文的范围检查和隔离验收。

## 权限层次与范围规则

外层非特权容器的 root 映射到父 user namespace 的非零 ID。内层 `security.privileged=true` 的 guest 使用控制环境中的 root 身份，不因此获得外部宿主的 root；内层 `security.privileged=false` 则需要在控制环境内再映射一段 UID/GID。特权 guest 成功不能证明非特权路径通过，也不能据此要求将外层特权化。内层需要运行 Docker 等嵌套工作负载时，还需相应的 `security.nesting=true` 与外层安全策略支持。

在同一 user namespace 内读取 `/proc/self/uid_map`、`gid_map` 时，每行为 `内部起点 父命名空间起点 数量`。检查内层可使用哪些 ID 时，取第一列及数量计算范围，不能把第二列的父级 ID 当作控制环境内部 ID。Incus 配置中的 `Hostid` 和 subordinate ID 文件中的起点，在这里都相对于承载 Incus daemon 的控制环境。

以下仅为范围计算示例，不是可直接应用的部署配置：

```text
内部起点    父命名空间起点    数量
0           100000            65536
65536       165536            65536
```

该示例的内部有效范围为 `0–131071`。若 guest 请求 `0 → 100000，长度 65536`，其使用的控制环境 ID 为 `100000–165535`，其中 `131072–165535` 未被外层映射覆盖。即使 `/etc/subuid` 声明 `root:100000:65536`，也不能增加内核实际拥有的范围。

每一个 UID 和 GID 映射都必须同时满足：

1. 请求范围被控制环境的有效映射完整覆盖；多段映射逐段求并集，不能只比较最小值和最大值。
2. 调用用户的 subordinate ID 授权覆盖请求范围；root 使用 helper 时也要检查授权。使用 NSS subordinate ID 来源时，以实际查询结果为准。
3. `newuidmap`、`newgidmap` 可用且拥有必要权限；文件 capability 可以代替 setuid 位，不能仅凭 mode 为 `0755` 判错。
4. 所选范围已审核账户、文件属主、其他实例和运行时占用；“映射存在”不等于“范围空闲”。使用独立映射的多个 guest 还需足够的不重叠范围，共用范围的隔离效果不同。

示例中的 `65536–131071` 在外层有效映射内，但只有占用审核和 subordinate ID 授权均通过后才能使用。不得把这个示例值推广为所有部署的默认值，也不得缩小到不能覆盖 guest 所需用户和组的范围来绕过失败。

## 只读检查

先从受保护配置确认控制 SSH alias，经该 alias 进入控制环境。以下 Incus、`/proc` 和 systemd 命令在控制环境内执行；不得据此访问 PVE 或未经授权的外部宿主。将项目、profile 和实例变量设为已核实的实际对象，不从镜像名或历史实例名猜测。

```sh
systemd-detect-virt
incus version
cat /proc/self/uid_map /proc/self/gid_map
incus_pid=$(incus query /1.0 | python3 -c 'import json,sys; print(json.load(sys.stdin)["environment"]["server_pid"])')
cat "/proc/$incus_pid/uid_map" "/proc/$incus_pid/gid_map"
readlink /proc/self/ns/user "/proc/$incus_pid/ns/user"
cat /etc/subuid /etc/subgid
command -v newuidmap newgidmap
stat -c '%a %U:%G %n' /usr/bin/newuidmap /usr/bin/newgidmap
getcap /usr/bin/newuidmap /usr/bin/newgidmap
incus profile show "$incus_profile" --project "$incus_project"
incus list --all-projects --format json
getent passwd
getent group
```

系统提供 `getsubids` 时，运行 `getsubids root` 和 `getsubids -g root`，核对实际 UID/GID 授权来源。若 shell 与 daemon 的 user namespace 不同，先解释差异，以 daemon 及其实际 helper 的 namespace 为准，不能直接用 shell 的范围下结论。账户查询不覆盖文件属主和其他 subordinate ID 使用者；占用审核还需在已核实的相关文件系统中有界检查，不扫描外部宿主。

对于已存在的失败实例，保存 `incus config show "$instance" --expanded --project "$incus_project"` 和 `incus info "$instance" --show-log --project "$incus_project"`。关注 `security.privileged`、`security.idmap.base`、`security.idmap.size`、`raw.idmap`、`volatile.idmap.current`、`volatile.idmap.next`、`volatile.last_state.idmap` 以及日志中实际执行的 `newuidmap` 参数。不要手工改写 `volatile.*` 来强迫迁移。实例已删除时，只能读取确实存在的留存日志，不能假定该资源仍然存在。

| 证据 | 判断与下一步 |
| --- | --- |
| `uid range ... not allowed` 或对应 GID 错误 | helper 授权检查未通过；检查实际 subordinate ID 来源、调用用户和范围。 |
| `write to uid_map failed: Operation not permitted` | 内核写入被拒绝；先核对父级映射完整覆盖，再检查 helper capability、daemon namespace、seccomp、LSM 等限制。不能仅凭 `EPERM` 认定唯一根因。 |
| `Failed to set up id mapping` | 本次启动在映射阶段中止；保存前面的具体 helper 命令和错误，不能当作镜像内容损坏。 |
| AppArmor `without stacking`、BPF device 程序加载失败 | 分别记录隔离能力缺失；用日志顺序和对照实验判断是否阻止本次启动。成功启动不证明 AppArmor 或设备过滤已生效。 |

## 修复路径

权限或配置修改、创建测试容器、启停和清理都需要明确的基础设施授权。维护前按[维护 drain 与恢复](maintenance.md)确认没有发布事务竞争，等待有效租约和 pending transition 收敛，记录原配置、文件哈希、权限及精确资源身份。所有真实配置、地址、清单和运行输出保存在受保护运维记录中，不提交仓库。

优先考虑保留外层非特权边界，在其已有有效范围内分配内层 ID，协调 subordinate ID 授权与实例的 `security.idmap.base`、`security.idmap.size`。例如前述计算示例在占用审核通过后，可为 root 授权 `root:65536:65536`，并为独立测试实例设置相同 base/size。UID 与 GID 两边必须同时审核；备份 `/etc/subuid`、`/etc/subgid` 后，通过受控编辑或配置管理修改实际授权来源，保留其他用户及既有运行时需要的条目，不直接覆盖整个文件。

先在临时实例上显式指定映射验证，不修改生产 profile 或现有实例。Incus 会保留实例和 daemon 的映射状态；更改默认授权文件不保证已有实例或 daemon 立即采用新的默认范围。默认范围切换和已有 rootfs 的属主迁移需要独立的维护方案、备份及版本对应的重载指导，不为试验盲目重启共享 daemon，也不对数据目录递归 `chown`。

已有范围不足时，可由另行获授权的外部管理员扩大外层映射，再协调内部授权；这需要父级分配、冲突检查、挂载和文件属主迁移证据，不属于控制环境内的普通 guest 操作。继续使用内层特权 profile 是另一项需要审核信任边界的部署选择，会使 guest root 与控制环境 root 身份重合，不能作为非特权验收结果。任何路径都不应默认将外层改为特权、禁用安全策略或删除映射 helper。

## 隔离验收

在任务专属环境中使用独立存储池、已审核的测试网络及已有、固定 fingerprint 的容器镜像。下面变量由受保护配置和运维记录提供；`diag_root` 必须是已核实的控制环境 Incus 文件系统内新建的专属空目录，`diag_name`、`diag_pool` 必须唯一且尚不存在。记录 owner 标记，不使用产品的 managed session 标签冒充受管实例。

```sh
mkdir -m 0700 "$diag_root/pool"
incus storage create "$diag_pool" dir "source=$diag_root/pool" --description "$diag_name"
incus init "$image_fingerprint" "$diag_name" \
  --project "$incus_project" --no-profiles --storage "$diag_pool" \
  --network "$test_network" \
  -c boot.autostart=false -c security.privileged=false -c security.nesting=true \
  -c limits.cpu=2 -c limits.memory=2GiB \
  -c "user.permission-diag.owner=$diag_name"
```

默认映射已知无效时，先保存一次启动失败的退出码和日志作为基线。将修复拆成可比较阶段：仅调整测试实例的 base/size，核对是否仍被 helper 授权拒绝；协调对应授权后再启动。以下参数必须来自范围审核，不能直接复制前述数字：

```sh
incus config set "$diag_name" --project "$incus_project" \
  "security.idmap.base=$reviewed_base" "security.idmap.size=$reviewed_size"
incus start "$diag_name" --project "$incus_project"
incus config show "$diag_name" --expanded --project "$incus_project"
incus exec "$diag_name" --project "$incus_project" -- sh -c \
  'cat /proc/1/uid_map /proc/1/gid_map; id; systemctl is-system-running'
```

核对 expanded config 明确为 `security.privileged=false`，映射与审核值一致，guest 已就绪。仅有 Docker daemon 响应还不够；使用已验证、固定 digest 的测试镜像实际运行 Docker 容器，验证 root 与高位非 root 用户、可写层及所需网络能力。下面 `docker_test_image` 应指向已缓存且包含 `sh`、`id`、`nslookup` 的已验证镜像，`dns_test_name` 是获准查询的域名；缺少缓存时先按授权拉取并保存 digest，不能把下载失败当作 namespace 失败。

```sh
incus exec "$diag_name" --project "$incus_project" -- docker version
incus exec "$diag_name" --project "$incus_project" -- \
  docker run --rm --pull=never "$docker_test_image" sh -ec \
  'id; echo writable > /tmp/permission-test; cat /tmp/permission-test'
incus exec "$diag_name" --project "$incus_project" -- \
  docker run --rm --pull=never --user 65534:65534 "$docker_test_image" sh -ec \
  'id; echo writable > /tmp/permission-test; cat /tmp/permission-test'
incus exec "$diag_name" --project "$incus_project" -- \
  docker run --rm --pull=never "$docker_test_image" nslookup "$dns_test_name"
```

保存候选修订、环境身份、镜像 digest、显式权限模式、实际映射、每阶段命令与退出码。AppArmor/BPF 状态单独记录；一次 Docker smoke test 不覆盖 Docker build、所有工作负载或完整安全隔离。继承特权 profile 的成功结果不得计入非特权覆盖。

## 清理与回退

先导出本次测试配置和日志，再逐项检查精确实例的 owner 标记、root 设备所指 pool、停止状态，以及 pool 的 description、source 和 `used_by`。只删除本次测试实例；确认 pool 已无使用者后删除该 pool。不要对未知或受管 cache 执行全局 GC，也不要递归删除池父目录。

若仅作诊断，停止并删除测试实例后撤回本次临时授权：先确认当前文件与记录相比只有本次改动；存在并发编辑时停止自动恢复，不能以旧备份覆盖他人改动。通过受控的原子替换恢复原始内容、owner 和 mode，并核对哈希。仅删除已有 ownership 证据、无挂载且精确归属本次任务的诊断目录及日志。保留受保护证据，再按维护手册完成健康、对账和准入恢复。

永久修复需要另行确认默认 profile 策略、范围分配、已有实例与文件迁移、维护窗口及回退条件；临时试验成功并恢复原配置后，不能宣称默认部署已修复。若证据仍依赖外部安全策略，列出所需配置供获授权管理员核对，不自行探索外部宿主。

## 依据

- [Linux user namespaces：映射与 capability 规则](https://www.man7.org/linux/man-pages/man7/user_namespaces.7.html)。
- [Incus subordinate ID 与 user namespace 映射](https://linuxcontainers.org/incus/docs/main/userns-idmap/)。
- [Incus 实例映射、特权与 nesting 选项](https://linuxcontainers.org/incus/docs/main/reference/instance_options/)。
