import { AlertTriangle, CheckCircle2, CircleAlert } from "lucide-react";
import { useTranslation } from "react-i18next";
import type { ObservationState, SafeError } from "../app/types";
import { Badge } from "./ui";

export function StatusNotice({ state, message, code }: { state: ObservationState | string; message?: string; code?: string | null }) {
  const { t } = useTranslation();
  const tone = state === "available" || state === "ready" || state === "passed" ? "success" : ["failed", "unavailable"].includes(state) ? "danger" : "warning";
  const Icon = tone === "success" ? CheckCircle2 : tone === "danger" ? CircleAlert : AlertTriangle;
  return <div className={`status-notice notice-${tone}`}><Icon aria-hidden="true" /><div><strong>{message ?? t(`errors.${code ?? "generic"}`)}</strong>{code ? <span className="mono notice-code">{code}</span> : null}</div><Badge tone={tone}>{state}</Badge></div>;
}

export function SafeErrorNotice({ error }: { error: SafeError }) {
  const { t } = useTranslation();
  return <StatusNotice state="failed" code={error.code} message={t(`errors.${error.code}`, { defaultValue: t("errors.generic") })} />;
}
