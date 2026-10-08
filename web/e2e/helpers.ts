import AxeBuilder from "@axe-core/playwright";
import { expect, test as base } from "@playwright/test";
import type { APIRequestContext, Page } from "@playwright/test";

export const VIEWPORTS = [{ width: 1440, height: 900 }, { width: 1280, height: 800 }];
export const THEMES = ["dark", "light"] as const;

// watch collects console errors, page errors and failed requests on a page.
export function watch(page: Page) {
  const problems: string[] = [];
  const allowed: RegExp[] = [];
  const ok = (s: string) => allowed.some((re) => re.test(s));
  page.on("console", (m) => { if (m.type() === "error" && !ok(m.text())) problems.push(`console: ${m.text().slice(0, 300)}`); });
  page.on("pageerror", (e) => { if (!ok(String(e))) problems.push(`page error: ${String(e).slice(0, 300)}`); });
  page.on("requestfailed", (r) => {
    const f = r.failure()?.errorText || "";
    if (f.includes("ERR_ABORTED") || f.includes("NS_BINDING_ABORTED")) return; // navigation cancels SSE and in-flight polls
    if (!ok(r.url())) problems.push(`request failed: ${r.method()} ${r.url()} ${f}`);
  });
  page.on("response", (r) => { if (r.status() >= 400 && !ok(r.url())) problems.push(`HTTP ${r.status()}: ${r.request().method()} ${r.url()}`); });
  return {
    allow: (re: RegExp) => { allowed.push(re); },
    check: () => expect(problems, "console errors and failed requests").toEqual([]),
  };
}

// test: the default page fails the test on console errors and failed requests.
export const test = base.extend<{ guard: ReturnType<typeof watch> }>({
  guard: [async ({ page }, use) => {
    const g = watch(page);
    await use(g);
    g.check();
  }, { auto: true }],
});
export { expect };

export async function setTheme(page: Page, theme: string) {
  await page.addInitScript((t) => { try { localStorage.setItem("upwell.theme", t); localStorage.setItem("upwell.time", "utc"); } catch { /* ignore */ } }, theme);
}

// settle waits for skeletons to go away and the page to stop changing size.
export async function settle(page: Page, ready?: string) {
  if (ready) await page.locator(ready).first().waitFor({ state: "visible" });
  await expect(page.locator('[aria-busy="true"]')).toHaveCount(0, { timeout: 20_000 });
  await page.evaluate(() => document.fonts.ready);
  await page.waitForTimeout(600);
}

export interface Ids { mig: string; draft: string; db: string; go: string; nogo: string; stress: string; longDb: string }

export async function ids(request: APIRequestContext): Promise<Ids> {
  const r = await request.get("/api/v1/migrations");
  expect(r.ok()).toBeTruthy();
  const list = (await r.json()) as { migration: { short_id: string; name: string } }[];
  const by = (p: string) => list.find((v) => v.migration.name.startsWith(p))?.migration.short_id || "";
  const stress = by("Stress fixture");
  const sv = await (await request.get(`/api/v1/migrations/${stress}`)).json();
  const longDb = (sv.databases as { source_name: string }[]).map((d) => d.source_name).sort((a, b) => b.length - a.length)[0];
  return { mig: process.env.E2E_MIG || by("Example Customer"), draft: process.env.E2E_DRAFT || by("Draft"), db: process.env.E2E_DB || "e2e_shop", go: by("Completed fixture"), nogo: by("Failed fixture"), stress, longDb };
}

export interface Violation { kind: string; detail: string }

// layoutViolations checks overlapping text, clipping without ellipsis and
// tooltip, and horizontal scroll, inside the browser.
export async function layoutViolations(page: Page): Promise<Violation[]> {
  return page.evaluate(() => {
    const out: { kind: string; detail: string }[] = [];
    const desc = (el: Element) => {
      const t = (el.textContent || "").trim().replace(/\s+/g, " ").slice(0, 60);
      return `<${el.tagName.toLowerCase()}${el.className && typeof el.className === "string" ? " ." + el.className.trim().split(/\s+/).join(".") : ""}> "${t}"`;
    };
    // Horizontal scroll: the document and the main scroller.
    const de = document.documentElement;
    if (de.scrollWidth > window.innerWidth + 1) out.push({ kind: "hscroll", detail: `document is ${de.scrollWidth}px wide in a ${window.innerWidth}px viewport` });
    for (const el of Array.from(document.querySelectorAll<HTMLElement>(".main, .page, .tablewrap, .tabs, .stepper"))) {
      if (el.scrollWidth > el.clientWidth + 1 && el.getBoundingClientRect().width > 0) out.push({ kind: "hscroll", detail: `${desc(el).slice(0, 80)} scrolls horizontally (${el.scrollWidth} > ${el.clientWidth})` });
    }
    // Clip rectangles of overflow-hidden ancestors.
    const clipCache = new Map<Element, DOMRect | null>();
    const clipOf = (el: Element): DOMRect | null => {
      if (clipCache.has(el)) return clipCache.get(el)!;
      let r: DOMRect | null = null;
      const cs = getComputedStyle(el);
      if (cs.overflowX !== "visible" || cs.overflowY !== "visible") r = el.getBoundingClientRect();
      const parent = el.parentElement;
      const pr = parent ? clipOf(parent) : null;
      if (pr) r = r ? new DOMRect(Math.max(r.left, pr.left), Math.max(r.top, pr.top), Math.max(0, Math.min(r.right, pr.right) - Math.max(r.left, pr.left)), Math.max(0, Math.min(r.bottom, pr.bottom) - Math.max(r.top, pr.top))) : pr;
      clipCache.set(el, r);
      return r;
    };
    const boxes: { el: Element; l: number; t: number; r: number; b: number }[] = [];
    // With a modal open, only the modal is visible and interactive.
    const modals = Array.from(document.querySelectorAll('[aria-modal="true"]'));
    const scope = modals.length ? modals[modals.length - 1] : document.body;
    const walker = document.createTreeWalker(scope, NodeFilter.SHOW_TEXT);
    while (walker.nextNode()) {
      const n = walker.currentNode as Text;
      if (!n.textContent || !n.textContent.trim()) continue;
      const el = n.parentElement;
      if (!el || el.closest("svg, select, option, textarea, .sr-only, [aria-hidden='true'], iframe, script, style")) continue;
      const cs = getComputedStyle(el);
      if (cs.visibility === "hidden" || Number(cs.opacity) === 0) continue;
      const range = document.createRange();
      range.selectNodeContents(n);
      const clip = clipOf(el);
      for (const rr of Array.from(range.getClientRects())) {
        let l = rr.left, t = rr.top, r = rr.right, b = rr.bottom;
        if (clip) { l = Math.max(l, clip.left); t = Math.max(t, clip.top); r = Math.min(r, clip.right); b = Math.min(b, clip.bottom); }
        if (r - l < 1 || b - t < 1) continue;
        boxes.push({ el, l, t, r, b });
      }
    }
    boxes.sort((a, b) => a.t - b.t);
    const seen = new Set<string>();
    for (let i = 0; i < boxes.length; i++) {
      const a = boxes[i];
      for (let j = i + 1; j < boxes.length && boxes[j].t < a.b - 2; j++) {
        const b = boxes[j];
        if (a.el === b.el || a.el.contains(b.el) || b.el.contains(a.el)) continue;
        const w = Math.min(a.r, b.r) - Math.max(a.l, b.l), h = Math.min(a.b, b.b) - Math.max(a.t, b.t);
        if (w <= 2 || h <= 2) continue;
        // Ignore text covered by an opaque layer above both (dialogs, sticky headers).
        const cx = (Math.max(a.l, b.l) + Math.min(a.r, b.r)) / 2, cy = (Math.max(a.t, b.t) + Math.min(a.b, b.b)) / 2;
        const top = document.elementFromPoint(cx, cy);
        if (!top || !(a.el.contains(top) || b.el.contains(top) || top.contains(a.el) || top.contains(b.el))) continue;
        const key = desc(a.el) + "|" + desc(b.el);
        if (seen.has(key)) continue;
        seen.add(key);
        out.push({ kind: "overlap", detail: `${desc(a.el)} overlaps ${desc(b.el)} by ${Math.round(w)}x${Math.round(h)}px` });
      }
    }
    // Clipping: overflow hidden that cuts text needs an ellipsis and the full value in a tooltip.
    for (const el of Array.from(scope.querySelectorAll<HTMLElement>("*"))) {
      if (el.closest("svg, select, input, textarea, iframe, .sr-only")) continue;
      if (!el.textContent || !el.textContent.trim()) continue;
      const cs = getComputedStyle(el);
      if (cs.overflowX !== "hidden" && cs.overflowX !== "clip") continue;
      if (el.scrollWidth <= el.clientWidth + 1 || el.clientWidth === 0) continue;
      const text = el.textContent.trim();
      const titled = el.closest("[title]") as HTMLElement | null;
      const hasTitle = !!titled && (titled.title.includes(text) || text.includes(titled.title) && titled.title.length > 0);
      if (cs.textOverflow !== "ellipsis" || !hasTitle) out.push({ kind: "clip", detail: `${desc(el)} is clipped${cs.textOverflow !== "ellipsis" ? " without an ellipsis" : ""}${hasTitle ? "" : " without a full-value tooltip"}` });
    }
    return out;
  });
}

export async function axeViolations(page: Page) {
  const r = await new AxeBuilder({ page: page as unknown as ConstructorParameters<typeof AxeBuilder>[0]["page"] }).analyze();
  return r.violations.map((v) => `${v.id} (${v.impact}): ${v.help} — ${v.nodes.slice(0, 3).map((n) => n.target.join(" ")).join(" | ")}`);
}
