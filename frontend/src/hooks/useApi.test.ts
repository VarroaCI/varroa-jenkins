import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError, bffFetch, logout } from "./useApi";

// Mock BFF_BASE used by the request helper
vi.mock("../api/client", () => ({ BFF_BASE: "/api/v1" }));

describe("logout URL composition", () => {
  afterEach(() => vi.restoreAllMocks());

  it("calls POST /api/v1/logout (BFF_BASE + /logout)", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ redirect: "/login" }), { status: 200, headers: { "content-type": "application/json" } }));
    vi.stubGlobal("fetch", fetchMock);
    const result = await logout();
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, opts] = fetchMock.mock.calls[0];
    expect(url).toBe("/api/v1/logout");
    expect(opts.method).toBe("POST");
    expect(result).toEqual({ redirect: "/login" });
  });
});

describe("bffFetch errors", () => {
  afterEach(() => vi.restoreAllMocks());
  it("preserves status and structured body without exposing raw response copy", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({ code: "PREFLIGHT_FAILED", checks: [{ id: "version" }], secret: "internal" }), { status: 400, headers: { "content-type": "application/json" } })));
    const error = await bffFetch("/test").catch((value) => value) as ApiError;
    expect(error).toMatchObject({ status: 400, message: "Preflight checks failed." });
    expect(error.message).not.toContain("internal");
    expect(error.body).toMatchObject({ checks: [{ id: "version" }] });
  });
  it("uses status zero for network failures", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("private network detail")));
    const error = await bffFetch("/test").catch((value) => value) as ApiError;
    expect(error.status).toBe(0);
    expect(error.message).not.toContain("private network detail");
  });
  it("signals authentication handling without navigating", async () => {
    const listener = vi.fn();
    window.addEventListener("varroa:unauthorized", listener);
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response("secret", { status: 401 })));
    await expect(bffFetch("/test")).rejects.toMatchObject({ status: 401 });
    expect(listener).toHaveBeenCalledOnce();
    window.removeEventListener("varroa:unauthorized", listener);
  });
});
