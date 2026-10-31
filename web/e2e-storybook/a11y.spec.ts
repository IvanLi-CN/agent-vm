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

test("open position menu has no accessibility violations", async ({ page }) => {
  await page.goto("/iframe.html?id=agent-vm-console--demo-inspector-position-menu&viewMode=story");
  await page.getByRole("button", { name: "Inspector position" }).click({ button: "right" });
  await expect(page.getByRole("menu", { name: "Choose inspector position" })).toBeVisible();
  await page.addScriptTag({ content: axeSource });
  const results = await page.evaluate(async () => window.axe.run(document.querySelector('[role="menu"]')!));
  expect(results.violations, JSON.stringify(results.violations, null, 2)).toEqual([]);
});
