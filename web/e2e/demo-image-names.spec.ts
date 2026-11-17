import { expect, test } from "@playwright/test";
import { join } from "node:path";
import { mkdirSync } from "node:fs";
for (const viewport of [{ name: "desktop", width: 1440, height: 1000 }, { name: "mobile", width: 393, height: 852 }]) {
    test.describe(viewport.name, () => {
        test.use({ viewport: { width: viewport.width, height: viewport.height } });
        test("clears a name and confirms rebind with complete fingerprints", async ({ page }) => {
            const requests: string[] = [];
            page.on("request", request => { if (new URL(request.url()).pathname.startsWith("/api/v1"))
                requests.push(request.url()); });
            await page.addInitScript(() => localStorage.setItem("agent-vm:demo-inspector-layout", "minimized"));
            await page.goto("/images?demo=normal&d_persona=admin&d_network=online&d_data=full");
            await page.getByRole("button", { name: "Clear name", exact: true }).click();
            await page.getByRole("dialog").getByRole("button", { name: "Confirm and submit" }).click();
            const input = page.getByRole("textbox", { name: "Image name", exact: true });
            await expect(input).toBeEnabled();
            await input.fill("2026.01");
            await page.getByRole("button", { name: "Set or rebind name", exact: true }).click();
            const dialog = page.getByRole("dialog");
            await expect(dialog).toBeVisible();
            await expect(dialog.getByText("a".repeat(64), { exact: true })).toBeVisible();
            await expect(dialog.getByText("b".repeat(64), { exact: true })).toBeVisible();
            expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
            if (process.env.AGENT_VM_VISUAL_DIR) {
                mkdirSync(process.env.AGENT_VM_VISUAL_DIR, { recursive: true });
                await page.screenshot({ path: join(process.env.AGENT_VM_VISUAL_DIR, `image-name-${viewport.name}.png`) });
            }
            await dialog.getByRole("button", { name: "Confirm and submit" }).click();
            await expect(dialog).not.toBeVisible();
            await expect(page.getByRole("button", { name: "Clear name", exact: true })).toBeEnabled();
            expect(requests).toEqual([]);
        });
    });
}
