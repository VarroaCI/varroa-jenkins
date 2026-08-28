import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import React from "react";
import ErrorBoundary from "./ErrorBoundary";

/** A child component that throws an error during render. */
const ThrowError: React.FC<{ message?: string }> = ({ message = "Test error" }) => {
  throw new Error(message);
};

/** A child component that renders normally. */
function GoodChild() {
  return <div>All good</div>;
}

describe("ErrorBoundary", () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("renders children normally when no error", () => {
    render(
      <ErrorBoundary>
        <GoodChild />
      </ErrorBoundary>,
    );
    expect(screen.getByText("All good")).toBeInTheDocument();
  });

  it("catches error thrown by child and renders fallback UI", () => {
    vi.spyOn(console, "error").mockImplementation(() => {});

    render(
      <ErrorBoundary>
        <ThrowError message="Something broke" />
      </ErrorBoundary>,
    );

    expect(screen.getByText("Something went wrong")).toBeInTheDocument();
    expect(screen.getByText("Something broke")).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: /try again/i }),
    ).toBeInTheDocument();
  });

  it("'Try again' button resets error state and re-renders children", async () => {
    vi.spyOn(console, "error").mockImplementation(() => {});

    const { rerender } = render(
      <ErrorBoundary>
        <ThrowError message="Simulated error" />
      </ErrorBoundary>,
    );
    expect(screen.getByText("Something went wrong")).toBeInTheDocument();

    // Step 1: Fix the error condition by replacing the throwing child
    // with a non-throwing child (the cause of the error is resolved).
    rerender(
      <ErrorBoundary>
        <GoodChild />
      </ErrorBoundary>,
    );
    // ErrorBoundary still shows fallback because state.error is still set
    expect(screen.getByText("Something went wrong")).toBeInTheDocument();

    // Step 2: Click "Try again" to clear the error state
    await userEvent.click(screen.getByRole("button", { name: /try again/i }));

    // Now the boundary should render the fixed children
    expect(screen.getByText("All good")).toBeInTheDocument();
    expect(screen.queryByText("Something went wrong")).not.toBeInTheDocument();
  });

  it("does not render fallback when no error has occurred", () => {
    render(
      <ErrorBoundary>
        <GoodChild />
      </ErrorBoundary>,
    );
    expect(
      screen.queryByText("Something went wrong"),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: /try again/i }),
    ).not.toBeInTheDocument();
  });
});
