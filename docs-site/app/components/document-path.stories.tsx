import type { Meta, StoryObj } from '@storybook/react';
import { expect, userEvent, within } from '@storybook/test';
import { DocumentPath } from './page';

const meta = {
  title: 'Documentation/DocumentPath',
  component: DocumentPath,
  tags: ['autodocs'],
  decorators: [(Story) => <div className="p-6 max-w-3xl"><Story/></div>],
  args: { path: 'docs/usage/quickstart.md', githubHref: 'https://github.com/IvanLi-CN/agent-vm/blob/main/docs/usage/quickstart.md' },
} satisfies Meta<typeof DocumentPath>;
export default meta;
type Story = StoryObj<typeof meta>;

export const CopyPath: Story = { play: async ({ canvasElement }) => {
  const canvas = within(canvasElement);
  await userEvent.click(canvas.getByRole('button', { name: '复制文档路径' }));
  await expect(await canvas.findByRole('status')).toHaveTextContent('文档路径已复制');
} };
