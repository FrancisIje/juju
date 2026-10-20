import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { until } from "../src/lib/time";
import { InvitePage } from "../src/pages/InvitePage";
import { Team } from "../src/pages/Team";
import { fakeServer, me, renderWithApp } from "./helpers";

const info = { organization_id: 1, organization: "Acme", invited_by: "Ada", expires_at: new Date(Date.now() + 6 * 864e5).toISOString() };

test("until", () => {
  const now = Date.parse("2026-10-20T12:00:00Z");
  expect(until("2026-10-27T12:00:00Z", now)).toBe("in 7d");
  expect(until("2026-10-20T15:00:00Z", now)).toBe("in 3h");
  expect(until("2026-10-20T11:00:00Z", now)).toBe("now");
});

describe("invite page", () => {
  test("logged out: sign up and join in one step, without naming an organization", async () => {
    const { calls } = fakeServer({ "GET /api/ui/invites/tok123": info, "POST /api/ui/auth/signup": {} });
    const joined = vi.fn();
    render(<InvitePage token="tok123" onJoined={joined} />);
    expect(await screen.findByRole("heading", { name: "Join Acme" })).toBeTruthy();
    expect(screen.getByText(/Ada invited you/)).toBeTruthy();
    expect(screen.queryByLabelText("Organization")).toBeNull();
    // Already have an account: log in, then come back here.
    expect(screen.getByRole("link", { name: "Log in to join" }).getAttribute("href")).toBe("/login?next=%2Finvite%2Ftok123");

    fireEvent.change(screen.getByLabelText("Email"), { target: { value: "bob@example.com" } });
    fireEvent.change(screen.getByLabelText("Password"), { target: { value: "a long enough password" } });
    fireEvent.click(screen.getByRole("button", { name: "Create account and join Acme" }));
    await waitFor(() => expect(joined).toHaveBeenCalledWith(1));
    expect(calls.find((c) => c.method === "POST")!.body).toMatchObject({ email: "bob@example.com", invite: "tok123" });
  });

  test("logged in: one button to join", async () => {
    const { calls } = fakeServer({
      "GET /api/ui/invites/tok123": { ...info, organization_id: 9, organization: "Other Co" },
      "GET /api/ui/me": me,
      "POST /api/ui/invites/tok123/accept": { organization_id: 9, already_member: false },
    });
    const joined = vi.fn();
    render(<InvitePage token="tok123" onJoined={joined} />);
    fireEvent.click(await screen.findByRole("button", { name: "Join Other Co" }));
    await waitFor(() => expect(joined).toHaveBeenCalledWith(9));
    expect(calls.some((c) => c.method === "POST" && c.path === "/api/ui/invites/tok123/accept")).toBe(true);
  });

  test("already a member: no join, just open it", async () => {
    fakeServer({ "GET /api/ui/invites/tok123": info, "GET /api/ui/me": me }); // me is in Acme (id 1)
    render(<InvitePage token="tok123" onJoined={() => {}} />);
    expect(await screen.findByRole("button", { name: /already in Acme/ })).toBeTruthy();
  });

  test("a used or expired link says so and what to do", async () => {
    fakeServer({}); // the invite lookup answers 404
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response(JSON.stringify({ error: "this invite link has expired or was already used; ask for a new one" }), { status: 404 })),
    );
    render(<InvitePage token="old" onJoined={() => {}} />);
    expect(await screen.findByRole("heading", { name: "This invite doesn't work any more" })).toBeTruthy();
    expect(screen.getByText(/ask for a new one/)).toBeTruthy();
  });
});

describe("team tab", () => {
  const org = me.organizations[0]!;
  const team = (role: "owner" | "member") => ({
    members: [
      { user_id: 1, email: "ada@example.com", name: "Ada", role: "owner", joined_at: new Date().toISOString() },
      { user_id: 2, email: "bob@example.com", name: "", role: "member", joined_at: new Date().toISOString() },
    ],
    invites: role === "owner" ? [] : undefined,
    your_role: role,
    you: role === "owner" ? 1 : 2,
  });

  test("an owner creates a link, sees it once, and can remove others but not themselves", async () => {
    const { calls } = fakeServer({
      "GET /api/ui/orgs/1/members": team("owner"),
      "POST /api/ui/orgs/1/invites": { url: "http://juju.test/invite/abc", invite: { id: 5, created_by: "Ada", created_at: new Date().toISOString(), expires_at: info.expires_at } },
      "DELETE /api/ui/orgs/1/members/2": {},
    });
    const { toast } = renderWithApp(<Team org={org} />);
    fireEvent.click(await screen.findByRole("button", { name: "Create invite link" }));
    expect(await screen.findByText("http://juju.test/invite/abc")).toBeTruthy();
    expect(screen.getByText(/shown only now/)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Revoke" })).toBeTruthy(); // the new link is listed

    // Status in words: Owner / Member. Ada can't remove herself.
    expect(screen.getByText("Owner")).toBeTruthy();
    expect(screen.getAllByRole("button", { name: "Remove…" })).toHaveLength(1);
    fireEvent.click(screen.getByRole("button", { name: "Remove…" }));
    fireEvent.click(screen.getByRole("button", { name: "Remove bob@example.com" }));
    await waitFor(() => expect(toast).toHaveBeenCalledWith("bob@example.com was removed from Acme"));
    expect(calls.some((c) => c.method === "DELETE" && c.path === "/api/ui/orgs/1/members/2")).toBe(true);
  });

  test("a member sees who to ask, and no invite or remove controls", async () => {
    fakeServer({ "GET /api/ui/orgs/1/members": team("member") });
    renderWithApp(<Team org={org} />);
    expect(await screen.findByText(/Only owners can invite people. Ask Ada/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Create invite link|Remove/ })).toBeNull();
    expect(screen.queryByRole("heading", { name: "Open invite links" })).toBeNull();
  });
});
