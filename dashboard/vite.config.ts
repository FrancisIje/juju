import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5174,
    // In development the dashboard runs on Vite's server and calls the Go
    // server for data. Proxying /api keeps it same-origin, like production,
    // so cookies and the CSRF check behave the same.
    proxy: { "/api": "http://localhost:8080" },
  },
  build: { outDir: "dist", sourcemap: false }, // keeps the Go binary small
  test: { environment: "happy-dom", include: ["test/**/*.test.{ts,tsx}"], globals: true, restoreMocks: true },
});
