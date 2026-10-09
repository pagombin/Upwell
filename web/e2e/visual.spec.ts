import { expect, type Ids, ids, setTheme, settle, test } from "./helpers";

// Visual regression baseline on screens whose content does not depend on the
// clock; times and durations are masked. Refresh with dev/e2e.sh --update.
let IDS: Ids;
test.use({ storageState: "e2e/.auth/admin.json", viewport: { width: 1440, height: 900 } });
test.beforeAll(async ({ browser }) => {
  const ctx = await browser.newContext({ storageState: "e2e/.auth/admin.json", ignoreHTTPSErrors: true });
  IDS = await ids(ctx.request);
  await ctx.close();
});

const PAGES = [
  { name: "settings", path: () => "/settings" },
  { name: "verification-nogo", path: (i: Ids) => `/migrations/${i.nogo}/verification` },
  { name: "cutover-go", path: (i: Ids) => `/migrations/${i.go}/cutover` },
  { name: "wizard-options", path: (i: Ids) => `/migrations/${i.draft}/setup?step=4` },
];

for (const p of PAGES) {
  for (const theme of ["dark", "light"]) {
    test(`visual: ${p.name} ${theme}`, async ({ page }) => {
      await setTheme(page, theme);
      await page.goto(p.path(IDS));
      await settle(page);
      const mask = [page.locator("text=/\\d{2}:\\d{2}(:\\d{2})?|\\d+[smhd]( \\d+[smh])?|ago|Up \\d/"), page.locator(".rail nav"), page.locator(".topbar"), page.locator(".banner"), page.locator(".mono")];
      await expect(page).toHaveScreenshot(`${p.name}-${theme}.png`, { mask, fullPage: false });
    });
  }
}
