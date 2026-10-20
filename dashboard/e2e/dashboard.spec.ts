import { expect, test, type Page } from "@playwright/test";
import { randomUUID } from "node:crypto";

const shots = process.env.JUJU_E2E_SHOTS;
const slack = process.env.JUJU_E2E_SLACK;

async function shoot(page: Page, name: string) {
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
  expect(dsn).toMatch(/^http:\/\/pk_[^@]+@localhost:8080\/\d+$/);
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
