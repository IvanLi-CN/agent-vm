import { type ComponentProps, useState } from 'react';
import { DocsPage } from 'fumadocs-ui/layouts/docs/page';
import { Accordion, Accordions } from 'fumadocs-ui/components/accordion';
import { Callout } from 'fumadocs-ui/components/callout';
import { buttonVariants } from 'fumadocs-ui/components/ui/button';
import { useCopyButton } from 'fumadocs-ui/utils/use-copy-button';
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

export function Page({ children, ...props }: ComponentProps<typeof DocsPage>) {
  return <DocsPage {...props} footer={{ enabled: false }}>
    {data.contentDiagnostics.length > 0 && <Callout type="error" title="正文来源诊断" role="alert">{data.contentDiagnostics.map((d, i) => <p key={i}>{d.path}：{d.message}</p>)}</Callout>}
    {children}
    <DocumentActions identity={data.identity}/>
  </DocsPage>;
}
