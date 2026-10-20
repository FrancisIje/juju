import { defineConfig } from "@playwright/test";

// End-to-end tests drive the real dashboard, embedded in a real juju server,
// in the Chrome already installed on this machine (no browser download).
// They expect a fresh server: see e2e/README.md.
export default defineConfig({
  testDir: "e2e",
  timeout: 60_000,
  workers: 1,
  use: { baseURL: process.env.JUJU_E2E_URL ?? "http://localhost:8080", channel: "chrome", trace: "retain-on-failure" },
});
