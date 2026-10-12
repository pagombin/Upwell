#!/usr/bin/env bash
# Renders docs/architecture/architecture.html to docs/architecture.png with
# the Playwright Chromium the browser tests use.
set -euo pipefail
cd "$(dirname "$0")/../web"
node -e '
const { chromium } = require("@playwright/test");
(async () => {
  const b = await chromium.launch(process.env.PLAYWRIGHT_BROWSERS_PATH ? {} : { executablePath: "/opt/pw-browsers/chromium" });
  const p = await b.newPage({ viewport: { width: 1640, height: 1230 }, deviceScaleFactor: 2 });
  await p.goto("file://" + require("path").resolve("../docs/architecture/architecture.html"));
  await p.locator("svg").screenshot({ path: "../docs/architecture.png" });
  await b.close();
})().catch((e) => { console.error(e); process.exit(1); });
'
echo "docs/architecture.png"
