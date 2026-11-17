import type { Meta, StoryObj } from '@storybook/react';
import { MemoryRouter } from 'react-router';
import { within, userEvent, expect } from '@storybook/test';
import { ReviewView } from './review-view';
import data from '../data';
import { exampleData } from './examples';
const meta = {
  title:'Documentation/Requirement review',component:ReviewView,tags:['autodocs'],
  decorators:[Story=><MemoryRouter><div data-evidence-surface="review-component" style={{padding:32,minHeight:600,background:'var(--color-fd-background)',color:'var(--color-fd-foreground)'}}><Story/></div></MemoryRouter>],
  args:{data}
} satisfies Meta<typeof ReviewView>;
export default meta;
type Story = StoryObj<typeof meta>;
export const Complete: Story = {};
export const Problems: Story = {args:{data:exampleData}};
export const Filter: Story = {play:async({canvasElement})=>{
  const canvas=within(canvasElement);
  await userEvent.type(canvas.getByPlaceholderText('编号或关键词'),'REQ-DOCS-002');
  await expect(canvas.getByRole('status')).toHaveTextContent('1 项需求');
}};
export const Narrow: Story = {parameters:{viewport:{defaultViewport:'mobile1'}}};
