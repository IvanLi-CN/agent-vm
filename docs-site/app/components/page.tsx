import { type ComponentProps, useState } from 'react';
import { DocsPage } from 'fumadocs-ui/layouts/docs/page';
import { Accordion, Accordions } from 'fumadocs-ui/components/accordion';
import { Callout } from 'fumadocs-ui/components/callout';
import { buttonVariants } from 'fumadocs-ui/components/ui/button';
import { useCopyButton } from 'fumadocs-ui/utils/use-copy-button';
import { Check, Copy, ExternalLink } from 'lucide-react';
import data from '../data';

interface DocumentActionsProps {
  identity: { revision: string; dirty: boolean; sourceId: string };
  copyLink?: () => Promise<void>;
}

export function DocumentActions({ identity, copyLink = () => navigator.clipboard.writeText(location.href) }: DocumentActionsProps) {
  const [copyError, setCopyError] = useState(false);
  const [copied, copy] = useCopyButton(async () => {
    setCopyError(false);
    try { await copyLink(); }
    catch { setCopyError(true); }
  });
  return <footer className="site-footer">
      <button className={buttonVariants({ variant: 'outline', size: 'sm' })} onClick={copy}>{copyError ? '复制失败，请重试' : copied ? '已复制' : '复制页面链接'}</button>
      <span className="sr-only" role="status">{copyError ? '无法访问剪贴板，请重试或复制地址栏链接。' : copied ? '页面链接已复制' : ''}</span>
      <Accordions type="single" collapsible>
        <Accordion title="文档来源" value="source"><p>{identity.revision.slice(0, 12)} · {identity.dirty ? '包含本地修改' : '干净提交'} · 内容 {identity.sourceId.slice(0, 12)}</p></Accordion>
      </Accordions>
    </footer>;
}

export function DocumentPath({ path, githubHref }: { path: string; githubHref: string }) {
  const [copyError, setCopyError] = useState(false);
  const [copied, copy] = useCopyButton(async () => {
    setCopyError(false);
    try { await navigator.clipboard.writeText(path); }
    catch { setCopyError(true); }
  });
  return <div className="document-path" aria-label="文档来源文件">
    <div className="document-path-value">
      <span className="document-path-label">来源文件</span>
      <code title={path}>{path}</code>
    </div>
    <div className="document-path-actions">
      <a className={buttonVariants({ variant: 'ghost', size: 'sm' })} href={githubHref} target="_blank" rel="noreferrer">
        <ExternalLink aria-hidden="true" size={15}/><span>在 GitHub 查看</span>
      </a>
      <button type="button" className={buttonVariants({ variant: 'ghost', size: 'sm' })} onClick={copy} aria-label="复制文档路径" title="复制文档路径">
        {copied ? <Check aria-hidden="true" size={15}/> : <Copy aria-hidden="true" size={15}/>}<span>{copyError ? '复制失败，请重试' : copied ? '已复制' : '复制路径'}</span>
      </button>
    </div>
    <span className="sr-only" role="status">{copyError ? '无法访问剪贴板，请重试。' : copied ? '文档路径已复制' : ''}</span>
  </div>;
}

export function Page({ children, ...props }: ComponentProps<typeof DocsPage>) {
  return <DocsPage {...props} footer={{ enabled: false }}>
    {data.contentDiagnostics.length > 0 && <Callout type="error" title="正文来源诊断" role="alert">{data.contentDiagnostics.map((d, i) => <p key={i}>{d.path}：{d.message}</p>)}</Callout>}
    {children}
    <DocumentActions identity={data.identity}/>
  </DocsPage>;
}
