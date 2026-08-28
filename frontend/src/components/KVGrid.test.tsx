import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { KVGrid } from "./KVGrid";

describe("KVGrid", () => {
  const items = [
    { key: "Name", value: "test-controller" },
    { key: "Version", value: "2.492.3" },
    { key: "Status", value: "Running" },
  ];

  it("renders all key-value pairs", () => {
    render(<KVGrid items={items} />);
    expect(screen.getByText("Name")).toBeInTheDocument();
    expect(screen.getByText("test-controller")).toBeInTheDocument();
    expect(screen.getByText("Version")).toBeInTheDocument();
    expect(screen.getByText("2.492.3")).toBeInTheDocument();
    expect(screen.getByText("Status")).toBeInTheDocument();
    expect(screen.getByText("Running")).toBeInTheDocument();
  });

  it("renders empty state when items array is empty", () => {
    const { container } = render(<KVGrid items={[]} />);
    expect(container.firstChild).toBeInTheDocument();
    // The KVGrid should render the outer container with no children
    expect(container.firstChild?.childNodes.length).toBe(0);
  });

  it("handles undefined/null values gracefully", () => {
    // value is typed as string, but test runtime behavior with non-string values
    const { container } = render(
      <KVGrid
        items={[
          { key: "NullValue", value: null as any },
          { key: "UndefinedValue", value: undefined as any },
        ]}
      />,
    );
    // null becomes "null" and undefined becomes "" when rendered as text content
    expect(screen.getByText("NullValue")).toBeInTheDocument();
    expect(screen.getByText("UndefinedValue")).toBeInTheDocument();
    expect(container.firstChild?.childNodes.length).toBe(2);
  });

  it("renders a single item correctly", () => {
    render(<KVGrid items={[{ key: "Only", value: "Single" }]} />);
    expect(screen.getByText("Only")).toBeInTheDocument();
    expect(screen.getByText("Single")).toBeInTheDocument();
  });
});
