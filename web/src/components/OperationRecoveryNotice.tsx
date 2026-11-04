import { AlertTriangle, CheckCircle2, RefreshCw } from "lucide-react";
import { useTranslation } from "react-i18next";
import type { OperationRecovery } from "../app/operationRecovery";
import { Button, StatusBadge } from "./ui";

export function OperationRecoveryNotice({ recovery }: { recovery: OperationRecovery }) {
  const { t } = useTranslation();
  if (recovery.state === "idle" || !recovery.operationId) return null;
  const status = recovery.operation ? t(`status.${recovery.operation.status}`) : null;
  const message = recovery.state === "checking"
    ? t("operationRecovery.checking")
    : recovery.state === "resolved"
      ? t("operationRecovery.resolved", { status })
      : recovery.state === "blocked"
        ? t("operationRecovery.blocked", { status })
        : recovery.state === "not_found"
          ? t("operationRecovery.notFound")
          : t("operationRecovery.error");
  const tone = recovery.state === "resolved" ? "notice-success" : recovery.state === "error" ? "notice-danger" : "notice-warning";
  return <div className={`status-notice operation-recovery-notice ${tone}`} data-testid="operation-recovery-notice" role="status">
    {recovery.state === "resolved" ? <CheckCircle2 aria-hidden="true" /> : <AlertTriangle aria-hidden="true" />}
    <div>
      <strong>{t("operationRecovery.title")}</strong>
      <span>{message}</span>
      <span className="mono notice-code">{recovery.operationId}</span>
      {status ? <StatusBadge state={recovery.operation?.status ?? "unknown"} /> : null}
    </div>
    {recovery.state !== "resolved" ? <div className="operation-recovery-actions">
      <Button variant="ghost" icon={<RefreshCw />} onClick={recovery.retry}>{t("operationRecovery.retry")}</Button>
      {recovery.state === "not_found" ? <Button variant="ghost" onClick={recovery.clear}>{t("operationRecovery.clear")}</Button> : null}
    </div> : null}
  </div>;
}
