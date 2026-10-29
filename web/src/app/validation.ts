export const AUDIT_REASON_MAX_LENGTH = 512;

export function optionalAuditReason(reason: string | null | undefined): string | undefined {
  return reason && reason.trim() ? reason : undefined;
}
