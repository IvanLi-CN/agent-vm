import defaultComponents from 'fumadocs-ui/mdx';
import type {MDXComponents} from 'mdx/types';
import { useState } from 'react';
import { Link } from 'react-router';
import defaultData from '../data';
import type { Ref } from '../../content/trace';
const excerpts=import.meta.glob('../../.generated/excerpts/*.md',{eager:true}) as Record<string,{default:React.ComponentType<{components:MDXComponents}>}>;
function Excerpt({refItem}:{refItem:Ref}){const Content=excerpts['../../.generated/excerpts/'+refItem.id+'.md']?.default;return Content?<div className="prose excerpt"><Content components={defaultComponents}/></div>:<pre className="excerpt">{refItem.excerpt}</pre>;}
type ReviewData = typeof defaultData;
export function ReviewView({ data }: { data: ReviewData }) {
  const [query, setQuery] = useState(''), [onlyProblems, setOnlyProblems] = useState(false);
  const [selected, setSelected] = useState(''), [diagnosticKind, setDiagnosticKind] = useState('all');
  const rows = data.requirements.filter(r => (r.id + ' ' + r.title + ' ' + r.excerpt).toLowerCase().includes(query.toLowerCase()) && (!onlyProblems || !r.design.length || !r.verification.length));
  const active = rows.find(r => r.id === selected) ?? rows[0];
  function source(ref: Pick<Ref, 'path' | 'line'>) { return '/source?path=' + encodeURIComponent(ref.path) + '&line=' + ref.line; }
  function body(ref: Ref) {
    const document = data.documents.find(d => d.path === ref.path);
    return document ? `/docs/${document.slug}#${ref.anchor}` : source(ref);
  }
  return <>
    <h1>需求追溯</h1><p>选择需求，展开它的设计和验收依据。关联完整不代表实现完成或验收通过。</p>
    <label>筛选需求<input value={query} onChange={e => setQuery(e.target.value)} placeholder="编号或关键词"/></label>
    <label className="check"><input type="checkbox" checked={onlyProblems} onChange={e => setOnlyProblems(e.target.checked)}/>只看关联缺失</label>
    <p role="status">{rows.length} 项需求 · {data.diagnostics.length} 条来源诊断</p>
    <div className="trace"><div className="requirements" aria-label="需求列表">{rows.map(r => <button key={r.id} aria-pressed={active?.id === r.id} onClick={() => setSelected(r.id)}><strong>{r.id}</strong><span>{r.title.split('—')[1]}</span></button>)}</div>
      <section className="detail">{active ? <>
        <h2>{active.title}</h2><Link to={body(active)}>阅读需求原文 ↗</Link>
        {(['design', 'verification'] as const).map(kind => <div key={kind}><h3>{kind === 'design' ? '设计依据' : '验收依据'}</h3>
          {active[kind].length ? active[kind].map(ref => <details key={ref.id}><summary>{ref.id} · {ref.title}</summary><Excerpt refItem={ref}/><p>{ref.path}:{ref.line}</p><Link to={body(ref)}>定位正文</Link>{' · '}<Link to={source(ref)}>构建来源</Link></details>) : <p className="warning">尚未关联。{active.note ?? '需要补充依据或明确说明。'}</p>}
        </div>)}
      </> : <p>没有匹配的需求。</p>}</section>
    </div>
    {data.diagnostics.length > 0 && <section><h2>来源诊断</h2>
      <label>诊断类型 <select value={diagnosticKind} onChange={e => setDiagnosticKind(e.target.value)}><option value="all">全部问题</option><option value="missing">尚未关联</option><option value="broken">失效目标</option><option value="unparseable">无法解析</option></select></label>
      {data.diagnostics.filter(d => diagnosticKind === 'all' || d.kind === diagnosticKind).map((d, i) => <p key={i}>{d.kind} · {d.message} <Link to={source(d)}>检查来源</Link></p>)}
    </section>}
  </>;
}
export default function Review() { return <ReviewView data={defaultData}/>; }
