import type { Meta, StoryObj } from "@storybook/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { expect, fn, userEvent, within } from "@storybook/test";
import "../app/i18n";
import { ImagesPage } from "../pages/ImagesPage";
import { OperationsPage } from "../pages/OperationsPage";
import { OverviewPage } from "../pages/OverviewPage";
import { SystemPage } from "../pages/SystemPage";
import { VMsPage } from "../pages/VMsPage";
import { Shell } from "../components/Shell";
import { DemoInspector } from "../demo/DemoInspector";
import type { PageKey } from "../app/types";

function StoryFrame({ page, children }: { page: PageKey; children: React.ReactNode }) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return <QueryClientProvider client={client}><Shell page={page} onNavigate={fn()}>{children}</Shell></QueryClientProvider>;
}

const meta = { title: "Agent VM/Console", parameters: { layout: "fullscreen" } } satisfies Meta;
export default meta;
type Story = StoryObj<typeof meta>;

export const Overview: Story = { render: () => <StoryFrame page="overview"><OverviewPage /></StoryFrame> };
export const VMInspection: Story = { render: () => <StoryFrame page="vms"><VMsPage /></StoryFrame>, play: async ({ canvasElement }) => { const canvas = within(canvasElement); expect(await canvas.findByTestId("vms-page")).toBeInTheDocument(); await userEvent.click(await canvas.findByRole("option", { name: /task aurora 02/i })); expect(canvas.getByText("session_aurora_02")).toBeInTheDocument(); } };
export const ImageImport: Story = { render: () => <StoryFrame page="images"><ImagesPage /></StoryFrame> };
export const OperationsAudit: Story = { render: () => <StoryFrame page="operations"><OperationsPage /></StoryFrame> };
export const SystemMaintenance: Story = { render: () => <StoryFrame page="system"><SystemPage /></StoryFrame> };
export const DemoInspectorPanel: Story = { render: () => <div style={{ minHeight: "720px" }}><DemoInspector /></div> };
export const DemoInspectorFloating: Story = { render: () => <div style={{ minHeight: "720px" }}><DemoInspector layout="floating" /></div> };
export const DemoInspectorPositionMenu: Story = { render: () => <div style={{ minHeight: "720px" }}><DemoInspector layout="floating" /></div>, play: async ({ canvasElement }) => { const button = await within(canvasElement).findByRole("button", { name: "Inspector position" }); await userEvent.pointer({ keys: "[MouseRight]", target: button }); expect(await within(document.body).findByRole("menu", { name: "Choose inspector position" })).toBeVisible(); } };
export const DemoInspectorRightRail: Story = { render: () => <div style={{ minHeight: "720px" }}><DemoInspector layout="right" /></div> };
export const DemoInspectorMinimized: Story = { render: () => <div style={{ minHeight: "720px" }}><DemoInspector layout="minimized" /></div> };
