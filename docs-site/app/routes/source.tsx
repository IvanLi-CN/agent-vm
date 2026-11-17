import { useEffect } from 'react';
import { useSearchParams } from 'react-router';
import { DocsBody, DocsTitle } from 'fumadocs-ui/layouts/docs/page';
import { CodeBlock, Pre } from 'fumadocs-ui/components/codeblock';
import { Callout } from 'fumadocs-ui/components/callout';
import { Page } from '../components/page';
import data from '../data';

export default function Source() {
  const [params] = useSearchParams();
  const path = params.get('path') ?? data.documents[0].path;
  useEffect(() => {
    const line = params.get('line');
    if (line && /^\d+$/.test(line)) document.getElementById('L' + line)?.scrollIntoView({ block: 'center' });
  }, [params]);
  const text = data.snapshots[path];
  const lines = text?.split('\n');
  return <Page breadcrumb={{ enabled: false }}>
    <DocsTitle>构建来源</DocsTitle>
    <DocsBody><p>{path}</p><p>此快照对应当前预览内容，不受之后的工作区编辑影响。</p>
      {text !== undefined ? <CodeBlock title={path}><Pre className="source-code"><code>{lines!.map((line, i) => <span id={'L' + (i + 1)} key={i} data-line-number={i + 1} className={params.get('line') === String(i + 1) ? 'source-line selected' : 'source-line'}>{line}{i < lines!.length - 1 ? '\n' : ''}</span>)}</code></Pre></CodeBlock> : <Callout type="error" role="alert">来源未纳入本次构建。</Callout>}
    </DocsBody>
  </Page>;
}
