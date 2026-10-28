import { expect, test } from "@playwright/test";

test("keeps the demo isolated while a scenario and simulated action change", async ({ page }) => {
  const realRequests: string[] = [];
  page.on("request", (request) => { if (request.url().includes("/api/v1")) realRequests.push(request.url()); });
  await page.goto("?demo=normal&d_persona=admin&d_network=online&token=must-not-share#private-fragment");
  await expect(page.getByRole("heading", { name: "Demo inspector" })).toBeVisible();
  await page.getByLabel("Scene").selectOption("capacity");
  await expect(page.getByTestId("overview-page").getByText("Admission is unknown")).toBeVisible();
  await expect(page).toHaveURL(/demo=capacity/);
  expect((await page.url())).not.toContain("must-not-share");
  expect((await page.url())).not.toContain("private-fragment");
  await page.goBack();
  await expect(page).toHaveURL(/demo=normal/);
  await expect(page.getByLabel("Scene")).toHaveValue("normal");
  await page.getByLabel("Scene").selectOption("capacity");
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
  await page.getByRole("button", { name: "Close demo inspector" }).click();
  await expect(page.getByRole("button", { name: "Open demo inspector" })).toBeVisible();
  await page.getByRole("button", { name: "Open demo inspector" }).click();
  await expect(page.getByRole("heading", { name: "Demo inspector" })).toBeVisible();
  await page.goto("?demo=normal&d_bootstrap=fail");
  await expect(page.getByRole("heading", { name: "Demo could not start" })).toBeVisible();
  await expect(page.getByText("The simulated environment was not ready")).toBeVisible();
  await expect(page.getByRole("heading", { name: "Demo inspector" })).toHaveCount(0);
});
