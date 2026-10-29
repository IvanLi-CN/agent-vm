import { Check, Clipboard, Minimize2, Move, PanelLeft, PanelRight, RotateCcw, Wifi, WifiOff } from "lucide-react";
import { useEffect, useRef, useState, type PointerEvent } from "react";
import { useTranslation } from "react-i18next";
import { demoShareUrl, getDemoInspectorPosition, saveDemoInspectorPosition, setDemoControl, useDemoRuntime, type DemoDataMode, type DemoInspectorLayout, type DemoInspectorPosition, type DemoPersona, type DemoRuntimeState, type DemoScene } from "./runtime";
import { Select, SelectContent, SelectGroup, SelectItem, SelectTrigger, SelectValue } from "../components/Select";
import { Button, IconButton, StatusBadge } from "../components/ui";

type ExpandedLayout = Exclude<DemoInspectorLayout, "minimized">;

interface DemoInspectorProps {
  layout?: DemoInspectorLayout;
  onLayoutChange?: (layout: DemoInspectorLayout) => void;
}

function LayoutIcon({ layout }: { layout: DemoInspectorLayout }) {
  if (layout === "left") return <PanelLeft aria-hidden="true" />;
  if (layout === "right") return <PanelRight aria-hidden="true" />;
  if (layout === "minimized") return <Minimize2 aria-hidden="true" />;
  return <Move aria-hidden="true" />;
}

function clampPosition(position: DemoInspectorPosition, width: number, height: number): DemoInspectorPosition {
  const margin = 8;
  return {
    left: Math.min(Math.max(margin, position.left), Math.max(margin, window.innerWidth - width - margin)),
    top: Math.min(Math.max(margin, position.top), Math.max(margin, window.innerHeight - height - margin)),
  };
}

export function DemoInspector({ layout: controlledLayout, onLayoutChange }: DemoInspectorProps = {}) {
  const { t } = useTranslation();
  const runtime = useDemoRuntime();
  const [localLayout, setLocalLayout] = useState<DemoInspectorLayout>(controlledLayout ?? "floating");
  const [menuOpen, setMenuOpen] = useState(false);
  const [copied, setCopied] = useState(false);
  const [copyFailed, setCopyFailed] = useState(false);
  const [floatingPosition, setFloatingPosition] = useState<DemoInspectorPosition | null>(() => getDemoInspectorPosition());
  const [dragging, setDragging] = useState(false);
  const menuRef = useRef<HTMLDivElement>(null);
  const inspectorRef = useRef<HTMLElement>(null);
  const lastExpandedLayout = useRef<ExpandedLayout>(controlledLayout && controlledLayout !== "minimized" ? controlledLayout : "floating");
  const longPressTimer = useRef<number | null>(null);
  const suppressClick = useRef(false);
  const floatingPositionRef = useRef(floatingPosition);
  const dragRef = useRef<{ pointerId: number; offsetX: number; offsetY: number } | null>(null);
  const layout = controlledLayout ?? localLayout;
  const isMinimized = layout === "minimized";

  useEffect(() => {
    floatingPositionRef.current = floatingPosition;
  }, [floatingPosition]);

  useEffect(() => {
    if (controlledLayout && controlledLayout !== "minimized") lastExpandedLayout.current = controlledLayout;
  }, [controlledLayout]);

  useEffect(() => {
    if (!menuOpen) return;
    const closeOnOutsidePointer = (event: globalThis.PointerEvent) => {
      if (menuRef.current && !menuRef.current.contains(event.target as Node)) setMenuOpen(false);
    };
    const closeOnEscape = (event: KeyboardEvent) => {
      if (event.key === "Escape") setMenuOpen(false);
    };
    document.addEventListener("pointerdown", closeOnOutsidePointer);
    document.addEventListener("keydown", closeOnEscape);
    return () => {
      document.removeEventListener("pointerdown", closeOnOutsidePointer);
      document.removeEventListener("keydown", closeOnEscape);
    };
  }, [menuOpen]);

  useEffect(() => {
    if (layout !== "floating") return;
    const keepPositionInViewport = () => {
      const inspector = inspectorRef.current;
      const current = floatingPositionRef.current;
      if (!inspector || !current) return;
      const rect = inspector.getBoundingClientRect();
      const next = clampPosition(current, rect.width, rect.height);
      if (next.left === current.left && next.top === current.top) return;
      floatingPositionRef.current = next;
      setFloatingPosition(next);
    };
    keepPositionInViewport();
    window.addEventListener("resize", keepPositionInViewport);
    return () => window.removeEventListener("resize", keepPositionInViewport);
  }, [layout]);

  const clearLongPress = () => {
    if (longPressTimer.current !== null) window.clearTimeout(longPressTimer.current);
    longPressTimer.current = null;
  };

  const changeLayout = (next: DemoInspectorLayout) => {
    if (next !== "minimized") lastExpandedLayout.current = next;
    if (!controlledLayout) setLocalLayout(next);
    onLayoutChange?.(next);
    setMenuOpen(false);
  };

  const restoreLayout = () => changeLayout(lastExpandedLayout.current);

  const handleLayoutPointerDown = (event: PointerEvent<HTMLButtonElement>) => {
    clearLongPress();
    if (event.button !== 0) return;
    longPressTimer.current = window.setTimeout(() => {
      suppressClick.current = true;
      setMenuOpen(true);
    }, 550);
  };

  const handleLayoutClick = () => {
    if (suppressClick.current) {
      suppressClick.current = false;
      return;
    }
    setMenuOpen((value) => !value);
  };

  const handleLayoutContextMenu = (event: React.MouseEvent<HTMLButtonElement>) => {
    event.preventDefault();
    clearLongPress();
    setMenuOpen(true);
  };

  const handleDragStart = (event: PointerEvent<HTMLDivElement>) => {
    if (layout !== "floating" || event.button !== 0) return;
    const inspector = inspectorRef.current;
    if (!inspector) return;
    const rect = inspector.getBoundingClientRect();
    const current = clampPosition(floatingPositionRef.current ?? { left: rect.left, top: rect.top }, rect.width, rect.height);
    floatingPositionRef.current = current;
    setFloatingPosition(current);
    dragRef.current = { pointerId: event.pointerId, offsetX: event.clientX - current.left, offsetY: event.clientY - current.top };
    event.currentTarget.setPointerCapture(event.pointerId);
    event.preventDefault();
    setDragging(true);
  };

  const handleDragMove = (event: PointerEvent<HTMLDivElement>) => {
    const drag = dragRef.current;
    const inspector = inspectorRef.current;
    if (!drag || drag.pointerId !== event.pointerId || !inspector) return;
    const rect = inspector.getBoundingClientRect();
    const next = clampPosition({ left: event.clientX - drag.offsetX, top: event.clientY - drag.offsetY }, rect.width, rect.height);
    floatingPositionRef.current = next;
    setFloatingPosition(next);
  };

  const finishDrag = (event: PointerEvent<HTMLDivElement>) => {
    const drag = dragRef.current;
    if (!drag || drag.pointerId !== event.pointerId) return;
    if (event.currentTarget.hasPointerCapture(event.pointerId)) event.currentTarget.releasePointerCapture(event.pointerId);
    dragRef.current = null;
    setDragging(false);
    const finalPosition = floatingPositionRef.current;
    if (finalPosition) saveDemoInspectorPosition(finalPosition);
  };

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
  const layoutOptions: Array<{ value: DemoInspectorLayout; label: string; icon: typeof PanelRight }> = [
    { value: "left", label: t("demo.layoutLeft"), icon: PanelLeft },
    { value: "right", label: t("demo.layoutRight"), icon: PanelRight },
    { value: "floating", label: t("demo.layoutFloating"), icon: Move },
    { value: "minimized", label: t("demo.layoutMinimized"), icon: Minimize2 },
  ];

  return <div className={`demo-inspector-shell demo-inspector-shell--${layout} ${isMinimized ? "is-closed" : "is-open"}`}>
    <button type="button" className={`demo-bubble ${isMinimized ? "" : "is-open"}`} aria-label={t("demo.openInspector")} onClick={restoreLayout}><PanelRight aria-hidden="true" /></button>
    {!isMinimized ? <aside ref={inspectorRef} className={`demo-inspector ${dragging ? "is-dragging" : ""}`} aria-label={t("demo.inspector")} style={layout === "floating" && floatingPosition ? { left: `${floatingPosition.left}px`, top: `${floatingPosition.top}px`, right: "auto", bottom: "auto" } : undefined}>
      <div className="demo-inspector-header"><div className={`demo-inspector-heading ${layout === "floating" ? "demo-inspector-drag-handle" : ""}`} onPointerDown={handleDragStart} onPointerMove={handleDragMove} onPointerUp={finishDrag} onPointerCancel={finishDrag} title={layout === "floating" ? t("demo.dragInspector") : undefined}><span className="eyebrow-label">{t("demo.eyebrow")}</span><h2>{t("demo.inspector")}</h2></div><div className="demo-layout-menu" ref={menuRef}><IconButton label={t("demo.layoutButton")} aria-haspopup="menu" aria-expanded={menuOpen} onClick={handleLayoutClick} onContextMenu={handleLayoutContextMenu} onPointerDown={handleLayoutPointerDown} onPointerUp={clearLongPress} onPointerCancel={clearLongPress}><LayoutIcon layout={layout} /></IconButton>{menuOpen ? <div className="demo-layout-menu-popover" role="menu" aria-label={t("demo.layoutMenu")}>{layoutOptions.map(({ value, label, icon: Icon }) => <button type="button" role="menuitemradio" aria-checked={layout === value} className={layout === value ? "selected" : ""} key={value} onClick={() => changeLayout(value)}><Icon aria-hidden="true" /><span>{label}</span>{layout === value ? <Check aria-hidden="true" /> : null}</button>)}</div> : null}</div></div>
      <div className="demo-safe-banner"><span className="demo-safe-dot" aria-hidden="true" /><div><strong>{t("demo.safeTitle")}</strong><span>{t("demo.safeText")}</span></div></div>
      <label className="demo-field"><span>{t("demo.scene")}</span><Select value={runtime.scene} onValueChange={(value) => set("scene", value as DemoScene)}><SelectTrigger aria-label={t("demo.scene")}><SelectValue /></SelectTrigger><SelectContent><SelectGroup><SelectItem value="normal">{t("demo.scenes.normal")}</SelectItem><SelectItem value="capacity">{t("demo.scenes.capacity")}</SelectItem><SelectItem value="conflict">{t("demo.scenes.conflict")}</SelectItem><SelectItem value="empty">{t("demo.scenes.empty")}</SelectItem></SelectGroup></SelectContent></Select></label>
      <label className="demo-field"><span>{t("demo.persona")}</span><Select value={runtime.persona} onValueChange={(value) => set("persona", value as DemoPersona)}><SelectTrigger aria-label={t("demo.persona")}><SelectValue /></SelectTrigger><SelectContent><SelectGroup><SelectItem value="admin">{t("demo.personas.admin")}</SelectItem><SelectItem value="viewer">{t("demo.personas.viewer")}</SelectItem></SelectGroup></SelectContent></Select></label>
      <label className="demo-field"><span>{t("demo.data")}</span><Select value={runtime.dataMode} onValueChange={(value) => set("dataMode", value as DemoDataMode)}><SelectTrigger aria-label={t("demo.data")}><SelectValue /></SelectTrigger><SelectContent><SelectGroup><SelectItem value="full">{t("demo.dataModes.full")}</SelectItem><SelectItem value="minimal">{t("demo.dataModes.minimal")}</SelectItem></SelectGroup></SelectContent></Select></label>
      <div className="demo-field"><span>{t("demo.network")}</span><div className="demo-network-toggle" role="group" aria-label={t("demo.network")}><button type="button" className={runtime.network === "online" ? "selected" : ""} onClick={() => set("network", "online")}><Wifi aria-hidden="true" />{t("demo.networkOnline")}</button><button type="button" className={runtime.network === "offline" ? "selected" : ""} onClick={() => set("network", "offline")}><WifiOff aria-hidden="true" />{t("demo.networkOffline")}</button></div></div>
      <div className="demo-state-row"><span>{t("demo.access")}</span><StatusBadge state={runtime.persona === "admin" ? "ready" : "unavailable"} /></div>
      <div className="demo-state-row"><span>{t("demo.status")}</span><StatusBadge state={runtime.network === "online" ? "available" : "failed"} /></div>
      <div className="demo-actions"><Button variant="primary" icon={copied ? <Check /> : <Clipboard />} onClick={() => void copyLink()}>{copied ? t("demo.copied") : copyFailed ? t("demo.copyFailed") : t("demo.copyShare")}</Button><Button variant="ghost" icon={<RotateCcw />} onClick={() => setDemoControl({ scene: runtime.scene, persona: runtime.persona, network: runtime.network, dataMode: runtime.dataMode })}>{t("demo.reset")}</Button></div>
      {copyFailed ? <p className="demo-copy-error" role="status">{t("demo.copyFailedText")}</p> : null}
      <p className="demo-footnote">{t("demo.urlHint")}</p>
    </aside> : null}
  </div>;
}
