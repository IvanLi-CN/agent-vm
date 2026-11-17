# Agent VM 文档审阅实现覆盖

> 长期需求以 [SPEC.md](SPEC.md) 为准；本文件记录实现及验证事实。

## Current Status

- Implementation：独立 Fumadocs 静态站、单源适配、追溯索引、问题预览、来源快照与组件状态已实现。
- Lifecycle：active。
- 当前候选尚待项目负责人实际审阅及截图确认；未取得人工验收，未宣称 merge-ready。
- PR/Main 文档作业已接入既有 gate；当前没有远端 CI 或正式 PR 审查结论。

## Implementation Coverage

| 需求 | 实现依据 | 验证与缺口 |
| --- | --- | --- |
| REQ-DOCS-001、REQ-DOCS-002 | `docs-site/content/generate.ts` 明确接入七份来源，正文单源维护；设计列表声明读取到追溯索引。 | 来源、追溯与身份单测通过；没有第二份手工规范正文。 |
| REQ-DOCS-003、REQ-DOCS-004 | 独立 Bun workspace；Fumadocs MDX/UI、React Router 静态预渲染；导航、目录、明暗主题、代码与 Mermaid。 | 选定版本组合构建通过，页面与章节链接浏览器测试通过。 |
| REQ-DOCS-005、REQ-DOCS-006 | 需求关键词筛选、设计与验收依据展开、来源行定位；异常示例与诊断筛选；严格检查独立于可容错预览。 | 完整、缺失、损坏、无法解析、重复 ID 具有自动测试；桌面与窄屏键盘流程通过。 |
| REQ-DOCS-007 | 基准提交、修改状态、输入摘要、实际正文快照；外部引用固定提交，修改中的引用使用快照。 | 干净／修改／未跟踪身份测试及摘要测试通过；最终候选冻结后仍需重建确认。 |
| REQ-DOCS-008 | 真实需求审阅入口与独立异常示例；构建测量脚本。 | 下方记录实测；负责人确认交互价值仍待完成，自动化不代替人工确认。 |
| REQ-DOCS-009 | 首期站点与管理台、Go 服务和部署独立；项目级 Skill 提供维护入口。 | 未增加管理台嵌入、共享文档包、公开托管或产品发布。 |

## Validation

本地已完成 `bun run docs:check`、`bun run docs:build`、`bun test docs-site/content`、独立 Storybook 构建与桌面／`1024px` 中间宽度／`393×852` Playwright 检查；当前包含 14 项来源单测和 21 项浏览器检查（含显式证据捕获）。浏览器覆盖需求筛选、设计及验收入口、构建来源、异常诊断、键盘操作、章节链接和 Mermaid。

仓库 `bun run check`、`bun run test:repo`、`bun run test:skills` 与 Go 测试通过。Skill 测试在干净候选提交上重跑，发布打包保护正常通过。质量门禁声明检查通过，保留既有必需 gate 名称，新增文档作业作为依赖。

### 构建测量

环境为 macOS arm64、Apple M4、Bun `1.4.4`、Node `24.12.0`，依赖已安装。测量基准为干净提交 `7ea08c362b8231aa8bd449def6390b20cb0068fd`，输入摘要 `500b99a33344672c5f891289ade4c69481481ada6d84c0cbd67946d44ba75d85`；该摘要对应测量时的正文与配置，不冒充后续候选的摘要。

| 项目 | 实测 | 方法 |
| --- | --- | --- |
| 冷构建 | 5,917 ms | 清除该子项目 `.source/` 和 `build/` 后，完成生成、编译与静态预渲染。 |
| 完整预渲染构建 | 4,422 ms | 相同来源再次运行完整构建。 |
| 单文档开发重编译 | 441 ms | 从原始 Markdown 写入到生成模块经 Vite 转换的 HTTP 响应；临时探针随后恢复。 |

测量入口为 `DOCS_PORT=<leased-port> bun docs-site/content/measure.ts`，结果输出到忽略的 `.generated/performance.json`。没有预设性能阈值；以上为一次环境绑定观察，不能推断其他机器或全量项目文档的性能。开发循环没有重建 Go。

## References

- [需求合同](SPEC.md)
- [主题背景](HISTORY.md)
- [站点维护命令](../../../docs-site/README.md)
