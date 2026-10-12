import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClientProvider } from "@tanstack/react-query";
import { createTestQueryClient } from "../../test/render-utils";
import { ApiError } from "../../hooks/useApi";
import { RestartDrawer } from "./RestartDrawer";

const mockRestartRun = vi.fn();
vi.mock("../../api/client", () => ({
  restartRun: (...args: unknown[]) => mockRestartRun(...args),
}));

const STAGES = ["build", "deploy"];

function renderDrawer() {
  const onClose = vi.fn();
  const onSubmitted = vi.fn();
  const result = render(
    <QueryClientProvider client={createTestQueryClient()}>
      <RestartDrawer runId="run-1" stages={STAGES} onClose={onClose} onSubmitted={onSubmitted} />
    </QueryClientProvider>,
  );
  return { ...result, onClose, onSubmitted };
}

describe("RestartDrawer", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockRestartRun.mockResolvedValue({ runId: "run-2", intentKey: "restart:run-1:run-2", state: "requested" });
  });

  it("lists every restartable stage and selects the first", () => {
    renderDrawer();
    const radios = screen.getAllByRole("radio") as HTMLInputElement[];
    expect(radios.map((r) => r.value)).toEqual(STAGES);
    expect(radios[0].checked).toBe(true);
  });

  it("restarts from the chosen stage and reports the new run", async () => {
    const { onSubmitted } = renderDrawer();
    fireEvent.click(screen.getByLabelText("deploy"));
    fireEvent.click(screen.getByRole("button", { name: "Restart" }));

    await waitFor(() => expect(onSubmitted).toHaveBeenCalledWith("run-2"));
    expect(mockRestartRun).toHaveBeenCalledWith("run-1", "deploy");
  });

  it("shows the server's refusal message", async () => {
    mockRestartRun.mockRejectedValue(
      new ApiError(409, "conflict", { error: "Conflict: stage deploy is not restartable" }),
    );
    renderDrawer();
    fireEvent.click(screen.getByRole("button", { name: "Restart" }));

    expect(await screen.findByText("Conflict: stage deploy is not restartable")).toBeTruthy();
  });

  it("closes from the close control", () => {
    const { onClose } = renderDrawer();
    fireEvent.click(screen.getByRole("button", { name: "Close" }));
    expect(onClose).toHaveBeenCalled();
  });
});
