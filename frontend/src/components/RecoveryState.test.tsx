import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { RecordStoreUnreachablePage } from "./RecoveryState";

describe("RecordStoreUnreachablePage", () => {
  it("states that the record store cannot be reached", () => {
    render(
      <MemoryRouter>
        <RecordStoreUnreachablePage onRetry={() => {}} />
      </MemoryRouter>,
    );
    expect(screen.getByRole("heading", { name: "Pipeline data is unreachable" })).toBeInTheDocument();
    expect(
      screen.getByText("Varroa cannot reach the record store. Build history returns when the database is back."),
    ).toBeInTheDocument();
  });

  it("calls onRetry when Retry is pressed", async () => {
    const onRetry = vi.fn();
    render(
      <MemoryRouter>
        <RecordStoreUnreachablePage onRetry={onRetry} />
      </MemoryRouter>,
    );
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(onRetry).toHaveBeenCalledTimes(1);
  });
});
