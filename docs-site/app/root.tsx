import { Links, Meta, Outlet, Scripts, ScrollRestoration, Link } from 'react-router';
import { RootProvider } from 'fumadocs-ui/provider/react-router';
import { useState } from 'react';
import data from './data';
import './style.css';
export function Layout({ children }: { children: React.ReactNode }) {
  return <html lang="zh-CN" suppressHydrationWarning><head><meta charSet="utf-8"/><meta name="viewport" content="width=device-width, initial-scale=1"/><title>Agent VM · 设计文档</title><Meta/><Links/></head><body><RootProvider search={{ enabled: false }}>{children}</RootProvider><ScrollRestoration/><Scripts/></body></html>;
}
function Materials(){return <>{data.documents.map(d => <Link key={d.slug} to={'/docs/' + d.slug}>{d.text.match(/^# (.*)/m)?.[1] ?? d.path}</Link>)}</>}
export default function App() {
  const [copied,setCopied]=useState(false);
  return <>
    <a className="skip" href="#main-content">跳到正文</a>
    <header className="site-header"><Link to="/" className="brand">Agent VM <small>设计文档</small></Link><nav className="site-nav" aria-label="主导航"><Link to="/docs/0">阅读文档</Link><button onClick={() => document.documentElement.classList.toggle('dark')}>切换明暗</button><button onClick={async()=>{try{await navigator.clipboard.writeText(location.href);setCopied(true);setTimeout(()=>setCopied(false),2000);}catch{setCopied(false);}}}>{copied?'已复制':'复制页面链接'}</button></nav></header>
    <div className="source-identity">{data.identity.revision.slice(0, 12)} · {data.identity.dirty ? '包含本地修改' : '干净提交'} · 内容 {data.identity.sourceId.slice(0, 12)}</div>
    <details className="mobile-materials"><summary>本次设计 · {data.documents.length} 份来源材料</summary><Materials/></details>
    <div className="site-shell"><aside className="site-sidebar"><h2>本次设计</h2><Materials/></aside><main className="site-main" id="main-content">{data.contentDiagnostics.length>0&&<section role="alert"><h2>正文来源诊断</h2>{data.contentDiagnostics.map((d,i)=><p key={i}>{d.path}：{d.message}</p>)}</section>}<Outlet/></main></div>
  </>;
}
