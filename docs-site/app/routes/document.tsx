import { useParams, Link } from 'react-router';
import { useEffect, useId, useRef, useState } from 'react';
import type { MDXComponents } from 'mdx/types';
import defaultComponents from 'fumadocs-ui/mdx';
import data from '../data';
const modules = import.meta.glob('../../.generated/content/*.{md,mdx}', { eager: true }) as Record<string, { default: React.ComponentType<{ components: MDXComponents }> }>;
export function Mermaid({ code }: { code: string }) {
  const ref = useRef<HTMLDivElement>(null), id = useId().replace(/[^a-z0-9]/gi, '');
  const [error, setError] = useState('');
  useEffect(() => {
    let cancelled = false;
    import('mermaid').then(async ({ default: mermaid }) => {
      mermaid.initialize({ startOnLoad: false, securityLevel: 'strict' });
      const { svg } = await mermaid.render(`diagram${id}`, code);
      if (!cancelled && ref.current) ref.current.innerHTML = svg;
    }).catch(() => { if (!cancelled) setError('图表无法呈现，请检查构建来源中的 Mermaid 语法。'); });
    return () => { cancelled = true; };
  }, [code, id]);
  return <figure><div ref={ref} role="img" aria-label="设计流程图"/>{error && <p role="alert">{error}</p>}<details><summary>图表源码</summary><pre>{code}</pre></details></figure>;
}
export default function Document() {
  const { slug } = useParams();
  const doc = data.documents.find(d => d.slug === slug);
  const Content = doc && modules[`../../.generated/content/${slug}${doc.path.slice(doc.path.lastIndexOf('.'))}`]?.default;
  if (!doc || !Content) return <p role="alert">找不到此文档。</p>;
  return <>
    <p><Link to={'/source?path=' + encodeURIComponent(doc.path)}>查看构建来源 ↗</Link></p>
    <nav className="toc" aria-label="页内目录">{doc.toc.filter(h => h.depth > 1 && h.depth < 4).map(h => <a key={h.anchor} href={'#' + h.anchor}>{h.title}</a>)}</nav>
    <article className="prose"><Content components={{ ...defaultComponents, Mermaid }}/></article>
  </>;
}
