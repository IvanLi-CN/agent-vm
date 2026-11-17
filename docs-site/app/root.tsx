import { Links, Meta, Outlet, Scripts, ScrollRestoration, Link, NavLink, useLocation } from 'react-router';
import { RootProvider } from 'fumadocs-ui/provider/react-router';
import { useState } from 'react';
import data from './data';
import './style.css';

export const meta = () => [{ title: 'Agent VM 文档' }];

export function Layout({ children }: { children: React.ReactNode }) {
  return <html lang="zh-CN" suppressHydrationWarning><head><meta charSet="utf-8"/><meta name="viewport" content="width=device-width, initial-scale=1"/><Meta/><Links/></head><body><RootProvider search={{ enabled: false }}>{children}</RootProvider><ScrollRestoration/><Scripts/></body></html>;
}

function Materials() {
  const { pathname } = useLocation();
  const links = (section: string) => data.documents.filter(doc => doc.section === section).map(doc => <NavLink key={doc.slug} to={'/docs/' + doc.slug}>{doc.title}</NavLink>);
  return <nav aria-label="文档导航">
    <section className="nav-section"><h2>开始使用</h2>{links('开始使用')}</section>
    <section className="nav-section"><h2>开发文档</h2>{links('开发文档')}
      <details className="nav-topic" open={pathname.startsWith('/docs/docs-site-')}><summary>文档站</summary><div>{links('文档工程')}</div></details>
    </section>
  </nav>;
}

export default function App() {
  const [copied, setCopied] = useState(false);
  return <>
    <a className="skip" href="#main-content">跳到正文</a>
    <header className="site-header"><Link to="/" className="brand">Agent VM <small>文档</small></Link><nav className="site-nav" aria-label="主导航"><Link to="/docs/cli">使用指南</Link><Link to="/docs/architecture">开发文档</Link><button onClick={() => document.documentElement.classList.toggle('dark')}>切换明暗</button><button onClick={async () => { try { await navigator.clipboard.writeText(location.href); setCopied(true); setTimeout(() => setCopied(false), 2000); } catch { setCopied(false); } }}>{copied ? '已复制' : '复制页面链接'}</button></nav></header>
    <details className="mobile-materials"><summary>文档导航</summary><Materials/></details>
    <div className="site-shell"><aside className="site-sidebar"><Materials/></aside><main className="site-main" id="main-content">
      {data.contentDiagnostics.length > 0 && <section role="alert"><h2>正文来源诊断</h2>{data.contentDiagnostics.map((d, i) => <p key={i}>{d.path}：{d.message}</p>)}</section>}
      <Outlet/>
      <footer className="site-footer"><details><summary>文档来源</summary><p>{data.identity.revision.slice(0, 12)} · {data.identity.dirty ? '包含本地修改' : '干净提交'} · 内容 {data.identity.sourceId.slice(0, 12)}</p></details></footer>
    </main></div>
  </>;
}
