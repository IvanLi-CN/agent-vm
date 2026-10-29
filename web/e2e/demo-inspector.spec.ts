import { expect, test } from "@playwright/test";

test("supports left, right, floating and minimized inspector positions", async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto("?demo=normal");

  const runtime = page.locator(".demo-runtime");
  const shell = page.locator(".demo-inspector-shell");
  await expect(runtime).toHaveClass(/demo-runtime--right/);
  await expect(page.locator(".demo-inspector")).toHaveCSS("position", "sticky");
  const positionButton = page.getByRole("button", { name: "Inspector position" });
  await positionButton.click();
  await expect(page.getByRole("menu")).toHaveCount(0);
  await expect(runtime).toHaveClass(/demo-runtime--right/);
  await positionButton.click({ button: "right" });
  await expect(page.getByRole("menu")).toBeVisible();
  await page.keyboard.press("Escape");
  const positionBox = await positionButton.boundingBox();
  if (!positionBox) throw new Error("Inspector position button is not measurable");
  await page.mouse.move(positionBox.x + positionBox.width / 2, positionBox.y + positionBox.height / 2);
  await page.mouse.down();
  await page.waitForTimeout(650);
  await expect(page.getByRole("menu")).toBeVisible();
  await page.mouse.up();

  await page.getByRole("menuitemradio", { name: "Left rail" }).click();
  await expect(runtime).toHaveClass(/demo-runtime--left/);
  await expect(page.locator(".demo-inspector")).toHaveCSS("position", "sticky");
  await expect(page.getByRole("separator", { name: "Drag panel" })).toHaveCount(0);

  await positionButton.click({ button: "right" });
  await page.getByRole("menuitemradio", { name: "Floating" }).click();
  await expect(runtime).toHaveClass(/demo-runtime--floating/);
  await expect(page.locator(".demo-inspector")).toHaveCSS("position", "fixed");
  const inspector = page.locator(".demo-inspector");
  const dragHandle = page.getByRole("separator", { name: "Drag panel" });
  await expect(dragHandle).toBeVisible();
  await expect(dragHandle).toHaveAttribute("data-slot", "resizable-handle");
  await expect(dragHandle.locator("div")).toBeVisible();
  await expect(dragHandle).toHaveAttribute("aria-keyshortcuts", "ArrowLeft ArrowRight ArrowUp ArrowDown");
  const beforeDrag = await inspector.boundingBox();
  const dragBox = await dragHandle.boundingBox();
  if (!beforeDrag || !dragBox) throw new Error("Floating inspector is not measurable");
  expect(dragBox.width).toBeGreaterThan(beforeDrag.width - 40);
  await page.mouse.move(dragBox.x + dragBox.width / 2, dragBox.y + dragBox.height / 2);
  await page.mouse.down();
  await page.mouse.move(dragBox.x + dragBox.width / 2 - 120, dragBox.y + dragBox.height / 2 + 60);
  await page.mouse.up();
  const afterDrag = await inspector.boundingBox();
  if (!afterDrag) throw new Error("Floating inspector disappeared after drag");
  expect(afterDrag.x).toBeLessThan(beforeDrag.x - 50);
  expect(afterDrag.y).toBeGreaterThan(beforeDrag.y + 20);
  const beforeKeyboardMove = await inspector.boundingBox();
  await dragHandle.focus();
  await page.keyboard.press("ArrowRight");
  const afterKeyboardMove = await inspector.boundingBox();
  if (!beforeKeyboardMove || !afterKeyboardMove) throw new Error("Floating inspector keyboard position is not measurable");
  expect(afterKeyboardMove.x).toBeGreaterThan(beforeKeyboardMove.x);

  await positionButton.click({ button: "right" });
  await page.getByRole("menuitemradio", { name: "Minimize" }).click();
  await expect(runtime).toHaveClass(/demo-runtime--minimized/);
  await expect(shell).toHaveClass(/is-closed/);
  await expect(page.getByRole("separator", { name: "Drag panel" })).toHaveCount(0);
  await expect(shell).toHaveCSS("display", "contents");
  await expect(page.locator(".demo-bubble")).toHaveCSS("position", "fixed");
  await expect(page.getByRole("button", { name: "Open demo inspector" })).toBeVisible();
  await page.getByRole("button", { name: "Open demo inspector" }).click();
  await expect(runtime).toHaveClass(/demo-runtime--floating/);
});

test("keeps the demo isolated while a scenario and simulated action change", async ({ page }) => {
  const realRequests: string[] = [];
  page.on("request", (request) => { if (request.url().includes("/api/v1")) realRequests.push(request.url()); });
  await page.addInitScript(() => {
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText: async () => undefined } });
  });
  await page.goto("?demo=normal&d_persona=admin&d_network=online&token=must-not-share#private-fragment");
  await expect(page.getByRole("heading", { name: "Demo inspector" })).toBeVisible();
  const sceneSelect = page.getByRole("combobox", { name: "Scene" });
  await sceneSelect.click();
  await expect(page.getByRole("option", { name: "Capacity or data fault" })).toBeVisible();
  await page.getByRole("option", { name: "Capacity or data fault" }).click();
  await expect(page.getByTestId("overview-page").getByText("Admission is unknown")).toBeVisible();
  await expect(page).toHaveURL(/demo=capacity/);
  expect((await page.url())).not.toContain("must-not-share");
  expect((await page.url())).not.toContain("private-fragment");
  await page.goBack();
  await expect(page).toHaveURL(/demo=normal/);
  await expect(sceneSelect).toHaveText("Normal operation");
  await sceneSelect.click();
  await expect(page.getByRole("option", { name: "Capacity or data fault" })).toBeVisible();
  await page.getByRole("option", { name: "Capacity or data fault" }).click();
  await page.getByRole("button", { name: "VMs" }).click();
  await expect(page).toHaveURL(/\/demo\/vms\?demo=capacity/);
  await expect(page.getByTestId("vms-page")).toBeVisible();
  await page.getByRole("button", { name: "Release lease" }).click();
  await expect(page.getByRole("dialog")).toBeVisible();
  await page.getByRole("textbox", { name: "Reason" }).fill("Exercise the safe simulated mutation path");
  await page.getByRole("button", { name: "Confirm and submit" }).click();
  await expect(page.getByRole("dialog")).toBeHidden();
  await page.getByRole("button", { name: "Operations & Audit" }).click();
  await expect(page.getByTestId("operations-page")).toBeVisible();
  await page.getByRole("tab", { name: "Audit records" }).click();
  await expect(page.getByText("Exercise the safe simulated mutation path")).toBeVisible();
  await page.getByRole("button", { name: "Copy share link" }).click();
  await expect(page.getByText("Link copied")).toBeVisible();
  expect(realRequests).toEqual([]);
});

test("supports a mobile inspector drawer and fail-closed bootstrap", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("?demo=empty");
  await expect(page.getByTestId("overview-page")).toBeVisible();
  const mobilePositionButton = page.getByRole("button", { name: "Inspector position" });
  await mobilePositionButton.click({ button: "right" });
  await page.getByRole("menuitemradio", { name: "Right rail" }).click();
  await mobilePositionButton.click();
  await expect(page.getByRole("menu")).toHaveCount(0);
  await page.getByRole("button", { name: "Inspector position" }).click({ button: "right" });
  await page.getByRole("menuitemradio", { name: "Minimize" }).click();
  await expect(page.getByRole("button", { name: "Open demo inspector" })).toBeVisible();
  await page.getByRole("button", { name: "Open demo inspector" }).click();
  await expect(page.getByRole("heading", { name: "Demo inspector" })).toBeVisible();
  await page.goto("?demo=normal&d_bootstrap=fail");
  await expect(page.getByRole("heading", { name: "Demo could not start" })).toBeVisible();
  await expect(page.getByText("The simulated environment was not ready")).toBeVisible();
  await expect(page.getByRole("heading", { name: "Demo inspector" })).toHaveCount(0);
});

test("covers route pages, denied reads, keyboard controls and clipboard failure", async ({ page }) => {
  await page.goto("?demo=normal");
  await page.getByRole("button", { name: "Images" }).click();
  await expect(page.getByTestId("images-page")).toBeVisible();
  await page.getByRole("button", { name: "System" }).click();
  await expect(page.getByTestId("system-page")).toBeVisible();
  await page.getByRole("button", { name: "Inspector position" }).focus();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("menu")).toHaveCount(0);
  await page.keyboard.press("Shift+F10");
  await expect(page.getByRole("menu")).toBeVisible();
  await page.getByRole("menuitemradio", { name: "Minimize" }).click();
  await expect(page.getByRole("button", { name: "Open demo inspector" })).toBeVisible();
  await page.getByRole("button", { name: "Open demo inspector" }).click();

  await page.goto("?demo=normal&d_persona=viewer");
  await expect(page.getByRole("heading", { name: "Overview" })).toBeVisible();
  await expect(page.getByTestId("overview-page")).toBeVisible();
  await page.getByRole("button", { name: "VMs" }).click();
  await expect(page.getByTestId("vms-page")).toBeVisible();
  await page.getByRole("button", { name: "Release lease" }).click();
  await expect(page.getByText("The simulated identity is not allowed to perform this action.")).toBeVisible();

  await page.addInitScript(() => {
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText: async () => { throw new Error("permission denied"); } } });
  });
  await page.goto("?demo=normal&token=private-value#private-fragment");
  expect((await page.url())).not.toContain("private-value");
  expect((await page.url())).not.toContain("private-fragment");
  await page.getByRole("button", { name: "Copy share link" }).click();
  await expect(page.getByText("Copy failed")).toBeVisible();
});

test("refreshes a conflict deep link with an inspectable unknown outcome", async ({ page }) => {
  await page.goto("/demo/operations?demo=conflict&d_persona=admin");
  await expect(page.getByTestId("operations-page")).toBeVisible();
  await expect(page.getByText("Outcome unknown").nth(1)).toBeVisible();
  await page.reload();
  await expect(page.getByTestId("operations-page")).toBeVisible();
  await expect(page.getByText("Outcome unknown").nth(1)).toBeVisible();
});
