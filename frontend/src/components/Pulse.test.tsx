import { describe, it, expect } from "vitest";
import { render } from "@testing-library/react";
import { Pulse } from "./Pulse";

describe("Pulse", () => {
  it("renders with active class when active=true", () => {
    const { container } = render(<Pulse active={true} />);
    const span = container.firstChild as HTMLElement;
    expect(span).toBeInTheDocument();
  });

  it("renders without active class when active=false", () => {
    const { container } = render(<Pulse active={false} />);
    const span = container.firstChild as HTMLElement;
    expect(span).toBeInTheDocument();
  });

  it("has different classNames for active vs inactive", () => {
    const { container, rerender } = render(<Pulse active={true} />);
    const activeClassName = (container.firstChild as HTMLElement).className;

    rerender(<Pulse active={false} />);
    const inactiveClassName = (container.firstChild as HTMLElement).className;

    expect(activeClassName).not.toBe(inactiveClassName);
  });

  it("applies custom size when provided", () => {
    const { container } = render(<Pulse active={true} size={20} />);
    const span = container.firstChild as HTMLElement;
    expect(span.style.width).toBe("20px");
    expect(span.style.height).toBe("20px");
  });

  it("renders with default size when size prop is omitted", () => {
    const { container } = render(<Pulse active={true} />);
    const span = container.firstChild as HTMLElement;
    expect(span.style.width).toBe("11px");
    expect(span.style.height).toBe("11px");
  });
});
