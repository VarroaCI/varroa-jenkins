import { describe, it, expect, vi } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import VersionPicker from "./VersionPicker";
import type { VersionCatalogEntry } from "../types";

const versions: VersionCatalogEntry[] = [
  { version: "2.570", channel: "weekly", name: "2.570" },
  { version: "2.555", channel: "lts", recommended: true, name: "2.555" },
  { version: "2.541", channel: "lts", eol: "2026-04-15", name: "2.541" },
];

describe("VersionPicker", () => {
  it("renders an option per version with recommended and EOL metadata", () => {
    render(<VersionPicker versions={versions} value="2.555" onChange={() => {}} />);
    expect(screen.getByText("2.570")).toBeInTheDocument();
    expect(screen.getByText("2.555")).toBeInTheDocument();
    expect(screen.getByText("recommended")).toBeInTheDocument();
    expect(screen.getByText(/EOL 2026-04-15/)).toBeInTheDocument();
  });

  it("marks the selected value as pressed", () => {
    render(<VersionPicker versions={versions} value="2.555" onChange={() => {}} />);
    const selected = screen.getByText("2.555").closest("[role=button]")!;
    expect(selected.getAttribute("aria-pressed")).toBe("true");
  });

  it("calls onChange when an option is clicked", () => {
    const onChange = vi.fn();
    render(<VersionPicker versions={versions} value="2.555" onChange={onChange} />);
    fireEvent.click(screen.getByText("2.570"));
    expect(onChange).toHaveBeenCalledWith("2.570");
  });

  it("does not call onChange when disabled", () => {
    const onChange = vi.fn();
    render(<VersionPicker versions={versions} value="2.555" onChange={onChange} disabled />);
    fireEvent.click(screen.getByText("2.570"));
    expect(onChange).not.toHaveBeenCalled();
  });

  it("is keyboard-focusable and calls onChange on Enter/Space", () => {
    const onChange = vi.fn();
    render(<VersionPicker versions={versions} value="2.555" onChange={onChange} />);
    const option = screen.getByText("2.570").closest("[role=button]")!;
    expect(option.getAttribute("tabindex")).toBe("0");
    fireEvent.keyDown(option, { key: "Enter" });
    expect(onChange).toHaveBeenCalledWith("2.570");
    fireEvent.keyDown(option, { key: " " });
    expect(onChange).toHaveBeenCalledTimes(2);
  });

  it("is not tabbable and ignores keyboard activation when disabled", () => {
    const onChange = vi.fn();
    render(<VersionPicker versions={versions} value="2.555" onChange={onChange} disabled />);
    const option = screen.getByText("2.570").closest("[role=button]")!;
    expect(option.getAttribute("tabindex")).toBe("-1");
    fireEvent.keyDown(option, { key: "Enter" });
    expect(onChange).not.toHaveBeenCalled();
  });

  it("falls back to a free-text input when the catalog is empty", () => {
    const onChange = vi.fn();
    render(<VersionPicker versions={[]} value="2.462.1" onChange={onChange} />);
    const input = screen.getByLabelText("Jenkins version") as HTMLInputElement;
    expect(input.value).toBe("2.462.1");
    fireEvent.change(input, { target: { value: "2.463.1" } });
    expect(onChange).toHaveBeenCalledWith("2.463.1");
  });
});
