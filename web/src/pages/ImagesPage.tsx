import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Check, ChevronLeft, ChevronRight, FileUp, Trash2 } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { actionLabel, formatDateTime, shortFingerprint } from "../app/format";
import { ManagementRequestError, createOperationId, listImages, previewImageAction, startImageImport, submitImageAction, uploadImageImportContent } from "../app/api";
import type { ActionPreview, ImageImportRequest, Locale, ManagedImage } from "../app/types";
import { auditReasonWithinLimit, optionalAuditReason } from "../app/validation";
import { ActionDialog } from "../components/ActionDialog";
import { Select, SelectContent, SelectGroup, SelectItem, SelectTrigger, SelectValue } from "../components/ui/select";
import { CopyButton, EmptyState, Input, Panel, PanelHeader, StatusBadge, Button } from "../components/ui";
import { DefinitionList, PageIntro, QueryError } from "../components/PagePrimitives";

type ImageAction = "default" | "delete" | "retire";
type ImportSubmission = { request: ImageImportRequest; content?: File | { metadata: File; rootfs: File } };
const importRejectedCodes = new Set(["invalid_request", "source_not_allowed", "permission_denied", "stale_target", "operation_conflict", "not_found", "upload_too_large", "staging_capacity_low", "fingerprint_mismatch"]);

export function ImagesPage() {
  const { t, i18n } = useTranslation();
  const locale: Locale = i18n.language === "zh-CN" ? "zh-CN" : "en";
  const client = useQueryClient();
  const [search, setSearch] = useState("");
  const [cursor, setCursor] = useState<string | undefined>();
  const [cursorStack, setCursorStack] = useState<string[]>([]);
  const [selectedFingerprint, setSelectedFingerprint] = useState<string | null>(null);
  const [sourceKind, setSourceKind] = useState<"trusted_remote" | "local_upload">("trusted_remote");
  const [format, setFormat] = useState<"unified" | "split">("unified");
  const [versionLabel, setVersionLabel] = useState("2026.02");
  const [sourceId, setSourceId] = useState("ubuntu-staging");
  const [expectedFingerprint, setExpectedFingerprint] = useState("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa");
  const [expectedSha, setExpectedSha] = useState("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc");
  const [expectedMetadataSha, setExpectedMetadataSha] = useState("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc");
  const [expectedRootfsSha, setExpectedRootfsSha] = useState("dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd");
  const [expectedSize, setExpectedSize] = useState("");
  const [uploadFile, setUploadFile] = useState<File | null>(null);
  const [metadataFile, setMetadataFile] = useState<File | null>(null);
  const [rootfsFile, setRootfsFile] = useState<File | null>(null);
  const [reason, setReason] = useState("");
  const importReasonTooLong = !auditReasonWithinLimit(reason);
  const [importMessage, setImportMessage] = useState("");
  const [pendingImport, setPendingImport] = useState<ImportSubmission | null>(null);
  const [action, setAction] = useState<ImageAction | null>(null);
  const [preview, setPreview] = useState<ActionPreview | null>(null);
  const [operationId, setOperationId] = useState<string | null>(null);
  const query = useQuery({ queryKey: ["images", search, cursor], queryFn: () => listImages(search, cursor), refetchInterval: 10_000, refetchIntervalInBackground: false });
  const selected = query.data?.items.find((image) => image.fingerprint === selectedFingerprint) ?? query.data?.items[0] ?? null;
  const importMutation = useMutation({
    mutationFn: async ({ request, content }: ImportSubmission) => {
      const operation = await startImageImport(request);
      return content && operation.status === "awaiting_upload" ? uploadImageImportContent(operation.operation_id, content) : operation;
    },
    onSuccess: (operation) => {
      if (operation.status === "failed") {
        setImportMessage(t(`errors.${operation.error?.code ?? "generic"}`, { defaultValue: t("errors.generic") }));
        setPendingImport(null);
        return;
      }
      if (operation.status === "outcome_unknown") {
        setImportMessage(t("errors.authority_unavailable"));
        return;
      }
      setImportMessage(`${t("images.importStarted")} · ${operation.operation_id}`);
      setPendingImport(null);
      setReason("");
      void client.invalidateQueries({ queryKey: ["operations"] });
    },
    onError: (error) => {
      if (error instanceof ManagementRequestError && importRejectedCodes.has(error.code)) setPendingImport(null);
      setImportMessage(error instanceof ManagementRequestError ? t(`errors.${error.code}`, { defaultValue: t("errors.generic") }) : t("errors.generic"));
    },
  });
  const actionMutation = useMutation({ mutationFn: ({ reason: actionReason }: { reason: string }) => submitImageAction(selected?.fingerprint ?? "", action!, actionReason, preview!, operationId ?? undefined), onSuccess: () => { setAction(null); setPreview(null); setOperationId(null); void client.invalidateQueries({ queryKey: ["images"] }); void client.invalidateQueries({ queryKey: ["operations"] }); }, onError: (error) => setImportMessage(error instanceof ManagementRequestError ? t(`errors.${error.code}`, { defaultValue: t("errors.generic") }) : t("errors.generic")) });

  const submitImport = (submission: ImportSubmission) => {
    setPendingImport(submission);
    importMutation.mutate(submission);
  };

  const startImport = () => {
    if (importReasonTooLong) { setImportMessage(t("common.reasonTooLong")); return; }
    if (pendingImport) {
      importMutation.mutate(pendingImport);
      return;
    }
    const targetVersion = query.data?.catalog_version;
    if (!targetVersion) { setImportMessage(t("images.targetUnavailable")); return; }
    const operation_id = createOperationId();
    const providedReason = optionalAuditReason(reason);
    const optionalReason = providedReason === undefined ? {} : { reason: providedReason };
    if (sourceKind === "trusted_remote") {
      const request: ImageImportRequest = { operation_id, ...optionalReason, target_version: targetVersion, version_label: versionLabel, source: { kind: "trusted_remote", source_id: sourceId, expected_fingerprint: expectedFingerprint } };
      submitImport({ request });
      return;
    }
    if (format === "unified") {
      if (!uploadFile) { setImportMessage(t("images.fileRequired")); return; }
      if (!expectedSha.trim()) { setImportMessage(t("images.expectedShaRequired")); return; }
      const request: ImageImportRequest = { operation_id, ...optionalReason, target_version: targetVersion, version_label: versionLabel, source: { kind: "local_upload", format: "unified", expected_sha256: expectedSha.trim(), expected_size_bytes: String(uploadFile.size) } };
      submitImport({ request, content: uploadFile });
      return;
    }
    if (!metadataFile || !rootfsFile) { setImportMessage(t("images.fileRequired")); return; }
    if (!expectedMetadataSha.trim() || !expectedRootfsSha.trim()) { setImportMessage(t("images.expectedShaRequired")); return; }
    const request: ImageImportRequest = { operation_id, ...optionalReason, target_version: targetVersion, version_label: versionLabel, source: { kind: "local_upload", format: "split", metadata: { expected_sha256: expectedMetadataSha.trim(), expected_size_bytes: String(metadataFile.size) }, rootfs: { expected_sha256: expectedRootfsSha.trim(), expected_size_bytes: String(rootfsFile.size) } } };
    submitImport({ request, content: { metadata: metadataFile, rootfs: rootfsFile } });
  };

  const nextPage = () => { const next = query.data?.page.next_cursor; if (!next) return; setCursorStack((stack) => [...stack, cursor ?? ""]); setCursor(next); };
  const previousPage = () => {
    const previous = cursorStack[cursorStack.length - 1];
    setCursorStack((stack) => stack.slice(0, -1));
    setCursor(previous || undefined);
  };

  const openAction = async (nextAction: ImageAction) => {
    if (!selected) return;
    setAction(nextAction);
    setOperationId(createOperationId());
    const targetVersion = nextAction === "default" ? query.data?.selection_version : selected.resource_version;
    if (!targetVersion) return;
    try {
      setPreview(await previewImageAction(selected.fingerprint, nextAction, targetVersion));
    } catch (error) {
      setAction(null);
      setImportMessage(error instanceof ManagementRequestError ? t(`errors.${error.code}`, { defaultValue: t("errors.generic") }) : t("errors.generic"));
    }
  };

  return <div className="page-stack" data-testid="images-page">
    <PageIntro title={t("images.title")} description={t("images.subtitle")} onRefresh={() => void query.refetch()} refreshing={query.isFetching} />
    <div className="split-layout images-layout">
      <Panel className="list-panel">
        <PanelHeader title={t("images.listTitle")} description={t("images.count", { count: query.data?.items.length ?? 0 })} />
        <div className="image-search"><Input value={search} onChange={(event) => { setSearch(event.target.value); setCursor(undefined); setCursorStack([]); }} placeholder={t("common.search")} /></div>
        {query.isPending ? <div className="loading-rows"><span /><span /><span /></div> : query.isError ? <QueryError onRetry={() => void query.refetch()} /> : <div className="resource-list" role="listbox" aria-label={t("images.listTitle")}>
          {query.data.items.map((image) => <button className={`resource-row image-row ${selected?.fingerprint === image.fingerprint ? "is-selected" : ""}`} key={image.fingerprint} role="option" aria-selected={selected?.fingerprint === image.fingerprint} onClick={() => setSelectedFingerprint(image.fingerprint)}>
            <div className="resource-main"><strong>{image.version_label}</strong><span className="mono">{shortFingerprint(image.fingerprint)}</span></div>
            <div className="resource-summary"><StatusBadge state={image.state} />{image.is_default ? <span className="default-label">{t("images.default")}</span> : null}</div>
            <div className="resource-meta"><span>{image.architecture}</span><span>{image.source.kind === "legacy_config" ? t("images.legacy") : image.source.kind}</span></div>
          </button>)}
        </div>}<div className="pagination"><span>{query.data?.items.length ?? 0}</span><div><Button variant="ghost" aria-label={t("common.previous")} disabled={!cursorStack.length} onClick={previousPage} icon={<ChevronLeft />} /><Button variant="ghost" aria-label={t("common.next")} disabled={!query.data?.page.next_cursor} onClick={nextPage} icon={<ChevronRight />} /></div></div>
      </Panel>
      <ImageDetail image={selected} locale={locale} onAction={openAction} />
    </div>
    <Panel className="import-panel">
      <PanelHeader title={t("images.import")} description={t("images.uploadHint")} />
      <div className="import-mode" role="tablist" aria-label={t("images.import")}>
        <button className={sourceKind === "trusted_remote" ? "active" : ""} role="tab" aria-selected={sourceKind === "trusted_remote"} onClick={() => setSourceKind("trusted_remote")}>{t("images.trusted")}</button>
        <button className={sourceKind === "local_upload" ? "active" : ""} role="tab" aria-selected={sourceKind === "local_upload"} onClick={() => setSourceKind("local_upload")}>{t("images.local")}</button>
      </div>
      <div className="form-grid">
        <label className="field-label">{t("images.version")}<Input value={versionLabel} onChange={(event) => setVersionLabel(event.target.value)} /></label>
        {sourceKind === "trusted_remote" ? <>
          <label className="field-label">{t("images.sourceId")}<Input value={sourceId} onChange={(event) => setSourceId(event.target.value)} /></label>
          <label className="field-label field-span-2">{t("images.expectedFingerprint")}<Input className="mono" value={expectedFingerprint} onChange={(event) => setExpectedFingerprint(event.target.value)} /></label>
        </> : <>
          <label className="field-label">{t("images.packageFormat")}<Select value={format} onValueChange={(value) => setFormat(value as "unified" | "split")}><SelectTrigger aria-label={t("images.packageFormat")}><SelectValue /></SelectTrigger><SelectContent><SelectGroup><SelectItem value="unified">{t("images.unified")}</SelectItem><SelectItem value="split">{t("images.split")}</SelectItem></SelectGroup></SelectContent></Select></label>
          {format === "unified" ? <>
            <label className="field-label">{t("images.expectedSize")}<Input className="mono" value={expectedSize} readOnly placeholder="—" /></label>
            <label className="field-label">{t("images.expectedSha")}<Input className="mono" value={expectedSha} onChange={(event) => setExpectedSha(event.target.value)} /></label>
            <label className="file-field field-span-2"><span>{t("images.packageFile")}</span><span className="file-input"><FileUp aria-hidden="true" /><input type="file" accept="application/octet-stream,.tar,.tar.gz" onChange={(event) => { const file = event.target.files?.[0] ?? null; setUploadFile(file); setExpectedSize(file ? String(file.size) : ""); }} /><span>{uploadFile?.name ?? t("images.chooseFile")}</span></span></label>
          </> : <>
            <label className="field-label">{t("images.expectedMetadataSha")}<Input className="mono" value={expectedMetadataSha} onChange={(event) => setExpectedMetadataSha(event.target.value)} /></label>
            <label className="field-label">{t("images.expectedRootfsSha")}<Input className="mono" value={expectedRootfsSha} onChange={(event) => setExpectedRootfsSha(event.target.value)} /></label>
            <label className="file-field"><span>{t("images.metadataFile")}</span><span className="file-input"><FileUp aria-hidden="true" /><input type="file" accept="application/octet-stream,.tar,.tar.gz" onChange={(event) => setMetadataFile(event.target.files?.[0] ?? null)} /><span>{metadataFile?.name ?? t("images.chooseFile")}</span></span></label>
            <label className="file-field"><span>{t("images.rootfsFile")}</span><span className="file-input"><FileUp aria-hidden="true" /><input type="file" accept="application/octet-stream,.tar,.tar.gz" onChange={(event) => setRootfsFile(event.target.files?.[0] ?? null)} /><span>{rootfsFile?.name ?? t("images.chooseFile")}</span></span></label>
          </>}
        </>}
        <label className="field-label field-span-2">{t("images.importReason")}<Input value={reason} aria-invalid={importReasonTooLong ? "true" : undefined} aria-describedby={importReasonTooLong ? "image-import-reason-error" : undefined} onChange={(event) => { setReason(event.target.value); setImportMessage(""); }} placeholder={t("common.reasonPlaceholder")} /></label>
      </div>
      <div className="import-footer"><span id={importReasonTooLong ? "image-import-reason-error" : undefined} role={importReasonTooLong ? "alert" : undefined} className={!importReasonTooLong && (importMessage.includes("created") || importMessage.includes("已创建")) ? "success-copy" : "field-error"}>{importReasonTooLong ? t("common.reasonTooLong") : importMessage}</span><Button variant="primary" icon={<FileUp />} disabled={importMutation.isPending || importReasonTooLong} onClick={startImport}>{importMutation.isPending ? t("common.loading") : t("images.startImport")}</Button></div>
    </Panel>
    {action && preview && selected ? <ActionDialog preview={preview} actionLabel={actionLabel(action === "default" ? "default_image_select" : action === "delete" ? "image_delete" : "image_retire", locale)} target={`${selected.version_label} · ${shortFingerprint(selected.fingerprint)}`} dangerous={action === "delete" || action === "retire"} busy={actionMutation.isPending} onCancel={() => { setAction(null); setPreview(null); setOperationId(null); }} onConfirm={(actionReason) => actionMutation.mutate({ reason: actionReason })} /> : null}
  </div>;
}

function ImageDetail({ image, locale, onAction }: { image: ManagedImage | null; locale: Locale; onAction: (action: ImageAction) => void }) {
  const { t } = useTranslation();
  if (!image) return <Panel className="detail-rail"><EmptyState title={t("images.detailEmpty")} /></Panel>;
  const totalRefs = Object.values(image.references).reduce((sum, value) => sum + value, 0);
  const canDelete = totalRefs === 0 && !image.is_default;
  return <Panel className="detail-rail"><div className="detail-identity"><div><span className="eyebrow-label">{t("images.selected")}</span><h2>{image.version_label}</h2><span className="mono">{shortFingerprint(image.fingerprint)}</span></div>{image.is_default ? <StatusBadge state="ready" /> : <StatusBadge state={image.state} />}</div><div className="identity-line"><span className="mono">{image.fingerprint}</span><CopyButton value={image.fingerprint} /></div><DefinitionList items={[{ label: t("images.version"), value: image.version_label }, { label: t("images.architecture"), value: image.architecture }, { label: t("images.source"), value: image.source.kind === "legacy_config" ? t("images.legacy") : image.source.source_id ?? image.source.kind }, { label: t("images.created"), value: formatDateTime(image.created_at, locale) }, { label: t("images.default"), value: image.is_default ? <span className="success-copy">{t("images.defaultForNew")}</span> : "—" }]} /><div className="detail-section"><h3>{t("images.integrity")}</h3><div className="verification-row"><StatusBadge state={image.verification.integrity} /><span>{image.verification.integrity === "legacy_unverified" ? t("status.unverified") : t("status.passed")}</span></div><h3>{t("images.suitability")}</h3><div className="verification-row"><StatusBadge state={image.verification.suitability} /><span>{image.verification.suitability === "passed" ? <><Check aria-hidden="true" /> {t("status.passed")}</> : t("status.unverified")}</span></div></div><div className="detail-section"><h3>{t("images.references")}</h3><div className="reference-list"><Reference label={t("images.referenceDefault")} value={image.references.default} /><Reference label={t("images.referenceActive")} value={image.references.active} /><Reference label={t("images.referenceCached")} value={image.references.cached} /><Reference label={t("images.referenceReleasing")} value={image.references.releasing} /><Reference label={t("images.referencePending")} value={image.references.pending_creation} /><Reference label={t("images.referenceVerify")} value={image.references.verification} /></div></div><div className="detail-section image-actions"><h3>{canDelete ? t("images.safeToDelete") : t("images.cannotDelete")}</h3><p className="detail-hint">{canDelete ? t("images.safeToDelete") : t("images.deleteBlocked")}</p><Button variant="secondary" disabled={image.is_default || image.state === "retired" || image.verification.suitability !== "passed"} onClick={() => onAction("default")}>{t("images.changeDefault")}</Button><Button variant="ghost" disabled={image.is_default} onClick={() => onAction("retire")}>{t("images.retire")}</Button><Button variant="danger" disabled={!canDelete} icon={<Trash2 />} onClick={() => onAction("delete")}>{t("images.delete")}</Button></div></Panel>;
}

function Reference({ label, value }: { label: string; value: number }) {
  return <div><span>{label}</span><strong>{value}</strong></div>;
}
