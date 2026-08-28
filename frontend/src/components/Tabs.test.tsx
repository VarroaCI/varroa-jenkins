import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Tabs } from "./Tabs";

describe("Tabs", () => {
  const tabs = [
    { id: "tab1", label: "Tab One" },
    { id: "tab2", label: "Tab Two" },
    { id: "tab3", label: "Tab Three" },
  ];

  it("renders all tab labels", () => {
    render(<Tabs tabs={tabs} activeTab="tab1" onSelect={vi.fn()} />);
    expect(screen.getByText("Tab One")).toBeInTheDocument();
    expect(screen.getByText("Tab Two")).toBeInTheDocument();
    expect(screen.getByText("Tab Three")).toBeInTheDocument();
  });

  it("marks the controlled activeTab as active", () => {
    render(<Tabs tabs={tabs} activeTab="tab1" onSelect={vi.fn()} />);
    const tabButtons = screen.getAllByRole("button");
    // CSS module classes are hashed (e.g. "_on_88a7c2"), so use regex
    expect(tabButtons[0]).toHaveClass(/_on_/);
    expect(tabButtons[1]).not.toHaveClass(/_on_/);
    expect(tabButtons[2]).not.toHaveClass(/_on_/);
  });

  it("clicking a tab calls onSelect with the correct tab key", async () => {
    const onSelect = vi.fn();
    render(<Tabs tabs={tabs} activeTab="tab1" onSelect={onSelect} />);
    await userEvent.click(screen.getByText("Tab Two"));
    expect(onSelect).toHaveBeenCalledWith("tab2");
  });

  it("active tab gets active class when activeTab changes", () => {
    const { rerender } = render(
      <Tabs tabs={tabs} activeTab="tab1" onSelect={vi.fn()} />,
    );
    const tabButtons = screen.getAllByRole("button");
    expect(tabButtons[0]).toHaveClass(/_on_/);

    rerender(<Tabs tabs={tabs} activeTab="tab2" onSelect={vi.fn()} />);
    expect(tabButtons[1]).toHaveClass(/_on_/);
    expect(tabButtons[0]).not.toHaveClass(/_on_/);
  });
});
