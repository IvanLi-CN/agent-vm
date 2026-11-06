# 观测接入

数据路径为槽位只读观测 → Deployd 最近完成的快照 → Prometheus 拉取 Deployd 的 `/metrics` → Grafana 查询 Prometheus。两个槽位的 JSON/textfile 只是 Deployd 内部输入，不另建 target。Caddy 不提供指标路由。

## 本机抓取

默认 `deployd.json` 使用 `metrics_listen=127.0.0.1:9911`。Prometheus 与控制服务在同一控制环境时，抓取配置如下；外部 Prometheus 不能通过这个回环地址跨主机抓取。

```yaml
scrape_configs:
  - job_name: agent-vm-deployd
    scrape_interval: 15s
    metrics_path: /metrics
    static_configs:
      - targets: ["127.0.0.1:9911"]
```

## 跨主机抓取

将 Deployd `metrics_listen` 设置为部署方提供的私有管理地址，`network_acl` 设置为 Prometheus 来源的 CIDR 字符串，并配置 `metrics_tls.certificate=/etc/agent-vm-native/tls/metrics/server.crt`、`metrics_tls.private_key=/etc/agent-vm-native/tls/metrics/server.key`、`metrics_tls.client_ca=/etc/agent-vm-native/tls/metrics/client-ca.crt`。安装输入的 `files` 必须同时增加 `metrics_certificate`、`metrics_private_key`、`metrics_client_ca`，分别引用这三个文件的受保护源文件。证书的 SAN 必须匹配抓取使用的地址。

```yaml
scrape_configs:
  - job_name: agent-vm-deployd
    scrape_interval: 15s
    scheme: https
    metrics_path: /metrics
    tls_config:
      ca_file: /etc/prometheus/agent-vm/server-ca.crt
      cert_file: /etc/prometheus/agent-vm/client.crt
      key_file: /etc/prometheus/agent-vm/client.key
      server_name: <metrics-certificate-dns-name>
    static_configs:
      - targets: ["<private-management-address>:9911"]
```

服务端 CA、客户端证书及私钥由部署方提供，不能关闭 TLS 校验。服务端校验客户端 CA 和来源 CIDR；网络层还须只允许 Prometheus 来源。安装器不配置外部 Prometheus、Grafana 或网络设备。

## Grafana 查询与去重

活动槽位选择指标为 `agent_vm_deployd_slot_selected{slot="blue|green"}`。容量、任务等共享业务数据使用相同 job 与 instance 下的 selected 槽位过滤；不能对蓝绿镜像的同一业务数据求和。角色与进程面板必须保留两个槽位，不能用活动过滤掩盖 standby 失联。新鲜度与来源失败按[观测合同](../../docs/specs/agent-vm/contracts/observability.md)显示，不能把缺失序列补成健康零值。

模板与渲染命令见[观测模板说明](../observability/README.md)。渲染后的 datasource UID、部署地址和 provisioning 文件属于部署环境，不提交本仓库。加载后必须以真实任务核对图例、数据与唯一 target，不能仅凭 JSON 通过检查认定看板正常。

## 逐实例文件系统指标缺失

`incus_filesystem_*` 由 Incus 的实例存储指标能力提供，不是 Deployd 测量的物理池空间。某些存储驱动及实例组合不会返回这些序列；例如本次隔离验收的 dir 容器存储没有 rootfs 文件系统样本。应先确认唯一 target 为 up、`agent_vm_incus_metrics_up` 为 1，再直接检查对应 project 和实例是否存在 `incus_filesystem_size_bytes`。采集失败和上游未提供该指标必须分别记录，不能把两者解释成磁盘使用量为零。

来宾 rootfs 面板缺少样本时不能据此判断 VM 不可用，也不能据此通过该项容量验收。使用独立的物理存储池容量面板判断共享池空间；它不能替代逐实例用量或证明单实例硬配额。CPU、内存、网络、任务及 Deployd 角色指标仍须分别验证真实数据和新鲜度。
