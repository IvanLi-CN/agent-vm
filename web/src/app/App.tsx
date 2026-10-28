import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import type { PageKey } from "./types";
import { Shell } from "../components/Shell";
import { ImagesPage } from "../pages/ImagesPage";
import { OperationsPage } from "../pages/OperationsPage";
import { OverviewPage } from "../pages/OverviewPage";
import { SystemPage } from "../pages/SystemPage";
import { VMsPage } from "../pages/VMsPage";

const queryClient = new QueryClient({ defaultOptions: { queries: { staleTime: 4_000, retry: 1 } } });

function pageFromPath(path: string): PageKey {
  if (path.startsWith("/vms")) return "vms";
  if (path.startsWith("/images")) return "images";
  if (path.startsWith("/operations")) return "operations";
  if (path.startsWith("/system")) return "system";
  return "overview";
}

function ConsoleApp() {
  const { t } = useTranslation();
  const [page, setPage] = useState<PageKey>(() => pageFromPath(window.location.pathname));
  return <Shell page={page} onNavigate={setPage}><div className="sr-only" aria-live="polite">{t(`nav.${page}`)}</div>{page === "overview" ? <OverviewPage /> : page === "vms" ? <VMsPage /> : page === "images" ? <ImagesPage /> : page === "operations" ? <OperationsPage /> : <SystemPage />}</Shell>;
}

export function App() {
  return <QueryClientProvider client={queryClient}><ConsoleApp /></QueryClientProvider>;
}
