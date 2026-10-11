import { useState } from "react";
import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Tabs, tabPanelProps } from "./Tabs";

describe("Tabs", () => {
  const tabs = [
    { id: "tab1", label: "Tab One" },
    { id: "tab2", label: "Tab Two" },
    { id: "tab3", label: "Tab Three" },
  ];

  function Harness({ onSelect }: { onSelect: (id: string) => void }) {
    const [active, setActive] = useState("tab1");
    return (
      <Tabs
        tabs={tabs}
        activeTab={active}
        onSelect={(id) => {
          setActive(id);
          onSelect(id);
        }}
        idBase="t"
        ariaLabel="Sections"
      />
    );
  }

  it("renders all tab labels", () => {
    render(<Tabs tabs={tabs} activeTab="tab1" onSelect={vi.fn()} idBase="t" />);
    expect(screen.getByText("Tab One")).toBeInTheDocument();
    expect(screen.getByText("Tab Two")).toBeInTheDocument();
    expect(screen.getByText("Tab Three")).toBeInTheDocument();
  });

  it("marks the controlled activeTab as active", () => {
    render(<Tabs tabs={tabs} activeTab="tab1" onSelect={vi.fn()} idBase="t" />);
    const tabButtons = screen.getAllByRole("tab");
    // CSS module classes are hashed (e.g. "_on_88a7c2"), so use regex
    expect(tabButtons[0]).toHaveClass(/_on_/);
    expect(tabButtons[0]).toHaveAttribute("aria-selected", "true");
    expect(tabButtons[1]).not.toHaveClass(/_on_/);
    expect(tabButtons[1]).toHaveAttribute("aria-selected", "false");
    expect(tabButtons[2]).not.toHaveClass(/_on_/);
    expect(tabButtons[2]).toHaveAttribute("aria-selected", "false");
  });

  it("clicking a tab calls onSelect with the correct tab key", async () => {
    const onSelect = vi.fn();
    render(<Tabs tabs={tabs} activeTab="tab1" onSelect={onSelect} idBase="t" />);
    await userEvent.click(screen.getByText("Tab Two"));
    expect(onSelect).toHaveBeenCalledWith("tab2");
  });

  it("active tab gets active class when activeTab changes", () => {
    const { rerender } = render(
      <Tabs tabs={tabs} activeTab="tab1" onSelect={vi.fn()} idBase="t" />,
    );
    const tabButtons = screen.getAllByRole("tab");
    expect(tabButtons[0]).toHaveClass(/_on_/);

    rerender(<Tabs tabs={tabs} activeTab="tab2" onSelect={vi.fn()} idBase="t" />);
    expect(tabButtons[1]).toHaveClass(/_on_/);
    expect(tabButtons[0]).not.toHaveClass(/_on_/);
  });

  it("renders a tablist with tab roles and ids", () => {
    render(<Tabs tabs={tabs} activeTab="tab1" onSelect={vi.fn()} idBase="t" ariaLabel="Sections" />);
    const list = screen.getByRole("tablist", { name: "Sections" });
    const rendered = screen.getAllByRole("tab");
    expect(list).toBeInTheDocument();
    expect(rendered).toHaveLength(3);
    expect(rendered[0]).toHaveAttribute("id", "t-tab-tab1");
    expect(rendered[0]).toHaveAttribute("aria-controls", "t-panel-tab1");
    expect(rendered[1]).not.toHaveAttribute("aria-controls");
    expect(rendered[0]).toHaveAttribute("type", "button");
  });

  it("uses a roving tabindex", () => {
    const { rerender } = render(
      <Tabs tabs={tabs} activeTab="tab2" onSelect={vi.fn()} idBase="t" />,
    );
    let rendered = screen.getAllByRole("tab");
    expect(rendered.map((el) => el.tabIndex)).toEqual([-1, 0, -1]);

    rerender(<Tabs tabs={tabs} activeTab="unknown" onSelect={vi.fn()} idBase="t" />);
    rendered = screen.getAllByRole("tab");
    expect(rendered.map((el) => el.tabIndex)).toEqual([0, -1, -1]);
  });

  it("ArrowRight and ArrowLeft move focus and select, with wrap", async () => {
    const onSelect = vi.fn();
    render(<Harness onSelect={onSelect} />);
    await userEvent.click(screen.getByRole("tab", { name: "Tab One" }));

    await userEvent.keyboard("{ArrowRight}");
    expect(screen.getByRole("tab", { name: "Tab Two" })).toHaveFocus();
    expect(screen.getByRole("tab", { name: "Tab Two" })).toHaveAttribute("aria-selected", "true");

    await userEvent.keyboard("{ArrowRight}");
    await userEvent.keyboard("{ArrowRight}");
    expect(screen.getByRole("tab", { name: "Tab One" })).toHaveFocus();
    expect(screen.getByRole("tab", { name: "Tab One" })).toHaveAttribute("aria-selected", "true");

    await userEvent.keyboard("{ArrowLeft}");
    expect(screen.getByRole("tab", { name: "Tab Three" })).toHaveFocus();
    expect(screen.getByRole("tab", { name: "Tab Three" })).toHaveAttribute("aria-selected", "true");
  });

  it("Home and End jump to the first and last tab", async () => {
    const onSelect = vi.fn();
    render(<Harness onSelect={onSelect} />);
    await userEvent.click(screen.getByRole("tab", { name: "Tab One" }));

    await userEvent.keyboard("{End}");
    expect(screen.getByRole("tab", { name: "Tab Three" })).toHaveFocus();
    expect(onSelect).toHaveBeenLastCalledWith("tab3");

    await userEvent.keyboard("{Home}");
    expect(screen.getByRole("tab", { name: "Tab One" })).toHaveFocus();
    expect(onSelect).toHaveBeenLastCalledWith("tab1");
  });

  it("ignores arrow keys pressed with a modifier", async () => {
    const onSelect = vi.fn();
    render(<Tabs tabs={tabs} activeTab="tab1" onSelect={onSelect} idBase="t" />);
    const first = screen.getByRole("tab", { name: "Tab One" });
    first.focus();

    await userEvent.keyboard("{Control>}{ArrowRight}{/Control}");
    expect(onSelect).not.toHaveBeenCalled();
    expect(first).toHaveFocus();
  });

  it("ignores other keys", async () => {
    const onSelect = vi.fn();
    render(<Tabs tabs={tabs} activeTab="tab1" onSelect={onSelect} idBase="t" />);
    const first = screen.getByRole("tab", { name: "Tab One" });
    first.focus();

    await userEvent.keyboard("a");
    expect(onSelect).not.toHaveBeenCalled();
    expect(first).toHaveFocus();
  });

  it("tabPanelProps links the panel to its tab", () => {
    render(
      <>
        <Tabs tabs={tabs} activeTab="tab2" onSelect={vi.fn()} idBase="t" />
        <div {...tabPanelProps("t", "tab2")}>x</div>
      </>,
    );
    expect(screen.getByRole("tabpanel", { name: "Tab Two" })).toBeInTheDocument();
  });
});
