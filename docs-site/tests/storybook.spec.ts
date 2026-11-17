import {test,expect} from '@playwright/test';
const storybook=`http://127.0.0.1:${process.env.DOCS_STORYBOOK_PORT}`;
test('isolated review gallery exercises filtering and problem states',async({page},testInfo)=>{
 await page.goto(storybook+'/iframe.html?id=documentation-requirement-review--filter&viewMode=story');
 await expect(page.getByRole('status')).toContainText('1 项需求');
 await page.goto(storybook+'/iframe.html?id=documentation-requirement-review--problems&viewMode=story');
 await expect(page.locator('[data-kind=broken]')).toBeVisible();
 const surface=page.locator('[data-evidence-surface="review-component"]');
 const presentation=await surface.evaluate(el=>{const s=getComputedStyle(el);return {padding:s.padding,background:s.backgroundColor};});
 expect(presentation.padding).toBe('32px');expect(presentation.background).not.toBe('rgba(0, 0, 0, 0)');
 if(process.env.DOCS_CAPTURE&&testInfo.project.name==='desktop')await surface.screenshot({path:testInfo.outputPath('component-problems.png')});
});
