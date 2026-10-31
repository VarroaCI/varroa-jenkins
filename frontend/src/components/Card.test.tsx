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

  it("adds a className to the card root", () => {
    const { container } = render(<Card className="extra">Content</Card>);
    const root = container.firstElementChild as HTMLElement;
    expect(root.className).toContain("extra");
    expect(root.className).toMatch(/card/);
  });

  it("renders only the card class without a className", () => {
    const { container } = render(<Card>Content</Card>);
    const root = container.firstElementChild as HTMLElement;
    expect(root.className).toMatch(/card/);
    expect(root.className).not.toContain("undefined");
  });
});
