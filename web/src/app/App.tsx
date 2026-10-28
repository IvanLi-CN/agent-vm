import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useEffect, useSyncExternalStore } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import type { PageKey } from "./types";
import { Shell } from "../components/Shell";
import { ImagesPage } from "../pages/ImagesPage";
import { OperationsPage } from "../pages/OperationsPage";
import { OverviewPage } from "../pages/OverviewPage";
import { SystemPage } from "../pages/SystemPage";
import { VMsPage } from "../pages/VMsPage";
import { DemoInspector } from "../demo/DemoInspector";
import { DemoProvider, isDemoRuntime, useDemoRuntime } from "../demo/runtime";

const queryClient = new QueryClient({ defaultOptions: { queries: { staleTime: 4_000, retry: 1 } } });

function pageFromPath(path: string): PageKey {
  const normalized = path.replace(/^\/demo(?=\/|$)/, "");
  if (normalized.startsWith("/vms")) return "vms";
  if (normalized.startsWith("/images")) return "images";
  if (normalized.startsWith("/operations")) return "operations";
  if (normalized.startsWith("/system")) return "system";
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
  return <Shell key={`${page}-${runtime.revision}`} page={page}><div className="sr-only" aria-live="polite">{t(`nav.${page}`)}</div>{page === "overview" ? <OverviewPage /> : page === "vms" ? <VMsPage /> : page === "images" ? <ImagesPage /> : page === "operations" ? <OperationsPage /> : <SystemPage />}</Shell>;
}

export function App() {
  const demoRuntime = useDemoRuntime();
  return <DemoProvider><QueryClientProvider client={queryClient}><ConsoleApp />{isDemoRuntime() && !demoRuntime.bootstrapError ? <DemoInspector /> : null}</QueryClientProvider></DemoProvider>;
}
