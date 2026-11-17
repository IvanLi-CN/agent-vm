import data from '../data';
import { trace } from '../../content/trace';
const path='examples/trace-states.md';
const text=`# 可控关联示例（不是项目需求）
## REQ-EXAMPLE-001 — 尚未关联
- trace-note: 等待设计讨论，不表示验收通过。
## REQ-EXAMPLE-002 — 失效目标
## REQ-EXAMPLE-003 — 无法解析
## 完整设计
- design-id: DESIGN-EXAMPLE-001
- covers: REQ-EXAMPLE-001, REQ-UNKNOWN
## 无法解析的声明
- design-id: DESIGN-EXAMPLE-002
- covers: ???
`;
export const exampleData: typeof data = {...data,...trace([{path,text}]),snapshots:{[path]:text},documents:[]};
