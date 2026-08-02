import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import ConflictDialog from "./ConflictDialog";

describe("ConflictDialog", () => {
  const baseConflicts = [
    { field: ".spec.version", manager: "other-manager", message: "conflict with \"other-manager\"" },
    { field: ".spec.resources", message: "conflict with \"other-manager\"" },
  ];

  it("returns null when open is false", () => {
    const { container } = render(
      <ConflictDialog conflicts={baseConflicts} onReload={vi.fn()} onOverride={vi.fn()} open={false} />,
    );
    expect(container.innerHTML).toBe("");
  });

  it("renders conflict rows when open is true", () => {
    render(
      <ConflictDialog conflicts={baseConflicts} onReload={vi.fn()} onOverride={vi.fn()} open={true} />,
    );
    expect(screen.getByText(".spec.version")).toBeInTheDocument();
    expect(screen.getByText(".spec.resources")).toBeInTheDocument();
    expect(screen.getByText("Reload latest")).toBeInTheDocument();
    expect(screen.getByText("Override anyway")).toBeInTheDocument();
  });

  it("renders manager when present, falls back to message-only when absent", () => {
    render(
      <ConflictDialog conflicts={baseConflicts} onReload={vi.fn()} onOverride={vi.fn()} open={true} />,
    );
    // First conflict has a manager
    expect(screen.getByText("other-manager")).toBeInTheDocument();
    // Dialog heading
    expect(screen.getByText("Field ownership conflict")).toBeInTheDocument();
  });

  it("calls onReload when Reload latest is clicked", async () => {
    const onReload = vi.fn();
    render(
      <ConflictDialog conflicts={baseConflicts} onReload={onReload} onOverride={vi.fn()} open={true} />,
    );
    await userEvent.click(screen.getByText("Reload latest"));
    expect(onReload).toHaveBeenCalledTimes(1);
  });

  it("calls onOverride when Override anyway is clicked", async () => {
    const onOverride = vi.fn();
    render(
      <ConflictDialog conflicts={baseConflicts} onReload={vi.fn()} onOverride={onOverride} open={true} />,
    );
    await userEvent.click(screen.getByText("Override anyway"));
    expect(onOverride).toHaveBeenCalledTimes(1);
  });

  it("does not auto-fire callbacks on mount", () => {
    const onReload = vi.fn();
    const onOverride = vi.fn();
    render(
      <ConflictDialog conflicts={baseConflicts} onReload={onReload} onOverride={onOverride} open={true} />,
    );
    expect(onReload).not.toHaveBeenCalled();
    expect(onOverride).not.toHaveBeenCalled();
  });
});
