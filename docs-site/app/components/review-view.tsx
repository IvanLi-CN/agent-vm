import defaultComponents from 'fumadocs-ui/mdx';
import type { MDXComponents } from 'mdx/types';
import { useState } from 'react';
import { Link } from 'react-router';
import defaultData from '../data';
import type { Ref } from '../../content/trace';
const excerpts = import.meta.glob('../../.generated/excerpts/*.md', { eager: true }) as Record<string, { default: React.ComponentType<{ components: MDXComponents }> }>;
function Excerpt({ refItem }: { refItem: Ref }) {
  const Content = excerpts['../../.generated/excerpts/' + refItem.id + '.md']?.default;
  return Content ? <div className="prose rendered-excerpt"><Content components={defaultComponents}/></div> : <pre className="source-excerpt">{refItem.excerpt}</pre>;
}
type ReviewData = typeof defaultData;
const problemLabels = { missing: '尚未关联', broken: '失效目标', unparseable: '无法解析' };
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
  function title(ref: Ref) { return ref.title.replace(new RegExp('^' + ref.id + '\\s*[—–-]\\s*'), ''); }
  return <div className="review-page">
    <div className="review-heading"><h1>需求追溯</h1><p>选择需求，展开它的设计和验收依据。关联完整不代表实现完成或验收通过。</p></div>
    <div className="review-toolbar">
      <label className="search-field">筛选需求<input value={query} onChange={e => setQuery(e.target.value)} placeholder="编号或关键词"/></label>
      <label className="check"><input type="checkbox" checked={onlyProblems} onChange={e => setOnlyProblems(e.target.checked)}/>只看关联缺失</label>
      <p className="review-count" role="status">{rows.length} 项需求 · {data.diagnostics.length} 条来源诊断</p>
    </div>
    <div className="trace">
      <div className="requirements" aria-label="需求列表">{rows.map(r => <button key={r.id} aria-pressed={active?.id === r.id} onClick={() => setSelected(r.id)}><code>{r.id}</code><span>{title(r)}</span></button>)}</div>
      <label className="compact-picker">选择需求<select value={active?.id ?? ''} onChange={e => setSelected(e.target.value)}>{rows.length ? rows.map(r => <option key={r.id} value={r.id}>{r.id} · {title(r)}</option>) : <option value="">没有匹配的需求</option>}</select></label>
      <section className="detail">{active ? <>
        <div className="detail-heading"><div><code>{active.id}</code><h2 aria-label={active.title}>{title(active)}</h2></div><Link to={body(active)}>阅读需求原文</Link></div>
        {(['design', 'verification'] as const).map(kind => <section className="evidence-group" key={kind}><h3>{kind === 'design' ? '设计依据' : '验收依据'}</h3>
          {active[kind].length ? active[kind].map(ref => <details className="evidence" key={ref.id}><summary><span className="evidence-id">{ref.id}</span><span>{title(ref)}</span></summary><Excerpt refItem={ref}/><div className="evidence-footer"><span className="source-location">{ref.path}:{ref.line}</span><nav aria-label={ref.id + ' 来源'}><Link to={body(ref)}>定位正文</Link><Link to={source(ref)}>构建来源</Link></nav></div></details>) : <p className="warning">尚未关联。{active.note ?? '需要补充依据或明确说明。'}</p>}
        </section>)}
      </> : <div className="empty-state"><h2>没有匹配的需求</h2><p>试试其他编号、关键词，或关闭关联缺失筛选。</p></div>}</section>
    </div>
    {data.diagnostics.length > 0 && <section className="diagnostics"><div className="diagnostics-heading"><h2>来源诊断</h2>
      <label>诊断类型<select value={diagnosticKind} onChange={e => setDiagnosticKind(e.target.value)}><option value="all">全部问题</option><option value="missing">尚未关联</option><option value="broken">失效目标</option><option value="unparseable">无法解析</option></select></label></div>
      <ul>{data.diagnostics.filter(d => diagnosticKind === 'all' || d.kind === diagnosticKind).map((d, i) => <li key={i} data-kind={d.kind}><span className="diagnostic-kind">{problemLabels[d.kind]}</span><p>{d.message}</p><Link to={source(d)}>检查来源</Link></li>)}</ul>
    </section>}
  </div>;
}
export default function Review() { return <ReviewView data={defaultData}/>; }
