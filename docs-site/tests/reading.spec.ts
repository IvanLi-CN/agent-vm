import { readFileSync } from 'node:fs';
import { test, expect, type Page } from '@playwright/test';

const cliTitle = readFileSync(new URL('../../docs/specs/agent-vm/contracts/cli.md', import.meta.url), 'utf8').match(/^# (.+)$/m)![1];
const sourceIdentity = JSON.parse(readFileSync(new URL('../.generated/source.json', import.meta.url), 'utf8')).identity as { revision: string; modifiedPaths: string[] };

async function navigation(page: Page) {
  if (page.viewportSize()!.width < 768) {
    await expect(page.locator('#nd-sidebar-mobile')).toBeHidden();
    await page.locator('#nd-subnav').getByRole('button', { name: '打开文档导航' }).click();
    return page.locator('#nd-sidebar-mobile');
  }
  return page.locator('#nd-sidebar');
}

test('home leads directly to real documents and their GitHub source', async ({ page }) => {
  await page.goto('/');
  await expect(page.getByRole('heading', { name: 'Agent VM 文档', exact: true })).toBeVisible();
  await expect(page.locator('.docs-home')).not.toContainText('本次设计');
  await page.locator('.document-list a').filter({ hasText: '项目概览' }).click();
  await expect(page).toHaveURL(/\/docs\/overview$/);
  await expect(page).toHaveTitle('项目概览 | Agent VM 文档');
  await expect(page.locator('article h1')).toContainText('Agent VM');
  await expect(page.locator('.document-path code')).toHaveText('README.md');
  expect(sourceIdentity.modifiedPaths).not.toContain('README.md');
  const githubLink = page.getByRole('link', { name: '在 GitHub 查看', exact: true });
  await expect(githubLink).toHaveAttribute('target', '_blank');
  await expect(githubLink).toHaveAttribute('href', `https://github.com/IvanLi-CN/agent-vm/blob/${sourceIdentity.revision}/README.md`);
  await page.goto('/source?path=docs%2Fspecs%2Fdocumentation-review%2FSPEC.md&line=5');
  await expect(page.locator('#L5')).toBeInViewport();
});

test('original diagrams, sections and links support reading', async ({ page }) => {
  await page.goto('/docs/docs-site-design');
  await expect(page.locator('.diagram-viewport .diagram-svg')).toBeVisible();
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
  await nav.getByRole('button', { name: '深色' }).click();
  await expect(page.locator('html')).toHaveClass(/dark/);
  await nav.getByRole('button', { name: '浅色' }).click();
  await expect(page.locator('html')).not.toHaveClass(/dark/);
  if (page.viewportSize()!.width < 768) await page.keyboard.press('Escape');
  await expect(page.locator('article')).toHaveCSS('white-space', 'normal');
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);

});

test('captures actual document reading when requested', async ({ page }, testInfo) => {
  test.skip(!process.env.DOCS_CAPTURE, 'Explicit evidence capture only');
  await page.goto('/docs/cli');
  await expect(page.locator('article h1')).toContainText(cliTitle);
  await page.screenshot({ path: testInfo.outputPath('reading.png'), fullPage: false });
  await page.goto('/');
  await page.screenshot({ path: testInfo.outputPath('home.png'), fullPage: true });
  await page.goto('/docs/docs-site-design');
  await expect(page.locator('.diagram-viewport .diagram-svg')).toBeVisible();
  const nav = await navigation(page);
  await expect(nav.locator('a[aria-current="page"]')).toHaveText('文档站工程方案');
  await page.screenshot({ path: testInfo.outputPath('tree.png'), animations: 'disabled' });
  await nav.getByRole('button', { name: '深色' }).click();
  await expect(page.locator('html')).toHaveClass(/dark/);
  await page.screenshot({ path: testInfo.outputPath('tree-dark.png'), animations: 'disabled' });
});

test('project navigation connects usage and architecture with an active page', async ({ page }) => {
  await page.goto('/');
  let nav = await navigation(page);
  await nav.getByRole('link', { name: 'CLI 使用与命令', exact: true }).click();
  await expect(page).toHaveTitle('CLI 使用与命令 | Agent VM 文档');
  await expect(page.locator('article h1')).toContainText(cliTitle);
  nav = await navigation(page);
  await nav.getByRole('link', { name: '系统架构', exact: true }).click();
  await expect(page).toHaveURL(/\/docs\/architecture$/);
  await expect(page.locator('article h1')).toContainText('Agent VM 系统架构');
  nav = await navigation(page);
  await expect(nav.locator('a[aria-current="page"]')).toHaveText('系统架构');
  if (page.viewportSize()!.width < 768) await page.keyboard.press('Escape');
  const expectedDiagrams = readFileSync(new URL('../../docs/architecture/system.md', import.meta.url), 'utf8').match(/^```mermaid$/gm)!.length;
  await expect(page.locator('.diagram-viewport .diagram-svg svg')).toHaveCount(expectedDiagrams);
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
  const documentPath = page.locator('.document-path');
  await expect(documentPath.locator('code')).toHaveText('docs/proposals/agent-vm-documentation-stack.md');
  await documentPath.getByRole('button', { name: '复制文档路径', exact: true }).click();
  await expect(documentPath.getByRole('status')).toHaveText('文档路径已复制');
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe('docs/proposals/agent-vm-documentation-stack.md');
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
  const svg = diagram.locator('.diagram-svg');
  await expect(svg).toHaveAttribute('data-theme', 'light');
  expect(await diagram.evaluate(el => el.scrollWidth <= el.clientWidth)).toBe(true);
  const originalImage = await svg.innerHTML();
  await contextCopyDiagram(page, svg.locator('.nodeLabel').first());
  let nav = await navigation(page);
  await nav.getByRole('button', { name: '深色' }).click();
  if (page.viewportSize()!.width < 768) await page.keyboard.press('Escape');
  await expect(svg).toHaveAttribute('data-theme', 'dark');
  await expect.poll(async () => page.locator('.diagram-viewport .diagram-svg:not([data-theme="dark"])').count()).toBe(0);
  expect(await svg.innerHTML()).not.toBe(originalImage);
  await page.evaluate(async () => { await Promise.allSettled(document.getAnimations().map(animation => animation.finished)); });
  await diagram.scrollIntoViewIfNeeded();
  if (process.env.DOCS_CAPTURE) await page.screenshot({ path: testInfo.outputPath('architecture-dark.png'), animations: 'disabled' });
  const reset = diagram.getByRole('button', { name: '完整显示图表' });
  const canvas = diagram.getByRole('region', { name: '可缩放平移的图表' });
  const content = diagram.locator('.diagram-content');
  await diagram.getByRole('button', { name: '放大图表', exact: true }).click();
  await expect(reset).not.toHaveText('100%');
  await contextCopyDiagram(page, svg.locator('.nodeLabel').filter({ hasText: '蓝色 Agent VM 服务' }).first());
  const beforeDrag = await content.getAttribute('style');
  const bounds = (await canvas.boundingBox())!;
  await page.mouse.move(bounds.x + bounds.width / 2, bounds.y + bounds.height - 12);
  await page.mouse.down();
  await page.mouse.move(bounds.x + bounds.width / 2 + 45, bounds.y + bounds.height - 45, { steps: 8 });
  await page.mouse.up();
  await expect(content).not.toHaveAttribute('style', beforeDrag!);
  const beforeWheel = await reset.innerText();
  await page.mouse.move(bounds.x + bounds.width / 2, bounds.y + bounds.height / 2);
  await page.mouse.wheel(0, -150);
  await expect(reset).not.toHaveText(beforeWheel);
  await reset.click();
  await expect(reset).toHaveText('100%');
  await canvas.focus();
  await page.keyboard.press('+');
  await expect(reset).not.toHaveText('100%');
  const beforeKey = await content.getAttribute('style');
  await page.keyboard.press('ArrowRight');
  await expect(content).not.toHaveAttribute('style', beforeKey!);
  if (process.env.DOCS_CAPTURE) await page.screenshot({ path: testInfo.outputPath('architecture-zoom.png'), animations: 'disabled' });
  await page.keyboard.press('0');
  await expect(reset).toHaveText('100%');
  nav = await navigation(page);
  await nav.getByRole('button', { name: '浅色' }).click();
  if (page.viewportSize()!.width < 768) await page.keyboard.press('Escape');
  await expect(svg).toHaveAttribute('data-theme', 'light');
  await page.evaluate(async () => { await Promise.allSettled(document.getAnimations().map(animation => animation.finished)); });
  await diagram.scrollIntoViewIfNeeded();
  if (process.env.DOCS_CAPTURE) await page.screenshot({ path: testInfo.outputPath('architecture-light.png'), animations: 'disabled' });
});

// Use the browser's selection and real clipboard rather than inspecting SVG markup.
async function contextCopyDiagram(page: Page, label: import('@playwright/test').Locator) {
  await page.context().grantPermissions(['clipboard-read', 'clipboard-write']);
  await expect(label).toHaveCSS('user-select', 'text');
  await label.dblclick();
  const text = await page.evaluate(() => window.getSelection()?.toString());
  expect(text?.trim()).toBeTruthy();
  await page.keyboard.press('ControlOrMeta+c');
  await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe(text);
  // Text interaction must remain separate from dragging the canvas.
  await label.click();
}

test('touch gestures pinch and pan the inline diagram', async ({ browser }, testInfo) => {
  test.skip(testInfo.project.name !== 'mobile', 'Touch gesture contract uses the narrow viewport');
  const context = await browser.newContext({ viewport: { width: 393, height: 852 }, hasTouch: true, isMobile: true });
  try {
    const page = await context.newPage();
    await page.goto(`${testInfo.project.use.baseURL}/docs/architecture`);
    const diagram = page.locator('.diagram-viewport').first();
    const canvas = diagram.locator('.diagram-canvas');
    await canvas.scrollIntoViewIfNeeded();
    const bounds = (await canvas.boundingBox())!;
    const x = bounds.x + bounds.width / 2, y = bounds.y + bounds.height / 2;
    const cdp = await context.newCDPSession(page);
    const points = (gap: number) => [{ x: x - gap, y, id: 0 }, { x: x + gap, y, id: 1 }];
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: points(25) });
    for (const gap of [35, 45, 55, 65]) await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: points(gap) });
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
    await expect(diagram.getByRole('button', { name: '完整显示图表' })).not.toHaveText('100%');
    const content = diagram.locator('.diagram-content');
    const beforePan = await content.getAttribute('style');
    const start = { x: bounds.x + bounds.width / 2, y: bounds.y + bounds.height - 15, id: 0 };
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [start] });
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ ...start, x: start.x + 35, y: start.y - 30 }] });
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
    await expect(content).not.toHaveAttribute('style', beforePan!);
    await diagram.getByRole('button', { name: '展开图表' }).click();
    const expanded = page.getByRole('dialog', { name: '图表大视图' });
    const largeBounds = (await expanded.locator('.diagram-canvas').boundingBox())!;
    const center = { x: largeBounds.x + largeBounds.width / 2, y: largeBounds.y + largeBounds.height / 2 };
    const largePoints = (gap: number) => [{ x: center.x - gap, y: center.y, id: 0 }, { x: center.x + gap, y: center.y, id: 1 }];
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: largePoints(25) });
    for (const gap of [35, 45, 55, 65]) await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: largePoints(gap) });
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
    await expect(expanded.getByRole('button', { name: '完整显示图表' })).not.toHaveText('100%');
  } finally { await context.close(); }
});

test('expanded diagram offers a larger interactive viewport and returns focus', async ({ page }, testInfo) => {
  await page.goto('/docs/architecture');
  const inline = page.locator('.diagram-viewport').first();
  const originalCanvas = inline.locator('.diagram-canvas');
  await expect(inline.locator('.diagram-svg svg')).toBeVisible();
  await inline.getByRole('button', { name: '放大图表', exact: true }).click();
  const inlineScale = await inline.getByRole('button', { name: '完整显示图表' }).innerText();
  const originalBounds = (await originalCanvas.boundingBox())!;
  const expand = inline.getByRole('button', { name: '展开图表', exact: true });
  await expand.click();
  const dialog = page.getByRole('dialog', { name: '图表大视图' });
  await expect(dialog).toBeVisible();
  const canvas = dialog.locator('.diagram-canvas');
  const bounds = (await canvas.boundingBox())!;
  expect(bounds.width).toBeGreaterThan(originalBounds.width);
  expect(bounds.height).toBeGreaterThan(originalBounds.height);
  await expect(dialog.getByRole('button', { name: '完整显示图表' })).toHaveText('100%');
  const ids = await page.locator('.diagram-svg [id]').evaluateAll(elements => elements.map(el => el.id));
  expect(new Set(ids).size).toBe(ids.length);
  await contextCopyDiagram(page, dialog.locator('.nodeLabel').filter({ hasText: '蓝色 Agent VM 服务' }).first());
  await dialog.getByRole('button', { name: '放大图表', exact: true }).click();
  await expect(dialog.getByRole('button', { name: '完整显示图表' })).not.toHaveText('100%');
  await canvas.focus();
  const beforePan = await dialog.locator('.diagram-content').getAttribute('style');
  await page.keyboard.press('ArrowRight');
  await expect(dialog.locator('.diagram-content')).not.toHaveAttribute('style', beforePan!);
  await page.keyboard.press('Tab');
  expect(await dialog.evaluate(el => el.contains(document.activeElement))).toBe(true);
  if (process.env.DOCS_CAPTURE) await page.screenshot({ path: testInfo.outputPath('architecture-expanded.png'), animations: 'disabled' });
  await page.keyboard.press('Escape');
  await expect(dialog).toBeHidden();
  await expect(expand).toBeFocused();
  await expect(inline.getByRole('button', { name: '完整显示图表' })).toHaveText(inlineScale);
  await expect(inline.locator('.diagram-svg svg')).toBeVisible();
  await expand.click();
  await page.getByRole('button', { name: '关闭图表大视图' }).click();
  await expect(dialog).toBeHidden();
  await expect(expand).toBeFocused();
});

test('sidebar utilities expose the repository and actual build version', async ({ page }) => {
  await page.goto('/docs/documentation-guide');
  const nav = await navigation(page);
  const repository = nav.getByRole('link', { name: 'GitHub', exact: true });
  await expect(repository).toHaveAttribute('href', 'https://github.com/IvanLi-CN/agent-vm');
  await expect(repository.locator('svg')).toBeVisible();
  const { version } = JSON.parse(readFileSync(new URL('../.generated/source.json', import.meta.url), 'utf8')).identity;
  await expect(nav.locator('.docs-version')).toHaveText(version.label);
  await expect(nav.locator('.docs-version')).toHaveAttribute('title', `文档构建：${version.description}`);
  expect(await nav.locator('.docs-version').evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(true);
  await expect(nav.getByRole('button', { name: '深色' })).toBeVisible();
  await nav.getByRole('button', { name: '跟随系统' }).click();
  await page.emulateMedia({ colorScheme: 'dark' });
  await expect(page.locator('html')).toHaveClass(/dark/);
  await page.emulateMedia({ colorScheme: 'light' });
  await expect(page.locator('html')).not.toHaveClass(/dark/);
  await nav.getByRole('button', { name: '深色' }).click();
  await page.emulateMedia({ colorScheme: 'light' });
  await expect(page.locator('html')).toHaveClass(/dark/);
  await page.reload();
  await expect(page.locator('html')).toHaveClass(/dark/);
});
