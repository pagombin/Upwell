import { expect, test as setup } from "@playwright/test";

const PW = process.env.E2E_PASSWORD || "correct-horse-battery-staple";

for (const user of ["admin", "operator", "viewer"]) {
  setup(`sign in as ${user}`, async ({ page }) => {
    await page.goto("/login");
    await page.getByLabel("Username").fill(user);
    await page.getByLabel("Password").fill(PW);
    await page.getByRole("button", { name: "Sign in" }).click();
    await expect(page.getByRole("heading", { name: "Home" })).toBeVisible();
    await page.context().storageState({ path: `e2e/.auth/${user}.json` });
  });
}
