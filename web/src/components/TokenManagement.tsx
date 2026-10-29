import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { AlertTriangle, ChevronLeft, ChevronRight, KeyRound, ShieldCheck, UserRound } from "lucide-react";
import { FormEvent, useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { ManagementRequestError, createAgentToken, createOperationId, getManagementSession, getOperation, listTokens, previewTokenRevocation, submitTokenRevocation } from "../app/api";
import { formatDateTime } from "../app/format";
import type { ActionPreview, AgentToken, Locale, ManagementSession, Operation, TokenCreateResult, TokenRevokePreview, TokenScope } from "../app/types";
import { ActionDialog } from "./ActionDialog";
import { Badge, EmptyState, Button, CopyButton, Input, Panel, PanelHeader, StatusBadge, Textarea } from "./ui";

const scopeOptions: Array<{ scope: TokenScope; label: string }> = [
  { scope: "agent:read", label: "tokens.scopeRead" },
  { scope: "agent:operate", label: "tokens.scopeOperate" },
  { scope: "agent:sync", label: "tokens.scopeSync" },
  { scope: "agent:observe_admin", label: "tokens.scopeObserve" },
];

type CreateRequest = { scopes: TokenScope[]; expiresAt: string; auditReason: string; operationId: string };
type CreateRecoveryDecision = "check" | "retry_same" | "new_intent" | "blocked";

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
  const [createRequest, setCreateRequest] = useState<CreateRequest | null>(null);
  const [createOperationIdValue, setCreateOperationIdValue] = useState<string | null>(null);
  const [createRecoveryOperationId, setCreateRecoveryOperationId] = useState<string | null>(null);
  const [createRecoveryStatus, setCreateRecoveryStatus] = useState<Operation | null>(null);
  const [createRecoveryTokenId, setCreateRecoveryTokenId] = useState<string | null>(null);
  const [createRecoveryDecision, setCreateRecoveryDecision] = useState<CreateRecoveryDecision | null>(null);
  const [createRecoveryPending, setCreateRecoveryPending] = useState(false);
  const [createRecoveryError, setCreateRecoveryError] = useState<string | null>(null);
  const [plaintextToken, setPlaintextToken] = useState<string | null>(null);
  const [plaintextMetadata, setPlaintextMetadata] = useState<AgentToken | null>(null);
  const [plaintextConsumed, setPlaintextConsumed] = useState(false);
  const [revokeToken, setRevokeToken] = useState<AgentToken | null>(null);
  const [revokePreview, setRevokePreview] = useState<TokenRevokePreview | null>(null);
  const [revokeOperationId, setRevokeOperationId] = useState<string | null>(null);
  const [revokeError, setRevokeError] = useState<string | null>(null);
  const revokePreviewRequestRef = useRef(0);
  const revokeInFlightRef = useRef(false);
  const revokeTriggerRef = useRef<HTMLElement | null>(null);
  const [revokePreviewPending, setRevokePreviewPending] = useState(false);
  const [tokenCursor, setTokenCursor] = useState<string | undefined>();
  const [tokenCursorStack, setTokenCursorStack] = useState<string[]>([]);

  const sessionQuery = useQuery<ManagementSession>({ queryKey: ["management-session"], queryFn: getManagementSession, staleTime: 30_000 });
  const session = sessionQuery.data;
  const canManageTokens = session?.capabilities.includes("manage_tokens") ?? false;
  const tokensQuery = useQuery({ queryKey: ["tokens", tokenCursor], queryFn: () => listTokens(tokenCursor), enabled: canManageTokens, refetchInterval: 10_000, refetchIntervalInBackground: false });
  const clearSessionAfterAuthFailure = (error: unknown) => {
    if (error instanceof ManagementRequestError && (error.code === "authentication_required" || error.code === "csrf_invalid")) client.removeQueries({ queryKey: ["management-session"], exact: true });
  };

  useEffect(() => { clearSessionAfterAuthFailure(sessionQuery.error); }, [sessionQuery.error]);
  useEffect(() => { clearSessionAfterAuthFailure(tokensQuery.error); }, [tokensQuery.error]);

  const createMutation = useMutation({
    mutationFn: (request: CreateRequest) => createAgentToken(request.scopes, request.expiresAt, request.auditReason, request.operationId),
    onSuccess: (result: TokenCreateResult, request: CreateRequest) => {
      const mutation = client.getMutationCache().getAll().find((candidate) => candidate.state.variables === request);
      if (mutation) client.getMutationCache().remove(mutation);
      setCreateError(null);
      setCreateRequest(null);
      setReason("");
      setCreateOperationIdValue(null);
      setCreateRecoveryOperationId(null);
      setCreateRecoveryStatus(null);
      setCreateRecoveryTokenId(null);
      setCreateRecoveryDecision(null);
      setCreateRecoveryError(null);
      setTokenCursor(undefined);
      setTokenCursorStack([]);
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
      void client.invalidateQueries({ queryKey: ["audit"] });
    },
    onError: (error, request: CreateRequest) => {
      clearSessionAfterAuthFailure(error);
      setCreateError(errorMessage(error, t));
      setCreateRequest(request);
      setCreateOperationIdValue(null);
      setCreateRecoveryOperationId(request.operationId);
      setCreateRecoveryStatus(null);
      setCreateRecoveryDecision("check");
      setCreateRecoveryError(null);
    },
  });

  useEffect(() => {
    if (createMutation.isSuccess) createMutation.reset();
  }, [createMutation.isSuccess]);

  const revokeMutation = useMutation({
    mutationFn: ({ tokenId, preview, auditReason, operationId }: { tokenId: string; preview: TokenRevokePreview; auditReason: string; operationId: string }) => submitTokenRevocation(tokenId, auditReason, preview, operationId),
    onSuccess: () => {
      if (createRecoveryTokenId && revokeToken?.token_id === createRecoveryTokenId) {
        setCreateRequest(null);
        setCreateRecoveryOperationId(null);
        setCreateRecoveryStatus(null);
        setCreateRecoveryTokenId(null);
        setCreateRecoveryDecision(null);
      }
      revokeInFlightRef.current = false;
      setRevokeToken(null);
      setRevokePreview(null);
      setRevokeOperationId(null);
      setRevokePreviewPending(false);
      setRevokeError(null);
      void client.invalidateQueries({ queryKey: ["tokens"] });
      void client.invalidateQueries({ queryKey: ["operations"] });
      void client.invalidateQueries({ queryKey: ["audit"] });
    },
    onError: (error) => { clearSessionAfterAuthFailure(error); setRevokeError(errorMessage(error, t)); },
  });

  const resetCreateIntent = () => {
    if (createMutation.isPending) return;
    setCreateError(null);
    setCreateOperationIdValue(null);
    setCreateRecoveryError(null);
    if (!createRecoveryOperationId) {
      setCreateRequest(null);
      setCreateRecoveryStatus(null);
      setCreateRecoveryTokenId(null);
      setCreateRecoveryDecision(null);
      return;
    }
    if (createRecoveryDecision === "retry_same") {
      setCreateRequest(null);
      setCreateRecoveryDecision("new_intent");
    }
  };

  const toggleScope = (scope: TokenScope) => {
    resetCreateIntent();
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
    if (!canManageTokens) return;
    if (createRecoveryOperationId && createRecoveryDecision !== "retry_same" && createRecoveryDecision !== "new_intent") return;
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
    const retryRequest = createOperationIdValue && createRequest?.operationId === createOperationIdValue ? createRequest : null;
    const request: CreateRequest = retryRequest ?? { scopes: [...scopes], expiresAt: new Date(expiresAt).toISOString(), auditReason: reason.trim(), operationId: createOperationId() };
    setCreateRequest(request);
    setCreateRecoveryOperationId(null);
    setCreateRecoveryStatus(null);
    setCreateRecoveryTokenId(null);
    setCreateRecoveryDecision(null);
    setCreateRecoveryError(null);
    createMutation.mutate(request);
  };

  const checkCreateOperation = async () => {
    if (!createRecoveryOperationId) return;
    setCreateRecoveryPending(true);
    setCreateRecoveryError(null);
    try {
      const operation = await getOperation(createRecoveryOperationId);
      setCreateRecoveryStatus(operation);
      if (operation.status === "succeeded" && operation.result?.token_metadata) {
        setPlaintextMetadata(operation.result.token_metadata);
        setPlaintextToken(null);
        setPlaintextConsumed(true);
        setCreateError(null);
        setCreateRequest(null);
        setCreateOperationIdValue(null);
        setCreateRecoveryTokenId(operation.result.token_metadata.token_id);
        setCreateRecoveryDecision("blocked");
      } else if (operation.status === "failed" && !operation.result) {
        setCreateOperationIdValue(operation.operation_id);
        setCreateError(null);
        setCreateRecoveryDecision("retry_same");
      } else {
        setCreateOperationIdValue(null);
        setCreateRecoveryDecision("check");
      }
    } catch (error) {
      if (error instanceof ManagementRequestError && error.code === "not_found") {
        setCreateRecoveryError(t("tokens.operationNotFound"));
        setCreateRecoveryDecision("blocked");
      } else {
        setCreateRecoveryError(errorMessage(error, t));
        setCreateRecoveryDecision("check");
      }
    } finally {
      setCreateRecoveryPending(false);
    }
  };

  const openRevoke = async (token: AgentToken, trigger?: HTMLElement) => {
    if (revokeInFlightRef.current || revokeToken) return;
    revokeInFlightRef.current = true;
    revokeTriggerRef.current = trigger ?? (typeof document === "undefined" ? null : document.activeElement as HTMLElement);
    const requestId = revokePreviewRequestRef.current + 1;
    revokePreviewRequestRef.current = requestId;
    setRevokeError(null);
    setRevokePreview(null);
    setRevokePreviewPending(true);
    setRevokeToken(token);
    setRevokeOperationId(createOperationId());
    try {
      const preview = await previewTokenRevocation(token.token_id, token.resource_version);
      if (revokePreviewRequestRef.current === requestId) setRevokePreview(preview);
    } catch (error) {
      if (revokePreviewRequestRef.current === requestId) {
        revokeInFlightRef.current = false;
        setRevokeToken(null);
        setRevokePreview(null);
        setRevokeError(errorMessage(error, t));
      }
    } finally {
      if (revokePreviewRequestRef.current === requestId) setRevokePreviewPending(false);
    }
  };

  const cancelRevoke = () => {
    revokePreviewRequestRef.current += 1;
    revokeInFlightRef.current = false;
    setRevokePreviewPending(false);
    setRevokeToken(null);
    setRevokePreview(null);
    setRevokeOperationId(null);
    setRevokeError(null);
  };

  const nextTokenPage = () => {
    const next = tokensQuery.data?.page.next_cursor;
    if (!next) return;
    setTokenCursorStack((stack) => [...stack, tokenCursor ?? ""]);
    setTokenCursor(next);
  };

  const previousTokenPage = () => {
    const previous = tokenCursorStack[tokenCursorStack.length - 1];
    setTokenCursorStack((stack) => stack.slice(0, -1));
    setTokenCursor(previous || undefined);
  };

  const tokens = tokensQuery.data?.items ?? [];
  return <Panel className="token-panel" data-testid="token-management">
    <PanelHeader title={t("tokens.title")} description={t("tokens.subtitle")} action={<StatusBadge state={session ? "active" : "unknown"} />} />
    {session ? <SessionSummary session={session} locale={locale} /> : sessionQuery.isPending ? <div className="token-session-summary token-session-loading"><span /><span /><span /></div> : null}
    {!canManageTokens && session ? <p className="token-capability-note">{t("tokens.permissionDenied")}</p> : null}
    <div className="token-create-section">
      <div className="section-header token-section-header"><div><h3>{t("tokens.create")}</h3><p>{t("tokens.createHint")}</p></div></div>
      <form className="token-create-form" onSubmit={submitCreate}>
        <fieldset className="token-scope-fieldset" disabled={!canManageTokens}>
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
          <label className="field-label"><span>{t("tokens.expiresAt")}</span><Input type="datetime-local" value={expiresAt} onChange={(event) => { resetCreateIntent(); setExpiresAt(event.target.value); }} disabled={!canManageTokens} required /></label>
          <label className="field-label"><span>{t("tokens.reason")} <em>({t("common.optional")})</em></span><Textarea value={reason} onChange={(event) => { resetCreateIntent(); setReason(event.target.value); }} rows={2} disabled={!canManageTokens} placeholder={t("common.reasonPlaceholder")} /></label>
        </div>
        {createError ? <div role="alert"><p className="field-error">{createError}</p>{createOperationIdValue ? <p className="token-retry-hint">{t("tokens.retryCreate", { operationId: createOperationIdValue })}</p> : null}</div> : null}
        {createRecoveryOperationId ? <div className="token-recovery"><p className="token-retry-hint">{t("tokens.operationRecoveryHint", { operationId: createRecoveryOperationId })}</p><Button type="button" variant="ghost" disabled={createRecoveryPending} onClick={() => void checkCreateOperation()}>{createRecoveryPending ? t("common.loading") : t("tokens.checkOperation")}</Button>{createRecoveryError ? <p className="field-error" role="alert">{createRecoveryError}</p> : null}{createRecoveryStatus ? <p className="token-retry-hint" data-testid="token-operation-status">{t("tokens.operationStatus", { status: t(`status.${createRecoveryStatus.status}`) })}</p> : null}</div> : null}
        <div className="token-form-actions"><Button type="submit" variant="primary" disabled={createMutation.isPending || !canManageTokens || Boolean(createRecoveryOperationId && createRecoveryDecision !== "retry_same" && createRecoveryDecision !== "new_intent")} icon={<KeyRound />}>{createMutation.isPending ? t("common.loading") : createOperationIdValue ? t("tokens.retryCreateButton") : t("tokens.create")}</Button></div>
      </form>
    </div>
    {plaintextMetadata ? <PlaintextDisclosure token={plaintextMetadata} plaintext={plaintextToken} consumed={plaintextConsumed} onDismiss={() => { setPlaintextToken(null); setPlaintextConsumed(true); }} /> : null}
    <div className="token-list-section">
      <div className="section-header token-section-header"><div><h3>{t("tokens.listTitle")}</h3><p>{t("tokens.count", { count: tokens.length })}</p></div></div>
      {revokeError && !revokePreview ? <p className="field-error" role="alert">{revokeError}</p> : null}
      {sessionQuery.isPending ? <div className="loading-rows token-loading"><span /><span /><span /></div> : !canManageTokens ? <p className="token-capability-note">{t("tokens.permissionDenied")}</p> : tokensQuery.isPending ? <div className="loading-rows token-loading"><span /><span /><span /></div> : tokens.length ? <div className="token-list" role="list">{tokens.map((token) => <TokenRow key={token.token_id} token={token} locale={locale} revokeDisabled={Boolean(revokeToken) || revokePreviewPending || revokeMutation.isPending || !canManageTokens} onRevoke={(trigger) => void openRevoke(token, trigger)} />)}</div> : <EmptyState title={t("tokens.noTokens")} />}
      {tokensQuery.isError ? <p className="field-error" role="alert">{t("errors.generic")}</p> : null}
      <div className="pagination token-pagination"><span>{tokens.length}</span><div><Button variant="ghost" aria-label={t("common.previous")} disabled={!tokenCursorStack.length} onClick={previousTokenPage} icon={<ChevronLeft />} /><Button variant="ghost" aria-label={t("common.next")} disabled={!tokensQuery.data?.page.next_cursor} onClick={nextTokenPage} icon={<ChevronRight />} /></div></div>
    </div>
    {revokeToken && revokePreview ? <ActionDialog preview={actionPreview(revokePreview)} actionLabel={t("tokens.revoke")} target={`${t("tokens.tokenId")} · ${revokeToken.token_id}`} dangerous busy={revokeMutation.isPending} error={revokeError} confirmationText={t("tokens.revokeConfirm")} returnFocusTarget={revokeTriggerRef.current} onCancel={cancelRevoke} onConfirm={(auditReason) => revokeMutation.mutate({ tokenId: revokeToken.token_id, preview: revokePreview, auditReason, operationId: revokeOperationId ?? createOperationId() })} /> : null}
  </Panel>;
}

function SessionSummary({ session, locale }: { session: ManagementSession; locale: Locale }) {
  const { t } = useTranslation();
  return <div className="token-session-summary">
    <div className="token-session-heading"><div className="token-session-icon" aria-hidden="true"><UserRound /></div><div><h3>{t("tokens.adminSession")}</h3><p className="mono">{session.subject}</p></div><StatusBadge state="active" /></div>
    <dl className="token-session-grid"><div><dt>{t("tokens.principal")}</dt><dd className="mono">{session.subject}</dd></div><div><dt>{t("tokens.issuer")}</dt><dd className="mono">{session.issuer}</dd></div><div><dt>{t("tokens.role")}</dt><dd>{t("tokens.roleAdministrator")}</dd></div><div><dt>{t("tokens.expires")}</dt><dd>{formatDateTime(session.expires_at, locale)}</dd></div><div className="token-capabilities"><dt>{t("tokens.capabilities")}</dt><dd>{session.capabilities.map((capability) => <span className="scope-tag" key={capability}>{capability}</span>)}</dd></div></dl>
  </div>;
}

function TokenRow({ token, locale, revokeDisabled, onRevoke }: { token: AgentToken; locale: Locale; revokeDisabled: boolean; onRevoke: (trigger: HTMLElement) => void }) {
  const { t } = useTranslation();
  const state = tokenState(token);
  return <article className="token-row" role="listitem">
    <div className="token-row-main"><div className="token-row-title"><strong className="mono" title={token.token_id}>{token.token_id}</strong><CopyButton value={token.token_id} /><Badge tone={state === "active" ? "success" : state === "expired" ? "warning" : "danger"}>{t(`tokens.${state}`)}</Badge></div><div className="token-scopes">{token.scopes.map((scope) => <span className="scope-tag" key={scope}>{scope}</span>)}</div></div>
    <dl className="token-row-meta"><div><dt>{t("tokens.created")}</dt><dd>{formatDateTime(token.created_at, locale)}</dd></div><div><dt>{token.revoked_at ? t("tokens.revokedAt") : t("tokens.expires")}</dt><dd>{formatDateTime(token.revoked_at ?? token.expires_at, locale)}</dd></div></dl>
    <Button variant="danger" disabled={state !== "active" || revokeDisabled} onClick={(event) => onRevoke(event.currentTarget)} icon={<ShieldCheck />}>{t("tokens.revoke")}</Button>
  </article>;
}

function PlaintextDisclosure({ token, plaintext, consumed, onDismiss }: { token: AgentToken; plaintext: string | null; consumed: boolean; onDismiss: () => void }) {
  const { t } = useTranslation();
  return <div className="token-disclosure" role="region" aria-labelledby="token-plaintext-title" data-testid="token-disclosure"><div className="token-disclosure-heading"><AlertTriangle aria-hidden="true" /><div><strong id="token-plaintext-title">{t("tokens.plaintextTitle")}</strong><p>{consumed ? t("tokens.plaintextConsumed") : t("tokens.plaintextWarning")}</p></div></div>{plaintext ? <div className="token-value"><code data-testid="token-plaintext">{plaintext}</code><CopyButton value={plaintext} /></div> : null}<div className="token-disclosure-meta"><span className="mono">{token.token_id}</span><Button variant="ghost" onClick={onDismiss}>{t("tokens.dismissPlaintext")}</Button></div></div>;
}
