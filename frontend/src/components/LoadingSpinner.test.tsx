import { describe, it, expect } from "vitest";
import { render } from "@testing-library/react";
import LoadingSpinner from "./LoadingSpinner";

describe("LoadingSpinner", () => {
  it("renders without crashing", () => {
    const { container } = render(<LoadingSpinner />);
    expect(container.firstChild).toBeInTheDocument();
  });

  it("shows loading indicator element", () => {
    const { container } = render(<LoadingSpinner />);
    const outerDiv = container.firstChild as HTMLElement;
    expect(outerDiv).toBeTruthy();
    // The spinner component renders two nested divs (spinner > spin)
    expect(outerDiv.firstChild).toBeInTheDocument();
  });
});
