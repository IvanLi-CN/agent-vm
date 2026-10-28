import { RefreshCw } from "lucide-react";
import { useTranslation } from "react-i18next";
import { formatDateTime } from "../app/format";
import type { Locale } from "../app/types";
import { Button, LoadingRows, Panel, StatusBadge } from "./ui";

export function PageIntro({ title, description, observedAt, onRefresh, refreshing }: { title: string; description: string; observedAt?: string; onRefresh?: () => void; refreshing?: boolean }) {
  const { t, i18n } = useTranslation();
  const locale = i18n.language === "zh-CN" ? "zh-CN" : "en";
  return <div className="page-intro"><div><h1>{title}</h1><p>{description}</p></div>{observedAt || onRefresh ? <div className="intro-meta">{observedAt ? <span>{t("shell.lastUpdated", { time: formatDateTime(observedAt, locale) })}</span> : null}{onRefresh ? <Button variant="ghost" icon={<RefreshCw className={refreshing ? "spin-icon" : ""} />} onClick={onRefresh} disabled={refreshing}>{t("shell.refresh")}</Button> : null}</div> : null}</div>;
}

export function QueryError({ onRetry }: { onRetry: () => void }) {
  const { t } = useTranslation();
  return <Panel className="query-error"><strong>{t("errors.generic")}</strong><Button variant="ghost" onClick={onRetry}>{t("common.retry")}</Button></Panel>;
}

export function LoadingPanel() {
  return <Panel><LoadingRows rows={4} /></Panel>;
}

export function FreshnessLine({ state, code, observedAt }: { state: string; code?: string | null; observedAt?: string | null }) {
  const { t, i18n } = useTranslation();
  const locale: Locale = i18n.language === "zh-CN" ? "zh-CN" : "en";
  return <div className="freshness-line"><StatusBadge state={state} />{code ? <span className="mono">{code}</span> : null}{observedAt ? <span>{formatDateTime(observedAt, locale)}</span> : null}</div>;
}

export function MonoValue({ children, copy = false }: { children: string; copy?: boolean }) {
  return <span className={`mono ${copy ? "mono-copy" : ""}`}>{children}</span>;
}

export function DefinitionList({ items }: { items: Array<{ label: string; value: React.ReactNode; className?: string }> }) {
  return <dl className="definition-list">{items.map((item) => <div key={item.label} className={item.className}><dt>{item.label}</dt><dd>{item.value}</dd></div>)}</dl>;
}
