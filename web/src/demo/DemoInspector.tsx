import { Check, Clipboard, PanelRight, RotateCcw, Wifi, WifiOff, X } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { demoShareUrl, setDemoControl, useDemoRuntime, type DemoDataMode, type DemoNetwork, type DemoPersona, type DemoRuntimeState, type DemoScene } from "./runtime";
import { Button, IconButton, Select, StatusBadge } from "../components/ui";

export function DemoInspector() {
  const { t } = useTranslation();
  const runtime = useDemoRuntime();
  const [open, setOpen] = useState(true);
  const [copied, setCopied] = useState(false);
  const [copyFailed, setCopyFailed] = useState(false);
  const copyLink = async () => {
    setCopied(false);
    setCopyFailed(false);
    try {
      if (!navigator.clipboard?.writeText) throw new Error("clipboard_unavailable");
      await navigator.clipboard.writeText(demoShareUrl());
      setCopied(true);
    } catch {
      setCopyFailed(true);
    }
    window.setTimeout(() => setCopied(false), 1400);
  };
  const set = <Key extends "scene" | "persona" | "network" | "dataMode">(key: Key, value: DemoRuntimeState[Key]) => setDemoControl({ [key]: value } as Partial<Pick<DemoRuntimeState, "scene" | "persona" | "network" | "dataMode">>);
  return <>
    <button className={`demo-bubble ${open ? "is-open" : ""}`} aria-label={open ? t("demo.closeInspector") : t("demo.openInspector")} onClick={() => setOpen((value) => !value)}><PanelRight aria-hidden="true" /></button>
    {open ? <aside className="demo-inspector" aria-label={t("demo.inspector")}>
      <div className="demo-inspector-header"><div><span className="eyebrow-label">{t("demo.eyebrow")}</span><h2>{t("demo.inspector")}</h2></div><IconButton label={t("demo.closeInspector")} onClick={() => setOpen(false)}><X /></IconButton></div>
      <div className="demo-safe-banner"><span className="demo-safe-dot" aria-hidden="true" /><div><strong>{t("demo.safeTitle")}</strong><span>{t("demo.safeText")}</span></div></div>
      <label className="demo-field"><span>{t("demo.scene")}</span><Select value={runtime.scene} onChange={(event) => set("scene", event.target.value as DemoScene)}><option value="normal">{t("demo.scenes.normal")}</option><option value="capacity">{t("demo.scenes.capacity")}</option><option value="conflict">{t("demo.scenes.conflict")}</option><option value="empty">{t("demo.scenes.empty")}</option></Select></label>
      <label className="demo-field"><span>{t("demo.persona")}</span><Select value={runtime.persona} onChange={(event) => set("persona", event.target.value as DemoPersona)}><option value="admin">{t("demo.personas.admin")}</option><option value="viewer">{t("demo.personas.viewer")}</option></Select></label>
      <label className="demo-field"><span>{t("demo.data")}</span><Select value={runtime.dataMode} onChange={(event) => set("dataMode", event.target.value as DemoDataMode)}><option value="full">{t("demo.dataModes.full")}</option><option value="minimal">{t("demo.dataModes.minimal")}</option></Select></label>
      <div className="demo-field"><span>{t("demo.network")}</span><div className="demo-network-toggle" role="group" aria-label={t("demo.network")}><button className={runtime.network === "online" ? "selected" : ""} onClick={() => set("network", "online")}><Wifi aria-hidden="true" />{t("demo.networkOnline")}</button><button className={runtime.network === "offline" ? "selected" : ""} onClick={() => set("network", "offline")}><WifiOff aria-hidden="true" />{t("demo.networkOffline")}</button></div></div>
      <div className="demo-state-row"><span>{t("demo.access")}</span><StatusBadge state={runtime.persona === "admin" ? "ready" : "unavailable"} /></div>
      <div className="demo-state-row"><span>{t("demo.status")}</span><StatusBadge state={runtime.network === "online" ? "available" : "failed"} /></div>
      <div className="demo-actions"><Button variant="primary" icon={copied ? <Check /> : <Clipboard />} onClick={() => void copyLink()}>{copied ? t("demo.copied") : copyFailed ? t("demo.copyFailed") : t("demo.copyShare")}</Button><Button variant="ghost" icon={<RotateCcw />} onClick={() => setDemoControl({ scene: runtime.scene, persona: runtime.persona, network: runtime.network, dataMode: runtime.dataMode })}>{t("demo.reset")}</Button></div>
      {copyFailed ? <p className="demo-copy-error" role="status">{t("demo.copyFailedText")}</p> : null}
      <p className="demo-footnote">{t("demo.urlHint")}</p>
    </aside> : null}
  </>;
}
