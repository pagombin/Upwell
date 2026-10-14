import { expect, ids, test, totp } from "./helpers";

// The overnight items, each driven through the browser end to end. dev/e2e.sh
// runs these after the walkthrough, against migrations it prepared.

test.use({ storageState: "e2e/.auth/admin.json", viewport: { width: 1440, height: 900 } });
test.setTimeout(15 * 60_000);

const FREEZE = process.env.E2E_FREEZE || "";

test("write freeze: preview, apply at cutover, GO, undo", async ({ page }) => {
  test.skip(!FREEZE, "dev/e2e.sh prepares the write-freeze migration");
  // Start it from the plan review (the run lock is free once the walkthrough's
  // migration ended GO), and let it reach In sync.
  await page.goto(`/migrations/${FREEZE}/setup?step=6`);
  await expect(page.getByText("Engine command per database")).toBeVisible();
  const review = page.getByLabel(/I reviewed the \d+ preflight warnings/);
  if (await review.count()) await review.check();
  await page.getByRole("button", { name: "Start migration" }).click();
  await expect(page.locator("table[aria-label='Databases'] .pill", { hasText: "In sync" })).toBeVisible({ timeout: 300_000 });
  // Watched tables (overnight item 9) while it streams.
  await page.goto(`/migrations/${FREEZE}/databases/e2e_freeze`);
  await page.getByRole("button", { name: "Watch a table" }).click();
  const pick = page.getByRole("dialog", { name: "Watch a table in e2e_freeze" });
  await pick.getByLabel("Table").selectOption("public.orders");
  await pick.getByRole("button", { name: "Watch" }).click();
  await expect(page.locator("table[aria-label='Watched tables']")).toContainText("public.orders");
  await expect(page.locator("table[aria-label='Watched tables']")).toContainText("Equal", { timeout: 150_000 });
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

// defineInWizard walks the wizard's first five steps for one database.
async function defineInWizard(page: import("@playwright/test").Page, name: string, db: string, mode?: string) {
  const PW = process.env.E2E_PASSWORD_DB || "spike-password-not-secret";
  await page.goto("/migrations/new");
  await page.getByLabel("Migration name").fill(name);
  await page.getByLabel("Customer", { exact: true }).fill("Example Customer");
  await page.getByLabel("Customer account ID").fill("acct-0001");
  await page.getByLabel("Support ticket").fill("SUP-1234");
  await page.getByLabel("Permission granted by").fill("Jamie Customer");
  await page.getByRole("button", { name: "Save and continue" }).click();
  for (const [title, port] of [["Source cluster", "55432"], ["Target cluster", "55433"]] as const) {
    await expect(page.getByRole("heading", { name: title })).toBeVisible();
    await page.getByLabel("Host").fill("127.0.0.1");
    await page.getByLabel("Port").fill(port);
    await page.getByLabel("User").fill("doadmin");
    await page.getByLabel("Password").fill(PW);
    await page.getByLabel("TLS mode").selectOption("disable");
    await page.getByRole("button", { name: "Test connection" }).click();
    await expect(page.getByRole("list", { name: "Connection test results" })).toBeVisible();
    await page.getByRole("button", { name: "Save and continue" }).click();
  }
  const table = page.locator("table[aria-label='Databases to migrate']");
  await expect(table).toBeVisible();
  for (const box of await table.locator("input[type=checkbox]").all()) {
    const label = (await box.getAttribute("aria-label")) || "";
    if (await box.isDisabled()) continue;
    const want = label === `Include ${db}`;
    if ((await box.isChecked()) !== want) await box.click();
  }
  await page.getByRole("button", { name: "Save and continue" }).click();
  await expect(page.getByLabel("Decoding plugin")).toBeVisible();
  if (mode) await page.getByLabel("Mode").selectOption(mode);
  await page.getByRole("button", { name: "Save and continue" }).click();
  await page.getByRole("button", { name: "Run preflight" }).click();
  const panel = page.getByRole("dialog", { name: "Preflight" });
  await expect(panel.getByText("Done", { exact: true }).first()).toBeVisible({ timeout: 180_000 });
  await panel.getByRole("button", { name: "Close" }).last().click();
  await expect(page.locator(".tile").filter({ hasText: "Can start" })).toContainText("Yes");
  await page.getByRole("button", { name: "Continue to plan review" }).click();
  await expect(page.getByText("Engine command per database")).toBeVisible();
}

async function cleanUp(page: import("@playwright/test").Page, name: string) {
  await page.getByRole("button", { name: "Clean up" }).click();
  const confirm = page.getByRole("dialog", { name: "Clean up" });
  await confirm.getByLabel(/Type .* to confirm/).fill(name);
  await confirm.getByRole("button", { name: "Clean up" }).click();
  const cleanup = page.getByRole("dialog", { name: "Clean up" });
  await expect(cleanup.getByText("Done", { exact: true }).first()).toBeVisible({ timeout: 180_000 });
  await cleanup.getByRole("button", { name: "Close" }).last().click();
}

test("offline migration from the wizard to GO", async ({ page }) => {
  const name = `Offline ${new Date().toISOString().slice(11, 19)}`;
  await defineInWizard(page, name, process.env.E2E_OFFLINE_DB || "e2e_offline", "offline");
  // The engine command copies once.
  await expect(page.locator("pre.cmd").first()).not.toContainText("--follow");
  const review = page.getByLabel(/I reviewed the \d+ preflight warnings/);
  if (await review.count()) await review.check();
  const startBtn = page.getByRole("button", { name: "Start offline migration" });
  await expect(startBtn).toBeDisabled();
  await page.getByRole("checkbox", { name: "Every application writer to the source is stopped" }).check();
  await startBtn.click();
  const verdict = page.getByTestId("verdict");
  await expect(verdict).toBeVisible({ timeout: 600_000 });
  await expect(verdict.locator(".verdict")).toHaveText("GO");
  await expect(page.getByText("Offline migration", { exact: true })).toBeVisible();
  await cleanUp(page, name);
});

test.describe("signed out", () => {
  test.use({ storageState: { cookies: [], origins: [] } });
  test("second factor: set up, sign in with a code and a recovery code", async ({ page, guard }) => {
    // Signing in with the password alone answers 401 mfa_required: expected.
    guard.allow(/\/api\/v1\/auth\/login/);
    guard.allow(/status of 401/);
    const pw = process.env.E2E_PASSWORD || "correct-horse-battery-staple";
    const signIn = async (otp?: string) => {
      await page.goto("/login");
      await page.getByLabel("Username").fill("mfa-user");
      await page.getByLabel("Password").fill(pw);
      await page.getByRole("button", { name: "Sign in" }).click();
      if (otp) {
        await page.getByLabel("Authentication code").fill(otp);
        await page.getByRole("button", { name: "Sign in" }).click();
      }
      await expect(page.getByRole("heading", { name: "Home" })).toBeVisible();
    };
    const signOut = async () => {
      await page.getByRole("button", { name: /mfa-user/ }).click();
      await page.getByRole("menuitem", { name: "Sign out" }).click();
      await expect(page.getByLabel("Username")).toBeVisible();
    };
    await signIn();
    await page.goto("/account");
    await page.getByRole("button", { name: "Set up" }).click();
    const secret = (await page.getByTestId("totp-secret").textContent()) || "";
    await page.getByLabel("Code from the app").fill(await totp(secret));
    await page.getByRole("button", { name: "Turn on" }).click();
    const codes = ((await page.getByTestId("recovery-codes").locator("pre").textContent()) || "").split("\n");
    expect(codes).toHaveLength(10);
    await page.getByRole("button", { name: "I saved them" }).click();
    await expect(page.locator(".pill", { hasText: /^On$/ })).toBeVisible();
    // Sign in again with the next step's code, then with a recovery code.
    await signOut();
    await signIn(await totp(secret, 1));
    await signOut();
    await signIn(codes[0]);
  });
});

test("support bundle: preview, then download", async ({ page }) => {
  test.skip(!process.env.E2E_MIG, "dev/e2e.sh prepares the running migration");
  await page.goto(`/migrations/${process.env.E2E_MIG}`);
  await page.getByRole("button", { name: "Support bundle" }).click();
  const dlg = page.getByRole("dialog", { name: "Support bundle" });
  const files = dlg.locator("table[aria-label='Support bundle files']");
  await expect(files).toContainText("manifest.json");
  await expect(files).toContainText("logs/upwell.jsonl");
  const [dl] = await Promise.all([page.waitForEvent("download"), dlg.getByRole("link", { name: "Download .tar.gz" }).click()]);
  expect(dl.suggestedFilename()).toMatch(/^upwell-support-.*\.tar\.gz$/);
});

test("PDF report: download from the report tab", async ({ page }) => {
  test.skip(!FREEZE, "dev/e2e.sh prepares the write-freeze migration (GO after the freeze test)");
  await page.goto(`/migrations/${FREEZE}/report`);
  const [dl] = await Promise.all([page.waitForEvent("download"), page.getByRole("link", { name: "Download PDF" }).click()]);
  expect(dl.suggestedFilename()).toMatch(/^upwell-.*\.pdf$/);
  const path = await dl.path();
  const fs = await import("node:fs");
  expect(fs.readFileSync(path).subarray(0, 5).toString()).toBe("%PDF-");
});

test("queue: a ready migration is queued, reordered and unqueued", async ({ page }) => {
  test.skip(!process.env.E2E_DRAFT, "dev/e2e.sh prepares the draft migration");
  await page.goto(`/migrations/${process.env.E2E_DRAFT}/setup?step=6`);
  await expect(page.getByText("Engine command per database")).toBeVisible();
  const review = page.getByLabel(/I reviewed the \d+ preflight warnings/);
  if (await review.count()) await review.check();
  await page.getByRole("button", { name: "Queue instead" }).click();
  await expect(page.getByRole("heading", { name: "Home" })).toBeVisible();
  const q = page.locator("table[aria-label='Queue']");
  await expect(q).toContainText("Draft: wizard walkthrough");
  // The checkbox shows the stored value, so it turns on once the server has it.
  await q.getByRole("checkbox").click();
  await expect(q.getByRole("checkbox")).toBeChecked();
  await q.getByRole("button", { name: "Unqueue" }).click();
  await expect(page.getByText("No migrations are queued")).toBeVisible();
});

test("saved log views: save, pick, delete", async ({ page }) => {
  await page.goto("/logs");
  await page.getByRole("button", { name: "Save these filters" }).click();
  await page.getByLabel("View name").fill("e2e view");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect(page.getByLabel("Saved view")).toContainText("e2e view");
  await page.getByLabel("Saved view").selectOption({ label: "e2e view" });
  await page.getByRole("button", { name: "Delete view" }).click();
  await expect(page.getByLabel("Saved view")).not.toContainText("e2e view");
});

test("host: network throughput charts", async ({ page }) => {
  await page.goto("/host");
  await expect(page.getByRole("img", { name: "Network received" })).toBeVisible({ timeout: 60_000 });
  await expect(page.getByRole("img", { name: "Network sent" })).toBeVisible();
});

test("TLS: check, install and roll back an uploaded certificate", async ({ page }) => {
  const cert = process.env.E2E_UPLOAD_CERT, key = process.env.E2E_UPLOAD_KEY;
  test.skip(!cert || !key, "dev/e2e.sh generates the certificate");
  await page.goto("/settings");
  await page.getByRole("button", { name: "TLS", exact: true }).click();
  await page.getByLabel("Certificate file").setInputFiles(cert!);
  await page.getByLabel("Private key file").setInputFiles(key!);
  await page.getByRole("button", { name: "Check", exact: true }).click();
  await expect(page.getByTestId("cert-check")).toContainText("upwell-e2e.local");
  await page.getByRole("button", { name: "Install this certificate" }).click();
  await page.getByRole("dialog", { name: "Install this certificate" }).getByRole("button", { name: "Install" }).click();
  await expect(page.locator("dl.kv")).toContainText("upwell-e2e.local");
  await expect(page.locator("dl.kv")).toContainText("Uploaded here");
  await page.getByRole("button", { name: "Roll back to the previous certificate" }).click();
  await page.getByRole("dialog", { name: "Roll back the certificate" }).getByRole("button", { name: "Roll back" }).click();
  await expect(page.locator("dl.kv")).not.toContainText("upwell-e2e.local");
});

test("database page during a base copy: per-table progress and named charts", async ({ page }) => {
  // The droplet (2026-10-12): the bars stayed empty (COPY FROM STDIN reports
  // no total) and the charts had no titles, so "No data yet" said nothing.
  const I = await ids(page.request);
  const db = I.db;
  await page.route((u) => u.pathname.startsWith("/api/v1/migrations/") && u.pathname.endsWith(`/databases/${db}`), async (r) => {
    const real = await (await r.fetch()).json();
    real.database.state = "base_copy";
    real.copy_progress = {
      copies: [
        { table: "stock3", bytes: 7.1 * 2 ** 30, bytes_total: 0, tuples: 24_900_000, rows_estimate: 47_000_000, fraction: 24.9 / 47, source_bytes: 14 * 2 ** 30 },
        { table: "history1", bytes: 1024, bytes_total: 0, tuples: 10 },
      ],
      indexes: [],
    };
    await r.fulfill({ json: real });
  });
  await page.route((u) => u.pathname.startsWith("/api/v1/migrations/") && u.pathname.endsWith("/metrics"), (r) => r.fulfill({ json: [] }));
  await page.goto(`/migrations/${I.mig}/databases/${db}`);
  const card = page.locator(".card", { has: page.getByRole("heading", { name: "Base copy in progress" }) });
  await expect(card.getByRole("progressbar", { name: "stock3 copy" })).toHaveAttribute("aria-valuenow", "53");
  await expect(card.getByText("of about 47.0 M rows · 53%", { exact: false })).toBeVisible();
  await expect(card.getByText("No row estimate on the source")).toBeVisible();
  for (const name of ["Copied to the target", "Slot retained WAL", "Backlog", "Replay rate", "Heartbeat latency"]) {
    await expect(page.getByRole("heading", { name, exact: true })).toBeVisible();
  }
  await expect(page.getByText("Measured once the base copy has finished and the engine applies changes.").first()).toBeVisible();
});

test("database page during a base copy: a failed progress read is shown as such", async ({ page }) => {
  // D80: a read that failed was shown as "No COPY or index build is running".
  const I = await ids(page.request);
  const db = I.db;
  await page.route((u) => u.pathname.startsWith("/api/v1/migrations/") && u.pathname.endsWith(`/databases/${db}`), async (r) => {
    const real = await (await r.fetch()).json();
    real.database.state = "base_copy";
    real.copy_progress = { copies: [], indexes: [], at: Date.now() - 20_000, error: "connecting to the target: timeout: context deadline exceeded" };
    await r.fulfill({ json: real });
  });
  await page.goto(`/migrations/${I.mig}/databases/${db}`);
  const card = page.locator(".card", { has: page.getByRole("heading", { name: "Base copy in progress" }) });
  await expect(card.getByText("Upwell could not read the progress from the target")).toBeVisible();
  await expect(card.getByText("context deadline exceeded", { exact: false })).toBeVisible();
  await expect(card.getByText("No COPY or index build")).toHaveCount(0);
  await expect(card.getByText(/Read from the target .* ago/)).toBeVisible();
});

test("logs: older lines, dates, search highlight, error navigation (D86)", async ({ page }) => {
  const I = await ids(page.request);
  const day = 24 * 3600e3;
  const now = Date.now();
  const line = (seq: number, ts: number) => ({ seq, ts, level: seq % 500 === 0 ? "error" : "info", component: "engine", migration: I.mig, database: I.db, msg: `line ${seq} copying public.orders` });
  await page.route((u) => u.pathname === "/api/v1/logs", (r) => {
    const q = new URL(r.request().url()).searchParams;
    const before = Number(q.get("before") || 0), after = Number(q.get("after") || 0);
    if (after) return r.fulfill({ json: [] });
    // Newest 2,000 lines today; the 2,000 before them yesterday.
    const top = before ? before - 1 : 4000;
    const out = [];
    for (let s = top - 1999; s <= top; s++) out.push(line(s, s > 2000 ? now - (4000 - s) * 1000 : now - day - (2000 - s) * 1000));
    return r.fulfill({ json: out });
  });
  await page.route((u) => u.pathname.startsWith("/api/v1/stream/logs"), (r) => r.fulfill({ status: 200, contentType: "text/event-stream", body: ": no live lines in this test\n\n" }));
  await page.goto(`/migrations/${I.mig}/logs`);
  await expect(page.getByText(/^2,000 lines/)).toBeVisible();
  await page.getByRole("button", { name: "Load older lines" }).click();
  await expect(page.getByText(/^4,000 lines/)).toBeVisible();
  // Lines from yesterday carry their date.
  await expect(page.locator(".logrow").first().locator("span").first()).toHaveText(/^\d{2} \w{3} /);
  await page.getByRole("button", { name: "Next error line" }).click();
  await expect(page.locator(".logrow.hl")).toContainText("error");
  await page.getByLabel("Search messages").fill("orders");
  await expect(page.locator(".logrow mark").first()).toHaveText("orders");
});
