import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { AlertsView } from "../src/lib/api";
import { Issue } from "../src/pages/Issue";
import { Settings } from "../src/pages/Settings";
import { fakeServer, renderWithApp } from "./helpers";

const now = new Date().toISOString();
const project = { project: { id: 3, name: "shop-web", organization: "Acme", public_key: "pk_abc", dsn: "http://pk_abc@localhost:8080/3" } };

test("the issue page shows the fix an agent linked and the commit each release was built from", async () => {
  history.replaceState(null, "", "/p/3/issues/10");
  fakeServer({
    "GET /api/ui/projects/3/issues/10": {
      id: 10, title: "TypeError: cart.items is undefined", culprit: "renderCart (src/cart.js)", status: "resolved", platform: "javascript", last_release: "web@2.0.0",
      event_count: 3, user_count: 1, first_seen: now, last_seen: now, status_changed_at: now, regressed: false, trend: new Array(24).fill(0),
      fingerprint: "abcdef0123456789", environments: ["production"], releases: ["web@2.0.0", "web@1.9.0"],
      commits: [{ name: "web@2.0.0", commit: "9c0ffee1234567", repository: "https://github.com/acme/shop", updated_at: now }],
      links: [{ url: "https://github.com/acme/shop/pull/12", title: "Handle an empty cart", created_at: now }],
      activity: [{ kind: "resolved", created_at: now }],
    },
    "GET /api/ui/projects/3/issues/10/events": { events: [] },
  });
  renderWithApp(<Issue projectId={3} issueId={10} />);

  const fixes = await screen.findByRole("region", { name: "Fixes" });
  expect(within(fixes).getByRole("link", { name: "Handle an empty cart" }).getAttribute("href")).toBe("https://github.com/acme/shop/pull/12");
  // The recorded release links to its commit; the other has none to show.
  expect(screen.getByRole("link", { name: "Commit 9c0ffee of web@2.0.0" }).getAttribute("href")).toBe("https://github.com/acme/shop/commit/9c0ffee1234567");
  expect(screen.getByText("web@1.9.0")).toBeTruthy();
  expect(screen.queryByRole("link", { name: /of web@1\.9\.0/ })).toBeNull();
});

test("an issue nobody linked a fix to has no Fixes panel", async () => {
  fakeServer({
    "GET /api/ui/projects/3/issues/11": {
      id: 11, title: "Error", culprit: "", status: "unresolved", platform: "javascript", last_release: "", event_count: 1, user_count: 0, first_seen: now, last_seen: now,
      status_changed_at: null, regressed: false, trend: new Array(24).fill(0), fingerprint: "ff", environments: [], releases: [], commits: [], links: [], activity: [],
    },
    "GET /api/ui/projects/3/issues/11/events": { events: [] },
  });
  renderWithApp(<Issue projectId={3} issueId={11} />);
  await screen.findByText("Activity");
  expect(screen.queryByRole("region", { name: "Fixes" })).toBeNull();
});

describe("webhook settings", () => {
  const view = (extra: Partial<AlertsView> = {}): AlertsView => ({
    slack_webhook_set: false, slack_webhook_hint: "", email_to: [], on_new_issue: true, on_regression: true, email_available: false, webhook_url: "", deliveries: [], ...extra,
  });

  test("saving a webhook shows its signing secret once, and a reload doesn't", async () => {
    let saved = view();
    const { calls } = fakeServer({
      "GET /api/ui/projects/3": project,
      "GET /api/ui/projects/3/alerts": () => saved,
      "PUT /api/ui/projects/3/alerts": (body: unknown) => {
        const b = body as { webhook_url: string };
        saved = view({ webhook_url: b.webhook_url });
        return { ...saved, webhook_secret: "whsec_new" }; // only this response has it
      },
    });
    const user = userEvent.setup();
    renderWithApp(<Settings projectId={3} tab="alerts" />);

    await user.type(await screen.findByLabelText("Webhook for programs"), "https://agents.example.com/juju");
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(await screen.findByText("whsec_new")).toBeTruthy();
    const put = calls.find((c) => c.method === "PUT")!;
    expect(put.body).toMatchObject({ webhook_url: "https://agents.example.com/juju", rotate_webhook_secret: false });
    // The saved URL stays in the field, and the test button now works.
    expect((screen.getByLabelText("Webhook for programs") as HTMLInputElement).value).toBe("https://agents.example.com/juju");
    expect((screen.getByRole("button", { name: "Send a test alert" }) as HTMLButtonElement).disabled).toBe(false);
  });

  test("New signing secret asks the server to rotate it", async () => {
    const { calls } = fakeServer({
      "GET /api/ui/projects/3": project,
      "GET /api/ui/projects/3/alerts": view({ webhook_url: "https://agents.example.com/juju" }),
      "PUT /api/ui/projects/3/alerts": { ...view({ webhook_url: "https://agents.example.com/juju" }), webhook_secret: "whsec_rotated" },
    });
    const user = userEvent.setup();
    renderWithApp(<Settings projectId={3} tab="alerts" />);
    await user.click(await screen.findByRole("button", { name: "New signing secret" }));
    expect(await screen.findByText("whsec_rotated")).toBeTruthy();
    await waitFor(() => expect(calls.find((c) => c.method === "PUT")?.body).toMatchObject({ rotate_webhook_secret: true, webhook_url: "https://agents.example.com/juju" }));
  });
});

test("the AI agents tab gives the MCP address for this project, without a real key in it", async () => {
  fakeServer({ "GET /api/ui/projects/3": project });
  renderWithApp(<Settings projectId={3} tab="agents" />);
  const url = `${location.origin}/api/v1/projects/3/mcp`;
  expect(await screen.findByText(new RegExp(`claude mcp add --transport http juju ${url.replace(/[.]/g, "\\.")}`))).toBeTruthy();
  expect(screen.getByText(/"url": "http:\/\/[^"]+\/api\/v1\/projects\/3\/mcp"/)).toBeTruthy();
  expect(document.body.textContent).not.toMatch(/sk_[0-9a-f]/);
  expect(screen.getByText(/juju release -name/)).toBeTruthy();
});
