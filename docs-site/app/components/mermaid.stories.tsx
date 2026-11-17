import type { Meta, StoryObj } from '@storybook/react';
import { Mermaid } from './mermaid';
import { expect, userEvent, within } from '@storybook/test';
const meta = { title: 'Documentation/Diagram', component: Mermaid, tags: ['autodocs'], decorators: [(Story) => <main className="prose p-6 max-w-3xl"><Story/></main>] } satisfies Meta<typeof Mermaid>;
export default meta;
type Story = StoryObj<typeof meta>;
export const ContentFlow: Story = { args: { code: 'flowchart LR\n  A[原始文档] --> B[内容适配] --> C[文档页面]' }, play: async ({ canvasElement }) => { const canvas = within(canvasElement); await userEvent.click(canvas.getByRole('button', { name: '图表源码' })); await expect(await canvas.findByText(/flowchart LR/)).toBeVisible(); } };
export const InvalidSyntax: Story = { args: { code: 'not-a-valid-diagram' } };
