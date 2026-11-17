import { expect, test } from "@playwright/test";
import axe from "axe-core";

for (const story of ["unnamed", "named", "catalog-unavailable", "input", "rebind-confirmation"]) {
  test(`image name story: ${story}`, async ({ page }) => {
    await page.goto(`/iframe.html?id=agent-vm-image-name--${story}&viewMode=story`);
    await page.locator(".image-name-editor").waitFor();
    if (story === "rebind-confirmation") await expect(page.getByRole("dialog")).toBeVisible();
    if (story === "input") await expect(page.getByRole("textbox")).toHaveValue("debian-build");
    await page.addScriptTag({ content: axe.source });
    const results = await page.evaluate(async () => window.axe.run(document.querySelector('[role="dialog"]') ?? document.querySelector(".image-name-editor")!));
    expect(results.violations, JSON.stringify(results.violations, null, 2)).toEqual([]);
  });
}
