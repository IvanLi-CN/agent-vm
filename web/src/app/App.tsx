import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { lazy, Suspense, useEffect, useState, useSyncExternalStore } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import type { PageKey } from "./types";
import { ManagementAuthBoundary, ManagementAuthProvider } from "./auth";
import { DemoProvider, getDemoInspectorLayout, isDemoRuntime, saveDemoInspectorLayout, useDemoRuntime, type DemoInspectorLayout } from "../demo/runtime";

const ManagementShell = lazy(() => import("../components/Shell").then(({ Shell }) => ({ default: Shell })));
const managementPages = {
  overview: lazy(() => import("../pages/OverviewPage").then(({ OverviewPage }) => ({ default: OverviewPage }))),
  vms: lazy(() => import("../pages/VMsPage").then(({ VMsPage }) => ({ default: VMsPage }))),
  images: lazy(() => import("../pages/ImagesPage").then(({ ImagesPage }) => ({ default: ImagesPage }))),
  operations: lazy(() => import("../pages/OperationsPage").then(({ OperationsPage }) => ({ default: OperationsPage }))),
  system: lazy(() => import("../pages/SystemPage").then(({ SystemPage }) => ({ default: SystemPage }))),
};
const DemoInspector = lazy(() => import("../demo/DemoInspector").then(({ DemoInspector: component }) => ({ default: component })));

const queryClient = new QueryClient({ defaultOptions: { queries: { staleTime: 4_000, retry: 1 } } });

function pageFromPath(path: string): PageKey {
  if (path.startsWith("/vms")) return "vms";
  if (path.startsWith("/images")) return "images";
  if (path.startsWith("/operations")) return "operations";
  if (path.startsWith("/system")) return "system";
  return "overview";
}

function subscribeToLocation(listener: () => void): () => void {
  window.addEventListener("popstate", listener);
  return () => window.removeEventListener("popstate", listener);
}

function useLocationPath(): string {
  return useSyncExternalStore(subscribeToLocation, () => window.location.pathname, () => "/");
}

function ConsoleApp() {
  const { t } = useTranslation();
  const client = useQueryClient();
  const runtime = useDemoRuntime();
  const page = pageFromPath(useLocationPath());
  useEffect(() => {
    if (isDemoRuntime() && runtime.revision) client.clear();
  }, [client, runtime.revision]);
  if (isDemoRuntime() && runtime.bootstrapError) {
    return <div className="demo-boot-error"><div><span className="eyebrow-label">{t("demo.eyebrow")}</span><h1>{t("demo.bootFailed")}</h1><p>{t("demo.bootFailedText")}</p><code>{runtime.bootstrapError}</code></div></div>;
  }
  return <ManagementAuthProvider><ManagementAuthBoundary><Suspense fallback={<div className="app-route-loading" role="status">{t("common.loading")}</div>}><ManagementSurface page={page} revision={runtime.revision} /></Suspense></ManagementAuthBoundary></ManagementAuthProvider>;
}

function ManagementSurface({ page, revision }: { page: PageKey; revision: number }) {
  const { t } = useTranslation();
  const Page = managementPages[page];
  return <ManagementShell key={`${page}-${revision}`} page={page}><div className="sr-only" aria-live="polite">{t(`nav.${page}`)}</div><Page /></ManagementShell>;
}

export function App({ client = queryClient }: { client?: QueryClient } = {}) {
  const demoRuntime = useDemoRuntime();
  const isDemo = isDemoRuntime();
  const [inspectorLayout, setInspectorLayout] = useState<DemoInspectorLayout>(() => getDemoInspectorLayout());
  const setDemoInspectorLayout = (layout: DemoInspectorLayout) => {
    setInspectorLayout(layout);
    saveDemoInspectorLayout(layout);
  };
  const showInspector = isDemo && !demoRuntime.bootstrapError;
  return <DemoProvider><QueryClientProvider client={client}>{showInspector ? <div className={`demo-runtime demo-runtime--${inspectorLayout}`}><ConsoleApp /><DemoInspector layout={inspectorLayout} onLayoutChange={setDemoInspectorLayout} /></div> : <ConsoleApp />}</QueryClientProvider></DemoProvider>;
}
