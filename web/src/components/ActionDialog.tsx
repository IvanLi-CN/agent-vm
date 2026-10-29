import { AlertTriangle, ArrowRight, ShieldCheck } from "lucide-react";
import { useEffect, useRef, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import type { ActionPreview } from "../app/types";
import { formatDateTime } from "../app/format";
import { Button, Textarea } from "./ui";

export function ActionDialog({ preview, actionLabel, target, onCancel, onConfirm, dangerous = false, busy = false, error, children }: { preview: ActionPreview; actionLabel: string; target: string; onCancel: () => void; onConfirm: (reason: string) => void; dangerous?: boolean; busy?: boolean; error?: string | null; children?: ReactNode }) {
  const { t, i18n } = useTranslation();
  const locale = i18n.language === "zh-CN" ? "zh-CN" : "en";
  const [reason, setReason] = useState("");
  const reasonRef = useRef<HTMLTextAreaElement>(null);
  const returnFocusRef = useRef<HTMLElement | null>(typeof document === "undefined" ? null : document.activeElement as HTMLElement);
  useEffect(() => {
    reasonRef.current?.focus();
    return () => returnFocusRef.current?.focus();
  }, []);
  return <dialog open className="dialog-backdrop" aria-modal="true" aria-labelledby="action-dialog-title" onCancel={(event) => { event.preventDefault(); onCancel(); }}><div className="action-dialog"><div className="dialog-heading"><div className={`dialog-icon ${dangerous ? "danger" : ""}`} aria-hidden="true">{dangerous ? <AlertTriangle /> : <ShieldCheck />}</div><div><h2 id="action-dialog-title">{t("vms.actionConfirm", { action: actionLabel })}</h2><p>{target}</p></div></div><div className="dialog-target-row"><span>{t("dialog.preview")}</span><span className="mono">{preview.target_version}</span></div><div className="impact-grid"><div><span>{t("dialog.impactSessions")}</span><strong>{preview.impact.affected_session_count}</strong></div><div><span>{t("dialog.impactRefs")}</span><strong>{Object.values(preview.impact.affected_image_references).reduce((total, value) => total + value, 0)}</strong></div></div><div className="dialog-changes"><span>{t("dialog.changes")}</span>{preview.impact.changes.map((change) => <div key={change.code}><span className="change-code">{change.code}</span><span>{change.value ?? change.count ?? t("dialog.noImpact")}</span></div>)}</div>{children}{error ? <p className="field-error" role="alert">{error}</p> : null}<p className="dialog-expiry">{t("dialog.expires", { time: formatDateTime(preview.expires_at, locale) })}</p><label className="field-label" htmlFor="action-reason">{t("common.reason")} <span>({t("common.optional")})</span></label><Textarea ref={reasonRef} id="action-reason" value={reason} onChange={(event) => setReason(event.target.value)} placeholder={t("common.reasonPlaceholder")} rows={3} /><p className="dialog-warning">{t("vms.actionConfirmText")}</p><div className="dialog-actions"><Button variant="ghost" onClick={onCancel}>{t("common.cancel")}</Button><Button variant={dangerous ? "danger" : "primary"} disabled={busy} onClick={() => onConfirm(reason.trim())} icon={busy ? <ShieldCheck /> : <ArrowRight />}>{busy ? t("common.loading") : t("dialog.submit")}</Button></div></div></dialog>;
}
