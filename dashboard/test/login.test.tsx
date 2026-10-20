import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { Login } from "../src/pages/Login";
import { fakeServer } from "./helpers";

const setup = (needs_setup: boolean, signup_open: boolean) => ({ "GET /api/ui/auth/setup": { needs_setup, signup_open } });

test("login links to sign-up when it's open", async () => {
  fakeServer(setup(false, true));
  render(<Login page="login" onDone={() => {}} />);
  expect(await screen.findByRole("heading", { name: "Log in" })).toBeTruthy();
  expect(screen.getByRole("link", { name: "Create an account" }).getAttribute("href")).toBe("/signup");
});

test("with sign-up closed, login still says how to get an account", async () => {
  fakeServer(setup(false, false));
  const { unmount } = render(<Login page="login" onDone={() => {}} />);
  expect((await screen.findByRole("link", { name: "How to join" })).getAttribute("href")).toBe("/signup");
  unmount();

  // /signup explains how to get in, instead of a form that can only fail,
  // and without server commands a visitor couldn't run.
  render(<Login page="signup" onDone={() => {}} />);
  expect(await screen.findByRole("heading", { name: "Join with an invite" })).toBeTruthy();
  expect(screen.queryByLabelText("Password")).toBeNull();
  expect(document.body.textContent).not.toMatch(/juju-admin|JUJU_ALLOW_SIGNUP/);
  expect(screen.getByRole("link", { name: "Log in" })).toBeTruthy();
});

test("the first visit is setup, wherever it starts, with no way to skip it", async () => {
  fakeServer(setup(true, true));
  render(<Login page="login" onDone={() => {}} />);
  expect(await screen.findByRole("heading", { name: "Create your account" })).toBeTruthy();
  expect(screen.getByText(/you'll own it/)).toBeTruthy();
  expect(screen.queryByRole("link", { name: "Log in" })).toBeNull();
});

test("sign-up sends the form and shows why it failed", async () => {
  const { calls } = fakeServer(setup(false, true));
  render(<Login page="signup" onDone={() => {}} />);
  await screen.findByRole("heading", { name: "Create an account" });
  // Later sign-ups get their own organization; only the first person owns the server.
  expect(screen.queryByText(/you'll own it/)).toBeNull();
  vi.stubGlobal(
    "fetch",
    vi.fn(async (_url: string, init: RequestInit) => {
      calls.push({ method: init.method ?? "GET", path: String(_url), headers: init.headers as Record<string, string>, body: JSON.parse(String(init.body)) });
      return new Response(JSON.stringify({ error: "an organization with this name already exists; choose another name, or ask its owner to add you" }), { status: 409 });
    }),
  );
  fireEvent.change(screen.getByLabelText("Email"), { target: { value: "eve@example.com" } });
  fireEvent.change(screen.getByLabelText("Password"), { target: { value: "a long enough password" } });
  fireEvent.change(screen.getByLabelText("Organization"), { target: { value: "Acme" } });
  fireEvent.click(screen.getByRole("button", { name: "Create account" }));
  await waitFor(() => expect(screen.getByRole("alert").textContent).toContain("already exists"));
  expect(calls.at(-1)).toMatchObject({ method: "POST", path: "/api/ui/auth/signup", body: { email: "eve@example.com", organization: "Acme" } });
});
