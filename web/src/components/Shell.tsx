import { Activity, Boxes, Cpu, Gauge, Languages, Menu, Settings2, X } from "lucide-react";
import type { ReactNode } from "react";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { changeLocale } from "../app/i18n";
import type { Locale, PageKey } from "../app/types";
import { Button, IconButton } from "./ui";

const navItems: Array<{ key: PageKey; icon: typeof Gauge }> = [
  { key: "overview", icon: Gauge },
  { key: "vms", icon: Cpu },
  { key: "images", icon: Boxes },
  { key: "operations", icon: Activity },
  { key: "system", icon: Settings2 },
];

export function Shell({ page, onNavigate, children }: { page: PageKey; onNavigate: (next: PageKey) => void; children: ReactNode }) {
  const { t, i18n } = useTranslation();
  const [menuOpen, setMenuOpen] = useState(false);
  const locale = i18n.language === "zh-CN" ? "zh-CN" : "en";
  useEffect(() => { setMenuOpen(false); }, [page]);

  const navigate = (next: PageKey) => {
    onNavigate(next);
    window.history.replaceState({}, "", next === "overview" ? "/" : `/${next}`);
  };

  return <div className="app-frame">
    <aside className="sidebar" aria-label={t("product")}>
      <div className="brand-lockup"><div className="brand-mark" aria-hidden="true">A</div><div><strong>{t("product")}</strong><span>{t("productSub")}</span></div></div>
      <div className="nav-heading">{locale === "zh-CN" ? "工作区" : "Workspace"}</div>
      <nav className="primary-nav">
        {navItems.map(({ key, icon: Icon }) => <button className={`nav-item ${page === key ? "is-active" : ""}`} key={key} onClick={() => navigate(key)} aria-current={page === key ? "page" : undefined}><Icon aria-hidden="true" /><span>{t(`nav.${key}`)}</span></button>)}
      </nav>
      <div className="sidebar-footer"><span className="operator-dot" aria-hidden="true" /><div><strong>{t("shell.operator")}</strong><span>{t("shell.administrator")}</span></div></div>
    </aside>
    <div className="workspace">
      <header className="topbar"><div className="mobile-brand"><div className="brand-mark" aria-hidden="true">A</div><strong>{t("product")}</strong></div><IconButton label={t("shell.openMenu")} className="mobile-menu-button" onClick={() => setMenuOpen(true)}><Menu /></IconButton><div className="topbar-spacer" /><span className="synthetic-label">{t("shell.synthetic")}</span><div className="language-toggle" role="group" aria-label={t("shell.language")}><button className={locale === "zh-CN" ? "selected" : ""} onClick={() => void changeLocale("zh-CN")}>中</button><button className={locale === "en" ? "selected" : ""} onClick={() => void changeLocale("en")}>EN</button></div></header>
      <main className="work-surface">{children}</main>
    </div>
    {menuOpen ? <div className="mobile-menu-layer"><button type="button" className="mobile-menu-backdrop" aria-label={t("shell.closeMenu")} onClick={() => setMenuOpen(false)} /><dialog open className="mobile-menu" aria-labelledby="mobile-menu-title"><div className="mobile-menu-header"><strong id="mobile-menu-title">{t("product")}</strong><IconButton label={t("shell.closeMenu")} onClick={() => setMenuOpen(false)}><X /></IconButton></div>{navItems.map(({ key, icon: Icon }) => <button className={`nav-item ${page === key ? "is-active" : ""}`} key={key} onClick={() => navigate(key)}><Icon aria-hidden="true" /><span>{t(`nav.${key}`)}</span></button>)}<div className="mobile-menu-footer"><Button variant="ghost" icon={<Languages />} onClick={() => void changeLocale(locale === "zh-CN" ? "en" : "zh-CN")}>{locale === "zh-CN" ? "English" : "简体中文"}</Button></div></dialog></div> : null}
  </div>;
}
