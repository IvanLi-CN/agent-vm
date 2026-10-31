import { forwardRef, type ButtonHTMLAttributes, type InputHTMLAttributes, type ReactNode, type TextareaHTMLAttributes } from "react";
import { Check, CircleAlert, Copy, LoaderCircle } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { stateLabel, stateTone } from "../app/format";
import { Button as ShadcnButton, type ButtonProps as ShadcnButtonProps } from "./ui/button";

type Tone = "neutral" | "success" | "warning" | "danger";

export function Button({ variant = "secondary", tone = "neutral", icon, children, className, ...props }: ButtonHTMLAttributes<HTMLButtonElement> & { variant?: "primary" | "secondary" | "ghost" | "danger"; tone?: Tone; icon?: ReactNode; className?: string }) {
  const mappedVariant: ShadcnButtonProps["variant"] = variant === "primary" ? "default" : variant === "danger" ? "destructive" : variant === "secondary" ? "outline" : "ghost";
  return <ShadcnButton variant={mappedVariant} className={`ui-button ui-button-${variant} ui-button-${tone} ${className ?? ""}`} {...props}>{icon ? <span className="button-icon" aria-hidden="true">{icon}</span> : null}<span>{children}</span></ShadcnButton>;
}

export function IconButton({ label, children, className, ...props }: ButtonHTMLAttributes<HTMLButtonElement> & { label: string }) {
  return <ShadcnButton variant="ghost" size="icon" className={`icon-button ${className ?? ""}`} aria-label={label} title={label} {...props}>{children}</ShadcnButton>;
}

export function Panel({ as: Tag = "section", className = "", children, ...props }: { as?: "section" | "div"; className?: string; children: ReactNode } & React.HTMLAttributes<HTMLElement>) {
  return <Tag className={`panel ${className}`} {...props}>{children}</Tag>;
}

export function PanelHeader({ title, description, action }: { title: ReactNode; description?: ReactNode; action?: ReactNode }) {
  return <div className="panel-header"><div><h2>{title}</h2>{description ? <p>{description}</p> : null}</div>{action ? <div className="panel-action">{action}</div> : null}</div>;
}

export function Badge({ tone = "neutral", children }: { tone?: Tone; children: ReactNode }) {
  return <span className={`status-badge status-${tone}`}>{children}</span>;
}

export function StatusBadge({ state }: { state: string }) {
  const { i18n } = useTranslation();
  const label = stateLabel(state, i18n.language === "zh-CN" ? "zh-CN" : "en");
  return <Badge tone={stateTone(state)}>{label}</Badge>;
}

export function Input(props: InputHTMLAttributes<HTMLInputElement>) {
  return <input className="form-control" {...props} />;
}

export const Textarea = forwardRef<HTMLTextAreaElement, TextareaHTMLAttributes<HTMLTextAreaElement>>(function Textarea(props, ref) {
  return <textarea ref={ref} className="form-control form-textarea" {...props} />;
});

export function ProgressBar({ value, tone = "accent" }: { value: number; tone?: "accent" | "success" | "danger" }) {
  return <div className="progress-track" aria-label={`${Math.round(value)}%`}><span className={`progress-fill progress-${tone}`} style={{ width: `${Math.max(0, Math.min(value, 100))}%` }} /></div>;
}

export function Divider() {
  return <div className="divider" role="separator" />;
}

export function SectionHeader({ title, description, action }: { title: ReactNode; description?: ReactNode; action?: ReactNode }) {
  return <div className="section-header"><div><h2>{title}</h2>{description ? <p>{description}</p> : null}</div>{action ? <div>{action}</div> : null}</div>;
}

export function EmptyState({ title, description }: { title: ReactNode; description?: ReactNode }) {
  return <div className="empty-state"><div className="empty-mark" aria-hidden="true">—</div><strong>{title}</strong>{description ? <p>{description}</p> : null}</div>;
}

export function CopyButton({ value }: { value: string }) {
  const { t } = useTranslation();
  const [copied, setCopied] = useState(false);
  const [copyFailed, setCopyFailed] = useState(false);
  const copy = async () => {
    try {
      if (!navigator.clipboard?.writeText) throw new Error("clipboard_unavailable");
      await navigator.clipboard.writeText(value);
      setCopyFailed(false);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1200);
    } catch {
      setCopied(false);
      setCopyFailed(true);
    }
  };
  return <IconButton label={copied ? t("common.copied") : copyFailed ? t("common.copyFailed") : t("common.copy")} onClick={() => void copy()}>{copied ? <Check /> : copyFailed ? <CircleAlert /> : <Copy />}</IconButton>;
}

export function LoadingRows({ rows = 3 }: { rows?: number }) {
  return <div className="loading-rows" aria-label="Loading">{Array.from({ length: rows }, (_, index) => <div className="loading-row" key={index}><span /><span /><span /></div>)}</div>;
}

export function Spinner() {
  return <LoaderCircle className="spinner" aria-label="Loading" />;
}
