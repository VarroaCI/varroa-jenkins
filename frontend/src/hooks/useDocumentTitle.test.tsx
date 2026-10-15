import { describe, it, expect } from "vitest";
import { renderHook } from "@testing-library/react";
import { useDocumentTitle } from "./useDocumentTitle";

describe("useDocumentTitle", () => {
  it("sets the title on mount", () => {
    document.title = "Start";
    renderHook(() => useDocumentTitle("Pipelines"));
    expect(document.title).toBe("Pipelines");
  });

  it("changes the title when the argument changes", () => {
    document.title = "Start";
    const { rerender } = renderHook(({ t }: { t: string }) => useDocumentTitle(t), {
      initialProps: { t: "First" },
    });
    rerender({ t: "Second" });
    expect(document.title).toBe("Second");
  });

  it("restores the title the page found on unmount", () => {
    document.title = "Start";
    const { unmount } = renderHook(() => useDocumentTitle("Pipelines"));
    unmount();
    expect(document.title).toBe("Start");
  });

  it("leaves the title unchanged for null", () => {
    document.title = "Start";
    renderHook(() => useDocumentTitle(null));
    expect(document.title).toBe("Start");
  });
});
