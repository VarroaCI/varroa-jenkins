import "@testing-library/jest-dom/vitest";
import { configure } from "@testing-library/dom";

// Testing Library's waitFor/findBy* budget is SEPARATE from Vitest's
// testTimeout (raised to 15s in vite.config.ts for the same reason) and
// defaults to just 1000ms. Under `npm run coverage` on the self-hosted ARC
// runner, v8 instrumentation plus a loaded host regularly pushes a legitimate
// query-settle-then-render past 1s, so correctly-written waits fail
// non-deterministically in files the change under test never touched.
// Raise the async-util budget to match. Vitest still fails the test at
// testTimeout, so a genuinely hung wait is still caught — this only stops
// slow-but-correct waits from being reported as failures.
configure({ asyncUtilTimeout: 5000 });
