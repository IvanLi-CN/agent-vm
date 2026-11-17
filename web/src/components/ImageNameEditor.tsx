import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { createOperationId, ManagementRequestError, previewImageName, submitImageName } from "../app/api";
import type { ActionPreview, ManagedImage } from "../app/types";
import { ActionDialog } from "./ActionDialog";
import { useOperationRecovery } from "../app/operationRecovery";
import { OperationRecoveryNotice } from "./OperationRecoveryNotice";
import { Button, Input } from "./ui";
type Intent = {
    name: string | null;
    operationId: string;
    preview: ActionPreview;
};
export function ImageNameEditor({ image, catalogVersion }: {
    image: ManagedImage;
    catalogVersion?: string;
}) {
    const { t } = useTranslation();
    const client = useQueryClient();
    const [, setOperationId, recovery] = useOperationRecovery(`/images/name/${image.fingerprint}`);
    const [name, setName] = useState("");
    const [intent, setIntent] = useState<Intent | null>(null);
    const preview = useMutation({ mutationFn: async (value: string | null) => ({ name: value, operationId: createOperationId(), preview: await previewImageName(image.fingerprint, value, catalogVersion!) }), onSuccess: next => { setOperationId(next.operationId, false); setIntent(next); } });
    const submit = useMutation({ mutationFn: async (reason: string) => submitImageName(image.fingerprint, intent!.name, reason, intent!.preview, intent!.operationId), onSuccess: () => { setOperationId(null); setIntent(null); setName(""); void client.invalidateQueries({ queryKey: ["images"] }); void client.invalidateQueries({ queryKey: ["operations"] }); }, onError: error => { const rejected = error instanceof ManagementRequestError && ["stale_target", "image_name_conflict", "image_name_invalid", "image_not_found", "invalid_request", "operation_conflict", "precondition_required"].includes(error.code); if (!rejected) {
            recovery.persist();
            recovery.block();
            setIntent(null);
        } } });
    const error = submit.error ?? preview.error;
    const message = error instanceof ManagementRequestError ? t(`errors.${error.code}`, { defaultValue: t("errors.generic") }) : error ? t("errors.generic") : null;
    const busy = preview.isPending || submit.isPending || recovery.isBlocked;
    return <div className="detail-section image-name-editor"><OperationRecoveryNotice recovery={recovery}/><h3>{t("images.name")}</h3><p>{image.name ?? t("images.unnamed")}</p><p className="detail-hint">{t("images.nameHint")}</p>
 <label className="field-label">{t("images.name")}<Input value={name} maxLength={256} onChange={event => setName(event.target.value)} disabled={busy || !!image.name}/></label>
 <Button variant="secondary" disabled={busy || !catalogVersion || !!image.name || !name.trim()} onClick={() => { submit.reset(); preview.mutate(name.normalize("NFC").trim()); }}>{t("images.editName")}</Button>
 <Button variant="ghost" disabled={busy || !catalogVersion || !image.name} onClick={() => { submit.reset(); preview.mutate(null); }}>{t("images.clearName")}</Button>
 {!intent && message ? <p role="alert">{message}</p> : null}
 {intent ? <ActionDialog confirmationText={t("images.nameHint")} actionLabel={t("images.confirmName")} target={intent.name ?? image.name ?? t("images.unnamed")} preview={intent.preview} busy={submit.isPending} error={message} onCancel={() => { setIntent(null); submit.reset(); }} onConfirm={reason => { recovery.persist(); submit.mutate(reason); }}>
 </ActionDialog> : null}</div>;
}
