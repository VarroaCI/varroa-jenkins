import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { cutAtWord, revealRate, REVEAL_MIN_RATE, REVEAL_WINDOW_MS, useRevealedText } from "./reveal";

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["requestAnimationFrame", "cancelAnimationFrame"] });
});

afterEach(() => {
  vi.useRealTimers();
});

describe("revealRate", () => {
  it("floors at the minimum and empties a backlog in one window", () => {
    expect(revealRate(0)).toBe(REVEAL_MIN_RATE);
    expect(revealRate(100) * REVEAL_WINDOW_MS).toBeGreaterThanOrEqual(100);
  });
});

describe("cutAtWord", () => {
  it("steps back to a word end, and a final text ends anywhere", () => {
    expect(cutAtWord("alpha beta gam", 13, false)).toBe(11);
    expect("alpha beta gam".slice(0, cutAtWord("alpha beta gam", 13, false)).trim()).toBe("alpha beta");
    expect(cutAtWord("alpha beta gam", 14, true)).toBe(14);
  });
});

describe("useRevealedText", () => {
  it("returns the full text when it does not animate", () => {
    const { result } = renderHook(() => useRevealedText("hello there", false, false));
    expect(result.current).toBe("hello there");
  });

  it("starts empty and shows a final text within one window", () => {
    const text = "alpha beta gamma";
    const { result } = renderHook(() => useRevealedText(text, true, true));
    expect(result.current).toBe("");
    act(() => vi.advanceTimersByTime(600));
    expect(result.current).toBe(text);
  });

  it("never shows a partial word of a streaming turn", () => {
    const { result, rerender } = renderHook(
      ({ text, final }) => useRevealedText(text, true, final),
      { initialProps: { text: "alpha beta gam", final: false } },
    );
    act(() => vi.advanceTimersByTime(2000));
    expect(result.current.trim()).toBe("alpha beta");
    rerender({ text: "alpha beta gam", final: true });
    act(() => vi.advanceTimersByTime(600));
    expect(result.current).toBe("alpha beta gam");
  });

  it("shows a backlog above the snap size at once", () => {
    const text = "a ".repeat(1100);
    const { result } = renderHook(() => useRevealedText(text, true, true));
    expect(result.current).toBe(text);
  });

  it("animates only the text that arrives after a replay", () => {
    const { result, rerender } = renderHook(
      ({ text, animate }) => useRevealedText(text, animate, true),
      { initialProps: { text: "old text ", animate: false } },
    );
    expect(result.current).toBe("old text ");
    rerender({ text: "old text new words ", animate: true });
    expect(result.current.startsWith("old text ")).toBe(true);
    act(() => vi.advanceTimersByTime(1000));
    expect(result.current).toBe("old text new words ");
  });
});
