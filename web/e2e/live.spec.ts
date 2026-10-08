import { expect, type Ids, ids, settle, test, watch } from "./helpers";

let IDS: Ids;
test.use({ storageState: "e2e/.auth/admin.json", viewport: { width: 1440, height: 900 } });
test.beforeAll(async ({ browser }) => {
  const ctx = await browser.newContext({ storageState: "e2e/.auth/admin.json", ignoreHTTPSErrors: true });
  IDS = await ids(ctx.request);
  await ctx.close();
});

test("overview: cumulative layout shift under 0.1 while metrics stream", async ({ page }) => {
  await page.goto(`/migrations/${IDS.mig}`);
  await settle(page, "table[aria-label='Databases']");
  await page.evaluate(() => {
    (window as unknown as { __cls: number }).__cls = 0;
    new PerformanceObserver((l) => {
      for (const e of l.getEntries() as unknown as { value: number; hadRecentInput: boolean }[]) if (!e.hadRecentInput) (window as unknown as { __cls: number }).__cls += e.value;
    }).observe({ type: "layout-shift", buffered: false });
  });
  // The e2e server samples every 2 s and a writer runs on the source, so the
  // view, metrics and live values refresh many times in this window.
  const before = await page.locator("text=Live").count();
  await page.waitForTimeout(20_000);
  const cls = await page.evaluate(() => (window as unknown as { __cls: number }).__cls);
  expect(before).toBeGreaterThan(0);
  expect(cls).toBeLessThan(0.1);
});

test("overview: live updates arrive over the stream", async ({ page }) => {
  const events: string[] = [];
  page.on("response", (r) => { if (r.url().includes(`/api/v1/migrations/`) && r.url().includes("metrics")) events.push(r.url()); });
  await page.goto(`/migrations/${IDS.mig}`);
  await settle(page, "table[aria-label='Databases']");
  await page.waitForTimeout(12_000);
  expect(events.length).toBeGreaterThan(1);
});

const KEYBOARD = [
  { name: "home", path: () => "/" },
  { name: "overview", path: (i: Ids) => `/migrations/${i.mig}` },
  { name: "database", path: (i: Ids) => `/migrations/${i.mig}/databases/${encodeURIComponent(i.db)}` },
  { name: "cutover", path: (i: Ids) => `/migrations/${i.mig}/cutover` },
  { name: "preflight", path: (i: Ids) => `/migrations/${i.mig}/preflight` },
  { name: "wizard", path: (i: Ids) => `/migrations/${i.draft}/setup?step=3` },
  { name: "alerts", path: () => "/alerts" },
  { name: "settings", path: () => "/settings" },
  { name: "logs", path: () => "/logs" },
  { name: "system", path: () => "/system" },
];

for (const k of KEYBOARD) {
  test(`${k.name}: every control is reachable by Tab with a visible focus ring`, async ({ page }) => {
    await page.goto(k.path(IDS));
    await settle(page);
    const total = await page.evaluate(() => {
      const els = Array.from(document.querySelectorAll<HTMLElement>("a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex='0']"));
      let n = 0;
      for (const el of els) {
        const r = el.getBoundingClientRect();
        const cs = getComputedStyle(el);
        if (el.closest(".sr-only") && !el.matches("a[href='#main']")) continue;
        if ((r.width === 0 && r.height === 0) || cs.visibility === "hidden" || cs.display === "none") continue;
        if (el.matches("input[type=radio]") && !(el as HTMLInputElement).checked) continue; // a radio group is one tab stop
        el.dataset.kbd = String(n++);
      }
      return n;
    });
    const reached = new Set<string>();
    const noRing: string[] = [];
    await page.locator("body").click({ position: { x: 1, y: 1 } }).catch(() => {});
    for (let i = 0; i < total + 25 && reached.size < total; i++) {
      await page.keyboard.press("Tab");
      const r = await page.evaluate(() => {
        const el = document.activeElement as HTMLElement | null;
        if (!el || el === document.body) return null;
        const cs = getComputedStyle(el);
        const ring = (cs.outlineStyle !== "none" && parseFloat(cs.outlineWidth) > 0) || (cs.boxShadow && cs.boxShadow !== "none");
        return { id: el.dataset.kbd || "", ring, desc: `${el.tagName.toLowerCase()} "${(el.textContent || el.getAttribute("aria-label") || "").trim().slice(0, 40)}"` };
      });
      if (!r) continue;
      if (r.id) reached.add(r.id);
      if (!r.ring) noRing.push(r.desc);
    }
    const missed = await page.evaluate((got) => Array.from(document.querySelectorAll<HTMLElement>("[data-kbd]")).filter((e) => !got.includes(e.dataset.kbd!)).map((e) => `${e.tagName.toLowerCase()} "${(e.textContent || e.getAttribute("aria-label") || "").trim().slice(0, 40)}"`), [...reached]);
    expect(missed, "controls not reachable by Tab").toEqual([]);
    expect([...new Set(noRing)], "focused controls without a visible focus indicator").toEqual([]);
  });
}

test("logs: lines open from the keyboard", async ({ page }) => {
  await page.goto(`/migrations/${IDS.mig}/logs`);
  await settle(page, "[role=log]");
  await page.locator("[role=log]").focus();
  await page.keyboard.press("ArrowUp");
  await page.keyboard.press("Enter");
  await expect(page.getByRole("dialog", { name: "Log line" })).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(page.getByRole("dialog", { name: "Log line" })).toHaveCount(0);
});

test("command palette opens with Ctrl+K and navigates", async ({ page }) => {
  await page.goto("/");
  await settle(page);
  await page.keyboard.press("Control+k");
  await page.keyboard.type("Alerts");
  await page.keyboard.press("Enter");
  await expect(page).toHaveURL(/\/alerts$/);
});

test("viewer: action buttons are hidden, not disabled", async ({ browser }) => {
  const ctx = await browser.newContext({ storageState: "e2e/.auth/viewer.json", ignoreHTTPSErrors: true, viewport: { width: 1440, height: 900 } });
  const page = await ctx.newPage();
  const g = watch(page);
  await page.goto(`/migrations/${IDS.mig}`);
  await settle(page, "table[aria-label='Databases']");
  for (const name of ["Pause", "Abort", "Clean up"]) await expect(page.getByRole("button", { name, exact: true })).toHaveCount(0);
  await page.goto(`/migrations/${IDS.mig}/databases/${encodeURIComponent(IDS.db)}`);
  await settle(page, "text=Positions");
  for (const name of ["Stop", "Restart from zero"]) await expect(page.getByRole("button", { name, exact: true })).toHaveCount(0);
  await page.goto("/");
  await settle(page);
  await expect(page.getByRole("link", { name: "New migration" })).toHaveCount(0);
  g.check();
  await ctx.close();
});

test("operator: sees actions but not user management", async ({ browser }) => {
  const ctx = await browser.newContext({ storageState: "e2e/.auth/operator.json", ignoreHTTPSErrors: true, viewport: { width: 1440, height: 900 } });
  const page = await ctx.newPage();
  await page.goto(`/migrations/${IDS.mig}`);
  await settle(page, "table[aria-label='Databases']");
  await expect(page.getByRole("button", { name: "Pause", exact: true })).toBeVisible();
  await page.goto("/settings");
  await settle(page);
  await page.getByRole("button", { name: "Users and roles" }).click();
  await expect(page.getByText("Only Admins manage users.")).toBeVisible();
  await ctx.close();
});
