import { test, expect } from '@playwright/test';

test('home leads directly to real documents and their build snapshots', async ({ page }) => {
  await page.goto('/');
  await expect(page.getByRole('heading', { name: 'Agent VM 文档', exact: true })).toBeVisible();
  await expect(page.locator('.docs-home')).not.toContainText('本次设计');
  await page.locator('.document-list a').first().click();
  await expect(page).toHaveURL(/\/docs\/overview$/);
  await expect(page).toHaveTitle('项目概览 | Agent VM 文档');
  await expect(page.locator('article h1')).toContainText('Agent VM');
  await page.getByRole('link', { name: '查看构建来源', exact: true }).click();
  await expect(page.getByRole('heading', { name: '构建来源', exact: true })).toBeVisible();
  await expect(page.locator('pre')).toContainText('# Agent VM');
  await page.goto('/source?path=docs%2Fspecs%2Fdocumentation-review%2FSPEC.md&line=5');
  await expect(page.locator('#L5')).toBeInViewport();
});

test('original diagrams, sections and links support reading', async ({ page }) => {
  await page.goto('/docs/docs-site-design');
  await expect(page.getByRole('img', { name: '设计流程图' }).locator('svg')).toBeVisible();
  const toc = page.getByRole('navigation', { name: '页内目录' });
  if (page.viewportSize()!.width <= 1100) await toc.locator('summary').click();
  await toc.getByRole('link', { name: '单源展示流程' }).click();
  await expect(page.locator('article h2').filter({ hasText: '单源展示流程' })).toBeInViewport();
  expect(new URL(page.url()).hash).not.toBe('');
  if (page.viewportSize()!.width < 700) {
    const diagram = page.locator('.diagram-viewport');
    expect(await diagram.evaluate(el => el.scrollWidth > el.clientWidth)).toBe(true);
    await diagram.focus();
    await page.keyboard.press('ArrowRight');
    await expect.poll(() => diagram.evaluate(el => el.scrollLeft)).toBeGreaterThan(0);
  }
  await page.getByText('图表源码', { exact: true }).click();
  await expect(page.locator('figure details pre')).toContainText('flowchart');
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
});

test('keyboard navigation, theme and reading widths remain usable', async ({ page }) => {
  await page.goto('/docs/docs-site-requirements');
  await page.keyboard.press('Tab');
  await expect(page.getByRole('link', { name: '跳到正文' })).toBeFocused();
  await page.keyboard.press('Enter');
  await expect(page).toHaveURL(/#main-content$/);
  await page.getByRole('button', { name: '切换明暗' }).click();
  await expect(page.locator('html')).toHaveClass(/dark/);
  await page.getByRole('button', { name: '切换明暗' }).click();
  await expect(page.locator('html')).not.toHaveClass(/dark/);
  await expect(page.locator('article')).toHaveCSS('white-space', 'normal');
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  if (page.viewportSize()!.width < 700) {
    await page.locator('.mobile-materials > summary').click();
    await expect(page.locator('.mobile-materials a').first()).toBeVisible();
  }
});

test('captures actual document reading when requested', async ({ page }, testInfo) => {
  test.skip(!process.env.DOCS_CAPTURE, 'Explicit evidence capture only');
  await page.goto('/docs/cli');
  await expect(page.locator('article h1')).toContainText('Agent VM CLI');
  await page.screenshot({ path: testInfo.outputPath('reading.png'), fullPage: false });
  await page.goto('/');
  await page.screenshot({ path: testInfo.outputPath('home.png'), fullPage: true });
});

test('project navigation connects usage and architecture with an active page', async ({ page }) => {
  await page.goto('/');
  await page.getByRole('navigation', { name: '主导航' }).getByRole('link', { name: '使用指南' }).click();
  await expect(page).toHaveTitle('CLI 使用与命令 | Agent VM 文档');
  await expect(page.locator('article h1')).toContainText('Agent VM CLI 合同');
  await page.getByRole('navigation', { name: '主导航' }).getByRole('link', { name: '开发文档' }).click();
  await expect(page).toHaveURL(/\/docs\/architecture$/);
  await expect(page.locator('article h1')).toContainText('Agent VM 系统架构');
  await expect(page.locator('.site-sidebar a[aria-current="page"]')).toHaveText('系统架构');
  await expect(page.locator('.diagram-viewport svg')).toHaveCount(await page.locator('.diagram-viewport').count());
  await expect(page.locator('.diagram [role=alert]')).toHaveCount(0);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
});
