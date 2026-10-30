export const AUDIT_REASON_MAX_LENGTH = 512;

export function optionalAuditReason(reason: string | null | undefined): string | undefined {
  return reason && reason.trim() ? reason : undefined;
}

export function auditReasonCharacterLength(reason: string): number {
  return Array.from(reason).length;
}

export function auditReasonWithinLimit(reason: string): boolean {
  return auditReasonCharacterLength(reason) <= AUDIT_REASON_MAX_LENGTH;
}
