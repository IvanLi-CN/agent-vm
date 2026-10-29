import { useQuery } from "@tanstack/react-query";
import { ChevronLeft, ChevronRight, Clock3, ExternalLink } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { listAudit, listOperations } from "../app/api";
import { actionLabel, formatActor, formatDateTime } from "../app/format";
import type { Locale, Operation } from "../app/types";
import { PageIntro, QueryError } from "../components/PagePrimitives";
import { Button, Panel, PanelHeader, Select, StatusBadge } from "../components/ui";

const phaseProgress: Record<string, number> = { queued: 12, awaiting_upload: 22, upload: 38, integrity_check: 54, image_import: 68, suitability_check: 80, reference_check: 82, native_action: 88, reconciliation: 94, cleanup: 97, completed: 100 };

export function OperationsPage() {
  const { t, i18n } = useTranslation();
  const locale: Locale = i18n.language === "zh-CN" ? "zh-CN" : "en";
  const [status, setStatus] = useState("all");
  const [tab, setTab] = useState<"operations" | "audit">("operations");
  const [operationCursor, setOperationCursor] = useState<string | undefined>();
  const [operationCursorStack, setOperationCursorStack] = useState<string[]>([]);
  const [auditCursor, setAuditCursor] = useState<string | undefined>();
  const [auditCursorStack, setAuditCursorStack] = useState<string[]>([]);
  const operationsQuery = useQuery({ queryKey: ["operations", status, operationCursor], queryFn: () => listOperations(status, operationCursor), refetchInterval: status === "all" || ["queued", "running", "reconciling", "awaiting_upload"].includes(status) ? 2_000 : false, refetchIntervalInBackground: false });
  const auditQuery = useQuery({ queryKey: ["audit", auditCursor], queryFn: () => listAudit(auditCursor), enabled: tab === "audit" });
  const nextOperations = () => { const next = operationsQuery.data?.page.next_cursor; if (!next) return; setOperationCursorStack((stack) => [...stack, operationCursor ?? ""]); setOperationCursor(next); };
  const previousOperations = () => {
    const previous = operationCursorStack[operationCursorStack.length - 1];
    setOperationCursorStack((stack) => stack.slice(0, -1));
    setOperationCursor(previous || undefined);
  };
  const nextAudit = () => { const next = auditQuery.data?.page.next_cursor; if (!next) return; setAuditCursorStack((stack) => [...stack, auditCursor ?? ""]); setAuditCursor(next); };
  const previousAudit = () => {
    const previous = auditCursorStack[auditCursorStack.length - 1];
    setAuditCursorStack((stack) => stack.slice(0, -1));
    setAuditCursor(previous || undefined);
  };
  return <div className="page-stack" data-testid="operations-page">
    <PageIntro title={t("operations.title")} description={t("operations.subtitle")} onRefresh={() => { void operationsQuery.refetch(); void auditQuery.refetch(); }} refreshing={operationsQuery.isFetching || auditQuery.isFetching} />
    <div className="tabs-row" role="tablist"><button role="tab" aria-selected={tab === "operations"} className={tab === "operations" ? "active" : ""} onClick={() => setTab("operations")}>{t("operations.operations")}</button><button role="tab" aria-selected={tab === "audit"} className={tab === "audit" ? "active" : ""} onClick={() => setTab("audit")}>{t("operations.audit")}</button></div>
    {tab === "operations" ? <Panel className="table-panel">
      <PanelHeader title={t("operations.operations")} description={t("operations.runningHint")} action={<label className="compact-field"><span>{t("operations.filter")}</span><Select value={status} onChange={(event) => { setStatus(event.target.value); setOperationCursor(undefined); setOperationCursorStack([]); }}><option value="all">{t("operations.allStatuses")}</option><option value="running">{t("status.running")}</option><option value="queued">{t("status.queued")}</option><option value="succeeded">{t("status.succeeded")}</option><option value="failed">{t("status.failed")}</option><option value="outcome_unknown">{t("status.outcome_unknown")}</option></Select></label>} />
      {operationsQuery.isPending ? <div className="loading-rows"><span /><span /><span /></div> : operationsQuery.isError ? <QueryError onRetry={() => void operationsQuery.refetch()} /> : <div className="operations-table" role="table"><div className="table-row table-head" role="row"><span>{t("operations.operation")}</span><span>{t("operations.target")}</span><span>{t("operations.phase")}</span><span>{t("operations.outcome")}</span><span>{t("operations.occurred")}</span><span /></div>{operationsQuery.data.items.map((operation) => <OperationRow key={operation.operation_id} operation={operation} locale={locale} />)}</div>}
      <div className="pagination"><span>{operationsQuery.data?.items.length ?? 0}</span><div><Button variant="ghost" aria-label={t("common.previous")} disabled={!operationCursorStack.length} onClick={previousOperations} icon={<ChevronLeft />} /><Button variant="ghost" aria-label={t("common.next")} disabled={!operationsQuery.data?.page.next_cursor} onClick={nextOperations} icon={<ChevronRight />} /></div></div>
    </Panel> : <Panel className="table-panel">
      <PanelHeader title={t("operations.audit")} description={t("operations.reason")} />
      {auditQuery.isPending ? <div className="loading-rows"><span /><span /><span /></div> : auditQuery.isError ? <QueryError onRetry={() => void auditQuery.refetch()} /> : auditQuery.data?.items.length ? <div className="operations-table audit-table" role="table"><div className="table-row table-head"><span>{t("operations.occurred")}</span><span>{t("operations.operation")}</span><span>{t("operations.actor")}</span><span>{t("operations.reason")}</span><span>{t("operations.outcome")}</span></div>{auditQuery.data.items.map((event) => <div className="table-row" key={event.event_id}><span>{formatDateTime(event.occurred_at, locale)}</span><span><strong>{actionLabel(event.action, locale)}</strong><small className="mono">{event.operation_id}</small></span><span className="mono">{formatActor(event.actor)}</span><span>{event.reason ?? "—"}</span><span><StatusBadge state={event.outcome} />{event.safe_code ? <small className="mono safe-code">{event.safe_code}</small> : null}</span></div>)}</div> : <div className="panel-empty">{t("operations.noAudit")}</div>}
      <div className="pagination"><span>{auditQuery.data?.items.length ?? 0}</span><div><Button variant="ghost" disabled={!auditCursorStack.length} onClick={previousAudit} icon={<ChevronLeft />} aria-label={t("common.previous")} /><Button variant="ghost" disabled={!auditQuery.data?.page.next_cursor} onClick={nextAudit} icon={<ChevronRight />} aria-label={t("common.next")} /></div></div>
    </Panel>}
  </div>;
}

function OperationRow({ operation, locale }: { operation: Operation; locale: Locale }) {
  const { t } = useTranslation();
  const terminal = ["succeeded", "failed", "outcome_unknown"].includes(operation.status);
  const progress = terminal ? 100 : phaseProgress[operation.phase] ?? 20;
  return <div className={`table-row operation-row status-row-${operation.status}`} role="row"><span><strong>{actionLabel(operation.action, locale)}</strong><small className="mono">{operation.operation_id}</small></span><span><strong>{operation.target.id}</strong><small>{operation.target.kind}</small></span><span><div className="phase-cell"><span>{operation.phase}</span><div className="operation-progress"><span style={{ width: `${progress}%` }} /></div></div></span><span><StatusBadge state={operation.status} />{operation.error ? <small className="mono safe-code">{operation.error.code}</small> : null}</span><span><Clock3 aria-hidden="true" />{formatDateTime(operation.updated_at, locale)}</span><span><Button variant="ghost" icon={<ExternalLink />} aria-label={t("common.viewAll")}>{t("common.viewAll")}</Button></span>{operation.status === "outcome_unknown" ? <p className="row-callout">{t("operations.unknownHint")}</p> : operation.status === "failed" ? <p className="row-callout row-callout-danger">{t("operations.failedHint")}</p> : null}</div>;
}
