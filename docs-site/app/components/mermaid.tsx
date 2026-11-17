import { useEffect, useId, useState } from 'react';
import { Accordion, Accordions } from 'fumadocs-ui/components/accordion';
import { CodeBlock, Pre } from 'fumadocs-ui/components/codeblock';
import { ImageZoom } from 'fumadocs-ui/components/image-zoom';
import { Callout } from 'fumadocs-ui/components/callout';

// Mermaid's configuration is global; keep initialization and rendering together
// so concurrent diagrams and theme changes cannot use another render's palette.
let renderQueue: Promise<unknown> = Promise.resolve();

export function Mermaid({ code }: { code: string }) {
  const id = useId().replace(/[^a-z0-9]/gi, '');
  const [image, setImage] = useState<{ src: string; width: number; height: number; theme: string }>();
  const [error, setError] = useState('');
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
      if (cancelled) return;
      const container = document.createElement('div');
      container.innerHTML = svg;
      const element = container.querySelector('svg')!;
      const { width, height } = element.viewBox.baseVal;
      element.setAttribute('width', String(width));
      element.setAttribute('height', String(height));
      setImage({ src: 'data:image/svg+xml;charset=utf-8,' + encodeURIComponent(new XMLSerializer().serializeToString(element)), width, height, theme: dark ? 'dark' : 'light' });
    };
    renderQueue = renderQueue.catch(() => {}).then(render).catch(() => {
      if (!cancelled) {
        setImage(undefined);
        setError('图表无法呈现，请检查构建来源中的 Mermaid 语法。');
      }
    });
    return () => { cancelled = true; };
  }, [code, dark, id]);
  return <figure className="diagram"><div className="diagram-viewport">{image && <ImageZoom key={image.src} src={image.src} width={image.width} height={image.height} data-theme={image.theme} alt="设计流程图" rmiz={{ children: null, a11yNameButtonZoom: '放大图表', a11yNameButtonUnzoom: '关闭图表放大' }}/>}</div><figcaption className="diagram-hint">点击图表放大查看；按 Esc 返回正文。</figcaption>{error && <Callout type="error" role="alert">{error}</Callout>}<Accordions type="single" collapsible><Accordion title="图表源码" value="code"><CodeBlock title="Mermaid"><Pre><code>{code}</code></Pre></CodeBlock></Accordion></Accordions></figure>;
}
