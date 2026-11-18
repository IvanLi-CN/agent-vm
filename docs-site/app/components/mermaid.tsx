import { useEffect, useId, useState, type ReactNode } from 'react';
import { Collapsible, CollapsibleTrigger, CollapsibleContent } from 'fumadocs-ui/components/ui/collapsible';
import { buttonVariants } from 'fumadocs-ui/components/ui/button';
import { CodeBlock, Pre } from 'fumadocs-ui/components/codeblock';
import { TransformWrapper, TransformComponent, useControls, useTransformComponent } from 'react-zoom-pan-pinch';
import * as Dialog from '@radix-ui/react-dialog';
import { Maximize2, Minus, Plus, X } from 'lucide-react';
import { Callout } from 'fumadocs-ui/components/callout';

// Mermaid's configuration is global; keep initialization and rendering together
// so concurrent diagrams and theme changes cannot use another render's palette.
let renderQueue: Promise<unknown> = Promise.resolve();
type RenderedDiagram = { svg: string; theme: string; ratio: number };

export function Mermaid({ code }: { code: string }) {
  const id = useId().replace(/[^a-z0-9]/gi, '');
  const [diagram, setDiagram] = useState<RenderedDiagram>();
  const [expanded, setExpanded] = useState(false);
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
      element.setAttribute('width', '100%');
      element.removeAttribute('height');
      setDiagram({ svg: element.outerHTML, theme: dark ? 'dark' : 'light', ratio: element.viewBox.baseVal.width / element.viewBox.baseVal.height });
    };
    renderQueue = renderQueue.catch(() => {}).then(render).catch(() => {
      if (!cancelled) {
        setDiagram(undefined);
        setError('图表无法呈现，请在 GitHub 原始文件中检查 Mermaid 语法。');
      }
    });
    return () => { cancelled = true; };
  }, [code, dark, id]);
  return <figure className="diagram">
    {diagram && <Dialog.Root open={expanded} onOpenChange={setExpanded}>
      <DiagramViewport diagram={diagram} renderSvg={!expanded} action={<Dialog.Trigger asChild>
        <button type="button" className={buttonVariants({ variant: 'ghost', size: 'sm' })} aria-label="展开图表" title="展开图表"><Maximize2 aria-hidden="true" size={16}/></button>
      </Dialog.Trigger>}/>
      <Dialog.Portal>
        <Dialog.Overlay className="diagram-overlay"/>
        <Dialog.Content className="diagram-expanded" aria-describedby={undefined}>
          <Dialog.Title className="sr-only">图表大视图</Dialog.Title>
          <DiagramViewport diagram={diagram} expanded action={<Dialog.Close asChild>
            <button type="button" className={buttonVariants({ variant: 'ghost', size: 'sm' })} aria-label="关闭图表大视图" title="关闭图表大视图"><X aria-hidden="true" size={16}/></button>
          </Dialog.Close>}/>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>}
    {error && <Callout type="error" role="alert">{error}</Callout>}
    <Collapsible className="diagram-source">
      <CollapsibleTrigger className={buttonVariants({ variant: 'ghost', size: 'sm' })}>图表源码</CollapsibleTrigger>
      <CollapsibleContent><CodeBlock title="Mermaid"><Pre><code>{code}</code></Pre></CodeBlock></CollapsibleContent>
    </Collapsible>
  </figure>;
}

function DiagramViewport({ diagram, renderSvg = true, expanded = false, action }: { diagram: RenderedDiagram; renderSvg?: boolean; expanded?: boolean; action: ReactNode }) {
  return <div className="diagram-viewport">
    <TransformWrapper key={diagram.svg} minScale={1} maxScale={8}
      panning={{ excluded: ['nodeLabel', 'edgeLabel', 'text', 'tspan'], velocityDisabled: true }}
      doubleClick={{ disabled: true }} keyboard={{ disabled: false, animationTime: 0 }}
      velocityAnimation={{ disabled: true }} autoAlignment={{ animationTime: 0 }}>
      <DiagramControls action={action}/>
      <TransformComponent wrapperClass="diagram-canvas" contentClass="diagram-content"
        wrapperStyle={{ width: '100%', ...(expanded ? { height: '100%' } : { aspectRatio: String(diagram.ratio), minHeight: '220px' }) }}
        contentStyle={{ width: '100%', height: '100%' }}
        wrapperProps={{ role: 'region', 'aria-label': '可缩放平移的图表', 'aria-description': '拖动空白区域平移，滚轮或双指缩放；方向键平移，加减键缩放，0 键复位。图中文字可选择复制。' }}>
        {/* Keep only one live SVG: duplicate Mermaid IDs would break marker and style references. */}
        <div className="diagram-svg" data-theme={diagram.theme} dangerouslySetInnerHTML={{ __html: renderSvg ? diagram.svg : '' }}/>
      </TransformComponent>
    </TransformWrapper>
  </div>;
}

function DiagramControls({ action }: { action: ReactNode }) {
  const { zoomIn, zoomOut, resetTransform } = useControls();
  return useTransformComponent(({ state }) => <div className="diagram-controls" role="group" aria-label="图表视图控制">
    <button type="button" className={buttonVariants({ variant: 'ghost', size: 'sm' })} aria-label="缩小图表" title="缩小图表" disabled={state.scale <= 1} onClick={() => zoomOut(.3, 0)}><Minus aria-hidden="true" size={16}/></button>
    <button type="button" className={buttonVariants({ variant: 'ghost', size: 'sm' })} aria-label="完整显示图表" title="完整显示图表" onClick={() => resetTransform(0)}>{Math.round(state.scale * 100)}%</button>
    <button type="button" className={buttonVariants({ variant: 'ghost', size: 'sm' })} aria-label="放大图表" title="放大图表" disabled={state.scale >= 8} onClick={() => zoomIn(.3, 0)}><Plus aria-hidden="true" size={16}/></button>
    {action}
  </div>);
}
