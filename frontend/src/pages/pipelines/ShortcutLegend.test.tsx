import { describe, it, expect, vi } from "vitest";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { ShortcutLegend, BUILD_PAGE_SHORTCUTS } from "./ShortcutLegend";

describe("ShortcutLegend", () => {
  it("renders nothing while closed", () => {
    const { container } = render(<ShortcutLegend open={false} onClose={vi.fn()} />);
    expect(container).toBeEmptyDOMElement();
  });

  it("names every shortcut", () => {
    render(<ShortcutLegend open onClose={vi.fn()} />);
    const dialog = screen.getByRole("dialog", { name: "Keyboard shortcuts" });
    const rows = dialog.querySelectorAll("dl > div");
    expect(rows).toHaveLength(BUILD_PAGE_SHORTCUTS.length);
    expect(BUILD_PAGE_SHORTCUTS).toHaveLength(7);
    for (const s of BUILD_PAGE_SHORTCUTS) {
      expect(within(dialog).getByText(s.key, { selector: "kbd" })).toBeInTheDocument();
      expect(within(dialog).getByText(s.action)).toBeInTheDocument();
    }
  });

  it("focuses the close button on open", () => {
    render(<ShortcutLegend open onClose={vi.fn()} />);
    expect(screen.getByRole("button", { name: "Close" })).toHaveFocus();
  });

  it("calls onClose from the close button", () => {
    const onClose = vi.fn();
    render(<ShortcutLegend open onClose={onClose} />);
    fireEvent.click(screen.getByRole("button", { name: "Close" }));
    expect(onClose).toHaveBeenCalledTimes(1);
  });
});
