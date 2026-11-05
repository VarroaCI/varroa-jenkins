import { describe, it, expect } from "vitest";
import { render, screen, fireEvent, act } from "@testing-library/react";
import { useRef, useState } from "react";
import { useFocusFallback } from "./useFocusFallback";

/** Harness holds a fallback button, a control that can unmount, and two buttons that change state. */
function Harness() {
  const fallbackRef = useRef<HTMLButtonElement | null>(null);
  const keep = useFocusFallback(fallbackRef);
  const [shown, setShown] = useState(true);
  const [renders, setRenders] = useState(0);
  return (
    <>
      <button ref={fallbackRef} type="button">fallback</button>
      <button type="button" onClick={() => setShown(false)}>hide</button>
      <button type="button" onClick={() => setRenders((n) => n + 1)}>render</button>
      <span>{renders}</span>
      {shown && <button ref={keep} type="button">control</button>}
    </>
  );
}

describe("useFocusFallback", () => {
  it("moves focus to the fallback when the focused control unmounts", () => {
    render(<Harness />);
    act(() => screen.getByRole("button", { name: "control" }).focus());
    fireEvent.click(screen.getByRole("button", { name: "hide" }));
    expect(screen.queryByRole("button", { name: "control" })).toBeNull();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "fallback" }));
  });

  it("keeps focus in place when the control unmounts without focus", () => {
    render(<Harness />);
    const hide = screen.getByRole("button", { name: "hide" });
    act(() => hide.focus());
    fireEvent.click(hide);
    expect(screen.queryByRole("button", { name: "control" })).toBeNull();
    expect(document.activeElement).toBe(hide);
  });

  it("keeps focus on the control across a re-render", () => {
    render(<Harness />);
    const control = screen.getByRole("button", { name: "control" });
    act(() => control.focus());
    fireEvent.click(screen.getByRole("button", { name: "render" }));
    expect(screen.getByText("1")).toBeTruthy();
    expect(document.activeElement).toBe(control);
  });
});
