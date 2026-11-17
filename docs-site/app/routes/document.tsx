import { useParams, Link } from 'react-router';
import { useEffect, useState } from 'react';
import { Mermaid } from '../components/mermaid';
import type { MDXComponents } from 'mdx/types';
import defaultComponents from 'fumadocs-ui/mdx';
import data from '../data';
const modules = import.meta.glob('../../.generated/content/*.{md,mdx}', { eager: true }) as Record<string, { default: React.ComponentType<{ components: MDXComponents }> }>;
export function meta({ params }: { params: { slug?: string } }) {
  const doc = data.documents.find(d => d.slug === params.slug);
  return [{ title: doc ? `${doc.title} | Agent VM 文档` : 'Agent VM 文档' }];
}
export default function Document() {
  const { slug } = useParams();
  const [tocOpen, setTocOpen] = useState(false);
  useEffect(() => {
    const desktop = matchMedia('(min-width: 1101px)');
    const update = () => setTocOpen(desktop.matches);
    update();
    desktop.addEventListener('change', update);
    return () => desktop.removeEventListener('change', update);
  }, []);
  const doc = data.documents.find(d => d.slug === slug);
  const Content = doc && modules[`../../.generated/content/${slug}${doc.path.slice(doc.path.lastIndexOf('.'))}`]?.default;
  if (!doc || !Content) return <p role="alert">找不到此文档。</p>;
  return <>
    <p className="document-source"><Link to={'/source?path=' + encodeURIComponent(doc.path)}>查看构建来源</Link></p>
    <div className="document-reading"><nav className="toc" aria-label="页内目录"><details open={tocOpen} onToggle={event => setTocOpen(event.currentTarget.open)}><summary>本页目录</summary><div>{doc.toc.filter(h => h.depth > 1 && h.depth < 4).map(h => <a key={h.anchor} href={'#' + h.anchor}>{h.title}</a>)}</div></details></nav>
    <article className="prose"><Content components={{ ...defaultComponents, Mermaid }}/></article></div>
  </>;
}
