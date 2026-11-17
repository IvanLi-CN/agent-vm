import type { Meta, StoryObj } from '@storybook/react';
import { Mermaid } from './mermaid';
const meta = { title: 'Documentation/Diagram', component: Mermaid, decorators: [(Story) => <main className="site-main prose"><Story/></main>] } satisfies Meta<typeof Mermaid>;
export default meta;
type Story = StoryObj<typeof meta>;
export const ContentFlow: Story = { args: { code: 'flowchart LR\n  A[原始文档] --> B[内容适配] --> C[文档页面]' } };
export const InvalidSyntax: Story = { args: { code: 'not-a-valid-diagram' } };
