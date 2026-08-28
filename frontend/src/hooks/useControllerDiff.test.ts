import { describe, it, expect, vi, beforeEach } from "vitest";
import { renderHook, act } from "@testing-library/react";
import { useControllerDiff } from "./useControllerDiff";

vi.mock("../api/client", () => ({
  getControllerDiff: vi.fn(),
}));

import { getControllerDiff } from "../api/client";

describe("useControllerDiff", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("returns null diff initially", () => {
    const { result } = renderHook(() => useControllerDiff("core", "ns", "test"));
    expect(result.current.diff).toBeNull();
    expect(result.current.loading).toBe(false);
    expect(result.current.error).toBeNull();
  });

  it("fetches diff on demand", async () => {
    const mockDiff = {
      incoming: { jcasc: "yaml", items: "", plugins: "" },
      applied: { jcasc: "applied", items: "", plugins: "" },
    };
    vi.mocked(getControllerDiff).mockResolvedValueOnce(mockDiff);

    const { result } = renderHook(() => useControllerDiff("core", "ns", "test"));
    await act(async () => {
      await result.current.fetchDiff();
    });

    expect(result.current.diff).toEqual(mockDiff);
    expect(result.current.loading).toBe(false);
    expect(getControllerDiff).toHaveBeenCalledWith("core", "ns", "test");
  });

  it("handles fetch error", async () => {
    vi.mocked(getControllerDiff).mockRejectedValueOnce(new Error("fail"));

    const { result } = renderHook(() => useControllerDiff("core", "ns", "test"));
    await act(async () => {
      await result.current.fetchDiff();
    });

    expect(result.current.diff).toBeNull();
    expect(result.current.error).toBe("fail");
    expect(result.current.loading).toBe(false);
  });
});
