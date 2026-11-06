import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { App } from "../src/App";
import type { AdminOverview, AdminProject, Me } from "../src/lib/api";
import { AppContext } from "../src/lib/app";
import { Shell } from "../src/components/Shell";
import { Admin, AdminOrganization } from "../src/pages/Admin";
import { fakeServer, me, renderWithApp } from "./helpers";

const ago = (ms: number) => new Date(Date.now() - ms).toISOString();

const project = (over: Partial<AdminProject>): AdminProject => ({
  id: 3,
  organization_id: 1,
  organization: "Acme",
  name: "shop-web",
  created_at: ago(864e5),
  status: "active",
  unresolved: 2,
  events_this_month: 1200,
  events_last_month: 800,
  monthly_quota: null,
  rate_limit: null,
  errors_24h: 5,
  sessions_24h: 100,
  crashed_24h: 1,
  last_event_at: ago(60_000),
  ...over,
});

const overview: AdminOverview = {
  generated_at: ago(0),
  totals: {
    organizations: 2,
    new_organizations_7d: 1,
    projects: 3,
    new_projects_7d: 2,
    users: 4,
    new_users_7d: 1,
    events_this_month: 2300,
    events_last_month: 800,
    errors_24h: 5,
    unresolved: 2,
    by_status: { active: 1, over_quota: 1, no_events: 1 },
  },
  organizations: [
    { id: 1, name: "Acme", created_at: ago(864e5), owner_email: "ada@example.com", members: 2, projects: 1, status: "active", unresolved: 2, events_this_month: 1200, errors_24h: 5, last_event_at: ago(60_000) },
    { id: 2, name: "Globex", created_at: ago(864e5), owner_email: "hank@example.com", members: 2, projects: 2, status: "active", unresolved: 0, events_this_month: 1100, errors_24h: 0, last_event_at: ago(60_000) },
  ],
  projects: [
    project({}),
    project({ id: 7, organization_id: 2, organization: "Globex", name: "billing", status: "over_quota", events_this_month: 1100, monthly_quota: 1000, sessions_24h: 0, crashed_24h: 0 }),
    project({ id: 8, organization_id: 2, organization: "Globex", name: "new-thing", status: "no_events", events_this_month: 0, last_event_at: null, sessions_24h: 0, crashed_24h: 0 }),
  ],
};

beforeEach(() => history.replaceState(null, "", "/admin/projects"));

test("shows the totals in words a person can act on", async () => {
  fakeServer({ "GET /api/ui/admin/overview": overview });
  renderWithApp(<Admin tab="organizations" />);
  expect(await screen.findByText("Events this month", { selector: ".label" })).toBeTruthy();
  expect(screen.getByText("2.3k")).toBeTruthy();
  expect(screen.getByText("800 last month")).toBeTruthy();
  expect(screen.getByRole("link", { name: "Globex" }).getAttribute("href")).toBe("/admin/orgs/2");
  expect(screen.getByText("hank@example.com")).toBeTruthy();
});

test("projects that are losing events come first, with the status as a word", async () => {
  fakeServer({ "GET /api/ui/admin/overview": overview });
  renderWithApp(<Admin tab="projects" />);
  const table = await screen.findByRole("table");
  const rows = within(table).getAllByRole("row").slice(1);
  expect(rows[0]!.textContent).toContain("billing");
  expect(rows[0]!.textContent).toContain("Over quota");
  expect(rows[0]!.textContent).toContain("1.1k / 1k");
  expect(rows[1]!.textContent).toContain("shop-web");
  expect(rows[2]!.textContent).toContain("No events yet");
  expect(rows[2]!.textContent).toContain("Never");
});

test("a crash-free rate is only shown when there were sessions to measure", async () => {
  fakeServer({ "GET /api/ui/admin/overview": overview });
  renderWithApp(<Admin tab="projects" />);
  const table = await screen.findByRole("table");
  const rows = within(table).getAllByRole("row").slice(1);
  expect(rows[1]!.textContent).toContain("99"); // shop-web: 1 of 100 crashed
  expect(rows[0]!.textContent).not.toContain("%"); // billing: no sessions, not "100%"
});

// Links into a project only work for its members; anyone else gets a 404.
test("project names link only where you're a member", async () => {
  fakeServer({ "GET /api/ui/admin/overview": overview });
  renderWithApp(<Admin tab="projects" />); // me is in Acme (id 1) only
  expect((await screen.findByRole("link", { name: "shop-web" })).getAttribute("href")).toBe("/p/3/issues");
  expect(screen.getByText("billing")).toBeTruthy();
  expect(screen.queryByRole("link", { name: "billing" })).toBeNull();
});

test("search and status filters live in the URL, so a view can be shared", async () => {
  fakeServer({ "GET /api/ui/admin/overview": overview });
  renderWithApp(<Admin tab="projects" />);
  await screen.findByRole("table");
  fireEvent.change(screen.getByLabelText("Search projects"), { target: { value: "globex" } });
  expect(location.search).toBe("?q=globex");
  expect(screen.queryByText("shop-web")).toBeNull();
  expect(screen.getByText("billing")).toBeTruthy();

  fireEvent.change(screen.getByLabelText("Filter by status"), { target: { value: "no_events" } });
  expect(location.search).toBe("?q=globex&status=no_events");
  expect(screen.queryByText("billing")).toBeNull();
  expect(screen.getByText("new-thing")).toBeTruthy();

  fireEvent.change(screen.getByLabelText("Search projects"), { target: { value: "nothing-called-this" } });
  expect(screen.getByText("No project matches these filters.")).toBeTruthy();
});

test("the / shortcut focuses the search, but is just a letter while typing", async () => {
  fakeServer({ "GET /api/ui/admin/overview": overview });
  renderWithApp(<Admin tab="projects" />);
  await screen.findByRole("table");
  const search = screen.getByLabelText("Search projects") as HTMLInputElement;
  fireEvent.keyDown(window, { key: "/" });
  expect(document.activeElement).toBe(search);
  // Typing: a prevented default would keep the "/" out of the field.
  const typed = fireEvent.keyDown(search, { key: "/" });
  expect(typed).toBe(true);
});

test("a failed load says so and offers a retry", async () => {
  let up = false;
  fakeServer({});
  const fetchMock = vi.fn(async () => {
    if (!up) return new Response(JSON.stringify({ error: "something went wrong on the server; try again" }), { status: 500 });
    return new Response(JSON.stringify(overview), { status: 200 });
  });
  vi.stubGlobal("fetch", fetchMock);
  renderWithApp(<Admin tab="organizations" />);
  expect((await screen.findByRole("alert")).textContent).toContain("Something went wrong");
  up = true;
  fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
  expect(screen.getByRole("link", { name: "Globex" })).toBeTruthy();
});

test("an organization's page shows its projects and who to contact", async () => {
  fakeServer({
    "GET /api/ui/admin/orgs/2": {
      organization: overview.organizations[1],
      projects: overview.projects.slice(1),
      members: [
        { user_id: 9, email: "hank@example.com", name: "Hank", role: "owner", joined_at: ago(864e5) },
        { user_id: 10, email: "lee@example.com", name: "", role: "member", joined_at: ago(864e5) },
      ],
    },
  });
  renderWithApp(<AdminOrganization orgId="2" />);
  expect(await screen.findByRole("heading", { name: "Globex" })).toBeTruthy();
  expect(screen.getByText("billing")).toBeTruthy();
  expect(screen.getByText("lee@example.com")).toBeTruthy();
  expect(screen.getByText("Owner")).toBeTruthy();
});

test("an organization that doesn't exist is a clear not found, not a blank page", async () => {
  fakeServer({});
  renderWithApp(<AdminOrganization orgId="404" />);
  expect(await screen.findByRole("heading", { name: "Organization not found" })).toBeTruthy();
});

describe("who sees the way in", () => {
  const admin: Me = { ...me, user: { ...me.user, is_superadmin: true } };
  const shell = (who: Me) =>
    render(
      <AppContext.Provider value={{ me: who, reloadMe: async () => {}, toast: () => {} }}>
        <Shell projectId={null}>page</Shell>
      </AppContext.Provider>,
    );

  test("a super admin gets a sidebar link and palette entries", () => {
    shell(admin);
    expect(screen.getByRole("link", { name: "Organizations and projects" }).getAttribute("href")).toBe("/admin");
    fireEvent.keyDown(window, { key: "k", metaKey: true });
    expect(screen.getByText("Super admin: Projects")).toBeTruthy();
  });

  test("everyone else sees no trace of it", () => {
    shell(me);
    expect(screen.queryByText(/Super admin/i)).toBeNull();
    expect(screen.queryByRole("link", { name: "Organizations and projects" })).toBeNull();
    fireEvent.keyDown(window, { key: "k", metaKey: true });
    expect(screen.queryByText(/Super admin/i)).toBeNull();
  });

  test("/admin for a non-admin is the ordinary not-found page, and asks the server for nothing", async () => {
    history.replaceState(null, "", "/admin");
    const { calls } = fakeServer({ "GET /api/ui/me": me });
    render(<App />);
    expect(await screen.findByRole("heading", { name: "Page not found" })).toBeTruthy();
    expect(calls.some((c) => c.path.startsWith("/api/ui/admin"))).toBe(false);
  });

  test("/admin for a super admin opens the page", async () => {
    history.replaceState(null, "", "/admin");
    fakeServer({ "GET /api/ui/me": admin, "GET /api/ui/admin/overview": overview });
    render(<App />);
    expect(await screen.findByRole("link", { name: "Globex" })).toBeTruthy();
  });
});
