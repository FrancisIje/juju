import { expect, test, type Locator, type Page } from "@playwright/test";
import { randomUUID } from "node:crypto";

const shots = process.env.JUJU_E2E_SHOTS;
// The server has ClickHouse (JUJU_CLICKHOUSE_URL): check product analytics too.
const analyticsOn = process.env.JUJU_E2E_ANALYTICS === "1";
const slack = process.env.JUJU_E2E_SLACK;

/** `focus`: an element to keep in view (below the fold), scrolled to after each resize. */
async function shoot(page: Page, name: string, focus?: Locator) {
  // Every checked screen must fit a phone: nothing may make the page itself
  // scroll sideways (wide tables and code scroll inside their own box).
  await page.setViewportSize({ width: 390, height: 844 });
  await page.evaluate(() => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r))));
  // .main is its own scroll area, so too-wide content scrolls it sideways
  // while the document stays 390px: measure both. Intended scrolling (code,
  // tables, tab rows) happens in boxes deeper inside.
  const extra = await page.evaluate(() => {
    const main = document.querySelector(".main");
    return Math.max(document.documentElement.scrollWidth - window.innerWidth, main ? main.scrollWidth - main.clientWidth : 0);
  });
  expect(extra, `${name}: page is ${extra}px wider than a 390px phone`).toBeLessThanOrEqual(0);
  await page.setViewportSize({ width: 1360, height: 860 });
  if (!shots) return;
  for (const theme of ["dark", "light"] as const) {
    await page.evaluate((t) => document.documentElement.setAttribute("data-theme", t), theme);
    for (const [w, h, size] of [[1360, 860, "desktop"], [390, 844, "phone"]] as const) {
      await page.setViewportSize({ width: w, height: h });
      // Two animation frames: enough for layout to settle after a resize.
      await page.evaluate(() => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r))));
      if (focus) await focus.evaluate((el) => el.scrollIntoView({ block: "start" }));
      // animations: "disabled" finishes color transitions, so a theme switch
      // isn't captured halfway.
      await page.screenshot({ path: `${shots}/${name}-${theme}-${size}.png`, animations: "disabled" });
    }
  }
  await page.setViewportSize({ width: 1360, height: 860 });
}

/** Sends an error the way an SDK would: to the public ingest API with the DSN's key. */
async function sendError(page: Page, dsn: string, message: string, release = "web@1.0.0", fn = "checkout", file = "src/cart.ts") {
  const m = /^(https?):\/\/([^@]+)@([^/]+)\/(\d+)/.exec(dsn)!;
  const res = await page.request.post(`${m[1]}://${m[3]}/api/v1/projects/${m[4]}/events`, {
    headers: { "X-Juju-Key": m[2]! },
    data: {
      sdk: { name: "juju.e2e", version: "0.0.0" },
      events: [
        {
          event_id: randomUUID(),
          type: "error",
          platform: "javascript",
          timestamp: new Date().toISOString(),
          release,
          environment: "production",
          exception: { type: "TypeError", message, stacktrace: { frames: [{ function: fn, filename: file, lineno: 42, colno: 7, in_app: true }, { function: "dispatch", filename: "node_modules/react-dom/index.js", lineno: 9, in_app: false }, { function: "batch", filename: "node_modules/react-dom/index.js", lineno: 3, in_app: false }] } },
          breadcrumbs: [{ type: "navigation", timestamp: new Date(Date.now() - 4000).toISOString(), data: { from: "/", to: "/cart" } }, { type: "click", timestamp: new Date(Date.now() - 1000).toISOString(), message: "button#checkout" }],
          context: { browser: { name: "Chrome", version: "141" }, os: { name: "macOS" } },
          user: { id: "u_42" },
        },
      ],
    },
  });
  expect(res.status()).toBe(202);
}

/** Sends product analytics events the way an SDK would. */
async function sendAnalytics(page: Page, dsn: string, sdk: string, events: { name: string; who: string; props?: Record<string, unknown> }[]) {
  const m = /^(https?):\/\/([^@]+)@([^/]+)\/(\d+)/.exec(dsn)!;
  const res = await page.request.post(`${m[1]}://${m[3]}/api/v1/projects/${m[4]}/events`, {
    headers: { "X-Juju-Key": m[2]! },
    data: {
      sdk: { name: sdk, version: "0.1.0" },
      events: events.map((e) => ({ event_id: randomUUID(), type: "event", platform: "javascript", timestamp: new Date().toISOString(), name: e.name, distinct_id: e.who, properties: e.props ?? {} })),
    },
  });
  expect(res.status()).toBe(202);
}

test("first run → project → error → resolve → regression alert → invite a teammate", async ({ page, browser }) => {
  const errors: string[] = [];
  // Before logging in, GET /api/ui/me answers 401: that's how the app learns
  // nobody is logged in, and Chrome logs every 4xx. After login, any 401 is
  // a real problem.
  let loggedIn = false;
  page.on("pageerror", (e) => errors.push(e.message));
  page.on("console", (m) => {
    if (m.type() !== "error") return;
    if (!loggedIn && m.text().includes("status of 401")) return;
    errors.push(m.text());
  });

  // First visit: no accounts yet, so the login page offers setup.
  await page.goto("/");
  await expect(page).toHaveURL(/\/login/);
  await expect(page.getByRole("heading", { name: "Create your account" })).toBeVisible();
  await shoot(page, "01-setup");
  await page.getByLabel("Your name").fill("Ada");
  await page.getByLabel("Email").fill("ada@example.com");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByLabel("Organization").fill("Acme");
  await page.getByRole("button", { name: "Create account" }).click();
  loggedIn = true;

  // No projects yet → straight to creating one.
  await expect(page.getByRole("heading", { name: "New project" })).toBeVisible();
  await page.getByLabel("Project name").fill("shop-web");
  await page.getByRole("button", { name: "Create project" }).click();

  // The secret key is shown once, with the DSN.
  await expect(page.getByText("Copy it now: it's shown only this once")).toBeVisible();
  const dsn = (await page.locator("pre.code").nth(1).textContent())!;
  // The DSN points at this server (JUJU_PUBLIC_URL), whatever port the test runs on.
  const host = new URL(process.env.JUJU_E2E_URL ?? "http://localhost:8080").host;
  expect(dsn).toMatch(new RegExp(`^http://pk_[^@]+@${host.replace(/[.]/g, "\\.")}/\\d+$`));
  await shoot(page, "02-setup-dsn");
  // Gone after a reload, like on the server.
  await page.reload();
  await expect(page.getByText("Copy it now")).toHaveCount(0);

  // The issue list starts as onboarding, with the DSN filled in.
  await page.keyboard.press("g");
  await page.keyboard.press("i");
  await expect(page.getByRole("heading", { name: "Connect your app" })).toBeVisible();
  await expect(page.locator(".onboard")).toContainText(dsn);
  await shoot(page, "03-onboarding");

  if (slack) {
    await page.goto(page.url().replace(/issues.*$/, "settings/alerts"));
    await page.getByLabel("Slack incoming webhook").fill(slack);
    await page.getByRole("button", { name: "Save" }).click();
    await expect(page.getByText("Alert settings saved")).toBeVisible();
    await page.getByRole("button", { name: "Send a test alert" }).click();
    await expect(page.getByRole("status").filter({ hasText: "slack" })).toContainText("Sent");
    await shoot(page, "04-alerts");
    await page.keyboard.press("g");
    await page.keyboard.press("i");
  }

  // Errors arrive while the page is open.
  await sendError(page, dsn, "cart.items is undefined");
  await sendError(page, dsn, "cart.items is undefined");
  // Same type but a different place in the code: a different bug. (juju
  // groups by type + stack, not by message, so the first two group.)
  await sendError(page, dsn, "price.toFixed is not a function", "web@1.0.0", "formatPrice", "src/price.ts");
  // No reload: the server pushes "refresh" over SSE and the list updates
  // by itself, onboarding included.
  await expect(page.locator(".issue-row")).toHaveCount(2, { timeout: 5_000 });
  await shoot(page, "05-issues");

  // Keyboard only: j down to the cart issue, Enter to open it.
  const rows = await page.locator(".issue-row .msg").allTextContents();
  for (let i = 0; i < rows.indexOf("cart.items is undefined"); i++) await page.keyboard.press("j");
  await page.keyboard.press("Enter");
  await expect(page.getByRole("heading", { level: 1 })).toContainText("TypeError");
  await expect(page.locator(".frame.app .fn")).toHaveText("checkout");
  await expect(page.getByText("2 library frames · show")).toBeVisible();
  await shoot(page, "06-issue");
  await page.keyboard.press("2");
  await expect(page.getByText("button#checkout")).toBeVisible();
  await page.keyboard.press("4");
  await expect(page.locator("pre.code")).toContainText('"event_id"');
  await page.keyboard.press("1");

  // Resolve with r; it's no longer in the unresolved list.
  const title = (await page.getByRole("heading", { level: 1 }).textContent())!;
  await page.keyboard.press("r");
  await expect(page.getByText(/^Resolved\. /)).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(page.locator(".issue-row")).toHaveCount(1);

  // The same error again, in a newer release → regression, labelled in words.
  const message = title.split(": ").slice(1).join(": ");
  expect(message).toBe("cart.items is undefined");
  await sendError(page, dsn, message, "web@1.0.1");
  await expect(page.locator(".issue-row")).toHaveCount(2, { timeout: 5_000 });
  await expect(page.locator(".chip.regressed")).toHaveText("Regressed");

  // ⌘K finds the issue by text.
  await page.keyboard.press("ControlOrMeta+k");
  await page.getByLabel("Search commands").fill(message.slice(0, 8));
  await expect(page.getByRole("listbox").getByRole("option").first()).toContainText(message);
  await shoot(page, "07-palette");
  await page.keyboard.press("Escape");

  if (slack) {
    await page.goto(page.url().replace(/issues.*$/, "settings/alerts"));
    // new issue ×2 + regression + the test message
    await expect(page.locator("table.table tbody tr")).toHaveCount(4);
    await expect(page.locator("table.table")).toContainText("regression");
  }

  if (analyticsOn) {
    // Product analytics: before any event, the page says how to send one.
    const projectBase = page.url().replace(/(\/p\/\d+)\/.*$/, "$1");
    await page.goto(`${projectBase}/analytics`);
    await expect(page.getByRole("heading", { name: "Track what people do" })).toBeVisible();
    await sendAnalytics(page, dsn, "juju.web", [
      { name: "checkout_started", who: "anon-a", props: { plan: "pro" } },
      { name: "checkout_started", who: "anon-b", props: { plan: "free" } },
      { name: "checkout_completed", who: "anon-a" },
    ]);
    await sendAnalytics(page, dsn, "juju.flutter", [{ name: "checkout_started", who: "anon-c", props: { plan: "pro" } }]);
    // The writer batches every second: reload until the events are in.
    await expect.poll(async () => (await page.reload(), page.getByLabel("Event").isVisible()), { timeout: 15_000 }).toBe(true);
    await page.getByLabel("Break down by").selectOption("lib");
    await expect(page).toHaveURL(/breakdown=lib/);
    // Exactly what was sent, with names people read.
    await expect(page.getByRole("row", { name: /Web\s+2\s+67%/ })).toBeVisible();
    await expect(page.getByRole("row", { name: /Flutter\s+1\s+33%/ })).toBeVisible();
    await shoot(page, "10-analytics");
    await page.keyboard.press("2");
    await expect(page).toHaveURL(/\/analytics\/funnels/);
    await expect(page.getByText("3 people · 100%")).toBeVisible();
    await expect(page.getByText("1 person · 33%")).toBeVisible();
    await shoot(page, "11-funnel");
  }

  // Invite a teammate into Acme.
  await page.goto(page.url().replace(/\/p\/(\d+)\/.*$/, "/p/$1/settings/team"));
  await page.getByRole("button", { name: "Create invite link" }).click();
  const link = (await page.locator(".banner.ok pre.code").textContent())!;
  expect(link).toMatch(/\/invite\/[\w-]{43}$/);
  await shoot(page, "08-team");

  const bobContext = await browser.newContext(); // another person, logged out
  const bob = await bobContext.newPage();
  await bob.goto(link);
  await expect(bob.getByRole("heading", { name: "Join Acme" })).toBeVisible();
  await expect(bob.getByText(/Ada invited you/)).toBeVisible();
  await shoot(bob, "09-invite");
  await bob.getByLabel("Your name").fill("Bob");
  await bob.getByLabel("Email").fill("bob@example.com");
  await bob.getByLabel("Password").fill("another long password");
  await bob.getByRole("button", { name: "Create account and join Acme" }).click();
  // Straight into Ada's project, seeing the same issues.
  await expect(bob).toHaveURL(/\/p\/\d+\/issues$/);
  await expect(bob.locator(".issue-row")).toHaveCount(2);

  // The link worked once.
  const carol = await (await browser.newContext()).newPage();
  await carol.goto(link);
  await expect(carol.getByRole("heading", { name: "This invite doesn't work any more" })).toBeVisible();

  // Ada sees Bob on the team, as a member.
  await page.reload();
  await expect(page.getByRole("row", { name: /Bob.*Member/ })).toBeVisible();
  await bobContext.close();

  // Someone new, not invited, makes their own account and organization:
  // sign-up is open by default.
  const dana = await (await browser.newContext()).newPage();
  await dana.goto("/login");
  await dana.getByRole("link", { name: "Create an account" }).click();
  await expect(dana).toHaveURL(/\/signup$/);
  await expect(dana.getByRole("heading", { name: "Create an account" })).toBeVisible();
  await dana.getByLabel("Your name").fill("Dana");
  await dana.getByLabel("Email").fill("dana@example.com");
  await dana.getByLabel("Password").fill("dana's long password");
  // Naming someone else's organization doesn't get you into it.
  await dana.getByLabel("Organization").fill("Acme");
  await dana.getByRole("button", { name: "Create account" }).click();
  await expect(dana.getByRole("alert")).toContainText("already exists");
  await dana.getByLabel("Organization").fill("Dana Co");
  await dana.getByRole("button", { name: "Create account" }).click();
  await expect(dana.getByRole("heading", { name: "New project" })).toBeVisible();
  await dana.getByLabel("Project name").fill("dana-app");
  await dana.getByRole("button", { name: "Create project" }).click();
  await expect(dana.getByText("Copy it now: it's shown only this once")).toBeVisible();
  const danaDsn = (await dana.locator("pre.code").nth(1).textContent())!;
  expect(danaDsn).not.toBe(dsn); // her own project, her own key
  // Her account sees only her organization.
  const danaMe = await dana.evaluate(() => fetch("/api/ui/me").then((r) => r.json()));
  expect(danaMe.organizations.map((o: { name: string }) => o.name)).toEqual(["Dana Co"]);
  await dana.goto(page.url().replace(/settings\/team$/, "issues"));
  await expect(dana.getByText("Page not found").or(dana.getByText("project not found"))).toBeVisible();

  expect(errors).toEqual([]);
});

/** Sends events the way the SDKs do, to the public ingest API with the DSN's key. */
async function ingest(page: Page, dsn: string, events: unknown[]) {
  const m = /^(https?):\/\/([^@]+)@([^/]+)\/(\d+)/.exec(dsn)!;
  for (let i = 0; i < events.length; i += 100) {
    const res = await page.request.post(`${m[1]}://${m[3]}/api/v1/projects/${m[4]}/events`, {
      headers: { "X-Juju-Key": m[2]! },
      data: { sdk: { name: "juju.e2e", version: "0.0.0" }, events: events.slice(i, i + 100) },
    });
    expect(res.status()).toBe(202);
  }
}

const iso = (ms: number) => new Date(ms).toISOString();

// Phase 8: a week of sessions and page loads, then release health and
// performance, read the way a person would. Runs after the first test,
// whose account (Ada) and project it uses.
test("sessions and transactions → releases → performance → a slow page load's spans", async ({ page }) => {
  const errors: string[] = [];
  // As in the first test: before login, /api/ui/me answers 401 (that's how
  // the app learns nobody is logged in) and Chrome logs it. After, any 401
  // is a real problem.
  let loggedIn = false;
  page.on("pageerror", (e) => errors.push(e.message));
  page.on("console", (m) => {
    if (m.type() !== "error" || (!loggedIn && m.text().includes("status of 401"))) return;
    errors.push(m.text());
  });
  await page.goto("/login");
  await page.getByLabel("Email").fill("ada@example.com");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Log in" }).click();
  loggedIn = true;
  await expect(page).toHaveURL(/\/p\/\d+\/issues/);
  const projectId = /\/p\/(\d+)\//.exec(page.url())![1];
  const info = await page.evaluate((id) => fetch(`/api/ui/projects/${id}`).then((r) => r.json()), projectId);
  const dsn: string = info.project.dsn;

  // 7 days × 40 sessions. web@1.0.0 for the first 5 days with 1 crash a
  // day; web@1.0.1 since, crashing 3 times a day (a bad release).
  const day = 24 * 3600_000;
  const now = Date.now();
  const sessions: unknown[] = [];
  for (let d = 6; d >= 0; d--) {
    for (let i = 0; i < 40; i++) {
      const start = now - d * day - (i + 1) * 60_000;
      const release = d >= 2 ? "web@1.0.0" : "web@1.0.1";
      const crashed = i < (release === "web@1.0.1" ? 3 : 1);
      sessions.push({
        event_id: randomUUID(), type: "session", platform: "javascript", release, environment: "production", timestamp: iso(start + 50_000),
        user: { id: `u_${i % 25}` },
        session: { sid: randomUUID(), seq: 1, status: crashed ? "crashed" : "exited", started: iso(start), errors: crashed ? 1 : i % 7 === 0 ? 2 : 0, duration_ms: 50_000 },
      });
    }
  }
  await ingest(page, dsn, sessions);

  // Page loads: /checkout gets slower in web@1.0.1 because its cart API does.
  const loads: unknown[] = [];
  for (let d = 6; d >= 0; d--) {
    for (let i = 0; i < 12; i++) {
      const start = now - d * day - (i + 1) * 300_000;
      const slow = d < 2;
      const api = (slow ? 900 : 250) + i * 30;
      const total = 400 + api + 120;
      const failed = slow && i === 11; // the slowest one: a request that waited, then failed
      loads.push({
        event_id: randomUUID(), type: "transaction", platform: "javascript", release: slow ? "web@1.0.1" : "web@1.0.0", environment: "production", timestamp: iso(start + total),
        transaction: {
          name: "/checkout", op: "pageload", start_timestamp: iso(start), measurements: { ttfb: 180, fcp: 420, lcp: 380 + api },
          spans: [
            { span_id: "a", op: "browser", description: "Waiting for the server", start_timestamp: iso(start + 20), timestamp: iso(start + 180) },
            { span_id: "b", op: "browser", description: "Build page (DOMContentLoaded)", start_timestamp: iso(start + 190), timestamp: iso(start + 390) },
            { span_id: "c", op: "resource.script", description: "/assets/app.js", start_timestamp: iso(start + 200), timestamp: iso(start + 330) },
            { span_id: "d", op: "http.client", description: "GET /api/cart", status: failed ? "error" : "ok", data: { status_code: failed ? 503 : 200 }, start_timestamp: iso(start + 400), timestamp: iso(start + 400 + api) },
          ],
        },
      });
      loads.push({
        event_id: randomUUID(), type: "transaction", platform: "javascript", release: "web@1.0.0", environment: "production", timestamp: iso(start + 60 + i * 5),
        transaction: { name: "/products/:id", op: "navigation", start_timestamp: iso(start) },
      });
    }
  }
  await ingest(page, dsn, loads);
  // Ingest answers before the worker has stored everything (that's the
  // point of the queue), and the pages refresh every 30 s. Wait until it's
  // all stored, as a person opening the page a moment later would see it.
  await expect
    .poll(() => page.evaluate((id) => fetch(`/api/ui/projects/${id}/releases`).then((r) => r.json()).then((h) => h.totals.sessions), projectId), { timeout: 15_000 })
    .toBe(280);
  await expect
    .poll(() => page.evaluate((id) => fetch(`/api/ui/projects/${id}/transactions`).then((r) => r.json()).then((l) => l.transactions.reduce((n: number, t: { count: number }) => n + t.count, 0)), projectId), { timeout: 15_000 })
    .toBe(168);

  // g r: release health. 280 sessions, 11 crashed (5 × 1 + 2 × 3).
  await page.keyboard.press("g");
  await page.keyboard.press("r");
  await expect(page.getByRole("heading", { name: "Releases", level: 1 })).toBeVisible();
  await expect(page.locator(".stat").first()).toContainText("96.1%", { timeout: 10_000 });
  await expect(page.getByRole("row", { name: /web@1\.0\.1.*92\.5%/ })).toBeVisible(); // 6 of 80 crashed
  await expect(page.getByRole("row", { name: /web@1\.0\.0.*97\.5%/ })).toBeVisible(); // 5 of 200
  // The chart reads from the keyboard: focus it and step to a day.
  const chart = page.getByRole("img", { name: /Crash-free sessions per day/ });
  await chart.focus();
  await page.keyboard.press("ArrowLeft");
  await expect(page.locator(".line-chart-tip")).toContainText("sessions (crashed)");
  await page.keyboard.press("Escape");
  await chart.blur(); // the screenshot shows the page, not a keyboard focus ring
  await shoot(page, "10-releases");

  // g p: performance. /checkout is the most frequent transaction.
  await page.keyboard.press("g");
  await page.keyboard.press("p");
  await expect(page.getByRole("heading", { name: "Performance", level: 1 })).toBeVisible();
  await expect(page.locator("tbody tr").first()).toContainText("/checkout");
  await shoot(page, "11-performance");
  await page.keyboard.press("Enter");
  await expect(page).toHaveURL(/performance\/transaction\?name=%2Fcheckout&op=pageload/);
  // The slowest sample opens with its spans; the failed request says so in words.
  await expect(page.locator(".waterfall")).toContainText("GET /api/cart");
  await expect(page.locator(".waterfall .chip.unresolved")).toHaveText("Server error");
  // web@1.0.1 is slower than web@1.0.0: compare them in the "By release" table.
  const byRelease = page.getByRole("table").filter({ hasText: "By release" });
  await expect(byRelease.getByRole("row", { name: /web@1\.0\.1/ })).toBeVisible();
  await expect(byRelease.getByRole("row", { name: /web@1\.0\.0/ })).toBeVisible();
  await shoot(page, "12-transaction");
  await shoot(page, "13-waterfall", page.getByRole("heading", { name: /Selected sample/ }));
  await page.keyboard.press("Escape");
  await expect(page).toHaveURL(/\/performance$/);

  // A server error shows in words, with a retry.
  await page.route("**/api/ui/projects/*/releases*", (r) => r.fulfill({ status: 500, body: JSON.stringify({ error: "something went wrong on the server; try again" }) }));
  await page.keyboard.press("g");
  await page.keyboard.press("r");
  await expect(page.getByRole("alert")).toContainText("Something went wrong on the server");
  await expect(page.getByRole("button", { name: "Retry" })).toBeVisible();
  await page.unroute("**/api/ui/projects/*/releases*");
  await page.getByRole("button", { name: "Retry" }).click();
  await expect(page.getByRole("alert")).toHaveCount(0);

  expect(errors.filter((e) => !e.includes("status of 500"))).toEqual([]);
});
