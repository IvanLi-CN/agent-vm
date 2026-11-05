import { Activity, CheckCircle2, CircleHelp, Clock3, LogIn, TriangleAlert } from "lucide-react";
import { useEffect } from "react";
import { useQuery } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { getManagementLoginHref, getPublicStatus } from "../app/api";
import { formatDateTime, formatNumber, stateLabel, stateTone } from "../app/format";
import { changeLocale } from "../app/i18n";
import type { PublicServiceState } from "../app/types";
import { Badge, Button, Panel } from "../components/ui";

type PublicHomePageProps = { authFailure: boolean; normalizePath: boolean };

export function PublicHomePage({ authFailure, normalizePath }: PublicHomePageProps) {
  const { t, i18n } = useTranslation();
  const locale = i18n.language === "zh-CN" ? "zh-CN" : "en";
  const query = useQuery({
    queryKey: ["public-status"],
    queryFn: getPublicStatus,
    staleTime: 30_000,
    refetchInterval: 30_000,
    refetchIntervalInBackground: false,
    retry: 1,
  });

  useEffect(() => {
    if (typeof document !== "undefined") document.title = t("public.documentTitle");
  }, [i18n.language, t]);

  useEffect(() => {
    if (!normalizePath || typeof window === "undefined" || window.location.pathname === "/") return;
    const url = new URL(window.location.href);
    url.pathname = "/";
    url.search = "";
    window.history.replaceState({}, "", `${url.pathname}${url.search}${url.hash}`);
    window.dispatchEvent(new PopStateEvent("popstate"));
  }, [normalizePath]);

  const loginHref = getManagementLoginHref("/");
  const status = query.data;
  const statusTone = status ? stateTone(status.state) : "neutral";
  const statusLabel = status ? publicStateLabel(status.state, t) : t("public.statusLoading");
  const statusIcon = status?.state === "available" ? <CheckCircle2 /> : status?.state === "degraded" ? <TriangleAlert /> : <CircleHelp />;
  const observationLabel = status ? stateLabel(status.observation_state, locale) : t("common.loading");

  return <div className="public-home" data-testid="public-home">
    <header className="public-home-header">
      <div className="public-home-brand"><div className="auth-page-brand-mark" aria-hidden="true">A</div><div><strong>{t("product")}</strong><span>{t("public.subtitle")}</span></div></div>
      <div className="public-home-header-actions">
        <a className="ui-button ui-button-primary public-login-link" href={loginHref}><LogIn aria-hidden="true" /><span>{t("public.signIn")}</span></a>
        <div className="auth-page-locale" role="group" aria-label={t("shell.language")}>
          <button type="button" className={locale === "zh-CN" ? "selected" : ""} aria-pressed={locale === "zh-CN"} onClick={() => void changeLocale("zh-CN")}>中</button>
          <button type="button" className={locale === "en" ? "selected" : ""} aria-pressed={locale === "en"} onClick={() => void changeLocale("en")}>EN</button>
        </div>
      </div>
    </header>
    <main className="public-home-main">
      <div className="public-home-content">
        <div className="public-home-intro">
          <div className="public-home-intro-icon" aria-hidden="true"><Activity /></div>
          <div><h1>{t("public.title")}</h1><p>{t("public.intro")}</p></div>
        </div>
        {authFailure ? <div className="public-home-notice" role="status" aria-live="polite"><TriangleAlert aria-hidden="true" /><div><strong>{t("public.authFailed")}</strong><p>{t("public.authFailedText")}</p></div></div> : null}
        <Panel className="public-status-panel" data-testid="public-status-panel">
          <div className="public-status-header"><div><h2>{t("public.statusTitle")}</h2><p>{t("public.statusHint")}</p></div>{status ? <Badge tone={statusTone}>{statusLabel}</Badge> : null}</div>
          {query.isPending ? <div className="public-status-loading" role="status" aria-live="polite"><span className="public-status-spinner" aria-hidden="true" />{t("public.statusLoading")}</div> : query.isError ? <div className="public-status-error" role="status" aria-live="polite"><div><strong>{t("public.statusUnavailable")}</strong><p>{t("public.statusUnavailableText")}</p></div><Button variant="secondary" onClick={() => void query.refetch()}>{t("public.retryStatus")}</Button></div> : status ? <div className="public-status-live" role="status" aria-live="polite" aria-atomic="true">
            <div className={`public-status-summary public-status-summary-${statusTone}`}><div className="public-status-icon" aria-hidden="true">{statusIcon}</div><div><strong>{statusLabel}</strong><p>{t("public.statusSummary")}</p></div></div>
            <div className="public-status-details">
              <div><span>{t("public.activeSessions")}</span><strong>{formatNumber(status.active_sessions, locale, 0)}</strong></div>
              <div><span><Clock3 aria-hidden="true" />{t("public.observedAt")}</span><strong>{formatDateTime(status.observed_at, locale)}</strong></div>
              <div><span>{t("public.observation")}</span><strong>{observationLabel}</strong><small>{t("public.staleWindow", { seconds: status.stale_after_seconds })}</small></div>
            </div>
          </div> : null}
        </Panel>
        <p className="public-home-footnote">{t("public.statusDelayed")}</p>
      </div>
    </main>
    <footer className="auth-page-footer">{t("shell.oidcOnly")}</footer>
  </div>;
}

function publicStateLabel(state: PublicServiceState, translate: (key: string) => string): string {
  if (state === "available") return translate("public.statusAvailable");
  if (state === "degraded") return translate("public.statusDegraded");
  return translate("public.statusUnknown");
}
