import {test,expect} from 'bun:test';
import {trace} from './trace';
const req={path:'spec.md',text:'### REQ-X-001 — 中文\n\n内容\n\n### VER-X-001 — 检查\n\n- covers: `REQ-X-001`'};
test('reads source declarations and many-to-many relationships',()=>{const result=trace([req,{path:'design.md',text:'## 内容\n\n- design-id: `DESIGN-X-SOURCE`\n- covers: `REQ-X-001`'}]);expect(result.requirements[0].design).toHaveLength(1);expect(result.requirements[0].verification).toHaveLength(1);expect(result.diagnostics).toEqual([]);});
test('missing relationships remain visible',()=>expect(trace([req]).diagnostics[0].kind).toBe('missing'));
test('broken and malformed relationships are distinct',()=>{const r=trace([req,{path:'design.md',text:'## A\n- design-id: `DESIGN-X-A`\n- covers: `REQ-X-999`\n## B\n- covers: garbage'}]);expect(r.diagnostics.some(d=>d.kind==='broken')).toBe(true);expect(r.diagnostics.some(d=>d.kind==='unparseable')).toBe(true);});
test('duplicate IDs never overwrite each other',()=>{const r=trace([req,req]);expect(r.requirements).toHaveLength(0);expect(r.diagnostics.some(d=>d.message.includes('Duplicate'))).toBe(true);});
test('ignores declarations inside code examples',()=>{const r=trace([{path:'a.md',text:'```md\n### REQ-FAKE-001\n```'}]);expect(r.requirements).toHaveLength(0);});
