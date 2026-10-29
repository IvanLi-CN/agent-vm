import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ChevronLeft, ChevronRight, Copy, ExternalLink, RefreshCw, RotateCcw, Trash2 } from "lucide-react";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { actionLabel, formatBytes, formatDateTime, shortFingerprint } from "../app/format";
import { ManagementRequestError, createOperationId, listVMs, previewVMAction, submitVMAction } from "../app/api";
import type { ActionPreview, Locale, ManagedVM } from "../app/types";
import { ActionDialog } from "../components/ActionDialog";
import { Select, SelectContent, SelectGroup, SelectItem, SelectTrigger, SelectValue } from "../components/ui/select";
import { CopyButton, EmptyState, Input, Panel, PanelHeader, StatusBadge, Button } from "../components/ui";
import { DefinitionList, PageIntro, QueryError } from "../components/PagePrimitives";

function vmDisplayName(vm: ManagedVM): string {
  return vm.task_id.replaceAll("_", " ");
}

export function VMsPage() {
  const { t, i18n } = useTranslation();
  const locale: Locale = i18n.language === "zh-CN" ? "zh-CN" : "en";
  const queryClient = useQueryClient();
  const [q, setQ] = useState("");
  const [state, setState] = useState("all");
  const [leaseState, setLeaseState] = useState("all");
  const [cursor, setCursor] = useState<string | undefined>();
  const [cursorStack, setCursorStack] = useState<string[]>([]);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [action, setAction] = useState<string | null>(null);
  const [preview, setPreview] = useState<ActionPreview | null>(null);
  const [parameters, setParameters] = useState<Record<string, unknown>>({});
  const [operationId, setOperationId] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const vmQuery = useQuery({ queryKey: ["vms", q, state, leaseState, cursor], queryFn: () => listVMs({ q, state, leaseState, cursor }), refetchInterval: 10_000, refetchIntervalInBackground: false });
  const selected = vmQuery.data?.items.find((vm) => vm.task_id === selectedId) ?? vmQuery.data?.items[0] ?? null;

  useEffect(() => {
    if (selected && selected.task_id !== selectedId) setSelectedId(selected.task_id);
  }, [selected, selectedId]);

  const showError = (error: unknown) => setActionError(error instanceof ManagementRequestError ? t(`errors.${error.code}`, { defaultValue: t("errors.generic") }) : t("errors.generic"));
  const submitMutation = useMutation({
    mutationFn: ({ reason }: { reason: string }) => submitVMAction(selected?.task_id ?? "", action ?? "restart", reason, preview!, selected?.generation ?? "", parameters, operationId ?? undefined),
    onSuccess: () => { setAction(null); setPreview(null); setOperationId(null); setActionError(null); void queryClient.invalidateQueries({ queryKey: ["operations"] }); },
    onError: showError,
  });

  const openAction = async (nextAction: string) => {
    if (!selected) return;
    setActionError(null);
    setAction(nextAction);
    setOperationId(createOperationId());
    const initial = nextAction === "resize"
      ? { cpu_vcpu: selected.resources.cpu_vcpu ?? 2, memory_bytes: selected.resources.memory_bytes ?? "4294967296", disk_bytes: selected.resources.disk_bytes ?? "21474836480", live: false, allow_restart: false }
      : nextAction === "port_add"
        ? { port_name: "http", protocol: "tcp", host_port: null, guest_port: 8080 }
        : nextAction === "port_remove"
          ? { port_name: selected.port_mappings.find((port) => port.name !== "ssh")?.name ?? "http", protocol: selected.port_mappings.find((port) => port.name !== "ssh")?.protocol ?? "tcp" }
          : {};
    setParameters(initial);
    try {
      setPreview(await previewVMAction(selected.task_id, nextAction, selected, initial));
    } catch (error) {
      showError(error);
    }
  };

  const updateParameters = async (next: Record<string, unknown>) => {
    setParameters(next);
    if (!selected || !action) return;
    try {
      setPreview(await previewVMAction(selected.task_id, action, selected, next));
    } catch (error) {
      showError(error);
    }
  };

  const resetPage = () => { setCursor(undefined); setCursorStack([]); };
  const nextPage = () => { const next = vmQuery.data?.page.next_cursor; if (!next) return; setCursorStack((stack) => [...stack, cursor ?? ""]); setCursor(next); };
  const previousPage = () => {
    const previous = cursorStack[cursorStack.length - 1];
    setCursorStack((stack) => stack.slice(0, -1));
    setCursor(previous || undefined);
  };

  return <div className="page-stack" data-testid="vms-page">
    <PageIntro title={t("vms.title")} description={t("vms.subtitle")} onRefresh={() => void vmQuery.refetch()} refreshing={vmQuery.isFetching} />
    <Panel className="filter-bar">
      <label className="filter-search"><span className="sr-only">{t("vms.filterSearch")}</span><Input value={q} onChange={(event) => { setQ(event.target.value); resetPage(); }} placeholder={t("vms.filterSearch")} /></label>
      <label className="filter-field"><span>{t("vms.filterState")}</span><Select value={state} onValueChange={(value) => { setState(value); resetPage(); }}><SelectTrigger aria-label={t("vms.filterState")}><SelectValue /></SelectTrigger><SelectContent><SelectGroup><SelectItem value="all">{t("vms.allStates")}</SelectItem><SelectItem value="active">{t("status.active")}</SelectItem><SelectItem value="cached">{t("status.cached")}</SelectItem><SelectItem value="failed">{t("status.failed")}</SelectItem><SelectItem value="preparing">{t("status.preparing")}</SelectItem></SelectGroup></SelectContent></Select></label>
      <label className="filter-field"><span>{t("vms.filterLease")}</span><Select value={leaseState} onValueChange={(value) => { setLeaseState(value); resetPage(); }}><SelectTrigger aria-label={t("vms.filterLease")}><SelectValue /></SelectTrigger><SelectContent><SelectGroup><SelectItem value="all">{t("vms.allLeases")}</SelectItem><SelectItem value="effective">{t("status.effective")}</SelectItem><SelectItem value="expired">{t("status.expired")}</SelectItem><SelectItem value="absent">{t("status.absent")}</SelectItem></SelectGroup></SelectContent></Select></label>
    </Panel>
    {vmQuery.isPending ? <Panel><div className="loading-rows"><span /><span /><span /></div></Panel> : vmQuery.isError ? <QueryError onRetry={() => void vmQuery.refetch()} /> : <div className="split-layout">
      <Panel className="list-panel">
        <PanelHeader title={t("vms.listTitle")} description={t("vms.count", { count: vmQuery.data.items.length })} action={<Button variant="ghost" icon={<RefreshCw />} onClick={() => void vmQuery.refetch()}>{t("shell.refresh")}</Button>} />
        <div className="resource-list" role="listbox" aria-label={t("vms.listTitle")}>
          {vmQuery.data.items.length ? vmQuery.data.items.map((vm) => <button className={`resource-row ${selected?.task_id === vm.task_id ? "is-selected" : ""}`} key={vm.task_id} role="option" aria-selected={selected?.task_id === vm.task_id} onClick={() => setSelectedId(vm.task_id)}><div className="resource-main"><strong>{vmDisplayName(vm)}</strong><span className="mono">{vm.session_id}</span></div><div className="resource-summary"><StatusBadge state={vm.state} /><span>{vm.image.version_label ?? "—"}</span></div><div className="resource-meta"><span>{vm.native_state}</span><span>{vm.resources.cpu_vcpu ?? "—"} vCPU</span></div></button>) : <EmptyState title={t("vms.noResults")} description={t("vms.detailEmpty")} />}
        </div>
        <div className="pagination"><span>{vmQuery.data.items.length}</span><div><Button variant="ghost" aria-label={t("common.previous")} disabled={!cursorStack.length} onClick={previousPage} icon={<ChevronLeft />} /><Button variant="ghost" aria-label={t("common.next")} disabled={!vmQuery.data.page.next_cursor} onClick={nextPage} icon={<ChevronRight />} /></div></div>
      </Panel>
      <VMDetail vm={selected} locale={locale} onAction={openAction} actionError={actionError} />
    </div>}
    {action && preview && selected ? <ActionDialog preview={preview} actionLabel={actionLabel(action, locale)} target={`${selected.task_id} · ${selected.session_id}`} dangerous={action === "destroy" || action === "release"} busy={submitMutation.isPending} onCancel={() => { setAction(null); setPreview(null); setOperationId(null); }} onConfirm={(reason) => submitMutation.mutate({ reason })}><VMActionParameters action={action} parameters={parameters} onChange={(next) => void updateParameters(next)} /></ActionDialog> : null}
  </div>;
}

function VMActionParameters({ action, parameters, onChange }: { action: string; parameters: Record<string, unknown>; onChange: (parameters: Record<string, unknown>) => void }) {
  const { t } = useTranslation();
  if (action === "resize") return <div className="form-grid dialog-parameters"><label className="field-label">{t("vms.cpuVcpu")}<Input type="number" min="1" value={String(parameters.cpu_vcpu ?? "")} onChange={(event) => onChange({ ...parameters, cpu_vcpu: Number(event.target.value) })} /></label><label className="field-label">{t("vms.memoryBytes")}<Input value={String(parameters.memory_bytes ?? "")} onChange={(event) => onChange({ ...parameters, memory_bytes: event.target.value })} /></label><label className="field-label">{t("vms.diskBytes")}<Input value={String(parameters.disk_bytes ?? "")} onChange={(event) => onChange({ ...parameters, disk_bytes: event.target.value })} /></label></div>;
  if (action === "port_add") return <div className="form-grid dialog-parameters"><label className="field-label">{t("vms.portName")}<Input value={String(parameters.port_name ?? "")} onChange={(event) => onChange({ ...parameters, port_name: event.target.value })} /></label><label className="field-label">{t("vms.protocol")}<Select value={String(parameters.protocol ?? "tcp")} onValueChange={(value) => onChange({ ...parameters, protocol: value })}><SelectTrigger aria-label={t("vms.protocol")}><SelectValue /></SelectTrigger><SelectContent><SelectGroup><SelectItem value="tcp">TCP</SelectItem><SelectItem value="udp">UDP</SelectItem></SelectGroup></SelectContent></Select></label><label className="field-label">{t("vms.guestPort")}<Input type="number" min="1" max="65535" value={String(parameters.guest_port ?? "")} onChange={(event) => onChange({ ...parameters, guest_port: Number(event.target.value) })} /></label><label className="field-label">{t("vms.hostPort")}<Input type="number" min="0" max="65535" value={String(parameters.host_port ?? "")} onChange={(event) => onChange({ ...parameters, host_port: Number(event.target.value) })} /></label></div>;
  if (action === "port_remove") return <div className="form-grid dialog-parameters"><label className="field-label">{t("vms.portName")}<Input value={String(parameters.port_name ?? "")} onChange={(event) => onChange({ ...parameters, port_name: event.target.value })} /></label><label className="field-label">{t("vms.protocol")}<Select value={String(parameters.protocol ?? "tcp")} onValueChange={(value) => onChange({ ...parameters, protocol: value })}><SelectTrigger aria-label={t("vms.protocol")}><SelectValue /></SelectTrigger><SelectContent><SelectGroup><SelectItem value="tcp">TCP</SelectItem><SelectItem value="udp">UDP</SelectItem></SelectGroup></SelectContent></Select></label></div>;
  return null;
}

function VMDetail({ vm, locale, onAction, actionError }: { vm: ManagedVM | null; locale: Locale; onAction: (action: string) => void; actionError: string | null }) {
  const { t } = useTranslation();
  if (!vm) return <Panel className="detail-rail"><EmptyState title={t("vms.detailEmpty")} /></Panel>;
  return <Panel className="detail-rail"><div className="detail-identity"><div><span className="eyebrow-label">{t("vms.selectedTarget")}</span><h2>{vmDisplayName(vm)}</h2><span className="mono">{vm.task_id}</span></div><StatusBadge state={vm.state} /></div><div className="identity-line"><span className="mono">{vm.session_id}</span><CopyButton value={vm.session_id} /></div><DefinitionList items={[{ label: t("vms.identity"), value: <span className="mono">{vm.resource_version}</span> }, { label: t("vms.generation"), value: <span className="mono">{vm.generation}</span> }, { label: t("vms.image"), value: <span><strong>{vm.image.version_label ?? "—"}</strong><small className="mono">{shortFingerprint(vm.image.fingerprint)}</small></span> }, { label: t("vms.resources"), value: <span>{vm.resources.cpu_vcpu ?? "—"} vCPU · {formatBytes(vm.resources.memory_bytes, locale)}</span> }, { label: t("vms.lease"), value: <StatusBadge state={vm.lease.state} /> }, { label: t("vms.expires"), value: formatDateTime(vm.lease.expires_at, locale) }, { label: t("vms.updated"), value: formatDateTime(vm.updated_at, locale) }]} /><div className="detail-section"><h3>{t("vms.ports")}</h3>{vm.port_mappings.length ? vm.port_mappings.map((port) => <div className="port-row" key={`${port.name}-${port.host_port}`}><span>{t("vms.port", { host: port.host_port, guest: port.guest_port, protocol: port.protocol })}</span><ExternalLink aria-hidden="true" /></div>) : <span className="muted">—</span>}</div>{vm.last_error ? <div className="detail-section"><h3>{t("vms.lastError")}</h3><div className="inline-error"><span className="mono">{vm.last_error.code}</span><span>{vm.last_error.retryable ? t("common.retry") : t("status.failed")}</span></div></div> : null}<div className="detail-section"><h3>{t("vms.actions")}</h3><p className="detail-hint">{t("vms.actionHint")}</p><div className="action-grid">{vm.allowed_actions.map((action) => <Button key={action} variant={action === "destroy" ? "danger" : action === "restart" ? "primary" : "secondary"} icon={action === "restart" ? <RotateCcw /> : action === "destroy" ? <Trash2 /> : <Copy />} onClick={() => onAction(action)}>{actionLabel(action, locale)}</Button>)}</div>{actionError ? <p className="field-error">{actionError}</p> : null}</div></Panel>;
}
