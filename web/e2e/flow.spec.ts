import { expect, test } from "./helpers";

// The tester's walkthrough, driven through the browser end to end against the
// local clusters: define a migration in the wizard, preflight, start, stop and
// resume a database, cut over to GO, read the report, clean up.
// dev/e2e.sh runs this after the screen suite, with the source writer stopped.

const PW = process.env.E2E_PASSWORD_DB || "spike-password-not-secret";
const DB = process.env.E2E_FLOW_DB || "e2e_flow";
const NAME = `Walkthrough ${new Date().toISOString().slice(11, 19)}`;

test.use({ storageState: "e2e/.auth/admin.json", viewport: { width: 1440, height: 900 } });
test.setTimeout(15 * 60_000);

test("complete online migration from the browser", async ({ page }) => {
  await page.goto("/migrations/new");
  // 1. Customer and permission
  await page.getByLabel("Migration name").fill(NAME);
  await page.getByLabel("Customer", { exact: true }).fill("Example Customer");
  await page.getByLabel("Customer account ID").fill("acct-0001");
  await page.getByLabel("Support ticket").fill("SUP-1234");
  await page.getByLabel("Permission granted by").fill("Jamie Customer");
  await page.getByRole("button", { name: "Save and continue" }).click();
  // 2. Source, 3. Target
  for (const [title, port] of [["Source cluster", "55432"], ["Target cluster", "55433"]] as const) {
    await expect(page.getByRole("heading", { name: title })).toBeVisible();
    await page.getByLabel("Host").fill("127.0.0.1");
    await page.getByLabel("Port").fill(port);
    await page.getByLabel("User").fill("doadmin");
    await page.getByLabel("Password").fill(PW);
    await page.getByLabel("TLS mode").selectOption("disable");
    await page.getByRole("button", { name: "Test connection" }).click();
    await expect(page.getByRole("list", { name: "Connection test results" })).toBeVisible();
    await expect(page.locator("ul[aria-label='Connection test results'] li.failed")).toHaveCount(0);
    await page.getByRole("button", { name: "Save and continue" }).click();
  }
  // 4. Databases: only the flow database
  const table = page.locator("table[aria-label='Databases to migrate']");
  await expect(table).toBeVisible();
  for (const box of await table.locator("input[type=checkbox]").all()) {
    const label = (await box.getAttribute("aria-label")) || "";
    if (await box.isDisabled()) continue;
    const want = label === `Include ${DB}`;
    if ((await box.isChecked()) !== want) await box.click();
  }
  await page.getByRole("button", { name: "Save and continue" }).click();
  // 5. Options: defaults (heartbeat on)
  await expect(page.getByLabel("Decoding plugin")).toBeVisible();
  await page.getByRole("button", { name: "Save and continue" }).click();
  // 6. Preflight
  await page.getByRole("button", { name: "Run preflight" }).click();
  const panel = page.getByRole("dialog", { name: "Preflight" });
  await expect(panel).toBeVisible();
  await expect(panel.getByText("Done", { exact: true }).first()).toBeVisible({ timeout: 180_000 });
  await panel.getByRole("button", { name: "Close" }).last().click();
  await expect(page.locator(".tile").filter({ hasText: "Can start" })).toContainText("Yes");
  await page.getByRole("button", { name: "Continue to plan review" }).click();
  // 7. Plan review and start
  await expect(page.getByText("Engine command per database")).toBeVisible();
  const review = page.getByLabel(/I reviewed the \d+ preflight warnings/);
  if (await review.count()) await review.check();
  await page.getByRole("button", { name: "Start migration" }).click();
  await expect(page.getByRole("heading", { name: NAME })).toBeVisible();

  // Watch it reach In sync.
  const row = page.locator("table[aria-label='Databases'] tr", { hasText: DB });
  await expect(row.locator(".pill", { hasText: "In sync" })).toBeVisible({ timeout: 300_000 });

  // Stop and resume the database.
  await row.getByRole("link", { name: DB }).click();
  await page.getByRole("button", { name: "Stop", exact: true }).click();
  await expect(page.locator(".pill", { hasText: "Stopped" }).first()).toBeVisible({ timeout: 60_000 });
  await page.getByRole("button", { name: "Resume", exact: true }).click();
  await expect(page.locator(".pill", { hasText: "In sync" }).first()).toBeVisible({ timeout: 180_000 });

  // Cut over.
  await page.getByRole("link", { name: "Cutover", exact: true }).click();
  await expect(page.locator(".pill", { hasText: /^Ready$/ })).toBeVisible({ timeout: 180_000 });
  await page.getByRole("button", { name: "Start the cutover" }).click();
  const dlg = page.getByRole("dialog", { name: "Start the cutover" });
  await dlg.getByLabel("Every application writer to the source is stopped").check();
  await dlg.getByRole("button", { name: "Start the cutover" }).click();
  const op = page.getByRole("dialog", { name: "Cutover" });
  await expect(op).toBeVisible();
  await op.getByRole("button", { name: "Minimize to tray" }).click();
  const verdict = page.getByTestId("verdict");
  await expect(verdict).toBeVisible({ timeout: 600_000 });
  await expect(verdict.locator(".verdict")).toHaveText("GO");

  // Verification and the report.
  await page.getByRole("link", { name: "Verification", exact: true }).click();
  await expect(page.locator(`table[aria-label='Verification for ${DB}']`)).toBeVisible();
  await expect(page.locator(".pill", { hasText: "Failed" })).toHaveCount(0);
  await page.getByRole("link", { name: "Report", exact: true }).click();
  await expect(page.frameLocator("iframe").getByText(NAME).first()).toBeVisible();

  // Clean up.
  await page.getByRole("button", { name: "Clean up" }).click();
  const confirm = page.getByRole("dialog", { name: "Clean up" });
  await confirm.getByLabel(/Type .* to confirm/).fill(NAME);
  await confirm.getByRole("button", { name: "Clean up" }).click();
  const cleanup = page.getByRole("dialog", { name: "Clean up" });
  await expect(cleanup.getByText("Done", { exact: true }).first()).toBeVisible({ timeout: 180_000 });
  await cleanup.getByRole("button", { name: "Close" }).last().click();
  await expect(page.locator(".pill", { hasText: "Cleaned up" }).first()).toBeVisible({ timeout: 60_000 });
});
