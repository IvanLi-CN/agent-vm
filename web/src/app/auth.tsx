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
  const { t } = useTranslation();
  const auth = useManagementAuth();
  const content = <div className={`auth-content ${auth.status === "suspended" ? "auth-preserved-content" : ""}`} aria-hidden={auth.status === "suspended" ? "true" : undefined}>{children}</div>;
  if (!auth.managed || auth.status === "authenticated") return content;

  if (auth.status === "suspended") {
    return <>
      {content}
      <AuthGate title={t("shell.sessionExpired")} description={t("shell.sessionExpiredText")} loginLabel={t("shell.signInAgain")} loginHref={auth.loginHref} />
    </>;
  }
  if (auth.status === "loading") return <AuthGate title={t("shell.sessionPending")} description={t("shell.sessionPendingText")} />;
  if (auth.status === "error") return <AuthGate title={t("shell.authUnavailable")} description={t("shell.authUnavailableText")} retry={auth.retry} />;
  return <AuthGate
    title={auth.authFailure ? t("shell.authFailed") : t("shell.signedOut")}
    description={auth.authFailure ? t("shell.authFailedText") : t("shell.signInText")}
    loginLabel={t("shell.signIn")}
    loginHref={auth.loginHref}
  />;
}

function AuthGate({ title, description, loginLabel, loginHref, retry }: { title: string; description: string; loginLabel?: string; loginHref?: string; retry?: () => void }) {
  const { t } = useTranslation();
  return <Panel className="auth-gate" role="status" aria-live="polite">
    <div className="auth-gate-icon" aria-hidden="true"><ShieldAlert /></div>
    <div className="auth-gate-copy"><h1>{title}</h1><p>{description}</p></div>
    <div className="auth-gate-actions">
      {loginHref && loginLabel ? <a className="ui-button ui-button-primary auth-login-link" href={loginHref}><LogIn aria-hidden="true" /><span>{loginLabel}</span></a> : null}
      {retry ? <Button variant="ghost" onClick={retry}>{t("common.retry")}</Button> : null}
    </div>
  </Panel>;
}
