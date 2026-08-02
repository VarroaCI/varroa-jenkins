// Re-export default-exported components as named exports so the synth-entry
// `export *` exposes them on window.Varroa. Paths are relative to this file
// (frontend/.design-sync/). Wired via cfg.extraEntries.
export { default as ErrorBoundary } from "../src/components/ErrorBoundary";
export { default as Layout } from "../src/components/Layout";
export { default as LoadingSpinner } from "../src/components/LoadingSpinner";
