import type { AuditActor, Locale, ObservationState } from "./types";

export function formatDateTime(value: string | null, locale: Locale): string {
  if (!value) return "—";
  return new Intl.DateTimeFormat(locale, { year: "numeric", month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", timeZoneName: "short" }).format(new Date(value));
}

export function formatBytes(value: string | null, locale: Locale): string {
  if (!value) return "—";
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let amount = Number(value);
  let index = 0;
  while (amount >= 1024 && index < units.length - 1) {
    amount /= 1024;
    index += 1;
  }
  return `${new Intl.NumberFormat(locale, { maximumFractionDigits: index === 0 ? 0 : 1 }).format(amount)} ${units[index]}`;
}

export function formatNumber(value: number | null, locale: Locale, maximumFractionDigits = 1): string {
  if (value === null) return "—";
  return new Intl.NumberFormat(locale, { maximumFractionDigits }).format(value);
}

export function shortFingerprint(value: string): string {
  return `${value.slice(0, 8)}…${value.slice(-6)}`;
}

export function stateTone(state: ObservationState | string): "success" | "warning" | "danger" | "neutral" {
  if (["available", "passed", "ready", "succeeded", "active", "effective"].includes(state)) return "success";
  if (["stale", "unverified", "running", "queued", "reconciling", "preparing", "verifying", "unknown", "outcome_unknown"].includes(state)) return "warning";
  if (["failed", "degraded", "unavailable", "destroy", "expired", "retired", "revoked"].includes(state)) return "danger";
  return "neutral";
}

export function stateLabel(state: string, locale: Locale): string {
  const labels: Record<string, [string, string]> = {
    available: ["可用", "Available"], stale: ["已过期", "Stale"], unverified: ["未验证", "Unverified"], missing: ["缺失", "Missing"], failed: ["失败", "Failed"],
    active: ["运行中", "Active"], cached: ["已缓存", "Cached"], preparing: ["准备中", "Preparing"], closing: ["收尾中", "Closing"], deleting: ["删除中", "Deleting"], absent: ["不存在", "Absent"],
    effective: ["有效租约", "Effective lease"], expired: ["已过期租约", "Expired lease"], ready: ["就绪", "Ready"], degraded: ["降级", "Degraded"], unavailable: ["不可用", "Unavailable"], unknown: ["未知", "Unknown"],
    passed: ["通过", "Passed"], retired: ["已退役", "Retired"], verifying: ["验证中", "Verifying"], running: ["运行中", "Running"], queued: ["排队中", "Queued"], reconciling: ["恢复核对", "Reconciling"], succeeded: ["已完成", "Succeeded"], outcome_unknown: ["结果未知", "Outcome unknown"], revoked: ["已撤销", "Revoked"],
  };
  return labels[state]?.[locale === "zh-CN" ? 0 : 1] ?? state;
}

export function actionLabel(action: string, locale: Locale): string {
  const labels: Record<string, [string, string]> = {
    restart: ["重启", "Restart"], resize: ["扩容", "Resize"], release: ["释放租约", "Release lease"], destroy: ["销毁", "Destroy"], port_add: ["添加端口", "Add port"], port_remove: ["移除端口", "Remove port"], image_import: ["导入镜像", "Import image"], default_image_select: ["切换默认镜像", "Select default image"], image_retire: ["退役镜像", "Retire image"], image_delete: ["删除镜像", "Delete image"], drain_enter: ["进入排空", "Enter drain"], reconcile: ["恢复核对", "Reconcile"], gc: ["回收缓存", "Garbage collect"], token_create: ["创建 Token", "Create token"], token_revoke: ["撤销 Token", "Revoke token"],
  };
  return labels[action]?.[locale === "zh-CN" ? 0 : 1] ?? action;
}

export function formatActor(actor: AuditActor): string {
  if ("subject" in actor) return `${actor.subject} · ${actor.issuer}`;
  if ("raw_actor" in actor) return `${actor.raw_actor} · ${actor.kind}`;
  return actor.kind;
}
