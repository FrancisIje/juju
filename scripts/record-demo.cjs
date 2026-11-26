// Records the README demo (docs/img/demo/demo.mp4 and demo.gif): the web
// example on the left, the dashboard on the right, side by side.
//
// Needs a FRESH server (no accounts yet), the example served, and ffmpeg:
//   make dashboard sdk example-web-build
//   make devdb / make dev / make example-web     (each in its own terminal)
//   (cd dashboard && npx playwright install ffmpeg)   # once: Playwright's video encoder
//   node scripts/record-demo.cjs
//
// JUJU_DEMO_URL and JUJU_DEMO_APP override http://localhost:8080 and
// http://127.0.0.1:5173/examples/web/.
const path = require("node:path");
const fs = require("node:fs");
const os = require("node:os");
const { execFileSync } = require("node:child_process");
const root = path.join(__dirname, "..");
const { chromium } = require(path.join(root, "dashboard/node_modules/playwright"));

const DASH = process.env.JUJU_DEMO_URL || "http://localhost:8080";
const APP = process.env.JUJU_DEMO_APP || "http://127.0.0.1:5173/examples/web/";
const OUT = path.join(root, "docs/img/demo");
const W = 960, H = 760;
const pause = (ms) => new Promise((r) => setTimeout(r, ms));

(async () => {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "juju-demo-"));
  const browser = await chromium.launch({ channel: "chrome" });

  // Off camera: account, project, source map upload.
  const setup = await browser.newContext({ viewport: { width: W, height: H }, colorScheme: "dark" });
  const p = await setup.newPage();
  await p.goto(DASH + "/");
  await p.getByLabel("Your name").fill("Ada");
  await p.getByLabel("Email").fill("ada@example.com");
  await p.getByLabel("Password").fill("correct horse battery");
  await p.getByLabel("Organization").fill("Acme");
  await p.getByRole("button", { name: "Create account" }).click();
  await p.getByLabel("Project name").fill("shop-web");
  await p.getByRole("button", { name: "Create project" }).click();
  await p.getByText("Copy it now: it's shown only this once").waitFor();
  const codes = await p.locator("pre.code").allTextContents();
  const secret = codes.find((c) => c.startsWith("sk_")).trim();
  const dsn = codes.find((c) => c.includes("pk_")).trim();
  const project = /\/(\d+)$/.exec(dsn)[1];
  execFileSync("go", ["run", "./cmd/juju", "upload", "../examples/web/dist"], {
    cwd: path.join(root, "server"),
    env: { ...process.env, JUJU_URL: DASH, JUJU_PROJECT: project, JUJU_SECRET_KEY: secret },
    stdio: "inherit",
  });
  const state = path.join(tmp, "state.json");
  await setup.storageState({ path: state });
  await setup.close();

  // On camera. Both pages share one context, so they record at the same time.
  const ctx = await browser.newContext({
    viewport: { width: W, height: H }, colorScheme: "dark", storageState: state,
    recordVideo: { dir: tmp, size: { width: W, height: H } },
  });
  const app = await ctx.newPage();
  const dash = await ctx.newPage();
  await Promise.all([app.goto(APP + "?dsn=" + encodeURIComponent(dsn)), dash.goto(`${DASH}/p/${project}/issues`)]);
  await dash.getByRole("heading", { name: "Connect your app" }).waitFor();
  await pause(2500);

  const click = async (name) => {
    const b = app.getByRole("button", { name });
    await b.hover();
    await pause(500);
    await b.click();
    await pause(1300);
  };
  await click("Navigate to /cart (breadcrumb only)");
  await click("Uncaught TypeError");
  await dash.locator(".issue-row").first().waitFor({ timeout: 10000 });
  await pause(1200);
  await click("Unhandled promise rejection");
  await click("Handled error (captureException)");
  await pause(1500);

  const row = dash.locator(".issue-row", { hasText: "TypeError" }).first();
  await row.hover();
  await pause(600);
  await row.click();
  await dash.getByRole("heading", { level: 1 }).waitFor();
  await pause(3500); // the stack trace, mapped back to app.js
  await dash.keyboard.press("2"); // breadcrumbs
  await pause(3000);
  await dash.keyboard.press("1");
  await pause(800);
  await dash.keyboard.press("r"); // resolve
  await pause(2500);
  await dash.keyboard.press("Escape");
  await pause(2500);

  const [a, d] = [app.video(), dash.video()];
  await ctx.close();
  await browser.close();

  fs.mkdirSync(OUT, { recursive: true });
  const mp4 = path.join(OUT, "demo.mp4");
  execFileSync("ffmpeg", ["-loglevel", "error", "-y", "-i", await a.path(), "-i", await d.path(),
    "-filter_complex", "[0:v]setpts=PTS-STARTPTS,pad=iw+8:ih:0:0:color=0x0b0e12[a];[1:v]setpts=PTS-STARTPTS[b];[a][b]hstack=inputs=2,fps=30[v]",
    "-map", "[v]", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-crf", "23", "-movflags", "+faststart", mp4], { stdio: "inherit" });
  // A GIF too: GitHub READMEs play GIFs inline but not MP4s from the repo.
  execFileSync("ffmpeg", ["-loglevel", "error", "-y", "-i", mp4, "-vf",
    "fps=12,scale=1200:-1:flags=lanczos,split[s0][s1];[s0]palettegen=max_colors=128:stats_mode=diff[p];[s1][p]paletteuse=dither=bayer:bayer_scale=5:diff_mode=rectangle",
    path.join(OUT, "demo.gif")], { stdio: "inherit" });
  fs.rmSync(tmp, { recursive: true, force: true });
  console.log(`wrote ${OUT}/demo.mp4 and demo.gif`);
})().catch((e) => { console.error(e); process.exit(1); });
