# 原生验收镜像

本目录定义项目专用的 Linux、systemd、原生进程树和嵌套 Incus 验收镜像。它基于固定 Debian 13 amd64 容器镜像，与通用 Agent VM 镜像分别管理。

镜像仅包含测试工具链与运行前置，不包含源码 checkout、发布包、实际部署配置、凭据、状态数据库或部署方的来宾镜像。

## 必需属性

- 使用 Incus 系统容器镜像，基础为 Debian 13 amd64。
- Go 1.27.3，支持 `CGO_ENABLED=0` 构建；显式 race 测试路径另需 `build-essential` 和 `CGO_ENABLED=1`。
- Bun 1.4.4，从通用 Agent VM 镜像复制。
- Incus 6.x，以及已缓存的 Debian 13 内层镜像。
- systemd、OpenSSH、conntrack-tools、`ss`、文件系统扩容工具、Git、rsync、Python 和原生验收依赖。
- 启动 profile 开启 `security.nesting=true`；不得要求 KVM、QEMU 或 `/dev/kvm`。

镜像 alias 为 `agent-vm-native-validation`。构建证据记录发布 fingerprint 和基础镜像 fingerprint，不提交镜像导出文件。

## 验收

在专属环境把候选发布安装到 `/opt/agent-vm-native` 后执行：

```sh
deploy/scripts/agent_vm_native_acceptance.sh
```

此脚本验证已安装包、systemd 激活、UDS/Bun 传输、原生 worker 清理和嵌套 Incus 边界。完整新安装与蓝绿验收还须按[部署验收说明](../../../../deploy/docs/acceptance.md)执行，不能只凭测试镜像准备完成就宣称通过。
