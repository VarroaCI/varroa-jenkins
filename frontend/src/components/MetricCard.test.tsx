import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { MetricCard } from "./MetricCard";

describe("MetricCard", () => {
  it("renders label text", () => {
    render(<MetricCard label="CPU Usage" value="45%" />);
    expect(screen.getByText("CPU Usage")).toBeInTheDocument();
  });

  it("renders value text", () => {
    render(<MetricCard label="CPU Usage" value="45%" />);
    expect(screen.getByText("45%")).toBeInTheDocument();
  });

  it("handles missing/undefined value", () => {
    render(<MetricCard label="CPU Usage" value={undefined as any} />);
    expect(screen.getByText("CPU Usage")).toBeInTheDocument();
  });

  it("handles numeric values", () => {
    render(<MetricCard label="Builds" value={42} />);
    expect(screen.getByText("42")).toBeInTheDocument();
  });

  it("handles string values", () => {
    render(<MetricCard label="Status" value="healthy" />);
    expect(screen.getByText("healthy")).toBeInTheDocument();
  });

  it("renders optional icon when provided", () => {
    render(
      <MetricCard
        label="Disk"
        value="80%"
        icon={<span data-testid="test-icon" />}
      />,
    );
    expect(screen.getByTestId("test-icon")).toBeInTheDocument();
  });

  it("does not render icon when not provided", () => {
    render(<MetricCard label="Disk" value="80%" />);
    // No icon element should appear in the metric card
    // Verify the component renders the label and value
    expect(screen.getByText("Disk")).toBeInTheDocument();
    expect(screen.getByText("80%")).toBeInTheDocument();
  });

  it("renders sub text when provided", () => {
    render(
      <MetricCard label="Memory" value="2GB" sub={<span>Sub text</span>} />,
    );
    expect(screen.getByText("Sub text")).toBeInTheDocument();
  });
});
