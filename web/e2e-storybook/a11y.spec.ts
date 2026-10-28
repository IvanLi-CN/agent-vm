import { expect, test } from "@playwright/test";
import axe from "axe-core";

const axeSource = axe.source;

test("Demo inspector Storybook story has no accessibility violations", async ({ page }) => {
  await page.goto("/iframe.html?id=agent-vm-console--demo-inspector-panel&viewMode=story");
  await page.locator(".demo-inspector").waitFor();
  await page.addScriptTag({ content: axeSource });
  const results = await page.evaluate(async () => window.axe.run(document.querySelector(".demo-inspector") ?? document));
  expect(results.violations, JSON.stringify(results.violations, null, 2)).toEqual([]);
});
