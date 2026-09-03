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

  it("appends an attention tag and exposes the message as a tooltip", () => {
    render(
      <StatusPill
        phase="Provisioning"
        attention={{ kind: "bootFailed", message: "jenkins container exited with code 5 (283 restarts): Error" }}
      />,
    );
    expect(screen.getByText("Provisioning", { exact: false })).toBeInTheDocument();
    const tag = screen.getByText("Boot failed");
    expect(tag.className).toMatch(/attention/);
    expect(tag.closest("span[title]")).toHaveAttribute(
      "title",
      expect.stringContaining("exited with code 5"),
    );
  });

  it("renders no attention tag when nothing needs attention", () => {
    render(<StatusPill phase="Connected" />);
    expect(screen.queryByText(/Blocked|Boot failed|Apply failed|Plugin roll failed/)).toBeNull();
  });

  it("handles undefined phase without crashing", () => {
    const { container } = render(<StatusPill phase={undefined as any} />);
    expect(container.firstChild).toBeInTheDocument();
  });
});
