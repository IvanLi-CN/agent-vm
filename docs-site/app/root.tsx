import { Links, Meta, Outlet, Scripts, ScrollRestoration } from 'react-router';
import { RootProvider } from 'fumadocs-ui/provider/react-router';
import { DocsLayout } from 'fumadocs-ui/layouts/docs';
import { ThemeSwitch, type ThemeSwitchProps } from 'fumadocs-ui/layouts/shared/slots/theme-switch';
import data from './data';
import { pageTree } from './page-tree';
import './style.css';

export const meta = () => [{ title: 'Agent VM 文档' }];
const translations = {
  'Open Sidebar(sidebar)(aria-label)': '打开文档导航',
  'Close Sidebar(sidebar)(aria-label)': '关闭文档导航',
  'Close Sidebar(aria-label)': '关闭文档导航',
  'Toggle Theme(theme switcher)(aria-label)': '切换明暗',
  'Light(theme switcher)(aria-label)': '浅色',
  'Dark(theme switcher)(aria-label)': '深色',
  'System(theme switcher)(aria-label)': '跟随系统',
  'Skip to Content(skip link)': '跳到正文',
  'On this page(table of contents)': '本页目录',
  'Copy Text(code block)(aria-label)': '复制代码',
  'Copied Text(code block)(aria-label)': '代码已复制',
};

export function Layout({ children }: { children: React.ReactNode }) {
  return <html lang="zh-CN" suppressHydrationWarning><head><meta charSet="utf-8"/><meta name="viewport" content="width=device-width, initial-scale=1"/><Meta/><Links/></head><body><RootProvider search={{ enabled: false }} i18n={{ locale: 'zh-CN', translations }}>{children}</RootProvider><ScrollRestoration/><Scripts/></body></html>;
}

export default function App() {
  return <DocsLayout tree={pageTree} githubUrl="https://github.com/IvanLi-CN/agent-vm" slots={{ themeSwitch: VersionThemeSwitch }} nav={{ title: 'Agent VM 文档', url: '/' }} tabs={false} sidebar={{ defaultOpenLevel: 1, collapsible: false }}><Outlet/></DocsLayout>;
}

function VersionThemeSwitch(props: ThemeSwitchProps) {
  return <div className="docs-sidebar-tools">
    <span className="docs-version" aria-label={`文档版本 ${data.identity.version.label}`} title={`文档构建：${data.identity.version.description}`}>{data.identity.version.label}</span>
    <ThemeSwitch {...props} mode="light-dark-system"/>
  </div>;
}
