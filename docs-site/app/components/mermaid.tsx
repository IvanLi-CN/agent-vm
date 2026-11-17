import { useEffect, useId, useRef, useState } from 'react';
import { Accordion, Accordions } from 'fumadocs-ui/components/accordion';
import { CodeBlock, Pre } from 'fumadocs-ui/components/codeblock';
import { Tabs } from 'fumadocs-ui/components/ui/tabs';
import { TabsList, TabsTrigger, TabsContent } from 'fumadocs-ui/components/tabs';
import { Callout } from 'fumadocs-ui/components/callout';

// Mermaid's configuration is global; keep initialization and rendering together
// so concurrent diagrams and theme changes cannot use another render's palette.
let renderQueue: Promise<unknown> = Promise.resolve();

export function Mermaid({ code }: { code: string }) {
  const ref = useRef<HTMLDivElement>(null), id = useId().replace(/[^a-z0-9]/gi, '');
  const [error, setError] = useState('');
  const [view, setView] = useState('detail');
  const [dark, setDark] = useState<boolean | null>(null);
  useEffect(() => {
    const update = () => setDark(document.documentElement.classList.contains('dark'));
    update();
    const observer = new MutationObserver(update);
    observer.observe(document.documentElement, { attributes: true, attributeFilter: ['class'] });
    return () => observer.disconnect();
  }, []);
  useEffect(() => {
    if (dark === null) return;
    let cancelled = false;
    setError('');
    const render = async () => {
      if (cancelled) return;
      const { default: mermaid } = await import('mermaid');
      mermaid.initialize({
        startOnLoad: false, securityLevel: 'strict', suppressErrorRendering: true,
        theme: 'base',
        themeVariables: {
          darkMode: dark, fontFamily: 'system-ui, sans-serif', fontSize: '16px',
          primaryColor: dark ? '#243247' : '#edf2f7',
          primaryTextColor: dark ? '#f1f5f9' : '#17212b',
          primaryBorderColor: dark ? '#94a3b8' : '#64748b',
          secondaryColor: dark ? '#273b3b' : '#e6f1ee',
          tertiaryColor: dark ? '#303044' : '#eeedf7',
          lineColor: dark ? '#cbd5e1' : '#475569',
          textColor: dark ? '#f1f5f9' : '#17212b',
          edgeLabelBackground: dark ? '#181d25' : '#ffffff',
          clusterBkg: dark ? '#1b2330' : '#f7f9fc',
          clusterBorder: dark ? '#94a3b8' : '#64748b',
        },
      });
      const { svg } = await mermaid.render(`diagram${id}`, code);
      if (cancelled || !ref.current) return;
      ref.current.innerHTML = svg;
      const element = ref.current.querySelector('svg');
      if (element) {
        // Preserve Mermaid's natural text size instead of shrinking a wide
        // architecture diagram to the article width; the viewport scrolls.
        element.style.width = `${Math.ceil(element.viewBox.baseVal.width)}px`;
        element.style.maxWidth = 'none';
        element.dataset.theme = dark ? 'dark' : 'light';
      }
    };
    renderQueue = renderQueue.catch(() => {}).then(render).catch(() => {
      if (!cancelled) {
        ref.current?.replaceChildren();
        setError('图表无法呈现，请检查构建来源中的 Mermaid 语法。');
      }
    });
    return () => { cancelled = true; };
  }, [code, dark, id]);
  return <figure className="diagram"><Tabs value={view} onValueChange={setView}><TabsList aria-label="图表显示方式"><TabsTrigger value="detail">阅读细节</TabsTrigger><TabsTrigger value="overview">完整概览</TabsTrigger></TabsList><TabsContent value={view}><div className="diagram-viewport" data-view={view} ref={ref} role="img" aria-label="设计流程图" tabIndex={0}/></TabsContent></Tabs><figcaption className="diagram-hint">较宽图表可横向滚动；键盘聚焦后可用方向键移动。</figcaption>{error && <Callout type="error" role="alert">{error}</Callout>}<Accordions type="single" collapsible><Accordion title="图表源码" value="code"><CodeBlock title="Mermaid"><Pre><code>{code}</code></Pre></CodeBlock></Accordion></Accordions></figure>;
}
