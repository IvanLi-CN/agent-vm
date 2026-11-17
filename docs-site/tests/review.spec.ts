import {test,expect} from '@playwright/test';
test('review navigation and source snapshots',async({page})=>{await page.goto('/review');await expect(page.getByRole('heading',{name:'需求追溯',exact:true})).toBeVisible();await page.getByPlaceholder('编号或关键词').fill('REQ-DOCS-002');await expect(page.getByRole('status')).toContainText('1 项需求');await page.locator('.detail summary').first().click();await page.getByRole('link',{name:'构建来源'}).first().click();await expect(page.getByRole('heading',{name:'构建来源'})).toBeVisible();await expect(page.locator('pre')).toContainText('design-id');});
test('source headings can be linked',async({page})=>{await page.goto('/docs/0');await expect(page.getByRole('heading',{name:/REQ-DOCS-001/})).toBeVisible();});
test('keyboard, missing associations and diagnostics remain usable',async({page})=>{
  await page.goto('/review/examples');
  await expect(page.getByText('尚未关联。',{exact:false})).toBeVisible();
  await expect(page.getByText('broken ·',{exact:false})).toBeVisible();
  await expect(page.getByText('unparseable ·',{exact:false})).toBeVisible();
  const input=page.getByPlaceholder('编号或关键词');await input.focus();
  await page.keyboard.type('REQ-EXAMPLE-003');
  await expect(page.getByRole('status')).toContainText('1 项需求');
  await page.keyboard.press('Tab');await page.keyboard.press('Space');
  await expect(page.getByRole('checkbox')).toBeChecked();
  expect(await page.evaluate(()=>document.documentElement.scrollWidth<=window.innerWidth)).toBe(true);
});
test('Mermaid and anchor navigation render from the original body',async({page})=>{
  await page.goto('/docs/5');
  await expect(page.getByRole('img',{name:'设计流程图'}).locator('svg')).toBeVisible();
  await page.getByRole('navigation',{name:'页内目录'}).getByRole('link',{name:'单源审阅数据流'}).click();
  await expect(page).toHaveURL(/#单源审阅数据流|#%E5/);
});
test('captures controlled review evidence when requested',async({page},testInfo)=>{
 test.skip(!process.env.DOCS_CAPTURE,'Explicit evidence capture only');
 await page.goto('/review');await page.getByPlaceholder('编号或关键词').fill('REQ-DOCS-002');
 await page.locator('.detail summary').first().click();
 await page.screenshot({path:testInfo.outputPath('review.png'),fullPage:true});
 await page.goto('/review/examples');
 await page.screenshot({path:testInfo.outputPath('problems.png'),fullPage:true});
});
