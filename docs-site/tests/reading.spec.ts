import { readFileSync } from 'node:fs';
import { test, expect, type Page } from '@playwright/test';

async function navigation(page: Page) {
  if (page.viewportSize()!.width < 768) {
    await expect(page.locator('#nd-sidebar-mobile')).toBeHidden();
    await page.locator('#nd-subnav').getByRole('button', { name: '打开文档导航' }).click();
    return page.locator('#nd-sidebar-mobile');
  }
  return page.locator('#nd-sidebar');
}

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
  await expect(page.locator('.diagram-viewport img')).toBeVisible();
  const compact = page.viewportSize()!.width < 1280;
  if (compact) await page.getByRole('button', { name: '本页目录', exact: true }).click();
  const toc = page.locator(compact ? '[data-toc-popover-content]' : '#nd-toc');
  await toc.getByRole('link', { name: '单源展示流程' }).click();
  await expect(page.locator('article h2').filter({ hasText: '单源展示流程' })).toBeInViewport();
  expect(new URL(page.url()).hash).not.toBe('');
  await page.getByText('图表源码', { exact: true }).click();
  await expect(page.locator('.diagram pre')).toContainText('flowchart');
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
});

test('keyboard navigation, theme and reading widths remain usable', async ({ page }) => {
  await page.goto('/docs/docs-site-requirements');
  await page.keyboard.press('Tab');
  await expect(page.getByRole('link', { name: '跳到正文' })).toBeFocused();
  await page.keyboard.press('Enter');
  await expect(page).toHaveURL(/#nd-page$/);
  let nav = await navigation(page);
  await nav.getByRole('button', { name: '切换明暗' }).click();
  await expect(page.locator('html')).toHaveClass(/dark/);
  await nav.getByRole('button', { name: '切换明暗' }).click();
  await expect(page.locator('html')).not.toHaveClass(/dark/);
  if (page.viewportSize()!.width < 768) await page.keyboard.press('Escape');
  await expect(page.locator('article')).toHaveCSS('white-space', 'normal');
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);

});

test('captures actual document reading when requested', async ({ page }, testInfo) => {
  test.skip(!process.env.DOCS_CAPTURE, 'Explicit evidence capture only');
  await page.goto('/docs/cli');
  await expect(page.locator('article h1')).toContainText('Agent VM CLI');
  await page.screenshot({ path: testInfo.outputPath('reading.png'), fullPage: false });
  await page.goto('/');
  await page.screenshot({ path: testInfo.outputPath('home.png'), fullPage: true });
  await page.goto('/docs/docs-site-design');
  await expect(page.locator('.diagram-viewport img')).toBeVisible();
  const nav = await navigation(page);
  await expect(nav.locator('a[aria-current="page"]')).toHaveText('文档站工程方案');
  await page.screenshot({ path: testInfo.outputPath('tree.png'), animations: 'disabled' });
  await nav.getByRole('button', { name: '切换明暗' }).click();
  await expect(page.locator('html')).toHaveClass(/dark/);
  await page.screenshot({ path: testInfo.outputPath('tree-dark.png'), animations: 'disabled' });
});

test('project navigation connects usage and architecture with an active page', async ({ page }) => {
  await page.goto('/');
  let nav = await navigation(page);
  await nav.getByRole('link', { name: 'CLI 使用与命令', exact: true }).click();
  await expect(page).toHaveTitle('CLI 使用与命令 | Agent VM 文档');
  await expect(page.locator('article h1')).toContainText('Agent VM CLI 合同');
  nav = await navigation(page);
  await nav.getByRole('link', { name: '系统架构', exact: true }).click();
  await expect(page).toHaveURL(/\/docs\/architecture$/);
  await expect(page.locator('article h1')).toContainText('Agent VM 系统架构');
  nav = await navigation(page);
  await expect(nav.locator('a[aria-current="page"]')).toHaveText('系统架构');
  if (page.viewportSize()!.width < 768) await page.keyboard.press('Escape');
  await expect(page.locator('.diagram-viewport img')).toHaveCount(await page.locator('.diagram-viewport').count());
  await expect(page.locator('.diagram [role=alert]')).toHaveCount(0);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
});

test('native folders share controls and open the current document ancestry', async ({ page }) => {
  await page.goto('/docs/docs-site-design');
  let nav = await navigation(page);
  for (const name of ['开始使用', '开发文档', '文档站']) {
    await expect(nav.getByRole('button', { name, exact: true })).toHaveAttribute('aria-expanded', 'true');
  }
  await expect(nav.locator('a[aria-current="page"]')).toHaveText('文档站工程方案');
  const folder = nav.getByRole('button', { name: '文档站', exact: true });
  await folder.focus();
  await page.keyboard.press('Space');
  await expect(folder).toHaveAttribute('aria-expanded', 'false');
  await expect(nav.getByRole('link', { name: '文档站工程方案', exact: true })).toBeHidden();
  await page.keyboard.press('Enter');
  await expect(folder).toHaveAttribute('aria-expanded', 'true');
  await nav.getByRole('link', { name: '内容来源与适配', exact: true }).click();
  await expect(page).toHaveURL(/\/docs\/docs-site-content$/);
  if (page.viewportSize()!.width < 768) await expect(page.locator('#nd-sidebar-mobile')).toBeHidden();
  nav = await navigation(page);
  await expect(nav.locator('a[aria-current="page"]')).toHaveText('内容来源与适配');
});

test('library disclosure and copy controls retain accurate source content', async ({ page, context }) => {
  await context.grantPermissions(['clipboard-read', 'clipboard-write']);
  await page.goto('/docs/docs-site-design');
  const footer = page.locator('.site-footer');
  const source = footer.getByRole('button', { name: '文档来源', exact: true });
  await source.focus();
  await page.keyboard.press('Enter');
  await expect(source).toHaveAttribute('aria-expanded', 'true');
  await expect(footer).toContainText('内容');
  await footer.getByRole('button', { name: '复制页面链接', exact: true }).click();
  await expect(footer.getByRole('status')).toHaveText('页面链接已复制');
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(page.url());
  await page.goto('/source?path=README.md&line=5');
  await expect(page.locator('#L5')).toBeInViewport();
  await page.getByRole('button', { name: '复制代码', exact: true }).click();
  await expect(page.getByRole('button', { name: '代码已复制', exact: true })).toBeVisible();
  const snapshots = JSON.parse(readFileSync(new URL('../.generated/source.json', import.meta.url), 'utf8')).snapshots;
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(snapshots['README.md']);
});

test('architecture diagrams fit by default, zoom and follow theme changes', async ({ page }, testInfo) => {
  await page.goto('/docs/architecture');
  const diagram = page.locator('.diagram-viewport').first();
  const svg = diagram.locator('img');
  await expect(svg).toHaveAttribute('data-theme', 'light');
  expect(await diagram.evaluate(el => el.scrollWidth <= el.clientWidth)).toBe(true);
  const originalImage = await svg.getAttribute('src');
  let nav = await navigation(page);
  await nav.getByRole('button', { name: '切换明暗' }).click();
  if (page.viewportSize()!.width < 768) await page.keyboard.press('Escape');
  await expect(svg).toHaveAttribute('data-theme', 'dark');
  await expect.poll(async () => page.locator('.diagram-viewport img:not([data-theme="dark"])').count()).toBe(0);
  expect(await svg.getAttribute('src')).not.toBe(originalImage);
  await diagram.scrollIntoViewIfNeeded();
  if (process.env.DOCS_CAPTURE) await page.screenshot({ path: testInfo.outputPath('architecture-dark.png'), animations: 'disabled' });
  await page.locator('.diagram').first().getByRole('button', { name: '放大图表' }).click();
  await expect(page.getByRole('dialog')).toBeVisible();
  await expect(page.getByRole('dialog').locator('img').first()).toBeVisible();
  if (process.env.DOCS_CAPTURE) await page.screenshot({ path: testInfo.outputPath('architecture-zoom.png'), animations: 'disabled' });
  await page.keyboard.press('Escape');
  await expect(page.getByRole('dialog')).not.toBeVisible();
  nav = await navigation(page);
  await nav.getByRole('button', { name: '切换明暗' }).click();
  if (page.viewportSize()!.width < 768) await page.keyboard.press('Escape');
  await expect(svg).toHaveAttribute('data-theme', 'light');
  await diagram.scrollIntoViewIfNeeded();
  if (process.env.DOCS_CAPTURE) await page.screenshot({ path: testInfo.outputPath('architecture-light.png'), animations: 'disabled' });
});
