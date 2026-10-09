import type { Page } from "@playwright/test";
import path from "node:path";
import { axeViolations, expect, type Ids, ids, layoutViolations, setTheme, settle, test, THEMES, VIEWPORTS, watch } from "./helpers";

// Every screen in the spec, at 1440x900 and 1280x800, dark and light:
// screenshot to docs/screenshots plus layout and accessibility checks.

const SHOTS = path.resolve(process.cwd(), "../docs/screenshots");

interface Screen { name: string; path: (i: Ids) => string; ready: string; auth?: "admin" | "none"; prepare?: (p: Page, i: Ids) => Promise<void>; setup?: (p: Page) => Promise<void> }

const enc = encodeURIComponent;
export const SCREENS: Screen[] = [
  { name: "setup", path: () => "/setup", ready: "text=Finish setting up Upwell", auth: "none", setup: async (p) => {
    await p.route("**/api/v1/auth/session", (r) => r.fulfill({ json: { user: null, needs_setup: true } }));
    await p.route("**/api/v1/setup", (r) => r.request().method() === "GET" ? r.fulfill({ json: { needs_setup: true, tls: { fingerprint: "8B:D3:85:6A:BE:EF:8D:48:0B:73:8E:D1:90:54:52:86:E8:BD:A8:89:FD:7D:1C:26:72:E8:E2:B1:3B:E6:38:80", not_after: 1825723878000, subject: "upwell.example", self_signed: true }, setup_token_file: "/var/lib/upwell/setup-token", version: "0.1.0" } }) : r.continue());
  } },
  { name: "sign-in", path: () => "/login", ready: "text=Sign in", auth: "none" },
  { name: "home", path: () => "/", ready: "h1:has-text('Home')" },
  { name: "palette", path: () => "/", ready: "h1:has-text('Home')", prepare: async (p) => { await p.keyboard.press("Control+k"); await p.getByRole("dialog", { name: "Command palette" }).waitFor(); } },
  { name: "wizard-1-permission", path: () => "/migrations/new", ready: "text=Customer and permission record" },
  { name: "wizard-2-source", path: (i) => `/migrations/${i.draft}/setup?step=1`, ready: "text=Source cluster" },
  { name: "wizard-3-target", path: (i) => `/migrations/${i.draft}/setup?step=2`, ready: "text=Target cluster" },
  { name: "wizard-4-databases", path: (i) => `/migrations/${i.draft}/setup?step=3`, ready: "table[aria-label='Databases to migrate']" },
  { name: "wizard-5-options", path: (i) => `/migrations/${i.draft}/setup?step=4`, ready: "text=Decoding plugin" },
  { name: "wizard-6-preflight", path: (i) => `/migrations/${i.draft}/setup?step=5`, ready: "text=Can start" },
  { name: "wizard-7-plan", path: (i) => `/migrations/${i.draft}/setup?step=6`, ready: "text=Engine command per database" },
  { name: "overview", path: (i) => `/migrations/${i.mig}`, ready: "table[aria-label='Databases']" },
  { name: "database", path: (i) => `/migrations/${i.mig}/databases/${enc(i.db)}`, ready: "text=Positions" },
  { name: "replication", path: (i) => `/migrations/${i.mig}/replication`, ready: "text=CDC across all databases" },
  { name: "source-health", path: (i) => `/migrations/${i.mig}/source-health`, ready: "text=Oldest snapshot" },
  { name: "preflight", path: (i) => `/migrations/${i.mig}/preflight`, ready: "text=Latest run" },
  { name: "cutover", path: (i) => `/migrations/${i.mig}/cutover`, ready: "text=Readiness" },
  { name: "migration-logs", path: (i) => `/migrations/${i.mig}/logs`, ready: "[role=log]" },
  { name: "migration-events", path: (i) => `/migrations/${i.mig}/events`, ready: "text=Timeline" },
  { name: "cutover-go", path: (i) => `/migrations/${i.go}/cutover`, ready: "[data-testid=verdict]" },
  { name: "operation-panel", path: (i) => `/migrations/${i.go}/cutover`, ready: "[data-testid=verdict]", prepare: async (p) => { await p.getByRole("button", { name: "Open with logs" }).click(); await p.getByRole("dialog", { name: "Cutover" }).waitFor(); } },
  { name: "cutover-nogo", path: (i) => `/migrations/${i.nogo}/cutover`, ready: "[data-testid=verdict]" },
  { name: "verification", path: (i) => `/migrations/${i.go}/verification`, ready: "table[aria-label^='Verification for']" },
  { name: "verification-nogo", path: (i) => `/migrations/${i.nogo}/verification`, ready: "table[aria-label^='Verification for']" },
  { name: "report", path: (i) => `/migrations/${i.go}/report`, ready: "iframe" },
  { name: "stress-overview", path: (i) => `/migrations/${i.stress}`, ready: "table[aria-label='Databases']" },
  { name: "stress-database", path: (i) => `/migrations/${i.stress}/databases/${enc(i.longDb)}`, ready: "text=Positions" },
  { name: "logs", path: () => "/logs", ready: "[role=log]" },
  { name: "events", path: () => "/events", ready: "table[aria-label='Events']" },
  { name: "audit", path: () => "/events", ready: "table[aria-label='Events']", prepare: async (p) => { await p.getByRole("tab", { name: "Audit log" }).click(); await p.locator("table[aria-label='Audit entries']").waitFor(); } },
  { name: "alerts", path: () => "/alerts", ready: "table[aria-label='Alerts']" },
  { name: "host", path: () => "/host", ready: "text=Work volume used" },
  { name: "settings", path: () => "/settings", ready: "text=Defaults for new migrations" },
  { name: "settings-users", path: () => "/settings", ready: "text=Defaults for new migrations", prepare: async (p) => { await p.getByRole("button", { name: "Users and roles" }).click(); await p.locator("table[aria-label='Users']").waitFor(); } },
  { name: "system", path: () => "/system", ready: "text=Installed components" },
];

let IDS: Ids;
test.beforeAll(async ({ browser }) => {
  const ctx = await browser.newContext({ storageState: "e2e/.auth/admin.json", ignoreHTTPSErrors: true });
  IDS = await ids(ctx.request);
  await ctx.close();
});

for (const s of SCREENS) {
  for (const vp of VIEWPORTS) {
    for (const theme of THEMES) {
      test(`${s.name} ${vp.width} ${theme}`, async ({ browser }) => {
        const ctx = await browser.newContext({ viewport: vp, ignoreHTTPSErrors: true, storageState: s.auth === "none" ? undefined : "e2e/.auth/admin.json", reducedMotion: "reduce" });
        const page = await ctx.newPage();
        const g = watch(page);
        await setTheme(page, theme);
        if (s.setup) await s.setup(page);
        await page.goto(s.path(IDS));
        await settle(page, s.ready);
        if (s.prepare) { await s.prepare(page, IDS); await settle(page); }
        await page.screenshot({ path: `${SHOTS}/${s.name}-${vp.width}-${theme}.png` });
        const layout = await layoutViolations(page);
        expect(layout, `layout on ${s.name} at ${vp.width} ${theme}`).toEqual([]);
        const axe = await axeViolations(page);
        expect(axe, `axe on ${s.name} at ${vp.width} ${theme}`).toEqual([]);
        g.check();
        await ctx.close();
      });
    }
  }
}
