/// <reference types="vitest/config" />
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  server: {
    port: 3000,
    proxy: {
      "/api": {
        target: process.env.VARROA_API_URL || "http://localhost:8080",
        changeOrigin: true,
      },
    },
  },
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./src/test/setup.ts"],
    // A few heavyweight userEvent "marathon" tests (ControllerDetail health
    // probes, ControllerWizard full-deploy, ComposedBundleEdit drafts) run
    // close to the 5s default. Under `npm run coverage` the v8 instrumentation
    // slows the slowest CI worker enough to tip them over, so raise the
    // per-test and hook budgets. Not a substitute for keeping tests fast.
    testTimeout: 15000,
    hookTimeout: 15000,
    coverage: {
      provider: "v8",
      include: ["src/**/*.{ts,tsx}"],
      // Vitest 4 stopped instrumenting spec files as part of the covered set
      // (Vitest 2 counted each `*.test.{ts,tsx}` file's own near-100%-executed
      // lines/branches as "covered source", padding the aggregate by several
      // points). That padding is gone, so these thresholds are the honest
      // production-code floor, not a lowered bar. Explicit test-file excludes
      // below make that behavior config-visible instead of relying on the
      // provider default. branches/functions/statements sit at the nearest
      // half-point below the current honest measurement (npm run coverage on
      // 2026-08-02); ratchet up as real coverage improves, don't push back down.
      thresholds: {
        lines: 80,
        branches: 74.5,
        functions: 73.5,
        statements: 78.5,
      },
      exclude: [
        "src/test/",
        "src/vite-env.d.ts",
        "src/**/*.{test,spec}.{ts,tsx}",
      ],
    },
  },
});
