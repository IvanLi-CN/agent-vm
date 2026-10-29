import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { AlertTriangle, KeyRound, ShieldCheck, UserRound } from "lucide-react";
import { FormEvent, useState } from "react";
import { useTranslation } from "react-i18next";
import { ManagementRequestError, createAgentToken, createOperationId, getManagementSession, listTokens, previewTokenRevocation, submitTokenRevocation } from "../app/api";
import { formatDateTime } from "../app/format";
import type { ActionPreview, AgentToken, Locale, ManagementSession, TokenCreateResult, TokenRevokePreview, TokenScope } from "../app/types";
import { ActionDialog } from "./ActionDialog";
import { EmptyState, Button, CopyButton, Input, Panel, PanelHeader, StatusBadge, Textarea } from "./ui";

const scopeOptions: Array<{ scope: TokenScope; label: string }> = [
  { scope: "agent:read", label: "tokens.scopeRead" },
  { scope: "agent:operate", label: "tokens.scopeOperate" },
  { scope: "agent:sync", label: "tokens.scopeSync" },
  { scope: "agent:observe_admin", label: "tokens.scopeObserve" },
];

function defaultExpiry(): string {
  const value = new Date(Date.now() + 30 * 24 * 60 * 60 * 1000);
  value.setSeconds(0, 0);
  return value.toISOString().slice(0, 16);
}

function tokenState(token: AgentToken): "active" | "expired" | "revoked" {
  if (token.revoked_at) return "revoked";
  return Date.parse(token.expires_at) <= Date.now() ? "expired" : "active";
}

function actionPreview(preview: TokenRevokePreview): ActionPreview {
  return {
    target_version: preview.target_version,
    confirmation_token: preview.confirmation_token,
    expires_at: preview.expires_at,
    impact: {
      summary_code: "token_revoke",
      affected_session_count: 0,
      affected_image_references: { default: 0, active: 0, cached: 0, releasing: 0, pending_creation: 0, verification: 0 },
      changes: [{ code: "token_revoked", count: null, value: preview.token_id }],
    },
  };
}

function errorMessage(error: unknown, t: (key: string, options?: { defaultValue?: string }) => string): string {
  if (error instanceof ManagementRequestError) return t(`errors.${error.code}`, { defaultValue: t("errors.generic") });
  return t("errors.generic");
}

export function TokenManagement() {
  const { t, i18n } = useTranslation();
  const locale: Locale = i18n.language === "zh-CN" ? "zh-CN" : "en";
  const client = useQueryClient();
  const [scopes, setScopes] = useState<TokenScope[]>(["agent:read"]);
  const [expiresAt, setExpiresAt] = useState(defaultExpiry);
  const [reason, setReason] = useState("");
  const [createError, setCreateError] = useState<string | null>(null);
  const [createOperationIdValue, setCreateOperationIdValue] = useState<string | null>(null);
  const [plaintextToken, setPlaintextToken] = useState<string | null>(null);
  const [plaintextMetadata, setPlaintextMetadata] = useState<AgentToken | null>(null);
  const [plaintextConsumed, setPlaintextConsumed] = useState(false);
  const [revokeToken, setRevokeToken] = useState<AgentToken | null>(null);
  const [revokePreview, setRevokePreview] = useState<TokenRevokePreview | null>(null);
  const [revokeOperationId, setRevokeOperationId] = useState<string | null>(null);
  const [revokeError, setRevokeError] = useState<string | null>(null);

  const sessionQuery = useQuery<ManagementSession>({ queryKey: ["management-session"], queryFn: getManagementSession, staleTime: 30_000 });
  const tokensQuery = useQuery({ queryKey: ["tokens"], queryFn: () => listTokens(), refetchInterval: 10_000, refetchIntervalInBackground: false });

  const createMutation = useMutation({
    mutationFn: ({ auditReason, operationId }: { auditReason: string; operationId: string }) => createAgentToken(scopes, new Date(expiresAt).toISOString(), auditReason, operationId),
    onSuccess: (result: TokenCreateResult) => {
      setCreateError(null);
      setReason("");
      setCreateOperationIdValue(null);
      setPlaintextMetadata(result.metadata);
      if ("token" in result) {
        setPlaintextToken(result.token);
        setPlaintextConsumed(false);
      } else {
        setPlaintextToken(null);
        setPlaintextConsumed(true);
      }
      void client.invalidateQueries({ queryKey: ["tokens"] });
      void client.invalidateQueries({ queryKey: ["operations"] });
    },
    onError: (error) => setCreateError(errorMessage(error, t)),
  });

  const revokeMutation = useMutation({
    mutationFn: ({ auditReason, operationId }: { auditReason: string; operationId: string }) => submitTokenRevocation(revokeToken?.token_id ?? "", auditReason, revokePreview!, operationId),
    onSuccess: () => {
      setRevokeToken(null);
      setRevokePreview(null);
      setRevokeOperationId(null);
      setRevokeError(null);
      void client.invalidateQueries({ queryKey: ["tokens"] });
      void client.invalidateQueries({ queryKey: ["operations"] });
    },
    onError: (error) => setRevokeError(errorMessage(error, t)),
  });

  const toggleScope = (scope: TokenScope) => {
    setScopes((current) => {
      if (current.includes(scope)) {
        if (scope === "agent:read" && current.includes("agent:operate")) return current;
        return current.filter((value) => value !== scope);
      }
      return scope === "agent:operate" && !current.includes("agent:read") ? [...current, "agent:read", scope] : [...current, scope];
    });
  };

  const submitCreate = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    setCreateError(null);
    const expiry = Date.parse(expiresAt);
    if (!scopes.length) {
      setCreateError(t("tokens.scopeRequired"));
      return;
    }
    if (!Number.isFinite(expiry) || expiry <= Date.now()) {
      setCreateError(t("tokens.expiryRequired"));
      return;
    }
    const operationId = createOperationId();
    setCreateOperationIdValue(operationId);
    createMutation.mutate({ auditReason: reason.trim(), operationId });
  };

  const openRevoke = async (token: AgentToken) => {
    setRevokeError(null);
    setRevokeToken(token);
    setRevokeOperationId(createOperationId());
    try {
      setRevokePreview(await previewTokenRevocation(token.token_id, token.resource_version));
    } catch (error) {
      setRevokeToken(null);
      setRevokePreview(null);
      setRevokeError(errorMessage(error, t));
    }
  };

  const session = sessionQuery.data;
  const tokens = tokensQuery.data?.items ?? [];
  return <Panel className="token-panel" data-testid="token-management">
    <PanelHeader title={t("tokens.title")} description={t("tokens.subtitle")} action={<StatusBadge state={session ? "active" : "unknown"} />} />
    {session ? <SessionSummary session={session} locale={locale} /> : sessionQuery.isPending ? <div className="token-session-summary token-session-loading"><span /><span /><span /></div> : null}
    <div className="token-create-section">
      <div className="section-header token-section-header"><div><h3>{t("tokens.create")}</h3><p>{t("tokens.createHint")}</p></div></div>
      <form className="token-create-form" onSubmit={submitCreate}>
        <fieldset className="token-scope-fieldset">
          <legend>{t("tokens.scopes")}</legend>
          <div className="token-scope-grid">
            {scopeOptions.map(({ scope, label }) => <label className="scope-option" key={scope}>
              <input type="checkbox" checked={scopes.includes(scope)} disabled={scope === "agent:read" && scopes.includes("agent:operate")} onChange={() => toggleScope(scope)} />
              <span><strong className="mono">{scope}</strong><small>{t(label)}</small></span>
            </label>)}
          </div>
          {scopes.includes("agent:operate") ? <p className="field-hint">{t("tokens.operateNeedsRead")}</p> : null}
        </fieldset>
        <div className="token-form-fields">
          <label className="field-label"><span>{t("tokens.expiresAt")}</span><Input type="datetime-local" value={expiresAt} onChange={(event) => setExpiresAt(event.target.value)} required /></label>
          <label className="field-label"><span>{t("tokens.reason")} <em>({t("common.optional")})</em></span><Textarea value={reason} onChange={(event) => setReason(event.target.value)} rows={2} placeholder={t("common.reasonPlaceholder")} /></label>
        </div>
        {createError ? <p className="field-error" role="alert">{createError}</p> : null}
        <div className="token-form-actions"><Button type="submit" variant="primary" disabled={createMutation.isPending || Boolean(createOperationIdValue)} icon={<KeyRound />}>{createMutation.isPending ? t("common.loading") : t("tokens.create")}</Button></div>
      </form>
    </div>
    {plaintextMetadata ? <PlaintextDisclosure token={plaintextMetadata} plaintext={plaintextToken} consumed={plaintextConsumed} onDismiss={() => { setPlaintextToken(null); setPlaintextConsumed(true); }} /> : null}
    <div className="token-list-section">
      <div className="section-header token-section-header"><div><h3>{t("tokens.listTitle")}</h3><p>{t("tokens.count", { count: tokens.length })}</p></div></div>
      {tokensQuery.isPending ? <div className="loading-rows token-loading"><span /><span /><span /></div> : tokens.length ? <div className="token-list" role="list">{tokens.map((token) => <TokenRow key={token.token_id} token={token} locale={locale} onRevoke={() => void openRevoke(token)} />)}</div> : <EmptyState title={t("tokens.noTokens")} />}
      {tokensQuery.isError ? <p className="field-error" role="alert">{t("errors.generic")}</p> : null}
    </div>
    {revokeToken && revokePreview ? <ActionDialog preview={actionPreview(revokePreview)} actionLabel={t("tokens.revoke")} target={`${t("tokens.tokenId")} · ${revokeToken.token_id}`} dangerous busy={revokeMutation.isPending} error={revokeError} onCancel={() => { setRevokeToken(null); setRevokePreview(null); setRevokeOperationId(null); setRevokeError(null); }} onConfirm={(auditReason) => revokeMutation.mutate({ auditReason, operationId: revokeOperationId ?? createOperationId() })}><p className="dialog-warning token-revoke-warning">{t("tokens.revokeConfirm")}</p></ActionDialog> : null}
  </Panel>;
}

function SessionSummary({ session, locale }: { session: ManagementSession; locale: Locale }) {
  const { t } = useTranslation();
  return <div className="token-session-summary">
    <div className="token-session-heading"><div className="token-session-icon" aria-hidden="true"><UserRound /></div><div><h3>{t("tokens.adminSession")}</h3><p className="mono">{session.subject}</p></div><StatusBadge state="active" /></div>
    <dl className="token-session-grid"><div><dt>{t("tokens.principal")}</dt><dd className="mono">{session.subject}</dd></div><div><dt>{t("tokens.issuer")}</dt><dd className="mono">{session.issuer}</dd></div><div><dt>{t("tokens.role")}</dt><dd>{session.role}</dd></div><div><dt>{t("tokens.expires")}</dt><dd>{formatDateTime(session.expires_at, locale)}</dd></div><div className="token-capabilities"><dt>{t("tokens.capabilities")}</dt><dd>{session.capabilities.map((capability) => <span className="scope-tag" key={capability}>{capability}</span>)}</dd></div></dl>
  </div>;
}

function TokenRow({ token, locale, onRevoke }: { token: AgentToken; locale: Locale; onRevoke: () => void }) {
  const { t } = useTranslation();
  const state = tokenState(token);
  return <article className="token-row" role="listitem">
    <div className="token-row-main"><div className="token-row-title"><strong className="mono">{token.token_id}</strong><StatusBadge state={state} /></div><div className="token-scopes">{token.scopes.map((scope) => <span className="scope-tag" key={scope}>{scope}</span>)}</div></div>
    <dl className="token-row-meta"><div><dt>{t("tokens.created")}</dt><dd>{formatDateTime(token.created_at, locale)}</dd></div><div><dt>{token.revoked_at ? t("tokens.revokedAt") : t("tokens.expires")}</dt><dd>{formatDateTime(token.revoked_at ?? token.expires_at, locale)}</dd></div></dl>
    <Button variant="danger" disabled={state !== "active"} onClick={onRevoke} icon={<ShieldCheck />}>{t("tokens.revoke")}</Button>
  </article>;
}

function PlaintextDisclosure({ token, plaintext, consumed, onDismiss }: { token: AgentToken; plaintext: string | null; consumed: boolean; onDismiss: () => void }) {
  const { t } = useTranslation();
  return <div className="token-disclosure" role="status" data-testid="token-disclosure"><div className="token-disclosure-heading"><AlertTriangle aria-hidden="true" /><div><strong>{t("tokens.plaintextTitle")}</strong><p>{consumed ? t("tokens.plaintextConsumed") : t("tokens.plaintextWarning")}</p></div></div>{plaintext ? <div className="token-value"><code data-testid="token-plaintext">{plaintext}</code><CopyButton value={plaintext} /></div> : null}<div className="token-disclosure-meta"><span className="mono">{token.token_id}</span><Button variant="ghost" onClick={onDismiss}>{t("tokens.dismissPlaintext")}</Button></div></div>;
}
