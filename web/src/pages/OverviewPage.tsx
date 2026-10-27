import { useQuery } from "@tanstack/react-query";
import { CheckCircle2, CircleHelp, Gauge, HardDrive, MemoryStick, Server, TriangleAlert } from "lucide-react";
import { useTranslation } from "react-i18next";
import { getOverview } from "../app/api";
import { formatBytes, formatNumber } from "../app/format";
import type { Locale, ObservedBoolean, ObservedBytes, ObservedNumber } from "../app/types";
import { PageIntro, FreshnessLine, LoadingPanel, QueryError } from "../components/PagePrimitives";
import { Badge, Panel, PanelHeader, StatusBadge } from "../components/ui";

function ObservedValue({ value, format, label }: { value: ObservedNumber | ObservedBytes | ObservedBoolean; format: (value: ObservedNumber | ObservedBytes | ObservedBoolean) => string; label: string }) {
  const { t } = useTranslation();
  const unknown = "value_bytes" in value ? value.value_bytes === null : value.value === null;
  return <div className="metric-cell"><div className="metric-label"><span>{label}</span>{value.state !== "available" ? <CircleHelp aria-label={t("overview.unknownNote")} /> : null}</div><strong className={unknown ? "metric-unknown" : ""}>{format(value)}</strong><FreshnessLine state={value.state} observedAt={value.observed_at} /></div>;
}

function metricNumber(locale: Locale, suffix = "") {
  return (value: ObservedNumber | ObservedBytes | ObservedBoolean) => {
    if ("value" in value && typeof value.value === "number") return `${formatNumber(value.value, locale)}${suffix}`;
    if ("value_bytes" in value) return formatBytes(value.value_bytes, locale);
    if ("value" in value && typeof value.value === "boolean") return value.value ? "true" : "false";
    return "—";
  };
}

export function OverviewPage() {
  const { t, i18n } = useTranslation();
  const locale: Locale = i18n.language === "zh-CN" ? "zh-CN" : "en";
  const query = useQuery({ queryKey: ["overview"], queryFn: getOverview, refetchInterval: 10_000, refetchIntervalInBackground: false });
  if (query.isPending) return <><PageIntro title={t("overview.title")} description={t("overview.subtitle")} /><LoadingPanel /></>;
  if (query.isError || !query.data) return <><PageIntro title={t("overview.title")} description={t("overview.subtitle")} /><QueryError onRetry={() => void query.refetch()} /></>;
  const data = query.data;
  const admissionTone = data.admission.ready === true && !data.admission.draining ? "success" : data.admission.ready === false || data.admission.draining ? "warning" : "neutral";
  return <div className="page-stack" data-testid="overview-page"><PageIntro title={t("overview.title")} description={t("overview.subtitle")} observedAt={data.observed_at} onRefresh={() => void query.refetch()} refreshing={query.isFetching} /><div className="overview-grid"><Panel className="admission-panel"><div className="admission-icon" aria-hidden="true">{admissionTone === "success" ? <CheckCircle2 /> : admissionTone === "warning" ? <TriangleAlert /> : <CircleHelp />}</div><div><span className="eyebrow-label">{t("overview.admission")}</span><strong>{data.admission.ready === true && !data.admission.draining ? t("overview.accepting") : data.admission.draining ? t("overview.draining") : t("overview.admissionUnknown")}</strong><p>{data.admission.reasons.length ? data.admission.reasons.join(" · ") : t("overview.noReasons")}</p></div><Badge tone={admissionTone}>{data.admission.ready === true ? <>{data.admission.draining ? t("status.stale") : t("status.ready")}</> : t("status.unknown")}</Badge></Panel><Panel className="capacity-panel"><PanelHeader title={t("overview.capacity")} description={t("shell.synthetic")} /><div className="metric-grid"><ObservedValue label={t("overview.cpuRemaining")} value={data.capacity.logical_cpu_remaining_vcpu} format={metricNumber(locale, " vCPU")} /><ObservedValue label={t("overview.cpuTotal")} value={data.capacity.ct_cpu_capacity_vcpu} format={metricNumber(locale, " vCPU")} /><ObservedValue label={t("overview.memoryRemaining")} value={data.capacity.logical_memory_remaining} format={metricNumber(locale)} /><ObservedValue label={t("overview.memoryTotal")} value={data.capacity.ct_memory_available} format={metricNumber(locale)} /><ObservedValue label={t("overview.disk")} value={data.capacity.physical_disk_available} format={metricNumber(locale)} /></div></Panel><Panel className="pressure-panel"><PanelHeader title={t("overview.pressure")} description={t("overview.unknownNote")} /><div className="metric-grid metric-grid-compact"><ObservedValue label={t("overview.cpuSome")} value={data.pressure.cpu_some_avg10} format={metricNumber(locale)} /><ObservedValue label={t("overview.cpuFull")} value={data.pressure.cpu_full_avg10} format={metricNumber(locale)} /><ObservedValue label={t("overview.threshold")} value={data.pressure.cpu_startup_pressure_threshold_percent} format={metricNumber(locale, "%")} /><ObservedValue label={t("overview.memoryPressure")} value={data.pressure.memory_pressure} format={metricNumber(locale)} /></div></Panel></div><div className="overview-lower"><Panel><PanelHeader title={t("overview.workload")} /><div className="workload-list">{Object.entries(data.task_counts).map(([key, value]) => <div className="workload-row" key={key}><span>{t(`status.${key}`, { defaultValue: key })}</span><strong>{value}</strong></div>)}</div></Panel><Panel><PanelHeader title={t("overview.sources")} /><div className="source-list">{data.sources.map((source) => <div className="source-row" key={source.name}><div><strong>{source.name}</strong><FreshnessLine state={source.state} code={source.safe_code} observedAt={source.observed_at} /></div><span className="source-window">{source.stale_after_seconds}s</span></div>)}</div></Panel></div><div className="overview-footnote"><Server aria-hidden="true" /><span>{t("overview.staleNote")}</span><HardDrive aria-hidden="true" /><span>{t("overview.unknownNote")}</span><Gauge aria-hidden="true" /><span className="mono">{data.resource_version}</span></div></div>;
}
