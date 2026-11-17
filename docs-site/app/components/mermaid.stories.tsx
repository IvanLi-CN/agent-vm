import type { Meta, StoryObj } from '@storybook/react';
import { Mermaid } from './mermaid';
import { expect, userEvent, within, waitFor } from '@storybook/test';
const meta = { title: 'Documentation/Diagram', component: Mermaid, tags: ['autodocs'], decorators: [(Story) => <main data-visual-evidence-surface className="prose p-6 max-w-3xl bg-fd-background"><div data-visual-evidence-target className="flow-root"><Story/></div></main>] } satisfies Meta<typeof Mermaid>;
export default meta;
type Story = StoryObj<typeof meta>;
export const ContentFlow: Story = { args: { code: 'flowchart LR\n  A[原始文档] --> B[内容适配] --> C[文档页面]' }, play: async ({ canvasElement }) => { const canvas = within(canvasElement); const label = await canvas.findByText('原始文档'); await userEvent.click(label); await userEvent.click(canvas.getByRole('button', { name: '放大图表' })); await waitFor(() => expect(canvas.getByRole('button', { name: '完整显示图表' })).not.toHaveTextContent('100%')); await userEvent.click(canvas.getByRole('button', { name: '完整显示图表' })); await waitFor(() => expect(canvas.getByRole('button', { name: '完整显示图表' })).toHaveTextContent('100%')); await userEvent.click(canvas.getByRole('button', { name: '图表源码' })); await expect(await canvas.findByText(/flowchart LR/)).toBeVisible(); } };
export const InvalidSyntax: Story = { args: { code: 'not-a-valid-diagram' } };

// Use the actual source diagram so the wide production case stays represented.
import architecture from '../../../docs/architecture/system.md?raw';
export const Architecture: Story = { args: { code: architecture.split('```mermaid\n')[1].split('```')[0] } };

export const Expanded: Story = {
  args: Architecture.args,
  play: async ({ canvasElement }) => {
    await userEvent.click(await within(canvasElement).findByRole('button', { name: '展开图表' }));
    const dialog = await within(canvasElement.ownerDocument.body).findByRole('dialog', { name: '图表大视图' });
    await expect(dialog).toBeVisible();
    await userEvent.click(within(dialog).getByRole('button', { name: '放大图表' }));
    await waitFor(() => expect(within(dialog).getByRole('button', { name: '完整显示图表' })).not.toHaveTextContent('100%'));
    await userEvent.click(within(dialog).getByRole('button', { name: '完整显示图表' }));
    await waitFor(() => expect(within(dialog).getByRole('button', { name: '完整显示图表' })).toHaveTextContent('100%'));
  },
};
