# Ubuntu 24.04 Agent VM 语言镜像

在 Linux/Incus 上执行一条命令，构建 Base、Rust、Java、Go 各自的 privileged/unprivileged 变体，共八个镜像。每种模式先构建并缓存已封装的公共层，再派生 Base 发布包和语言层。Base 包含公共工具，不安装 Rust、Zulu Java 或 Go 专用工具链。工具链采用官方二进制；不在宿主编译 Rust、Java 或 Go，不预装浏览器自动化依赖。

构建宿主必须支持当前 runc 的 nested Docker 工作负载。Ubuntu 24.04 初始发行的 Incus 6.0.0 缺少 nesting/AppArmor 修复；CI 使用 [Incus 官方安装文档](https://linuxcontainers.org/incus/docs/main/installing/)列出的 Zabbly 7.0 LTS 二进制软件源，并核对签名公钥指纹。不要通过关闭 AppArmor 或降低 Docker/runc 版本绕过验收。

```sh
bun run images:build -- --all
bun run images:build -- --profile base --variant both
bun run images:build -- --profile go --variant unprivileged
bun run images:build -- --all --check
```

`--check` 只验证锁与构建矩阵，不证明运行环境或镜像可用。默认使用 project/pool=`default`、network=`incusbr0`；通过以下环境变量选择已有资源。构建器不修改生产 profile、外层宿主权限或默认镜像。

```sh
AGENT_VM_IMAGE_PROJECT="$BUILD_PROJECT" \
AGENT_VM_IMAGE_POOL="$BUILD_POOL" \
AGENT_VM_IMAGE_NETWORK="$BUILD_NETWORK" \
AGENT_VM_IMAGE_OUTPUT="$BUILD_OUTPUT" \
bun run images:build -- --all
```

嵌套部署需要显式指定已审核的 `AGENT_VM_IMAGE_IDMAP_BASE`、`AGENT_VM_IMAGE_IDMAP_SIZE`。构建器使用无 profile 的临时容器，附加指定 pool/network、2 CPU 和 2 GiB 内存，每种模式至多一个并行构建实例。实例配置明确声明 privileged、nesting、mknod/setxattr interception；不挂载宿主 Docker socket。启动或 readiness 失败时不回退到其他模式。公共层 image 保留为缓存，临时实例及发布 alias 在任务结束时清理。

`images/lock/ubuntu-24.04-amd64.lock.yaml` 固定基础 fingerprint、版本和官方二进制 SHA256，另以 SHA256 绑定同目录的 APT 包锁。APT 锁保存精确版本及签名仓库元数据中的包摘要；安装前核对摘要，包不可用时失败。版本更新必须显式刷新两份锁并审查差异，禁止用构建时查询的最新版本覆盖锁。相同锁保证固定输入与版本，不保证含随机 SSH host key、时间戳的压缩资产字节相同。

每个镜像输出 `.tar.gz`、`.manifest.json`、`.sbom.json` 和 `.readiness.json`，总清单 `agent-vm-ubuntu24-SHA256SUMS` 覆盖全部文件。manifest 包含源码修订、lock 摘要、不可变镜像 fingerprint 和运行配置。SBOM 列出实际 Debian 包库存；语言/Node/Bun 二进制的版本、来源与摘要由 manifest 和锁补充。

Docker 验收使用 Ubuntu 的 busybox-static 创建本地 scratch 镜像，执行多层 build、删除文件、root/非 root run、Compose、DNS/HTTP、命名卷持久化和 daemon 重启。固定经典 overlay2 后端，避免新安装的 containerd snapshotter 默认值改变合同；只重启本次拥有的可丢弃 guest 的 Docker。公共缓存层和最终镜像封装前均停止 Docker socket、daemon 和 containerd，确认服务已停止且数据目录没有挂载后清空 `/var/lib/docker`、`/var/lib/containerd`，保留二进制、服务及配置；新实例启动时重新初始化运行状态。导出包必须拒绝块/字符设备节点、Docker/containerd 运行残留和不安全路径，目标下载验证及 Release 冻结也执行同一检查。导出前移除构建期 SSH host key 和 machine-id，配置独立的 multi-user.target 开机任务，在首次 SSH 连接前重新生成 host key，兼容 ssh.socket 激活。构建器必须从 sealed 镜像创建全新实例，先观察自动生成的 ed25519 key，再完成实际严格 SSH 登录和 Docker 工作负载验收；失败不得输出成功 manifest。导出的镜像仍须在最终环境完成实际 SSH 登录检查。

## 管理端验收

管理端按部署配置的 lifecycle profile 选择运行模式，仅复制 namespace 相关白名单；不继承其生产网络或挂载。通过更换经审核的 profile 选择 privileged/unprivileged，镜像自身不能改变容器权限。导入时核对 manifest 的运行配置与部署 profile 一致；不支持把非特权产物静默当作特权产物验收。

```sh
AGENT_VM_TEST_INCUS_PROJECT="$IMAGE_TEST_PROJECT" \
AGENT_VM_TEST_INCUS_POOL="$IMAGE_TEST_POOL" \
AGENT_VM_TEST_INCUS_PROFILE="$IMAGE_TEST_PROFILE" \
AGENT_VM_TEST_PRIVILEGED="$IMAGE_TEST_PRIVILEGED" \
AGENT_VM_TEST_IMAGE_FINGERPRINTS="$IMAGE_TEST_FINGERPRINTS" \
go -C skills/agent-vm-ops/control test ./internal/native \
  -run '^TestIncusImageVerificationIntegration$' -v -timeout 15m
```

此测试必须在明确授权的 Incus 运维环境运行，检查空 profile、仅 root disk 和预期权限模式，再使用真实 VerifyImage；普通 session guest SSH 权限不构成宿主运维授权。

## 发布与目标服务器导入

PR 预览由 `Guest image preview` 在 GitHub 托管 Ubuntu runner 上验证镜像，无需自托管 runner；它只产生短期 CI artifact，不是生产镜像来源。CI 先并行构建 privileged 和 unprivileged 两种 common base，分别上传带源码修订与 lock 摘要的短期 artifact；随后八个 Base/Rust/Java/Go 最终镜像槽位下载对应 variant 的 common base，再按最多三个并行任务派生和验收。`Guest image release` 是唯一的镜像发布入口，只接受从受保护 `main` dispatch 的手动运行，以该 dispatch SHA 作为精确源码身份；它复用相同的锁校验、common-base producer 和最终镜像矩阵，完成完整矩阵、源码身份、checksum 和 readiness 验证后，将 32 个镜像文件与独立的 `SHA256SUMS` 发布到 `agent-vm-images-<source-sha>` Release。常规产品 `Release` 不构建、不下载也不发布 guest images；镜像不进入控制服务、客户端或 Deployd 的二进制包，发布后不得追加或覆盖资产。

在目标服务器用 `gh release download "agent-vm-images-$IMAGE_SOURCE_REVISION"` 下载对应语言/模式的 tar、manifest、SBOM、readiness 和 `SHA256SUMS`，再运行 `python3 images/scripts/verify_assets.py --directory "$DOWNLOAD_DIR" --source-revision "$IMAGE_SOURCE_REVISION"` 验证所下载的镜像及四类 sidecar；核对 manifest 的 `source_revision` 等于镜像 Release 修订、模式匹配且 readiness 全通过。然后通过现有管理端本地包上传或配置的可信镜像来源登记到 catalog，并等待完整性和适用性通过；单独 `incus image import` 不会完成网页 catalog 登记。

先在一个目标完成导入和实际 Agent VM acquire/SSH/release，再处理另一个目标。两者通过后才由运维者选择新默认镜像；已有 task/cache 保持原 fingerprint。旧裸镜像按 catalog 退役流程保留已有引用。失败时保留旧默认，不自动删除旧镜像、缓存或现有会话。部署地址、凭据及实际 fingerprint 库存仅记录在受保护运维环境。

参考：[Incus syscall interception](https://linuxcontainers.org/incus/docs/main/syscall-interception/)、[Incus Docker nesting](https://linuxcontainers.org/incus/docs/main/faq/)、[Docker containerd image store](https://docs.docker.com/engine/storage/containerd/)。
