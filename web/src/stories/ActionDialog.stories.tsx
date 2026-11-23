import type { Meta, StoryObj } from "@storybook/react";
import { expect, fn, userEvent, within } from "@storybook/test";
import "../app/i18n";
import type { ActionPreview } from "../app/types";
import { ActionDialog } from "../components/ActionDialog";

const preview: ActionPreview = {
  target_version: "vm:task_aurora_02:revision-4",
  confirmation_token: "synthetic-confirmation",
  expires_at: "2026-01-15T11:34:00Z",
  impact: {
    summary_code: "destroy",
    affected_session_count: 1,
    affected_image_references: { default: 1, active: 1, cached: 1, releasing: 0, pending_creation: 0, verification: 0 },
    changes: [
      { code: "session_access_revoked", count: 1, value: null },
      { code: "pinned_vm_destroyed", count: 1, value: "Awaiting owner review of the migration result" },
    ],
  },
};

const pinnedWarning = "该 VM 处于保留状态：Awaiting owner review of the migration result。原到期时间 2026年1月15日 GMT+8 18:44；销毁会删除这个受保护的工作环境。";

const meta = {
  title: "Agent VM/Action Dialog",
  component: ActionDialog,
  tags: ["autodocs"],
  parameters: { layout: "padded" },
  args: {
    preview,
    actionLabel: "销毁",
    target: "task_aurora_02 · session_aurora_02",
    dangerous: true,
    busy: false,
    onCancel: fn(),
    onConfirm: fn(),
  },
  decorators: [Story => <div style={{ padding: 24 }}><Story /></div>],
} satisfies Meta<typeof ActionDialog>;

export default meta;
type Story = StoryObj<typeof meta>;

export const PinnedDestroyWarning: Story = {
  args: { confirmationText: pinnedWarning, reasonRequired: false },
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement);
    await expect(canvas.getByText(pinnedWarning)).toBeVisible();
    await expect(canvas.getByText("销毁保留中的 VM")).toBeVisible();
  },
};

export const PinRequiresReason: Story = {
  args: { actionLabel: "保留 VM", target: "task_aurora_02 · session_aurora_02", dangerous: false, confirmationText: undefined, reasonRequired: true },
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement);
    const submit = canvas.getByRole("button", { name: /确认提交|Submit/ });
    await expect(submit).toBeDisabled();
    await userEvent.type(canvas.getByRole("textbox"), "hold for owner review");
    await expect(submit).toBeEnabled();
  },
};
