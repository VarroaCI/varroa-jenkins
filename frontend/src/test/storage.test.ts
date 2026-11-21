import { describe, expect, it } from "vitest";

// Node 25 and later define localStorage and sessionStorage globals of their
// own. The tests must read and write the jsdom storage, as the app does in a
// browser. vite.config.ts turns the Node storage off in each test worker.
describe("test environment storage", () => {
  it("exposes the jsdom localStorage on the global and on window", () => {
    expect(localStorage).toBeInstanceOf(Storage);
    expect(window.localStorage).toBe(localStorage);
    window.localStorage.setItem("storage-probe", "local");
    expect(localStorage.getItem("storage-probe")).toBe("local");
    localStorage.removeItem("storage-probe");
    expect(localStorage.getItem("storage-probe")).toBeNull();
  });

  it("exposes the jsdom sessionStorage on the global and on window", () => {
    expect(sessionStorage).toBeInstanceOf(Storage);
    expect(window.sessionStorage).toBe(sessionStorage);
    window.sessionStorage.setItem("storage-probe", "session");
    expect(sessionStorage.getItem("storage-probe")).toBe("session");
    sessionStorage.removeItem("storage-probe");
    expect(sessionStorage.getItem("storage-probe")).toBeNull();
  });
});
