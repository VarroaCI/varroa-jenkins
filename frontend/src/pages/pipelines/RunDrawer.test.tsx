import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, fireEvent, within } from "@testing-library/react";
import { QueryClientProvider } from "@tanstack/react-query";
import { createTestQueryClient } from "../../test/render-utils";
import type { PipelineParameter, PipelineParameterKind } from "../../types";
import { RunDrawer } from "./RunDrawer";

const mockCreateRun = vi.fn();
vi.mock("../../api/client", () => ({
  createRun: (...args: unknown[]) => mockCreateRun(...args),
}));

function renderDrawer(parameters: PipelineParameter[], onSubmitted = vi.fn()) {
  const queryClient = createTestQueryClient();
  const onClose = vi.fn();
  const result = render(
    <QueryClientProvider client={queryClient}>
      <RunDrawer tenant="acme" jobPath="payments/checkout-api/main" parameters={parameters} onClose={onClose} onSubmitted={onSubmitted} />
    </QueryClientProvider>,
  );
  return { ...result, onClose, onSubmitted };
}

const choiceParam: PipelineParameter = {
  name: "Target",
  kind: "choice",
  schema: { type: "string", enum: ["dev", "staging", "prod"] },
};

const booleanParam: PipelineParameter = {
  name: "DryRun",
  kind: "boolean",
  schema: { type: "boolean" },
};

const unknownParam: PipelineParameter = {
  name: "Mystery",
  kind: "unrecognized" as PipelineParameterKind,
  schema: {},
};

describe("RunDrawer", () => {
  beforeEach(() => {
    mockCreateRun.mockReset();
    mockCreateRun.mockResolvedValue({ runId: "run-1", intentKey: "manual:run-1", state: "requested" });
  });

  it("renders a choice parameter as a selection holding its declared values", () => {
    renderDrawer([choiceParam]);
    const select = screen.getByLabelText("Target") as HTMLSelectElement;
    const options = within(select).getAllByRole("option").map((o) => o.textContent);
    expect(options).toEqual(expect.arrayContaining(["dev", "staging", "prod"]));
  });

  it("renders a boolean parameter as a checkbox", () => {
    renderDrawer([booleanParam]);
    // The repository's checkbox widget renders no associated <label> text
    // (see components/form/CheckboxField.tsx), so this asserts on the
    // control's own type/role rather than a label association.
    const checkbox = screen.getByRole("checkbox") as HTMLInputElement;
    expect(checkbox.type).toBe("checkbox");
  });

  it("renders a parameter of an unrecognized type as a plain, named string field", () => {
    renderDrawer([unknownParam]);
    const field = screen.getByLabelText("Mystery") as HTMLInputElement;
    expect(field.type).toBe("text");
  });

  it("carries the same client token across two submits", async () => {
    renderDrawer([]);
    const runButton = screen.getByRole("button", { name: "Run" });

    fireEvent.click(runButton);
    await waitFor(() => expect(mockCreateRun).toHaveBeenCalledTimes(1));

    fireEvent.click(runButton);
    await waitFor(() => expect(mockCreateRun).toHaveBeenCalledTimes(2));

    const firstToken = mockCreateRun.mock.calls[0][2].clientToken;
    const secondToken = mockCreateRun.mock.calls[1][2].clientToken;
    expect(firstToken).toBeTruthy();
    expect(firstToken).toBe(secondToken);
  });

  it("disables the control while the request is in flight", async () => {
    let resolveCreate: (v: { runId: string; intentKey: string; state: "requested" }) => void = () => {};
    mockCreateRun.mockReturnValue(new Promise((resolve) => { resolveCreate = resolve; }));
    renderDrawer([]);
    const runButton = screen.getByRole("button", { name: "Run" });

    fireEvent.click(runButton);
    await waitFor(() => expect(screen.getByRole("button", { name: "Starting…" })).toBeDisabled());

    resolveCreate({ runId: "run-1", intentKey: "manual:run-1", state: "requested" });
    await waitFor(() => expect(screen.getByRole("button", { name: "Run" })).toBeEnabled());
  });

  it("calls onSubmitted with the new run id once the request is accepted", async () => {
    const onSubmitted = vi.fn();
    renderDrawer([], onSubmitted);
    fireEvent.click(screen.getByRole("button", { name: "Run" }));
    await waitFor(() => expect(onSubmitted).toHaveBeenCalledWith("run-1"));
  });
});
