import { AlertTriangle, ArrowRight, ShieldCheck } from "lucide-react";
import { useEffect, useLayoutEffect, useRef, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import type { ActionPreview } from "../app/types";
import { formatDateTime } from "../app/format";
import { Button, Textarea } from "./ui";

export function ActionDialog({ preview, actionLabel, target, onCancel, onConfirm, dangerous = false, busy = false, error, confirmationText, returnFocusTarget, children }: { preview: ActionPreview; actionLabel: string; target: string; onCancel: () => void; onConfirm: (reason: string) => void; dangerous?: boolean; busy?: boolean; error?: string | null; confirmationText?: ReactNode; returnFocusTarget?: HTMLElement | null; children?: ReactNode }) {
  const { t, i18n } = useTranslation();
  const locale = i18n.language === "zh-CN" ? "zh-CN" : "en";
  const [reason, setReason] = useState("");
  const reasonRef = useRef<HTMLTextAreaElement>(null);
  const dialogRef = useRef<HTMLDialogElement>(null);
  const returnFocusRef = useRef<HTMLElement | null>(returnFocusTarget ?? (typeof document === "undefined" ? null : document.activeElement as HTMLElement));
  useLayoutEffect(() => {
    reasonRef.current?.focus();
    return () => {
      const targetElement = returnFocusRef.current;
      if (!targetElement) return;
      targetElement.focus();
      if (document.activeElement !== targetElement) window.setTimeout(() => targetElement.focus(), 0);
    };
  }, []);
  useEffect(() => {
    const dialog = dialogRef.current;
    if (!dialog) return;
    const inerted: Array<{ element: HTMLElement; inert: boolean; hadInertAttribute: boolean; ariaHidden: string | null }> = [];
    let current: HTMLElement = dialog;
    while (current.parentElement) {
      const parent = current.parentElement;
      for (const child of Array.from(parent.children)) {
        if (child === current || !(child instanceof HTMLElement)) continue;
        inerted.push({ element: child, inert: child.inert, hadInertAttribute: child.hasAttribute("inert"), ariaHidden: child.getAttribute("aria-hidden") });
        child.inert = true;
        child.setAttribute("inert", "");
        child.setAttribute("aria-hidden", "true");
      }
      if (parent === document.body) break;
      current = parent;
    }
    const handleFocusIn = (event: FocusEvent) => {
      if (dialog.contains(event.target as Node)) return;
      event.preventDefault();
      reasonRef.current?.focus();
    };
    const handleKeyDown = (event: KeyboardEvent) => {
      if (!dialog.contains(document.activeElement)) {
        if (event.key === "Tab") {
          event.preventDefault();
          reasonRef.current?.focus();
        }
        return;
      }
      if (event.key === "Escape") {
        event.preventDefault();
        if (!busy) onCancel();
        return;
      }
      if (event.key !== "Tab") return;
      const focusable = Array.from(dialog.querySelectorAll<HTMLElement>("button:not([disabled]), textarea:not([disabled]), input:not([disabled]), select:not([disabled]), a[href], [tabindex]:not([tabindex='-1'])"));
      if (!focusable.length) return;
      const first = focusable[0];
      const last = focusable[focusable.length - 1];
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault();
        first.focus();
      }
    };
    document.addEventListener("focusin", handleFocusIn);
    document.addEventListener("keydown", handleKeyDown);
    return () => {
      document.removeEventListener("focusin", handleFocusIn);
      document.removeEventListener("keydown", handleKeyDown);
      for (const { element, inert, hadInertAttribute, ariaHidden } of inerted) {
        element.inert = inert;
        if (hadInertAttribute) element.setAttribute("inert", "");
        else element.removeAttribute("inert");
        if (ariaHidden === null) element.removeAttribute("aria-hidden");
        else element.setAttribute("aria-hidden", ariaHidden);
      }
    };
  }, [busy, onCancel]);
  return <dialog ref={dialogRef} open className="dialog-backdrop" aria-modal="true" aria-labelledby="action-dialog-title" onCancel={(event) => { event.preventDefault(); if (!busy) onCancel(); }}><div className="action-dialog"><div className="dialog-heading"><div className={`dialog-icon ${dangerous ? "danger" : ""}`} aria-hidden="true">{dangerous ? <AlertTriangle /> : <ShieldCheck />}</div><div><h2 id="action-dialog-title">{t("vms.actionConfirm", { action: actionLabel })}</h2><p>{target}</p></div></div><div className="dialog-target-row"><span>{t("dialog.preview")}</span><span className="mono">{preview.target_version}</span></div><div className="impact-grid"><div><span>{t("dialog.impactSessions")}</span><strong>{preview.impact.affected_session_count}</strong></div><div><span>{t("dialog.impactRefs")}</span><strong>{Object.values(preview.impact.affected_image_references).reduce((total, value) => total + value, 0)}</strong></div></div><div className="dialog-changes"><span>{t("dialog.changes")}</span>{preview.impact.changes.map((change) => <div key={change.code}><span className="change-code">{change.code}</span><span>{change.value ?? change.count ?? t("dialog.noImpact")}</span></div>)}</div>{children}{error ? <p className="field-error" role="alert">{error}</p> : null}<p className="dialog-expiry">{t("dialog.expires", { time: formatDateTime(preview.expires_at, locale) })}</p><label className="field-label" htmlFor="action-reason">{t("common.reason")} <span>({t("common.optional")})</span></label><Textarea ref={reasonRef} id="action-reason" value={reason} onChange={(event) => setReason(event.target.value)} placeholder={t("common.reasonPlaceholder")} rows={3} /><p className="dialog-warning">{confirmationText ?? t("vms.actionConfirmText")}</p><div className="dialog-actions"><Button variant="ghost" disabled={busy} onClick={onCancel}>{t("common.cancel")}</Button><Button variant={dangerous ? "danger" : "primary"} disabled={busy} onClick={() => onConfirm(reason.trim())} icon={busy ? <ShieldCheck /> : <ArrowRight />}>{busy ? t("common.loading") : t("dialog.submit")}</Button></div></div></dialog>;
}
