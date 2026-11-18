# 镜像管理

镜像是 Agent VM 用于新会话和缓存恢复的受管对象，不能与停止缓存或产品发布包混为一谈。构建、锁文件和发布资产以[镜像 README](../../images/README.md)为来源；本页只解释使用者和维护者需要理解的选择、引用与删除边界。

## 来源和身份

每个可用镜像都应有受信来源、不可变 fingerprint、适用性结果和对应的 manifest、SBOM、readiness 证据。alias 只能作为查找入口，不能替代内容身份；页面和客户端应展示实际 fingerprint，并在来源或验证不完整时保留未知状态。

导入前验证来源、checksum、manifest 和锁定版本；导入后分别检查完整性与 Agent VM 运行适用性。镜像可用不等于已经成为默认镜像，也不等于任何生产环境已完成实际 acquire、SSH 和 release 验收。

## 默认镜像和会话绑定

默认镜像只影响之后创建的新会话。已有 VM、停止缓存和未完成的创建引用继续绑定原来的 fingerprint，不能因为默认选择改变就自动迁移。需要迁移时，维护者必须评估数据影响，显式销毁并按批准流程重建。

```text
镜像来源 -> 完整性验证 -> 适用性验证 -> catalog 登记 -> 可选默认
                                                    └-> 新会话选择
```

运行中的会话不会因为镜像页面的默认选择改变而换根文件系统。缓存恢复使用任务记录中的 `image_fingerprint`，绑定镜像缺失、退役或无法验证时，控制端应在启动、调整资源或暴露来宾前给出明确错误。

## 退役和删除

退役表示不再作为新会话的选择，但不等于立即删除。删除前检查默认引用、活跃 VM、停止缓存和正在进行的创建；只要仍有有效引用，就保留镜像或先完成明确迁移。失败时保留旧默认、旧缓存和现有会话，不自动清理以“看起来无引用”的对象。

生产导入由运维者在受保护环境执行。仓库中的示例、锁文件和构建脚本不记录真实地址、凭据或目标库存；目标服务器下载和验证镜像后，再通过管理端可信来源登记，不能用单独的 `incus image import` 代替 catalog 登记。

## 构建与验收入口

```sh
bun run images:build -- --all --check
bun run images:build -- --profile go --variant unprivileged
python3 images/scripts/verify_assets.py --directory <download-dir> --source-revision <revision>
```

`--check` 只验证锁和构建矩阵，不证明运行环境或镜像可用。实际构建、Incus 权限模式、SSH 登录和 Docker nesting 验收需要明确授权的隔离环境；详细步骤见[镜像构建与发布材料](../../images/README.md)。
