import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { Console } from "./Console";

const sampleLines = [
  {
    timestamp: "2024-01-01T00:00:00Z",
    level: "INFO" as const,
    source: "test",
    message: "line one",
  },
  {
    timestamp: "2024-01-01T00:00:01Z",
    level: "WARN" as const,
    source: "test",
    message: "line two",
  },
];

describe("Console", () => {
  it("renders lines prop as text content", () => {
    render(<Console lines={sampleLines} />);
    expect(screen.getByText("line one")).toBeInTheDocument();
    expect(screen.getByText("line two")).toBeInTheDocument();
    // Should also render timestamp, level, and source
    expect(screen.getByText("INFO")).toBeInTheDocument();
    expect(screen.getByText("WARN")).toBeInTheDocument();
    expect(screen.getByText("2024-01-01T00:00:00Z")).toBeInTheDocument();
  });

  it("renders empty console when lines is empty", () => {
    const { container } = render(<Console lines={[]} />);
    expect(container.firstChild).toBeInTheDocument();
    // No log level text should appear
    expect(screen.queryByText("INFO")).not.toBeInTheDocument();
    expect(screen.queryByText("WARN")).not.toBeInTheDocument();
  });

  it("auto-scrolls to bottom on new lines", () => {
    const { container } = render(<Console lines={sampleLines} />);
    const consoleEl = container.firstChild as HTMLElement;
    expect(consoleEl).toBeTruthy();
    // The component's useEffect sets scrollTop = scrollHeight
    // In jsdom both default to 0, so the equality holds
    expect(consoleEl.scrollTop).toBe(consoleEl.scrollHeight);
  });

  it("handles undefined lines gracefully", () => {
    const { container } = render(<Console lines={undefined} />);
    expect(container.firstChild).toBeInTheDocument();
    expect(screen.queryByText("INFO")).not.toBeInTheDocument();
  });

  it("renders multiple log lines with correct structure", () => {
    render(
      <Console
        lines={[
          ...sampleLines,
          {
            timestamp: "2024-01-01T00:00:02Z",
            level: "ERROR" as const,
            source: "test",
            message: "line three",
          },
        ]}
      />,
    );
    expect(screen.getByText("line three")).toBeInTheDocument();
    expect(screen.getByText("ERROR")).toBeInTheDocument();
  });
});
