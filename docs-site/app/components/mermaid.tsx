import { useEffect, useId, useRef, useState } from 'react';
import { Accordion, Accordions } from 'fumadocs-ui/components/accordion';
import { CodeBlock, Pre } from 'fumadocs-ui/components/codeblock';
import { Callout } from 'fumadocs-ui/components/callout';
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
  return <figure className="diagram"><div className="diagram-viewport" ref={ref} role="img" aria-label="设计流程图" tabIndex={0}/><figcaption className="diagram-hint">横向滚动查看完整图表</figcaption>{error && <Callout type="error" role="alert">{error}</Callout>}<Accordions type="single" collapsible><Accordion title="图表源码" value="code"><CodeBlock title="Mermaid"><Pre><code>{code}</code></Pre></CodeBlock></Accordion></Accordions></figure>;
}
