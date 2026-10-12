import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { RevisionLink } from "./RevisionLink";

const SHA = "abcdef1234567890";

afterEach(() => {
  vi.restoreAllMocks();
});

describe("RevisionLink", () => {
  it("links a GitHub repository to the commit", () => {
    render(<RevisionLink revision={SHA} repoUrl="https://github.com/example/app" length={8} />);
    const link = screen.getByRole("link", { name: "abcdef12" });
    expect(link).toHaveAttribute("href", `https://github.com/example/app/commit/${SHA}`);
    expect(link).toHaveAttribute("target", "_blank");
  });

  it("shows a copy button for a repository on another host", () => {
    render(<RevisionLink revision={SHA} repoUrl="https://git.example.org/x/y" length={7} />);
    expect(screen.queryByRole("link")).toBeNull();
    expect(screen.getByRole("button", { name: `Copy commit ${SHA}` })).toBeInTheDocument();
  });

  it("shows a copy button when no repository is known", () => {
    render(<RevisionLink revision={SHA} length={7} />);
    expect(screen.getByRole("button", { name: `Copy commit ${SHA}` })).toBeInTheDocument();
  });

  it("copies the full commit id from the copy button", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    render(<RevisionLink revision={SHA} length={7} />);
    fireEvent.click(screen.getByRole("button", { name: `Copy commit ${SHA}` }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(SHA));
    expect(await screen.findByText("Copied")).toBeInTheDocument();
  });
});
