import { useQuery, useQueryClient } from "@tanstack/react-query";
import { LogIn, ShieldAlert } from "lucide-react";
import { createContext, useContext, useEffect, useMemo, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import {
  getManagementLoginHref,
  getManagementSession,
  isManagementApiEnabled,
  isManagementAuthFailure,
  subscribeManagementAuthFailures,
  type ManagementRequestError,
} from "./api";
import type { ManagementSession } from "./types";
import { Button, Panel } from "../components/ui";
import { changeLocale } from "./i18n";

export type ManagementAuthStatus = "loading" | "unauthenticated" | "authenticated" | "suspended" | "error";

export type ManagementAuthContextValue = {
  managed: boolean;
  status: ManagementAuthStatus;
  session: ManagementSession | null;
  errorCode: string | null;
  authFailure: boolean;
  isAuthenticated: boolean;
  loginHref: string;
  retry: () => void;
};

const defaultAuth: ManagementAuthContextValue = {
  managed: false,
  status: "authenticated",
  session: null,
  errorCode: null,
  authFailure: false,
  isAuthenticated: true,
  loginHref: "/api/v1/auth/oidc/start?return_to=%2F",
  retry: () => undefined,
};

const ManagementAuthContext = createContext<ManagementAuthContextValue>(defaultAuth);

export function consumeAuthFailureFlag(): boolean {
  if (typeof window === "undefined") return false;
  const url = new URL(window.location.href);
  if (url.searchParams.get("auth") !== "failed") return false;
  url.searchParams.delete("auth");
  const query = url.searchParams.toString();
  window.history.replaceState({}, "", `${url.pathname}${query ? `?${query}` : ""}${url.hash}`);
  return true;
}

export function ManagementAuthProvider({ children, enabled = isManagementApiEnabled() }: { children: ReactNode; enabled?: boolean }) {
  const client = useQueryClient();
  const [suspended, setSuspended] = useState(false);
  const [authFailure, setAuthFailure] = useState(() => consumeAuthFailureFlag());
  const sessionQuery = useQuery<ManagementSession>({
    queryKey: ["management-session"],
    queryFn: getManagementSession,
    enabled,
    retry: false,
    staleTime: 30_000,
  });

  useEffect(() => {
    if (!enabled) return;
    return subscribeManagementAuthFailures((_error: ManagementRequestError) => {
      setSuspended(Boolean(sessionQuery.data));
      void client.cancelQueries({ predicate: (query) => query.queryKey[0] !== "management-session" });
    });
  }, [client, enabled, sessionQuery.data]);

  useEffect(() => {
    if (sessionQuery.data) {
      setSuspended(false);
      setAuthFailure(false);
    }
  }, [sessionQuery.data]);

  const value = useMemo<ManagementAuthContextValue>(() => {
    if (!enabled) return defaultAuth;
    const error = sessionQuery.error;
    const errorCode = error instanceof Error && "code" in error && typeof error.code === "string" ? error.code : null;
    const status: ManagementAuthStatus = suspended
      ? "suspended"
      : sessionQuery.isPending
        ? "loading"
        : sessionQuery.data
          ? "authenticated"
          : error && isManagementAuthFailure(error)
            ? "unauthenticated"
            : error
              ? "error"
              : "unauthenticated";
    return {
      managed: true,
      status,
      session: sessionQuery.data ?? null,
      errorCode,
      authFailure,
      isAuthenticated: status === "authenticated",
      loginHref: getManagementLoginHref(),
      retry: () => void sessionQuery.refetch(),
    };
  }, [authFailure, enabled, sessionQuery.data, sessionQuery.error, sessionQuery.isPending, sessionQuery.refetch, suspended]);

  return <ManagementAuthContext.Provider value={value}>{children}</ManagementAuthContext.Provider>;
}

export function useManagementAuth(): ManagementAuthContextValue {
  return useContext(ManagementAuthContext);
}

export function ManagementAuthBoundary({ children }: { children: ReactNode }) {
  const auth = useManagementAuth();
  if (!auth.managed || auth.status === "authenticated") return <>{children}</>;

  if (auth.status === "suspended") {
    return <AuthErrorLayout titleKey="shell.sessionExpired" descriptionKey="shell.sessionExpiredText" loginLabelKey="shell.signInAgain" loginHref={auth.loginHref} />;
  }
  if (auth.status === "loading") return <AuthErrorLayout titleKey="shell.sessionPending" descriptionKey="shell.sessionPendingText" />;
  if (auth.status === "error") return <AuthErrorLayout titleKey="shell.authUnavailable" descriptionKey="shell.authUnavailableText" retry={auth.retry} />;
  return <AuthErrorLayout
    titleKey={auth.authFailure ? "shell.authFailed" : "shell.signedOut"}
    descriptionKey={auth.authFailure ? "shell.authFailedText" : "shell.signInText"}
    loginLabelKey="shell.signInOidc"
    loginHref={auth.loginHref}
  />;
}

function AuthErrorLayout({ titleKey, descriptionKey, loginLabelKey, loginHref, retry }: { titleKey: string; descriptionKey: string; loginLabelKey?: string; loginHref?: string; retry?: () => void }) {
  const { t, i18n } = useTranslation();
  const locale = i18n.language === "zh-CN" ? "zh-CN" : "en";
  const title = t(titleKey);
  return <div className="auth-page" data-testid="auth-error-layout">
    <header className="auth-page-header">
      <div className="auth-page-brand"><div className="auth-page-brand-mark" aria-hidden="true">A</div><div><strong>{t("product")}</strong><span>{t("productSub")}</span></div></div>
      <div className="auth-page-header-actions">
        {loginHref && loginLabelKey ? <a className="ui-button ui-button-primary auth-login-link" href={loginHref}><LogIn aria-hidden="true" /><span>{t(loginLabelKey)}</span></a> : null}
        {retry ? <Button variant="ghost" onClick={retry}>{t("common.retry")}</Button> : null}
        <div className="auth-page-locale" role="group" aria-label={t("shell.language")}>
          <button className={locale === "zh-CN" ? "selected" : ""} onClick={() => void changeLocale("zh-CN")}>中</button>
          <button className={locale === "en" ? "selected" : ""} onClick={() => void changeLocale("en")}>EN</button>
        </div>
      </div>
    </header>
    <main className="auth-page-main">
      <Panel className="auth-page-card" role="status" aria-live="polite">
        <div className="auth-page-icon" aria-hidden="true"><ShieldAlert /></div>
        <div className="auth-page-copy"><h1>{title}</h1><p>{t(descriptionKey)}</p></div>
      </Panel>
    </main>
    <footer className="auth-page-footer">{t("shell.oidcOnly")}</footer>
  </div>;
}
