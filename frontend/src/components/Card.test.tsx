import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { Card } from "./Card";

describe("Card", () => {
  it("renders title text", () => {
    render(<Card title="Test Title">Content</Card>);
    expect(screen.getByText("Test Title")).toBeInTheDocument();
  });

  it("renders children content", () => {
    render(
      <Card>
        <p>Child content</p>
      </Card>,
    );
    expect(screen.getByText("Child content")).toBeInTheDocument();
  });

  it("renders without a title", () => {
    const { container } = render(<Card>Content</Card>);
    expect(container.firstChild).toBeInTheDocument();
    expect(screen.getByText("Content")).toBeInTheDocument();
  });

  it("renders headerRight element when provided", () => {
    render(
      <Card title="Title" headerRight={<button>Action</button>}>
        Content
      </Card>,
    );
    expect(screen.getByRole("button", { name: /action/i })).toBeInTheDocument();
  });
});
