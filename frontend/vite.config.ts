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
      thresholds: {
        lines: 80,
        branches: 80,
        functions: 74,
        statements: 80,
      },
      exclude: ["src/test/", "src/vite-env.d.ts"],
    },
  },
});
