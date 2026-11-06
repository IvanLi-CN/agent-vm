# 首次安装

本文的命令在要承载 Agent VM 的 Linux amd64 或 arm64 控制环境现场执行。实际安装需要 root；预检不会修改系统文件、生成安装记录或启动服务。先完成全部输入，再运行预检，最后显式使用 `--apply` 安装。正在使用的生产环境不得用作本流程的隔离验收环境。

## 准备环境与制品

控制环境必须已经运行 systemd 和 Incus，并具备专属项目、profile、网络、network ACL、存储池及已验证的容器镜像。安装器只读取和校验这些条件，不安装 Incus，不改动 PVE、CT 权限、挂载或配额。域名、OIDC 应用及回调地址须事先注册，提供对应 TLS 证书。缺失条件会汇总为预检阻塞项。

准备相同 `source_revision` 的 Linux 产品 tar 包和部署资料目录。产品包包含 `bin/agent-vm-deploy`；资料目录包含 `materials-manifest.json`。执行发布方提供的摘要校验后，将制品放入 root 拥有且其他用户不可写的目录。不要通过运行未验证的二进制来代替摘要校验。匹配资料的生成方法见 [资料入口](../README.md)。

```sh
sudo install -d -m 0700 /etc/agent-vm-install-inputs
sudo install -d -m 0700 /var/tmp/agent-vm-install
```

以下命令假定已验证的产品包位于 `/var/tmp/agent-vm-install/product.tar`，同修订部署资料位于 `/var/tmp/agent-vm-install/materials/`，用于发起安装的已验证 CLI 位于 `/var/tmp/agent-vm-install/agent-vm-deploy`。这些位置属于本次输入，不改变服务器固定安装布局。

将与产品包同版本、已完成摘要校验的部署资料附件放到 `/var/tmp/agent-vm-install/deployment-materials.tar`。在上述受保护目录中执行下面的命令，从产品包只读取 CLI 这一项，并把资料附件展开到全新目录；不要使用正在存放其他文件的目录，也不要跳过前面的制品来源与摘要校验。

```sh
sudo sh -eu -c 'tar -xOf /var/tmp/agent-vm-install/product.tar bin/agent-vm-deploy > /var/tmp/agent-vm-install/agent-vm-deploy'
sudo chmod 0700 /var/tmp/agent-vm-install/agent-vm-deploy
sudo mkdir -m 0700 /var/tmp/agent-vm-install/materials
sudo tar --extract --file /var/tmp/agent-vm-install/deployment-materials.tar \
  --directory /var/tmp/agent-vm-install/materials \
  --no-same-owner --no-same-permissions --keep-old-files
```

## 准备受保护输入

将 [`install.json.example`](../assets/install.json.example) 复制到 `/etc/agent-vm-install-inputs/install.json`。它引用的每个文件必须由执行安装的用户拥有、不是符号链接，权限使用 `0600`；文件路径不能包含符号链接父目录。不要把真实输入提交到仓库。

| 输入 | 内容与来源 |
| --- | --- |
| `blue.json`、`green.json` | 以 [`config.json.example`](../assets/config.json.example) 为字段示例，填入同一套已验证 Incus 条件，分别设置 `slot=blue`、`slot=green`，`socket` 分别为 `/run/agent-vm-native/blue/app.sock` 和 `/run/agent-vm-native/green/app.sock`；两个槽位共享同一 `state_db`，使用带 HTTP Basic authentication 的 `https://<username>:<URL编码password>@127.0.0.1:4001`，不得使用示例中的旧 SQLite 路径。 |
| Web/OIDC | 在两个槽位配置的 `management` 中启用 Web，使用 [`web.json.example`](../assets/web.json.example) 的 `management` 子树，填写实际 issuer、client ID、secret、external origin、redirect URI 和管理员主体；具体字段以 [Web 合同](../../docs/specs/agent-vm/contracts/management-api.md) 为准。 |
| `deployd.json` | 以 [`deployd.json.example`](../assets/deployd.json.example) 为基础，保留固定状态、socket、slot 和 active 路径；本机观测使用 loopback 监听，跨主机接入另见[观测说明](observability.md)。 |
| `Caddyfile`、`caddy.env` | 复制 [`Caddyfile.example`](../assets/blue-green/Caddyfile.example) 和 [`caddy.env.example`](../assets/blue-green/caddy.env.example)，填写已注册域名。Caddy 只代理 active 槽位，不代理指标。 |
| `rqlite-auth.json` | 根据 [`rqlite-auth.json.example`](../assets/blue-green/rqlite-auth.json.example) 设置独立的 control 和 backup 凭据；control 凭据必须与两个槽位的 `state_db` 一致。 |
| `rqlite.crt`、`rqlite.key` | 匹配的服务端证书与私钥；证书必须包含 IP SAN `127.0.0.1`。槽位和初始化命令通过固定 `SSL_CERT_FILE` 信任该证书。 |
| `caddy.crt`、`caddy.key` | 覆盖已注册域名的匹配证书与私钥。 |

```sh
sudo chmod 0600 /etc/agent-vm-install-inputs/install.json /etc/agent-vm-install-inputs/blue.json /etc/agent-vm-install-inputs/green.json /etc/agent-vm-install-inputs/deployd.json /etc/agent-vm-install-inputs/Caddyfile /etc/agent-vm-install-inputs/caddy.env /etc/agent-vm-install-inputs/rqlite-auth.json /etc/agent-vm-install-inputs/rqlite.crt /etc/agent-vm-install-inputs/rqlite.key /etc/agent-vm-install-inputs/caddy.crt /etc/agent-vm-install-inputs/caddy.key
```

## 预检与安装

先运行不带 `--apply` 的命令，检查 JSON 中的 `mode`、`source_revision`、`bundle_sha256` 和 `blockers`。`blockers` 非空或命令退出非零时，按报告补齐条件，不要继续安装。产品包直接通过读取 tar 验证，不在预检中解包落盘。

```sh
sudo /var/tmp/agent-vm-install/agent-vm-deploy install \
  --config /etc/agent-vm-install-inputs/install.json \
  --bundle /var/tmp/agent-vm-install/product.tar \
  --materials /var/tmp/agent-vm-install/materials
```

确认 `mode=fresh`、修订与预期一致且无阻塞后，使用相同输入安装：

```sh
sudo /var/tmp/agent-vm-install/agent-vm-deploy install \
  --config /etc/agent-vm-install-inputs/install.json \
  --bundle /var/tmp/agent-vm-install/product.tar \
  --materials /var/tmp/agent-vm-install/materials \
  --apply
```

安装器验证固定版本 Caddy 和 rqlite 的下载摘要，安装专用用户、目录、配置、二进制与 systemd 资源，再启动 rqlite 和 Deployd。Deployd 的首次安装事务委托控制服务直接初始化空 rqlite，验证 blue 后授予唯一 controller lease、启用 traffic 并原子创建 active；Caddy 最后启动。Deployd 的部署 SQLite 位于独立部署目录，不能将它误认为业务 SQLite。

冷启动时，槽位先通过受保护 HTTPS 接口验证 rqlite 状态与 strong read，再启动控制进程；进程健康接口建立后 Deployd 才开始组合观测。仅有 rqlite 进程或端口不代表后端就绪，检查失败会让 unit 启动失败并保留恢复证据。

安装命令的总时限为三十分钟，每个固定组件的下载时限为十分钟，TLS 握手和响应头分别最多等待三十秒，下载内容仍必须通过清单摘要验证。下载超时发生在服务启动前，可使用相同输入重试，保留原安装事务身份；超时不会放宽证书、摘要或路径校验。网络错误报告保留原因，不包含重定向地址中的临时签名。

## 核验与重试

```sh
sudo /opt/agent-vm-native/bin/agent-vm-deploy status --json
sudo systemctl is-active agent-vm-rqlite.service agent-vm-deployd.service agent-vm-native@blue.service agent-vm-caddy.service
curl --fail http://127.0.0.1:9911/metrics
```

成功结果必须同时包含新鲜一致观测、`active_slot=blue`、blue 就绪且拥有 controller 与 traffic、green 不拥有角色，以及没有恢复中的事务。再按[验收说明](acceptance.md)验证 VM 的创建、执行、续租、释放和销毁。服务进程启动不等于 VM 功能通过。

安装记录为 `/var/lib/agent-vm-native/deployment/install.json`，仅保存阶段、修订、摘要和原事务 ID。文件安装失败后可使用完全相同的输入重试；不匹配的已有文件不会被覆盖。已发送首次启动请求而结果不确定时，安装器不会重放，请先查询原事务：

```sh
sudo /opt/agent-vm-native/bin/agent-vm-deploy transaction --transaction '<原事务ID>' --json
```

active 尚不存在时，可以显式请求 `recover --transaction '<原事务ID>' --action resume-bootstrap`，它会重新验证无业务任务的初始 rqlite，保留原事务身份。active 已存在时，此动作会被拒绝，按[蓝绿手册](blue-green.md)的 `finalize` 合同恢复。原事务完成后，再次运行相同安装命令以完成尚未启动的 Caddy。不要删除安装记录、Deployd 数据库或业务状态来绕过恢复。

已有蓝绿安装只核验并指向升级入口，不覆盖配置、不排空实例；升级命令见[蓝绿手册](blue-green.md)。存在旧业务 SQLite 时必须走既有发布封装与 `migrate` 事务，不能当作空环境初始化；Go 安装器识别 `mode=legacy` 后，通过本机发布适配器委托既有发布封装和 Deployd 迁移事务，保留已有配置、原事务和不确定结果证据；必须已经存在受保护的 `/etc/agent-vm-native/release-wrapper.json`。迁移结果不确定时不会自动重放，先查询该原始发布事务和 Deployd 的迁移事务。
