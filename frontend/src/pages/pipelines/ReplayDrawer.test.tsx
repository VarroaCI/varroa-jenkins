import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClientProvider } from "@tanstack/react-query";
import { EditorState } from "@codemirror/state";
import { createTestQueryClient } from "../../test/render-utils";
import { ApiError } from "../../hooks/useApi";
import { ReplayDrawer } from "./ReplayDrawer";

const mockReplayRun = vi.fn();
vi.mock("../../api/client", () => ({
  replayRun: (...args: unknown[]) => mockReplayRun(...args),
}));

// jsdom cannot run a real EditorView. The mock view answers the text the
// test sets in editorText, and records destroy.
let editorText = "";
const mockDestroy = vi.fn();
vi.mock("@codemirror/state", () => ({ EditorState: { create: vi.fn(() => ({})) } }));
vi.mock("@codemirror/view", () => ({
  EditorView: Object.assign(
    class MockEditorView {
      state = { doc: { toString: () => editorText } };
      destroy = mockDestroy;
      dispatch = vi.fn();
    },
    { theme: vi.fn(() => []), updateListener: { of: vi.fn(() => []) } },
  ),
  keymap: Object.assign(vi.fn(() => []), { of: vi.fn(() => []) }),
  lineNumbers: vi.fn(() => []),
}));
vi.mock("@codemirror/commands", () => ({ defaultKeymap: [] }));

const SCRIPT = "pipeline {\n  agent any\n}\n";

function renderDrawer(loadedScripts: Record<string, string> = {}) {
  const onClose = vi.fn();
  const onSubmitted = vi.fn();
  const result = render(
    <QueryClientProvider client={createTestQueryClient()}>
      <ReplayDrawer
        runId="run-1"
        script={SCRIPT}
        loadedScripts={loadedScripts}
        onClose={onClose}
        onSubmitted={onSubmitted}
      />
    </QueryClientProvider>,
  );
  return { ...result, onClose, onSubmitted };
}

describe("ReplayDrawer", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    editorText = SCRIPT;
    mockReplayRun.mockResolvedValue({ runId: "run-2", intentKey: "replay:run-1:run-2", state: "requested" });
  });

  it("opens the editor with the run's own script", () => {
    renderDrawer();
    expect(vi.mocked(EditorState.create)).toHaveBeenCalledWith(expect.objectContaining({ doc: SCRIPT }));
    expect(screen.getByRole("dialog", { name: "Replay this build" })).toBeInTheDocument();
  });

  it("shows each loaded library script read only, below the editor", () => {
    const { container } = renderDrawer({
      "vars/deploy.groovy": "def call() { echo 'deploy' }",
      "src/org/acme/Util.groovy": "class Util {}",
    });

    expect(screen.getByText("vars/deploy.groovy")).toBeInTheDocument();
    expect(screen.getByText("src/org/acme/Util.groovy")).toBeInTheDocument();
    const bodies = Array.from(container.querySelectorAll("details pre")).map((el) => el.textContent);
    expect(bodies).toEqual(["def call() { echo 'deploy' }", "class Util {}"]);
    // A library body is plain text, never an editable control.
    expect(container.querySelectorAll("details textarea, details input, details [contenteditable]")).toHaveLength(0);
    const editor = screen.getByTestId("replay-editor");
    expect(editor.compareDocumentPosition(container.querySelector("details")!) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("shows no library section for an empty map", () => {
    const { container } = renderDrawer({});
    expect(container.querySelector("details")).toBeNull();
    expect(screen.queryByText(/Library scripts are read only/)).toBeNull();
  });

  it("submits the current editor text and reports the new run id", async () => {
    editorText = "pipeline { agent none }";
    const { onSubmitted } = renderDrawer();

    fireEvent.click(screen.getByRole("button", { name: "Replay" }));

    await waitFor(() => expect(onSubmitted).toHaveBeenCalledWith("run-2"));
    expect(mockReplayRun).toHaveBeenCalledWith("run-1", "pipeline { agent none }");
  });

  it("disables the submit control while the request is open", async () => {
    let resolve: (v: unknown) => void = () => {};
    mockReplayRun.mockReturnValue(new Promise((r) => (resolve = r)));
    renderDrawer();

    fireEvent.click(screen.getByRole("button", { name: "Replay" }));

    const pending = await screen.findByRole("button", { name: "Starting…" });
    expect(pending).toBeDisabled();
    resolve({ runId: "run-2", intentKey: "k", state: "requested" });
    await waitFor(() => expect(screen.getByRole("button", { name: "Replay" })).not.toBeDisabled());
  });

  it("shows the server's message for a refused replay", async () => {
    mockReplayRun.mockRejectedValue(new ApiError(409, "conflict", { error: "The run used a masked parameter." }));
    const { onSubmitted } = renderDrawer();

    fireEvent.click(screen.getByRole("button", { name: "Replay" }));

    expect(await screen.findByText("The run used a masked parameter.")).toBeInTheDocument();
    expect(onSubmitted).not.toHaveBeenCalled();
  });

  it("falls back to the status copy when the server gives no message", async () => {
    mockReplayRun.mockRejectedValue(new ApiError(413, "The script is too large."));
    renderDrawer();

    fireEvent.click(screen.getByRole("button", { name: "Replay" }));

    expect(await screen.findByText("The script is too large.")).toBeInTheDocument();
  });

  it("shows a generic message for an error that is not an API error", async () => {
    mockReplayRun.mockRejectedValue(new Error("network"));
    renderDrawer();

    fireEvent.click(screen.getByRole("button", { name: "Replay" }));

    expect(await screen.findByText("Varroa could not start this build.")).toBeInTheDocument();
  });

  it("closes from the close control and from the overlay, and destroys the editor on unmount", () => {
    const { onClose, container, unmount } = renderDrawer();

    fireEvent.click(screen.getByRole("button", { name: "Close" }));
    fireEvent.click(screen.getByRole("dialog"));
    expect(onClose).toHaveBeenCalledTimes(1);
    fireEvent.click(container.firstElementChild!);
    expect(onClose).toHaveBeenCalledTimes(2);

    unmount();
    expect(mockDestroy).toHaveBeenCalled();
  });
});
