import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { StatusPill } from "./StatusPill";
import type { ControllerPhase } from "../types";

describe("StatusPill", () => {
  const phases: ControllerPhase[] = [
    "Pending",
    "Provisioning",
    "Running",
    "Connected",
    "Failed",
  ];

  it("renders each ControllerPhase with correct text", () => {
    for (const phase of phases) {
      const { unmount } = render(<StatusPill phase={phase} />);
      expect(screen.getByText(phase)).toBeInTheDocument();
      unmount();
    }
  });

  it("renders Hibernated phase with distinct styling", () => {
    render(<StatusPill phase="Hibernated" />);
    expect(screen.getByText("Hibernated")).toBeInTheDocument();
  });

  it("applies correct CSS class per phase", () => {
    const { rerender } = render(<StatusPill phase="Connected" />);
    const connectedClass = screen.getByText("Connected").className;
    expect(connectedClass).toBeTruthy();

    rerender(<StatusPill phase="Failed" />);
    const failedClass = screen.getByText("Failed").className;
    expect(failedClass).toBeTruthy();
    expect(failedClass).not.toBe(connectedClass);

    rerender(<StatusPill phase="Running" />);
    const runningClass = screen.getByText("Running").className;
    expect(runningClass).toBeTruthy();
    expect(runningClass).not.toBe(connectedClass);
  });

  it("renders Stopped phase with default (pending) styling", () => {
    render(<StatusPill phase="Stopped" />);
    expect(screen.getByText("Stopped")).toBeInTheDocument();
  });

  it("handles unknown phase strings gracefully", () => {
    render(<StatusPill phase={"UnknownPhase" as any} />);
    expect(screen.getByText("UnknownPhase")).toBeInTheDocument();
  });

  it("handles null phase without crashing", () => {
    const { container } = render(<StatusPill phase={null as any} />);
    expect(container.firstChild).toBeInTheDocument();
  });

  it("handles undefined phase without crashing", () => {
    const { container } = render(<StatusPill phase={undefined as any} />);
    expect(container.firstChild).toBeInTheDocument();
  });
});
