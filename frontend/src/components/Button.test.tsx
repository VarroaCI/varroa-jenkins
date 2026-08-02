import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Button } from "./Button";

describe("Button", () => {
  it("renders button with label text", () => {
    render(<Button>Click me</Button>);
    expect(screen.getByRole("button", { name: /click me/i })).toBeInTheDocument();
  });

  it("calls onClick when clicked", async () => {
    const onClick = vi.fn();
    render(<Button onClick={onClick}>Click</Button>);
    await userEvent.click(screen.getByRole("button"));
    expect(onClick).toHaveBeenCalledTimes(1);
  });

  it("applies disabled attribute when disabled prop is true", () => {
    render(<Button disabled>Disabled</Button>);
    expect(screen.getByRole("button")).toBeDisabled();
  });

  it("renders different variant classes based on variant prop", () => {
    const { rerender } = render(<Button variant="default">Default</Button>);
    const defaultClass = screen.getByRole("button").className;

    rerender(<Button variant="primary">Primary</Button>);
    const primaryClass = screen.getByRole("button").className;
    expect(primaryClass).not.toBe(defaultClass);

    rerender(<Button variant="ghost">Ghost</Button>);
    const ghostClass = screen.getByRole("button").className;
    expect(ghostClass).not.toBe(defaultClass);
    expect(ghostClass).not.toBe(primaryClass);
  });

  it("does not call onClick when disabled", async () => {
    const onClick = vi.fn();
    render(
      <Button disabled onClick={onClick}>
        Click
      </Button>,
    );
    await userEvent.click(screen.getByRole("button"));
    expect(onClick).not.toHaveBeenCalled();
  });
});
