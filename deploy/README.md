# 部署资料

本目录保存 Agent VM 部署专用资料的唯一源码。Go 控制服务、Deployd 和 CLI 的实现仍位于原 Go 模块，服务器上的固定安装路径不随源码目录调整而变化。

从[简明部署指南](../docs/deployment.md)开始阅读；全新环境的输入准备、预检、安装与重试见[首次安装步骤](docs/install.md)。槽位布局、角色隔离、状态迁移、观测和恢复细节见[蓝绿部署手册](docs/blue-green.md)。长期约束由[Agent VM 规范](../docs/specs/agent-vm/SPEC.md)及其合同定义。

## 目录

- `assets/systemd/`：服务与兼容定时器资源。
- `assets/sysusers.d/`、`assets/tmpfiles.d/`：专用用户和目录配置。
- `assets/blue-green/`：Caddy、rqlite 配置示例及固定组件清单。
- `assets/*.example`：控制服务、Deployd 和 Web 的配置示例。
- `docs/`：部署操作细节。

示例中的占位符必须由部署方替换。真实地址、证书私钥、凭据、数据库和渲染后的观测配置不得提交到仓库。资料目录存在不代表自动安装或隔离验收已经完成。

## 部署资料清单

在仓库根目录运行下面的命令，将资料复制到指定的全新绝对路径，并生成记录源修订、文件摘要和权限的 `materials-manifest.json`。输出目录必须位于本目录之外；同一源码修订的产品包和部署资料应配套使用。

```sh
bun deploy/scripts/build-materials.ts --source-revision "$(git rev-parse HEAD)" --output /tmp/agent-vm-deployment-materials
```

此命令只生成资料目录与清单，不执行安装，不表示正式发布附件已生成。Go 安装入口已提供，完整隔离验收尚待完成，不能将构建资料视为验收通过。
