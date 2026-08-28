import { fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it, vi } from "vitest";
import { Shield } from "lucide-react";
import { NavIcon } from "./NavIcon";
import { RecoveryState } from "./RecoveryState";
import { SectionPage } from "./SectionPage";

describe("shell primitives", () => {
  it("renders decorative icons with consistent geometry", () => {
    const { container } = render(<NavIcon icon={Shield} />);
    const icon = container.querySelector("svg");
    expect(icon).toHaveAttribute("aria-hidden", "true");
    expect(icon).toHaveAttribute("width", "18");
    expect(icon).toHaveAttribute("stroke-width", "1.8");
  });
  it("renders exactly one SectionPage state", () => {
    const { rerender } = render(<SectionPage title="Users" loading>content</SectionPage>);
    expect(screen.queryByText("content")).not.toBeInTheDocument();
    rerender(<SectionPage title="Users" empty emptyMessage="No users" readOnly>content</SectionPage>);
    expect(screen.getByText("No users")).toBeInTheDocument();
    expect(screen.getByText("Read-only")).toBeInTheDocument();
  });
  it("offers retry, back, and home recovery actions", () => {
    const retry = vi.fn();
    render(<MemoryRouter><RecoveryState kind="error" title="Unable to load page" message="Varroa could not load this page." onRetry={retry} /></MemoryRouter>);
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(retry).toHaveBeenCalledOnce();
    expect(screen.getByRole("button", { name: "Back" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Home" })).toBeInTheDocument();
  });
});
