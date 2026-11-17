import { Links, Meta, Outlet, Scripts, ScrollRestoration } from 'react-router';
import { RootProvider } from 'fumadocs-ui/provider/react-router';
import { DocsLayout } from 'fumadocs-ui/layouts/docs';
import { useState } from 'react';
import { pageTree } from './page-tree';
import data from './data';
import './style.css';

export const meta = () => [{ title: 'Agent VM 文档' }];
const translations = {
  'Open Sidebar(sidebar)(aria-label)': '打开文档导航',
  'Close Sidebar(sidebar)(aria-label)': '关闭文档导航',
  'Close Sidebar(aria-label)': '关闭文档导航',
  'Toggle Theme(theme switcher)(aria-label)': '切换明暗',
  'Skip to Content(skip link)': '跳到正文',
};

export function Layout({ children }: { children: React.ReactNode }) {
  return <html lang="zh-CN" suppressHydrationWarning><head><meta charSet="utf-8"/><meta name="viewport" content="width=device-width, initial-scale=1"/><Meta/><Links/></head><body><RootProvider search={{ enabled: false }} i18n={{ locale: 'zh-CN', translations }}>{children}</RootProvider><ScrollRestoration/><Scripts/></body></html>;
}

export default function App() {
  const [copied, setCopied] = useState(false);
  return <DocsLayout tree={pageTree} nav={{ title: 'Agent VM 文档', url: '/' }} tabs={false} sidebar={{ defaultOpenLevel: 1, collapsible: false }}>
    <main className="site-main" id="nd-page">
      {data.contentDiagnostics.length > 0 && <section role="alert"><h2>正文来源诊断</h2>{data.contentDiagnostics.map((d, i) => <p key={i}>{d.path}：{d.message}</p>)}</section>}
      <Outlet/>
      <footer className="site-footer">
        <button className="copy-page-link" onClick={async () => { try { await navigator.clipboard.writeText(location.href); setCopied(true); setTimeout(() => setCopied(false), 2000); } catch { setCopied(false); } }}>{copied ? '已复制' : '复制页面链接'}</button>
        <details><summary>文档来源</summary><p>{data.identity.revision.slice(0, 12)} · {data.identity.dirty ? '包含本地修改' : '干净提交'} · 内容 {data.identity.sourceId.slice(0, 12)}</p></details>
      </footer>
    </main>
  </DocsLayout>;
}
