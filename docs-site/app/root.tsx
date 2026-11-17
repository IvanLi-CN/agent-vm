import { Links, Meta, Outlet, Scripts, ScrollRestoration } from 'react-router';
import { RootProvider } from 'fumadocs-ui/provider/react-router';
import { DocsLayout } from 'fumadocs-ui/layouts/docs';
import { pageTree } from './page-tree';
import './style.css';

export const meta = () => [{ title: 'Agent VM 文档' }];
const translations = {
  'Open Sidebar(sidebar)(aria-label)': '打开文档导航',
  'Close Sidebar(sidebar)(aria-label)': '关闭文档导航',
  'Close Sidebar(aria-label)': '关闭文档导航',
  'Toggle Theme(theme switcher)(aria-label)': '切换明暗',
  'Skip to Content(skip link)': '跳到正文',
  'On this page(table of contents)': '本页目录',
  'Copy Text(code block)(aria-label)': '复制代码',
  'Copied Text(code block)(aria-label)': '代码已复制',
};

export function Layout({ children }: { children: React.ReactNode }) {
  return <html lang="zh-CN" suppressHydrationWarning><head><meta charSet="utf-8"/><meta name="viewport" content="width=device-width, initial-scale=1"/><Meta/><Links/></head><body><RootProvider search={{ enabled: false }} i18n={{ locale: 'zh-CN', translations }}>{children}</RootProvider><ScrollRestoration/><Scripts/></body></html>;
}

export default function App() {
  return <DocsLayout tree={pageTree} nav={{ title: 'Agent VM 文档', url: '/' }} tabs={false} sidebar={{ defaultOpenLevel: 1, collapsible: false }}><Outlet/></DocsLayout>;
}
