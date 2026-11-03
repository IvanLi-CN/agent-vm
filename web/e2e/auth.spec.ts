import { expect, test } from "@playwright/test";

const session = {
  issuer: "https://issuer.example",
  subject: "admin@example",
  role: "administrator",
  capabilities: ["read", "operate", "manage_images", "maintain_system", "manage_tokens", "observe_admin"],
  csrf_token: "csrf-token",
  expires_at: "2099-01-01T00:00:00Z",
};

function jsonBody(value: unknown): string {
  return JSON.stringify(value);
}

test("shows an explicit login for an unauthenticated deep link", async ({ page }) => {
  await page.route("**/api/v1/session", (route) => route.fulfill({ status: 401, contentType: "application/problem+json", body: jsonBody({ error: { code: "authentication_required", phase: "authentication" } }) }));
  await page.goto("/vms");

  await expect(page.getByRole("heading", { name: /signed out|未登录/i })).toBeVisible();
  const login = page.getByRole("link", { name: /sign in to management|登录管理台/i });
  await expect(login).toHaveAttribute("href", "/api/v1/auth/oidc/start?return_to=%2Fvms");
  await expect(page.getByText(/sole administrator|单一管理员/i)).toHaveCount(0);
  await expect(page.getByTestId("vms-page")).toHaveCount(0);
});

test("localizes callback failure and removes the auth marker", async ({ page }) => {
  await page.route("**/api/v1/session", (route) => route.fulfill({ status: 401, contentType: "application/problem+json", body: jsonBody({ error: { code: "authentication_required", phase: "authentication" } }) }));
  await page.goto("/vms?auth=failed");

  await expect(page).toHaveURL(/\/vms$/);
  await expect(page.getByRole("heading", { name: /sign-in did not complete|登录未完成/i })).toBeVisible();
  await expect(page.locator("body")).not.toContainText("error_description");
});

test("suspends a page after session loss without refetching the resource", async ({ page }) => {
  let vmRequests = 0;
  await page.route("**/api/v1/session", (route) => route.fulfill({ status: 200, contentType: "application/json", body: jsonBody(session) }));
  await page.route("**/api/v1/vms*", (route) => {
    vmRequests += 1;
    return route.fulfill({ status: 401, contentType: "application/problem+json", body: jsonBody({ error: { code: "authentication_required", phase: "authentication" } }) });
  });
  await page.goto("/vms");

  await expect(page.getByRole("heading", { name: /session expired|会话已失效/i })).toBeVisible();
  await expect(page.getByRole("link", { name: /sign in again|重新登录/i })).toHaveAttribute("href", "/api/v1/auth/oidc/start?return_to=%2Fvms");
  await expect(page.getByText("admin@example", { exact: true })).toHaveCount(0);
  const requestCount = vmRequests;
  await page.waitForTimeout(300);
  expect(vmRequests).toBe(requestCount);
  await expect(page).toHaveURL(/\/vms$/);
});
