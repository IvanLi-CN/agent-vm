import { useParams, Link } from 'react-router';
import { Mermaid } from '../components/mermaid';
import type { MDXComponents } from 'mdx/types';
import defaultComponents from 'fumadocs-ui/mdx';
import data from '../data';
const modules = import.meta.glob('../../.generated/content/*.{md,mdx}', { eager: true }) as Record<string, { default: React.ComponentType<{ components: MDXComponents }> }>;
export default function Document() {
  const { slug } = useParams();
  const doc = data.documents.find(d => d.slug === slug);
  const Content = doc && modules[`../../.generated/content/${slug}${doc.path.slice(doc.path.lastIndexOf('.'))}`]?.default;
  if (!doc || !Content) return <p role="alert">找不到此文档。</p>;
  return <>
    <p className="document-source"><Link to={'/source?path=' + encodeURIComponent(doc.path)}>查看构建来源</Link></p>
    <div className="document-reading"><nav className="toc" aria-label="页内目录">{doc.toc.filter(h => h.depth > 1 && h.depth < 4).map(h => <a key={h.anchor} href={'#' + h.anchor}>{h.title}</a>)}</nav>
    <article className="prose"><Content components={{ ...defaultComponents, Mermaid }}/></article></div>
  </>;
}
