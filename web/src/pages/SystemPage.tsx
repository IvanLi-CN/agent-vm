import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { CheckCircle2, Database, Wrench } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { ManagementRequestError, createOperationId, getSystem, previewMaintenance, submitMaintenance } from "../app/api";
import { actionLabel, formatDateTime } from "../app/format";
import type { ActionPreview, Locale, SystemStatus } from "../app/types";
import { ActionDialog } from "../components/ActionDialog";
import { PageIntro, QueryError } from "../components/PagePrimitives";
import { Button, Panel, PanelHeader, StatusBadge } from "../components/ui";

type MaintenanceAction = "drain_enter" | "drain_exit" | "reconcile" | "gc";

export function SystemPage() {
  const { t, i18n } = useTranslation();
  const locale: Locale = i18n.language === "zh-CN" ? "zh-CN" : "en";
  const client = useQueryClient();
  const [action, setAction] = useState<MaintenanceAction | null>(null);
  const [preview, setPreview] = useState<ActionPreview | null>(null);
  const [operationId, setOperationId] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const query = useQuery({ queryKey: ["system"], queryFn: getSystem, refetchInterval: 10_000, refetchIntervalInBackground: false });
  const showError = (error: unknown) => setActionError(error instanceof ManagementRequestError ? t(`errors.${error.code}`, { defaultValue: t("errors.generic") }) : t("errors.generic"));
  const mutation = useMutation({
    mutationFn: ({ reason }: { reason: string }) => submitMaintenance(action!, reason, preview!, operationId ?? undefined),
    onSuccess: () => {
      setAction(null);
      setPreview(null);
      setOperationId(null);
      setActionError(null);
      void client.invalidateQueries({ queryKey: ["operations"] });
      void client.invalidateQueries({ queryKey: ["system"] });
    },
    onError: showError,
  });
  const openAction = async (nextAction: MaintenanceAction) => {
    setActionError(null);
    setAction(nextAction);
    setOperationId(createOperationId());
    try {
      setPreview(await previewMaintenance(nextAction, query.data?.resource_version));
    } catch (error) {
      setAction(null);
      setPreview(null);
      showError(error);
    }
  };
  if (query.isPending) return <><PageIntro title={t("system.title")} description={t("system.subtitle")} /><Panel><div className="loading-rows"><span /><span /><span /></div></Panel></>;
  if (query.isError || !query.data) return <><PageIntro title={t("system.title")} description={t("system.subtitle")} /><QueryError onRetry={() => void query.refetch()} /></>;
  return <div className="page-stack" data-testid="system-page">
    <PageIntro title={t("system.title")} description={t("system.subtitle")} onRefresh={() => void query.refetch()} refreshing={query.isFetching} />
    <div className="system-overview">
      <Panel><PanelHeader title={t("system.productVersion")} /><div className="product-version mono">{query.data.product_version}</div><div className="system-meta"><span className="mono">{query.data.resource_version}</span><span>{t("shell.synthetic")}</span></div></Panel>
      <Panel><PanelHeader title={t("system.maintenance")} description={t("system.maintainHint")} /><MaintenanceSummary system={query.data} /><div className="maintenance-actions"><Button variant="secondary" icon={<Wrench />} onClick={() => void openAction(query.data.maintenance.draining ? "drain_exit" : "drain_enter")}>{query.data.maintenance.draining ? t("system.exitDrain") : t("system.enterDrain")}</Button><Button variant="ghost" onClick={() => void openAction("reconcile")}>{t("system.reconcile")}</Button><Button variant="ghost" onClick={() => void openAction("gc")}>{t("system.gc")}</Button></div>{actionError && !action ? <p className="field-error" role="alert">{actionError}</p> : null}</Panel>
    </div>
    <div className="system-grid">
      <Panel><PanelHeader title={t("system.components")} /><div className="component-list">{query.data.components.map((component) => <div className="component-row" key={component.name}><div><strong>{component.name}</strong><span className="mono">{component.version ?? "—"}</span></div><div><StatusBadge state={component.state} /><small>{component.observed_at ? formatDateTime(component.observed_at, locale) : "—"}</small></div></div>)}</div></Panel>
      <Panel><PanelHeader title={t("system.capabilities")} /><div className="capability-list">{query.data.state_capabilities.map((capability) => <div key={capability}><Database aria-hidden="true" /><span className="mono">{capability}</span><CheckCircle2 aria-hidden="true" /></div>)}</div></Panel>
      <Panel><PanelHeader title={t("system.source")} /><div className="source-list">{query.data.sources.map((source) => <div className="source-row" key={source.name}><div><strong>{source.name}</strong><span>{source.safe_code ?? "—"}</span></div><StatusBadge state={source.state} /></div>)}</div></Panel>
    </div>
    {action && preview ? <ActionDialog preview={preview} actionLabel={actionLabel(action, locale)} target={`${t("system.title")} · ${t("system.maintenance")}`} error={actionError} dangerous={action === "drain_enter" || action === "gc"} busy={mutation.isPending} onCancel={() => { setAction(null); setPreview(null); setOperationId(null); setActionError(null); }} onConfirm={(reason) => mutation.mutate({ reason })} /> : null}
  </div>;
}

function MaintenanceSummary({ system }: { system: SystemStatus }) {
  const { t } = useTranslation();
  return <div className="maintenance-summary"><div><span>{t("system.draining")}</span><strong>{system.maintenance.draining === null ? "—" : system.maintenance.draining ? t("status.active") : t("status.available")}</strong></div><div><span>{t("system.reconciliation")}</span><strong>{system.maintenance.reconciliation_pending ?? "—"}</strong></div><div><span>{t("system.garbage")}</span><strong>{system.maintenance.garbage_collection_pending ?? "—"}</strong></div></div>;
}
