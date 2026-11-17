# 独立文档站

Fumadocs + React Router 的静态子项目，使用根目录的 Bun workspace 和锁文件。开发和预览不依赖 Go 服务或管理台。

## 命令

使用 Bun `1.4.4`、Node `24.12.0`，在仓库根目录执行 `bun install --frozen-lockfile`。本地启动前通过项目的 `$global-port-manager` 分配端口并将其传入 `DOCS_PORT`，不接管其他任务的监听端口。

```sh
DOCS_PORT=<leased-port> bun run docs:dev
bun run docs:check
bun run docs:build
DOCS_PORT=<leased-port> bun run docs:preview
DOCS_PORT=<leased-port> DOCS_STORYBOOK_PORT=<gallery-leased-port> bun run docs:test:e2e
bun test docs-site/content
bun run --cwd docs-site build:storybook
DOCS_PORT=<leased-port> bun run --cwd docs-site storybook
```

浏览器测试前先构建站点和独立 Storybook，并分别租用两个服务端口。浏览器测试首次运行前执行 `bunx playwright install chromium`；Linux CI 使用 `--with-deps chromium`。`build/client/` 是可部署到静态 Web 服务的独立产物，深层路由按目录输出 `index.html`。交付中没有管理台嵌入或发布部署。

## 内容与关联

`content/generate.ts` 中的来源清单明确列出首期七份材料，原始正文只在仓库既有位置维护。`.generated/`、`.source/`、`.react-router/` 和构建目录是忽略的生成输入，不手工编辑。开发服务器监听来源变动并重新生成索引和正文。

需求与验收从标题中的 `REQ-*`、`VER-*` 以及已有 `covers:` 读取；设计段落采用普通 Markdown 列表声明：

```md
## 单源内容适配

- design-id: `DESIGN-DOCS-SOURCE`
- covers: `REQ-DOCS-002`, `REQ-DOCS-007`
```

多个需求之间用逗号分隔。标识在整个来源集合中唯一。缺少关联的合理草稿在原始需求段落内使用 `- trace-note: 说明缺口原因`，这不会被当成设计或验收依据。

`docs:dev` 和 `docs:build` 保留诊断以便审阅；`docs:check` 严格拒绝重复 ID、解析错误、失效目标和没有说明的关联缺失。关联完整不表示功能已经实现，更不表示人工验收通过。

## 来源身份与审阅

页面显示基准提交、工作区修改状态和输入摘要。实际正文快照与行号随产物保存；未修改的外部文档引用定位到固定提交，修改中的引用材料使用构建快照。输入摘要覆盖正文、相关资源、站点源码和配置以及根依赖锁，不包含宿主配置或绝对机器路径。

从首页进入需求追溯，筛选需求后展开设计与验收依据，选择“定位正文”或“构建来源”。异常演示独立位于 `/review/examples`，明确标注为可控示例，不能当作真实项目验收。

当前候选的人工验收必须由项目负责人实际走通审阅路径，并确认减少跨文件查找成本。构建成功、自动测试或截图均不能代替这一确认。
