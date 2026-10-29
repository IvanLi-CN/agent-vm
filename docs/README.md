# 项目文档导航

先根据问题选择文档层次；系统架构中的目标设计应标明实现状态，不能将尚未实现的设计当成部署事实。

| 需要了解 | 文档与职责 |
| --- | --- |
| 产品范围和项目入口 | [项目 README](../README.md)、[产品定义](../PRODUCT.md)。 |
| 系统如何组成 | [系统架构](architecture/system.md)是唯一的架构文档，描述组件、接口和故障边界，并标明尚未实现的部分。 |
| 长期行为与接口 | [Spec 索引](specs/README.md)、[Agent VM Spec](specs/agent-vm/SPEC.md)和其 [CLI](specs/agent-vm/contracts/cli.md)、[管理 API](specs/agent-vm/contracts/management-api.md)、[发布](specs/agent-vm/contracts/release.md)、[观测](specs/agent-vm/contracts/observability.md)合同。 |
| 已接受的关键决定 | [ADR 目录](adr/)，每份文件记录特定决策及状态；按主题查看，不将提案视为已替代 ADR。 |
| 日常操作与恢复 | [Agent VM 运维手册](specs/agent-vm/OPERATIONS.md)和部署方自己的受保护记录。 |
| 设计研究和建议 | [proposals](proposals/) 保存决策备忘录，是否已采纳以对应 ADR、Spec 和实现为准。 |
| 可复用工程经验 | [solutions](solutions/agent-vm/) 保存特定问题的解释与处理经验。 |
| 界面视觉与交互 | 根目录 [DESIGN.md](../DESIGN.md) 是界面设计系统，不是系统架构；[Web 表面简报](../.impeccable/surfaces/web.md)记录界面方向。 |

新增或修改设计时，架构文档解释组件、接口和故障边界；Spec 定义长期行为合同；ADR 记录难逆转的决策；操作文档说明运行与恢复。实现状态应通过代码及相应合同确认，而不是仅凭目标图判断。
