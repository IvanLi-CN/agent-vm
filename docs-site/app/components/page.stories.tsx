import type { Meta, StoryObj } from '@storybook/react';
import { expect, userEvent, within } from '@storybook/test';
import { DocumentActions } from './page';
const meta = {
  title: 'Documentation/DocumentActions', component: DocumentActions, tags: ['autodocs'],
  decorators: [(Story) => <div className="p-6 max-w-3xl"><Story/></div>],
  args: { identity: { revision: '5a362fe12345', dirty: false, sourceId: '062b01280335' }, copyLink: async () => {} },
} satisfies Meta<typeof DocumentActions>;
export default meta;
type Story = StoryObj<typeof meta>;
export const Clean: Story = { play: async ({ canvasElement }) => {
  const canvas = within(canvasElement);
  await userEvent.click(canvas.getByRole('button', { name: '文档来源' }));
  await expect(await canvas.findByText(/干净提交/)).toBeVisible();
  await userEvent.click(canvas.getByRole('button', { name: '复制页面链接' }));
  await expect(await canvas.findByRole('button', { name: '已复制' })).toBeVisible();
} };
export const Modified: Story = { args: { identity: { ...meta.args.identity, dirty: true } } };
export const CopyUnavailable: Story = { args: { copyLink: async () => { throw new Error('Clipboard unavailable'); } }, play: async ({ canvasElement }) => {
  const canvas = within(canvasElement);
  await userEvent.click(canvas.getByRole('button', { name: '复制页面链接' }));
  await expect(await canvas.findByRole('status')).toHaveTextContent('无法访问剪贴板');
} };
