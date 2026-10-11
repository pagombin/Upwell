import { expect, test } from "./helpers";

// The overnight items, each driven through the browser end to end. dev/e2e.sh
// runs these after the walkthrough, against migrations it prepared.

test.use({ storageState: "e2e/.auth/admin.json", viewport: { width: 1440, height: 900 } });
test.setTimeout(15 * 60_000);

const FREEZE = process.env.E2E_FREEZE || "";

test("write freeze: preview, apply at cutover, GO, undo", async ({ page }) => {
  test.skip(!FREEZE, "dev/e2e.sh prepares the write-freeze migration");
  await page.goto(`/migrations/${FREEZE}/cutover`);
  await expect(page.locator(".pill", { hasText: /^Ready$/ })).toBeVisible({ timeout: 300_000 });
  await page.getByLabel(/Write freeze: Upwell makes the source databases read-only/).check();
  await page.getByRole("button", { name: "Start the cutover" }).click();
  const dlg = page.getByRole("dialog", { name: "Start the cutover with a write freeze" });
  await expect(dlg.getByText("ALTER DATABASE \"e2e_freeze\" SET default_transaction_read_only = on;", { exact: false })).toBeVisible();
  await expect(dlg.getByText(/a session can still run SET default_transaction_read_only = off/)).toBeVisible();
  const apply = dlg.getByRole("button", { name: "Apply the freeze and start" });
  await expect(apply).toBeDisabled();
  await dlg.getByLabel(/Type the source cluster's name/).fill("127.0.0.1");
  await apply.click();
  const op = page.getByRole("dialog", { name: "Cutover with write freeze" });
  await expect(op).toBeVisible();
  await op.getByRole("button", { name: "Minimize to tray" }).click();
  const verdict = page.getByTestId("verdict");
  await expect(verdict).toBeVisible({ timeout: 600_000 });
  await expect(verdict.locator(".verdict")).toHaveText("GO");
  const banner = page.getByTestId("freeze-banner");
  await expect(banner).toContainText("Write freeze applied on e2e_freeze");
  await banner.getByRole("button", { name: "Undo write freeze" }).click();
  await page.getByRole("dialog", { name: "Undo the write freeze" }).getByRole("button", { name: "Undo write freeze" }).click();
  await expect(page.getByTestId("freeze-undone")).toContainText("Write freeze undone", { timeout: 30_000 });
});
