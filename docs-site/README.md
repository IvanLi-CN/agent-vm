# 独立文档站

Fumadocs + React Router 的静态子项目，使用根目录的 Bun workspace 和锁文件。开发和预览不依赖 Go 服务或管理台。首页直接引导读者阅读真实设计材料。

## 命令

使用 Bun `1.4.4`、Node `24.12.0`，在仓库根目录执行 `bun install --frozen-lockfile`。本地启动前通过 `$global-port-manager` 分配端口并传入 `DOCS_PORT`，不接管其他任务的监听端口。

```sh
DOCS_PORT=<leased-port> bun run docs:dev
bun run docs:check
bun run docs:build
DOCS_PORT=<leased-port> bun run docs:preview
DOCS_PORT=<leased-port> bun run docs:test:e2e
bun test docs-site/content
bun run --cwd docs-site build:storybook
DOCS_PORT=<leased-port> bun run --cwd docs-site storybook
```

浏览器测试前先构建站点；首次运行前执行 `bunx playwright install chromium`，Linux CI 使用 `--with-deps chromium`。测试覆盖桌面、1024px 和 393×852。独立 Storybook 展示实际图表组件及错误状态，不承载额外产品功能。

`build/client/` 是可部署到静态 Web 服务的独立产物，深层路由按目录输出 `index.html`。交付中没有管理台嵌入或发布部署。

## 内容维护

`content/generate.ts` 的来源清单接入首期七份材料，原始正文在仓库既有位置维护。`.generated/`、`.source/`、`.react-router/` 和构建目录是忽略的生成输入，不手工编辑。开发服务器监听来源变动并重新生成正文。

Markdown 保持 Markdown 解析语义；MDX 用于确有必要的正文交互。适配层处理标题元数据、锚点、相对链接、资源和 Mermaid。无需增加设计关联、需求追溯标记或另一份 MDX 正文。图表支持查看源码，章节可通过目录及锚点定位。

`docs:dev` 和 `docs:build` 保留单篇正文诊断；`docs:check` 严格拒绝失效引用和正文解析错误。工具链整体失败会正常中止构建。

## 来源与验收

页面显示来源提交、工作区修改状态和输入摘要。实际正文快照及行号随产物保存；未修改的外部引用定位到固定提交，修改中的引用材料使用构建快照。摘要覆盖正文、相关资源、站点源码与配置及根依赖锁，不包含宿主配置或绝对机器路径。

负责人实际阅读本主题材料，检查内容是否完整、排版是否易读、能否通过目录和链接定位设计，以及图表是否帮助理解。构建成功和自动测试不能代替这一确认。独立审阅页、需求追溯和异常关联演示不属于本站功能。
