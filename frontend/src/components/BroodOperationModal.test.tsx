import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithProviders } from "../test/render-utils";
import { BroodOperationModal } from "./BroodOperationModal";

const mockPreviewBroodOperation = vi.fn();
const mockCreateBroodOperation = vi.fn();
const mockListCatalogItems = vi.fn();
vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    previewBroodOperation: (...args: unknown[]) => mockPreviewBroodOperation(...args),
    createBroodOperation: (...args: unknown[]) => mockCreateBroodOperation(...args),
    listCatalogItems: (...args: unknown[]) => mockListCatalogItems(...args),
  };
});

function renderModal(overrides?: Partial<{ targets: string[]; onClose: () => void; onSubmitted: (r: any) => void; embedded: boolean }>) {
  const onClose = overrides?.onClose ?? vi.fn();
  const onSubmitted = overrides?.onSubmitted ?? vi.fn();
  const targets = overrides?.targets ?? ["core/default/ctrl-a", "core/default/ctrl-b"];
  const utils = renderWithProviders(
    <BroodOperationModal targets={targets} onClose={onClose} onSubmitted={onSubmitted} embedded={overrides?.embedded} />,
  );
  return { ...utils, onClose, onSubmitted };
}

describe("BroodOperationModal", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockListCatalogItems.mockResolvedValue({ items: [] });
  });

  it("previews with bare names and the shared namespace (team tenancy shape)", async () => {
    const user = userEvent.setup();
    mockPreviewBroodOperation.mockResolvedValue({
      clusters: [
        { cluster: "core", ok: true, targets: [
          { namespace: "default", name: "ctrl-a", wave: 0, applicable: true },
          { namespace: "default", name: "ctrl-b", wave: 1, applicable: false, reason: "not Connected" },
        ]},
      ],
    });
    renderModal();

    await user.click(screen.getByText("Preview"));

    expect(mockPreviewBroodOperation).toHaveBeenCalledTimes(1);
    const body = mockPreviewBroodOperation.mock.calls[0][0];
    expect(body.namespace).toBe("default");
    expect(body.spec.targets.names).toEqual(["ctrl-a", "ctrl-b"]);
    expect(body.clusters).toEqual(["core"]);
    expect(await screen.findByText("not Connected")).toBeInTheDocument();
    expect(screen.getByText("yes")).toBeInTheDocument();
    expect(screen.getByText("no")).toBeInTheDocument();
  });

  it("uses qualified names and no namespace for a cross-namespace selection", async () => {
    const user = userEvent.setup();
    mockPreviewBroodOperation.mockResolvedValue({ clusters: [{ cluster: "core", ok: true, targets: [] }] });
    renderModal({ targets: ["core/default/ctrl-a", "core/team-b/ctrl-b"] });

    await user.click(screen.getByText("Preview"));

    const body = mockPreviewBroodOperation.mock.calls[0][0];
    expect(body.namespace).toBeUndefined();
    expect(body.spec.targets.names).toEqual(
      expect.arrayContaining(["default/ctrl-a", "team-b/ctrl-b"]),
    );
    expect(body.clusters).toEqual(["core"]);
  });

  it("shows the preview error when the preview call fails", async () => {
    const user = userEvent.setup();
    mockPreviewBroodOperation.mockRejectedValue(new Error("preview boom"));
    renderModal();

    await user.click(screen.getByText("Preview"));
    expect(await screen.findByText("preview boom")).toBeInTheDocument();
  });

  it("clears a stale preview table when a later preview attempt fails", async () => {
    const user = userEvent.setup();
    mockPreviewBroodOperation.mockResolvedValueOnce({
      clusters: [{ cluster: "core", ok: true, targets: [
        { namespace: "default", name: "ctrl-a", wave: 0, applicable: true },
      ]}],
    });
    renderModal();

    await user.click(screen.getByText("Preview"));
    expect(await screen.findByText("ctrl-a")).toBeInTheDocument();

    mockPreviewBroodOperation.mockRejectedValueOnce(new Error("preview boom"));
    await user.click(screen.getByText("Preview"));

    expect(await screen.findByText("preview boom")).toBeInTheDocument();
    expect(screen.queryByText("ctrl-a")).not.toBeInTheDocument();
  });

  it("clears a shown preview when the caller changes the target selection", async () => {
    const user = userEvent.setup();
    mockPreviewBroodOperation.mockResolvedValueOnce({
      clusters: [{ cluster: "core", ok: true, targets: [
        { namespace: "default", name: "ctrl-a", wave: 0, applicable: true },
      ]}],
    });
    const { rerender } = renderModal({ targets: ["core/default/ctrl-a"] });

    await user.click(screen.getByText("Preview"));
    expect(await screen.findByText("ctrl-a")).toBeInTheDocument();

    rerender(
      <BroodOperationModal
        targets={["core/default/ctrl-a", "core/default/ctrl-c"]}
        onClose={vi.fn()}
        onSubmitted={vi.fn()}
      />,
    );

    expect(screen.queryByText("ctrl-a")).not.toBeInTheDocument();
  });

  it("un-sticks the Preview button when targets change while a preview is in-flight", async () => {
    const user = userEvent.setup();
    mockPreviewBroodOperation.mockReturnValueOnce(new Promise(() => {})); // never resolves
    const { rerender } = renderModal({ targets: ["core/default/ctrl-a"] });

    await user.click(screen.getByText("Preview"));
    expect(screen.getByText("Previewing…")).toBeDisabled();

    rerender(
      <BroodOperationModal
        targets={["core/default/ctrl-b"]}
        onClose={vi.fn()}
        onSubmitted={vi.fn()}
      />,
    );

    const button = await screen.findByText("Preview");
    expect(button).not.toBeDisabled();
  });

  it("ignores an in-flight preview response once the target selection has since changed", async () => {
    const user = userEvent.setup();
    let resolvePreview!: (v: unknown) => void;
    mockPreviewBroodOperation.mockReturnValueOnce(
      new Promise((resolve) => { resolvePreview = resolve; }),
    );
    const { rerender } = renderModal({ targets: ["core/default/ctrl-a"] });

    await user.click(screen.getByText("Preview"));

    rerender(
      <BroodOperationModal
        targets={["core/default/ctrl-b"]}
        onClose={vi.fn()}
        onSubmitted={vi.fn()}
      />,
    );

    resolvePreview({
      clusters: [{ cluster: "core", ok: true, targets: [
        { namespace: "default", name: "ctrl-a", wave: 0, applicable: true },
      ]}],
    });

    await new Promise((r) => setTimeout(r, 0));
    expect(screen.queryByText("ctrl-a")).not.toBeInTheDocument();
  });

  it("modal options change verb, order, and failure policy", async () => {
    const user = userEvent.setup();
    mockPreviewBroodOperation.mockResolvedValue({ clusters: [{ cluster: "core", ok: true, targets: [] }] });
    renderModal();

    await user.selectOptions(screen.getByLabelText(/Verb:/), "restart");
    await user.selectOptions(screen.getByLabelText(/Order:/), "name");
    await user.selectOptions(screen.getByLabelText(/Failure policy:/), "FailAtEnd");
    const maxParallel = screen.getByLabelText(/Max parallel:/);
    await user.tripleClick(maxParallel);
    await user.keyboard("3");

    await user.click(screen.getByText("Preview"));
    const body = mockPreviewBroodOperation.mock.calls[0][0];
    expect(body.spec.action).toEqual({ verb: "restart" });
    expect(body.spec.execution).toEqual({ maxParallel: 3, order: "name", failurePolicy: "FailAtEnd" });
  });

  it("lets max parallel be cleared and retyped instead of appending to the default (#428)", async () => {
    const user = userEvent.setup();
    mockPreviewBroodOperation.mockResolvedValue({ clusters: [{ cluster: "core", ok: true, targets: [] }] });
    renderModal();

    const maxParallel = screen.getByLabelText(/Max parallel:/) as HTMLInputElement;
    expect(maxParallel.value).toBe("1");

    await user.click(maxParallel);
    await user.keyboard("{Backspace}");
    expect(maxParallel.value).toBe("");
    await user.keyboard("5");
    expect(maxParallel.value).toBe("5");

    await user.click(screen.getByText("Preview"));
    const body = mockPreviewBroodOperation.mock.calls[0][0];
    expect(body.spec.execution).toEqual({ maxParallel: 5, order: "rolloutWave", failurePolicy: "FailTidy" });
  });

  it("clamps max parallel back to 1 on blur when left empty", async () => {
    const user = userEvent.setup();
    renderModal();

    const maxParallel = screen.getByLabelText(/Max parallel:/) as HTMLInputElement;
    await user.click(maxParallel);
    await user.keyboard("{Backspace}");
    expect(maxParallel.value).toBe("");

    await user.tab();
    expect(maxParallel.value).toBe("1");
  });

  it("creates the run and calls onSubmitted with the result", async () => {
    const user = userEvent.setup();
    mockCreateBroodOperation.mockResolvedValue({ name: "broodop-reconcile-x1", namespace: "default" });
    const { onSubmitted } = renderModal();

    await user.click(screen.getByText("Create & Run"));

    expect(mockCreateBroodOperation).toHaveBeenCalledTimes(1);
    const body = mockCreateBroodOperation.mock.calls[0][0];
    expect(body.namespace).toBe("default");
    expect(body.spec.targets.names).toEqual(["ctrl-a", "ctrl-b"]);
    expect(body.clusters).toEqual(["core"]);
    expect(onSubmitted).toHaveBeenCalledWith({ name: "broodop-reconcile-x1", namespace: "default" });
    expect(await screen.findByText("Create & Run")).toBeInTheDocument();
  });

  it("shows the create error and keeps the form visible on failure", async () => {
    const user = userEvent.setup();
    mockCreateBroodOperation.mockRejectedValue(new Error("create boom"));
    renderModal();

    await user.click(screen.getByText("Create & Run"));
    expect(await screen.findByText("create boom")).toBeInTheDocument();
    expect(screen.getByText("Preview")).toBeInTheDocument();
  });

  it("calls onClose via Cancel without creating", async () => {
    const user = userEvent.setup();
    const { onClose } = renderModal();

    await user.click(screen.getByText("Cancel"));
    expect(onClose).toHaveBeenCalledTimes(1);
    expect(mockCreateBroodOperation).not.toHaveBeenCalled();
  });

  it("does not call onSubmitted if the modal unmounts while create is in-flight", async () => {
    const user = userEvent.setup();
    let resolveCreate!: (v: unknown) => void;
    mockCreateBroodOperation.mockReturnValueOnce(
      new Promise((resolve) => { resolveCreate = resolve; }),
    );
    const { onSubmitted, unmount } = renderModal();

    await user.click(screen.getByText("Create & Run"));
    unmount();

    resolveCreate({ name: "broodop-x1", namespace: "default" });
    await new Promise((r) => setTimeout(r, 0));

    expect(onSubmitted).not.toHaveBeenCalled();
  });

  it("calls onClose when Escape is pressed in the non-embedded overlay", async () => {
    const user = userEvent.setup();
    const { onClose } = renderModal();

    await user.keyboard("{Escape}");

    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("does not react to Escape when embedded — the host dialog owns that behavior", async () => {
    const user = userEvent.setup();
    const { onClose } = renderModal({ embedded: true });

    await user.keyboard("{Escape}");

    expect(onClose).not.toHaveBeenCalled();
  });

  it("omits the dialog title and outer chrome when embedded", () => {
    renderModal({ embedded: true });
    expect(screen.queryByText("Run Brood Operation")).not.toBeInTheDocument();
    expect(screen.getByText("Preview")).toBeInTheDocument();
  });

  it("shows the dialog title when not embedded", () => {
    renderModal();
    expect(screen.getByText("Run Brood Operation")).toBeInTheDocument();
  });

  // ---- executeGroovy tests ----

  it("shows executeGroovy as a selectable verb option", async () => {
    renderModal();
    const select = screen.getByLabelText(/Verb:/);
    expect(select).toContainHTML("executeGroovy");
  });

  it("shows the script textarea by default when executeGroovy is selected", async () => {
    const user = userEvent.setup();
    renderModal();

    await user.selectOptions(screen.getByLabelText(/Verb:/), "executeGroovy");

    expect(screen.getByText(/Inline script/)).toBeInTheDocument();
    expect(screen.getByPlaceholderText(/println Jenkins.VERSION/)).toBeInTheDocument();
  });

  it("sends script in create body when typing a script", async () => {
    const user = userEvent.setup();
    mockCreateBroodOperation.mockResolvedValue({ name: "broodop-x1", namespace: "default" });
    renderModal();

    await user.selectOptions(screen.getByLabelText(/Verb:/), "executeGroovy");
    const textarea = screen.getByPlaceholderText(/println Jenkins.VERSION/);
    await user.type(textarea, "println 'hello'");
    await user.click(screen.getByText("Create & Run"));

    expect(mockCreateBroodOperation).toHaveBeenCalledTimes(1);
    const body = mockCreateBroodOperation.mock.calls[0][0];
    expect(body.spec.action.verb).toBe("executeGroovy");
    expect(body.spec.action.groovy.script).toBe("println 'hello'");
    expect(body.spec.action.groovy.itemRef).toBeUndefined();
  });

  it("sends script in preview body when previewing a script", async () => {
    const user = userEvent.setup();
    mockPreviewBroodOperation.mockResolvedValue({ clusters: [{ cluster: "core", ok: true, targets: [] }] });
    renderModal();

    await user.selectOptions(screen.getByLabelText(/Verb:/), "executeGroovy");
    const textarea = screen.getByPlaceholderText(/println Jenkins.VERSION/);
    await user.type(textarea, "println 'hello'");
    await user.click(screen.getByText("Preview"));

    expect(mockPreviewBroodOperation).toHaveBeenCalledTimes(1);
    const body = mockPreviewBroodOperation.mock.calls[0][0];
    expect(body.spec.action.verb).toBe("executeGroovy");
    expect(body.spec.action.groovy.script).toBe("println 'hello'");
  });

  it("sends itemRef (no script) when a catalog item is selected", async () => {
    const user = userEvent.setup();
    mockCreateBroodOperation.mockResolvedValue({ name: "broodop-x1", namespace: "default" });
    mockListCatalogItems.mockResolvedValue({
      items: [
        { name: "my-groovy", namespace: "default", type: "groovy", valid: true, displayName: "My Groovy" },
      ],
    });
    renderModal();

    await user.selectOptions(screen.getByLabelText(/Verb:/), "executeGroovy");
    // Switch to Catalog item mode
    await user.click(screen.getByText("Catalog item"));

    // Wait for the picker to appear (async from useCatalogItems)
    const select = await screen.findByLabelText("Catalog item");
    await user.selectOptions(select, "default/my-groovy");
    await user.click(screen.getByText("Create & Run"));

    expect(mockCreateBroodOperation).toHaveBeenCalledTimes(1);
    const body = mockCreateBroodOperation.mock.calls[0][0];
    expect(body.spec.action.verb).toBe("executeGroovy");
    expect(body.spec.action.groovy.script).toBeUndefined();
    expect(body.spec.action.groovy.itemRef).toEqual({ name: "my-groovy", namespace: "default" });
  });

  it("renders variable inputs and sends filled variables in create body", async () => {
    const user = userEvent.setup();
    mockCreateBroodOperation.mockResolvedValue({ name: "broodop-x1", namespace: "default" });
    mockListCatalogItems.mockResolvedValue({
      items: [
        {
          name: "my-groovy", namespace: "default", type: "groovy", valid: true,
          displayName: "My Groovy",
          variables: [{ name: "MY_VAR", type: "string", description: "A test var" }],
        },
      ],
    });
    renderModal();

    await user.selectOptions(screen.getByLabelText(/Verb:/), "executeGroovy");
    await user.click(screen.getByText("Catalog item"));

    const select = await screen.findByLabelText("Catalog item");
    await user.selectOptions(select, "default/my-groovy");

    // Should see the variable heading
    expect(screen.getByText("Variables")).toBeInTheDocument();
    // Fill the variable
    const input = screen.getByLabelText("MY_VAR");
    await user.type(input, "my-value");
    await user.click(screen.getByText("Create & Run"));

    const body = mockCreateBroodOperation.mock.calls[0][0];
    expect(body.spec.action.groovy.itemRef.variables).toEqual({ MY_VAR: "my-value" });
  });

  it("materializes declared default for a blank optional variable", async () => {
    const user = userEvent.setup();
    mockCreateBroodOperation.mockResolvedValue({ name: "broodop-x1", namespace: "default" });
    mockListCatalogItems.mockResolvedValue({
      items: [
        {
          name: "my-groovy", namespace: "default", type: "groovy", valid: true,
          displayName: "My Groovy",
          variables: [{ name: "MY_VAR", type: "string", default: "my-default" }],
        },
      ],
    });
    renderModal();

    await user.selectOptions(screen.getByLabelText(/Verb:/), "executeGroovy");
    await user.click(screen.getByText("Catalog item"));

    const select = await screen.findByLabelText("Catalog item");
    await user.selectOptions(select, "default/my-groovy");

    await user.click(screen.getByText("Create & Run"));

    const body = mockCreateBroodOperation.mock.calls[0][0];
    expect(body.spec.action.groovy.itemRef.variables).toEqual({ MY_VAR: "my-default" });
  });

  it("omits variables field when no user value and no default", async () => {
    const user = userEvent.setup();
    mockCreateBroodOperation.mockResolvedValue({ name: "broodop-x1", namespace: "default" });
    mockListCatalogItems.mockResolvedValue({
      items: [
        {
          name: "my-groovy", namespace: "default", type: "groovy", valid: true,
          displayName: "My Groovy",
          variables: [{ name: "MY_VAR", type: "string", required: false }],
        },
      ],
    });
    renderModal();

    await user.selectOptions(screen.getByLabelText(/Verb:/), "executeGroovy");
    await user.click(screen.getByText("Catalog item"));

    const select = await screen.findByLabelText("Catalog item");
    await user.selectOptions(select, "default/my-groovy");
    await user.click(screen.getByText("Create & Run"));

    const body = mockCreateBroodOperation.mock.calls[0][0];
    expect(body.spec.action.groovy.itemRef.variables).toBeUndefined();
  });

  it("disables Create when a required variable with no default is unfilled", async () => {
    const user = userEvent.setup();
    mockListCatalogItems.mockResolvedValue({
      items: [
        {
          name: "my-groovy", namespace: "default", type: "groovy", valid: true,
          displayName: "My Groovy",
          variables: [{ name: "REQUIRED_VAR", type: "string", required: true }],
        },
      ],
    });
    renderModal();

    await user.selectOptions(screen.getByLabelText(/Verb:/), "executeGroovy");
    await user.click(screen.getByText("Catalog item"));

    const select = await screen.findByLabelText("Catalog item");
    await user.selectOptions(select, "default/my-groovy");

    expect(screen.getByText("Create & Run")).toBeDisabled();
  });

  it("clears selection and vars when switching between script and catalog mode", async () => {
    const user = userEvent.setup();
    mockListCatalogItems.mockResolvedValue({
      items: [
        {
          name: "my-groovy", namespace: "default", type: "groovy", valid: true,
          displayName: "My Groovy",
          variables: [{ name: "V", type: "string" }],
        },
      ],
    });
    renderModal();

    await user.selectOptions(screen.getByLabelText(/Verb:/), "executeGroovy");
    await user.click(screen.getByText("Catalog item"));

    const select = await screen.findByLabelText("Catalog item");
    await user.selectOptions(select, "default/my-groovy");

    // Switch back to inline script
    await user.click(screen.getByText("Inline script"));

    // The textarea should be back
    expect(screen.getByPlaceholderText(/println Jenkins.VERSION/)).toBeInTheDocument();
  });

  it("handles boolean variable type: default true renders checked, unchecking submits false", async () => {
    const user = userEvent.setup();
    mockCreateBroodOperation.mockResolvedValue({ name: "broodop-x1", namespace: "default" });
    mockListCatalogItems.mockResolvedValue({
      items: [
        {
          name: "my-groovy", namespace: "default", type: "groovy", valid: true,
          displayName: "My Groovy",
          variables: [{ name: "ENABLED", type: "boolean", default: "true" }],
        },
      ],
    });
    renderModal();

    await user.selectOptions(screen.getByLabelText(/Verb:/), "executeGroovy");
    await user.click(screen.getByText("Catalog item"));

    const select = await screen.findByLabelText("Catalog item");
    await user.selectOptions(select, "default/my-groovy");

    // Checkbox should be checked (default true)
    const checkbox = screen.getByLabelText("ENABLED") as HTMLInputElement;
    expect(checkbox.checked).toBe(true);

    // Create without changing — should submit "true"
    await user.click(screen.getByText("Create & Run"));
    let body = mockCreateBroodOperation.mock.calls[0][0];
    expect(body.spec.action.groovy.itemRef.variables).toEqual({ ENABLED: "true" });

    // Now uncheck and create again
    mockCreateBroodOperation.mockClear();
    await user.click(checkbox);
    await user.click(screen.getByText("Create & Run"));
    body = mockCreateBroodOperation.mock.calls[0][0];
    expect(body.spec.action.groovy.itemRef.variables).toEqual({ ENABLED: "false" });
  });

  it("handles number variable type correctly", async () => {
    const user = userEvent.setup();
    mockCreateBroodOperation.mockResolvedValue({ name: "broodop-x1", namespace: "default" });
    mockListCatalogItems.mockResolvedValue({
      items: [
        {
          name: "my-groovy", namespace: "default", type: "groovy", valid: true,
          displayName: "My Groovy",
          variables: [{ name: "COUNT", type: "number" }],
        },
      ],
    });
    renderModal();

    await user.selectOptions(screen.getByLabelText(/Verb:/), "executeGroovy");
    await user.click(screen.getByText("Catalog item"));

    const select = await screen.findByLabelText("Catalog item");
    await user.selectOptions(select, "default/my-groovy");

    const input = screen.getByLabelText("COUNT") as HTMLInputElement;
    expect(input.type).toBe("number");
    await user.type(input, "42");
    await user.click(screen.getByText("Create & Run"));

    const body = mockCreateBroodOperation.mock.calls[0][0];
    expect(body.spec.action.groovy.itemRef.variables).toEqual({ COUNT: "42" });
  });

  it("preserves whitespace in text variable values (not trimmed)", async () => {
    const user = userEvent.setup();
    mockCreateBroodOperation.mockResolvedValue({ name: "broodop-x1", namespace: "default" });
    mockListCatalogItems.mockResolvedValue({
      items: [
        {
          name: "my-groovy", namespace: "default", type: "groovy", valid: true,
          displayName: "My Groovy",
          variables: [{ name: "MSG", type: "string" }],
        },
      ],
    });
    renderModal();

    await user.selectOptions(screen.getByLabelText(/Verb:/), "executeGroovy");
    await user.click(screen.getByText("Catalog item"));

    const select = await screen.findByLabelText("Catalog item");
    await user.selectOptions(select, "default/my-groovy");

    const input = screen.getByLabelText("MSG");
    await user.type(input, "  x ");
    await user.click(screen.getByText("Create & Run"));

    const body = mockCreateBroodOperation.mock.calls[0][0];
    expect(body.spec.action.groovy.itemRef.variables).toEqual({ MSG: "  x " });
  });

  it("deduplicates duplicate-named variables (first-wins)", async () => {
    const user = userEvent.setup();
    mockListCatalogItems.mockResolvedValue({
      items: [
        {
          name: "my-groovy", namespace: "default", type: "groovy", valid: true,
          displayName: "My Groovy",
          variables: [
            { name: "DUP", type: "string", default: "first" },
            { name: "DUP", type: "string", default: "second" },
          ],
        },
      ],
    });
    renderModal();

    await user.selectOptions(screen.getByLabelText(/Verb:/), "executeGroovy");
    await user.click(screen.getByText("Catalog item"));

    const select = await screen.findByLabelText("Catalog item");
    await user.selectOptions(select, "default/my-groovy");

    // Only one "DUP" label (first-wins dedup)
    const labels = screen.getAllByText("DUP");
    expect(labels.length).toBe(1);
  });

  it("editing a variable invalidates an in-flight preview race", async () => {
    const user = userEvent.setup();
    let resolvePreview!: (v: unknown) => void;
    mockPreviewBroodOperation.mockReturnValueOnce(
      new Promise((resolve) => { resolvePreview = resolve; }),
    );
    mockListCatalogItems.mockResolvedValue({
      items: [
        {
          name: "my-groovy", namespace: "default", type: "groovy", valid: true,
          displayName: "My Groovy",
          variables: [{ name: "V", type: "string" }],
        },
      ],
    });
    renderModal();

    await user.selectOptions(screen.getByLabelText(/Verb:/), "executeGroovy");
    await user.click(screen.getByText("Catalog item"));

    const select = await screen.findByLabelText("Catalog item");
    await user.selectOptions(select, "default/my-groovy");

    // Click Preview (starts in-flight request)
    await user.click(screen.getByText("Preview"));

    // While preview is in-flight, type in the variable
    const input = screen.getByLabelText("V");
    await user.type(input, "x");

    // Now resolve the preview
    resolvePreview({
      clusters: [{ cluster: "core", ok: true, targets: [
        { namespace: "default", name: "ctrl-a", wave: 0, applicable: true },
      ]}],
    });

    await new Promise((r) => setTimeout(r, 0));
    // Preview should NOT be displayed (invalidatePreview was called)
    expect(screen.queryByText("ctrl-a")).not.toBeInTheDocument();
  });

  it("filters catalog items client-side (only groovy+valid shown)", async () => {
    const user = userEvent.setup();
    mockListCatalogItems.mockResolvedValue({
      items: [
        { name: "g1", namespace: "ns", type: "groovy", valid: true },
        { name: "jcasc1", namespace: "ns", type: "jcasc", valid: true },
        { name: "g2", namespace: "ns", type: "groovy", valid: false },
        { name: "g3", namespace: "ns", type: "groovy", valid: true },
      ],
    });
    renderModal();

    await user.selectOptions(screen.getByLabelText(/Verb:/), "executeGroovy");
    await user.click(screen.getByText("Catalog item"));

    const select = await screen.findByLabelText("Catalog item");
    const optionValues = Array.from(select.querySelectorAll("option")).map(o => (o as HTMLOptionElement).value);
    // Only valid groovy items should appear as option values
    expect(optionValues).toContain("ns/g1");
    expect(optionValues).not.toContain("ns/jcasc1");
    expect(optionValues).not.toContain("ns/g2");
    expect(optionValues).toContain("ns/g3");
  });

  it("disables Create when script is empty", async () => {
    const user = userEvent.setup();
    renderModal();

    await user.selectOptions(screen.getByLabelText(/Verb:/), "executeGroovy");

    expect(screen.getByText("Create & Run")).toBeDisabled();
  });

  it("disables Create when catalog mode with no selection", async () => {
    const user = userEvent.setup();
    mockListCatalogItems.mockResolvedValue({
      items: [
        { name: "g1", namespace: "ns", type: "groovy", valid: true },
      ],
    });
    renderModal();

    await user.selectOptions(screen.getByLabelText(/Verb:/), "executeGroovy");
    await user.click(screen.getByText("Catalog item"));

    // No selection yet → Create disabled
    expect(screen.getByText("Create & Run")).toBeDisabled();
  });

  it("shows single-cluster message when targets span multiple clusters", async () => {
    const user = userEvent.setup();
    renderModal({ targets: ["core/ns/a", "edge/ns/b"] });

    await user.selectOptions(screen.getByLabelText(/Verb:/), "executeGroovy");
    await user.click(screen.getByText("Catalog item"));

    expect(await screen.findByText(/single target cluster/)).toBeInTheDocument();
    expect(screen.getByText("Create & Run")).toBeDisabled();
  });

  it("clears shown preview after editing the script", async () => {
    const user = userEvent.setup();
    mockPreviewBroodOperation.mockResolvedValueOnce({
      clusters: [{ cluster: "core", ok: true, targets: [
        { namespace: "default", name: "ctrl-a", wave: 0, applicable: true },
      ]}],
    });
    renderModal();

    await user.selectOptions(screen.getByLabelText(/Verb:/), "executeGroovy");
    const textarea = screen.getByPlaceholderText(/println Jenkins.VERSION/);
    await user.type(textarea, "println 'hello'");
    await user.click(screen.getByText("Preview"));

    expect(await screen.findByText("ctrl-a")).toBeInTheDocument();

    // Edit the script → preview should clear
    await user.type(textarea, " more");
    expect(screen.queryByText("ctrl-a")).not.toBeInTheDocument();
  });
});
