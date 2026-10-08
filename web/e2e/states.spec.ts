import type { Page, Route } from "@playwright/test";
import { expect, type Ids, ids, settle, test } from "./helpers";

// Loading, empty, error and stale states on every screen, provoked by
// intercepting the screen's main API call.

// api: the request path; "~/suffix" matches any migration ID (the UI uses
// short and full IDs) followed by suffix.
interface S { name: string; path: (i: Ids) => string; api: (i: Ids) => string; empty?: (real: unknown) => unknown; emptyText?: string }

const enc = encodeURIComponent;
const STATES: S[] = [
  { name: "home", path: () => "/", api: () => "/api/v1/migrations", empty: () => [], emptyText: "No migration is running" },
  { name: "overview", path: (i) => `/migrations/${i.mig}`, api: (i) => `/api/v1/migrations/${i.mig}`, empty: (r) => ({ ...(r as object), databases: [], included: 0 }), emptyText: "No databases included" },
  { name: "database", path: (i) => `/migrations/${i.mig}/databases/${enc(i.db)}`, api: (i) => `~/databases/${enc(i.db)}`, empty: (r) => ({ ...(r as object), attempts: [] }), emptyText: "The engine has not started for this database yet" },
  { name: "replication", path: (i) => `/migrations/${i.mig}/replication`, api: () => "~/metrics", empty: () => [], emptyText: "No data yet" },
  { name: "source-health", path: (i) => `/migrations/${i.mig}/source-health`, api: () => "~/source-health", empty: () => ({ available: false, reason: "The source could not be reached." }), emptyText: "Source health is not available" },
  { name: "preflight", path: (i) => `/migrations/${i.mig}/preflight`, api: () => "~/preflight/latest", empty: () => ({ run: null, results: [], catalog: [] }), emptyText: "Preflight has not run yet" },
  { name: "cutover", path: (i) => `/migrations/${i.mig}/cutover`, api: () => "~/cutover/readiness", empty: () => ({ conditions: [], ready: false }), emptyText: "Starts when you confirm writers are stopped" },
  { name: "verification", path: (i) => `/migrations/${i.go}/verification`, api: () => "~/verification", empty: () => ({ run_id: "", results: [] }), emptyText: "Not verified yet" },
  { name: "wizard-plan", path: (i) => `/migrations/${i.draft}/setup?step=6`, api: () => "~/plan", empty: () => ({ databases: [], estimates: {} }), emptyText: "Engine command per database" },
  { name: "logs", path: () => "/logs", api: () => "/api/v1/logs", empty: () => [], emptyText: "No log lines match" },
  { name: "events", path: () => "/events", api: () => "/api/v1/events", empty: () => [], emptyText: "No events yet" },
  { name: "alerts", path: () => "/alerts", api: () => "/api/v1/alerts", empty: () => [], emptyText: "No alerts" },
  { name: "host", path: () => "/host", api: () => "/api/v1/host/metrics", empty: () => [], emptyText: "No data yet" },
  { name: "settings", path: () => "/settings", api: () => "/api/v1/settings", empty: () => [], emptyText: "Nothing to configure here" },
  { name: "system", path: () => "/system", api: () => "/api/v1/system" },
  { name: "report", path: (i) => `/migrations/${i.go}/report`, api: (i) => `/api/v1/migrations/${i.go}` },
];

let IDS: Ids;
test.use({ storageState: "e2e/.auth/admin.json", viewport: { width: 1440, height: 900 } });
test.beforeAll(async ({ browser }) => {
  const ctx = await browser.newContext({ storageState: "e2e/.auth/admin.json", ignoreHTTPSErrors: true });
  IDS = await ids(ctx.request);
  await ctx.close();
});

function matcher(apiPath: string) {
  if (apiPath.startsWith("~/")) {
    const suffix = apiPath.slice(1);
    return (u: URL) => u.pathname.startsWith("/api/v1/migrations/") && u.pathname.endsWith(suffix);
  }
  return (u: URL) => u.pathname === apiPath;
}

async function intercept(page: Page, apiPath: string, handler: (r: Route) => Promise<void> | void) {
  await page.route(matcher(apiPath), handler);
}

for (const s of STATES) {
  test(`${s.name}: loading`, async ({ page }) => {
    let release: () => void = () => {};
    const held = new Promise<void>((r) => { release = r; });
    await intercept(page, s.api(IDS), async (r) => { await held; await r.continue().catch(() => {}); });
    await page.goto(s.path(IDS));
    await expect(page.locator('[aria-busy="true"]').first()).toBeVisible();
    release();
  });

  test(`${s.name}: error`, async ({ page, guard }) => {
    guard.allow(/\/api\//);
    guard.allow(/Failed to load resource/);
    await intercept(page, s.api(IDS), (r) => r.fulfill({ status: 500, json: { code: "internal", message: "Something went wrong on the server; the error is in the app log.", remediation: "" } }));
    await page.goto(s.path(IDS));
    await expect(page.locator(".errorstate").first()).toBeVisible();
    await expect(page.getByText("Something went wrong on the server").first()).toBeVisible();
  });

  if (s.empty) {
    test(`${s.name}: empty`, async ({ page }) => {
      await intercept(page, s.api(IDS), async (r) => {
        const real = await (await r.fetch()).json().catch(() => ({}));
        await r.fulfill({ json: s.empty!(real) });
      });
      await page.goto(s.path(IDS));
      await expect(page.getByText(s.emptyText!).first()).toBeVisible();
    });
  }

  test(`${s.name}: stale`, async ({ page, guard }) => {
    guard.allow(/\/api\//);
    guard.allow(/Failed to load resource/);
    await page.clock.install();
    await page.goto(s.path(IDS));
    await settle(page);
    await page.route("**/api/**", (r) => r.abort("failed"));
    await page.clock.fastForward(45_000);
    await page.clock.runFor(16_000);
    await expect(page.getByTestId("stale-banner")).toBeVisible();
    await expect(page.getByTestId("stale-banner")).toContainText("last updated");
  });
}
