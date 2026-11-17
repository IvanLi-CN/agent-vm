import { useParams, Link } from 'react-router';
import { DocsBody } from 'fumadocs-ui/layouts/docs/page';
import { Callout } from 'fumadocs-ui/components/callout';
import { Page } from '../components/page';
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
  const doc = data.documents.find(d => d.slug === slug);
  const Content = doc && modules[`../../.generated/content/${slug}${doc.path.slice(doc.path.lastIndexOf('.'))}`]?.default;
  if (!doc || !Content) return <Page><Callout type="error" role="alert">找不到此文档。</Callout></Page>;
  const toc = doc.toc.filter(h => h.depth > 1 && h.depth < 4).map(h => ({ title: h.title, url: '#' + h.anchor, depth: h.depth }));
  return <Page key={slug} toc={toc} tableOfContentPopover={{ trigger: { 'aria-label': '本页目录' } }}>
    <p className="document-source"><Link to={'/source?path=' + encodeURIComponent(doc.path)}>查看构建来源</Link></p>
    <DocsBody><Content components={{ ...defaultComponents, Mermaid }}/></DocsBody>
  </Page>;
}
