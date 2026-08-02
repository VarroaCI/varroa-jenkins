import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ToastProvider } from "../Toast";
import fixture from "../../api/__fixtures__/openapi.json";

// CodeMirror requires browser APIs not available in jsdom — mock it exactly
// like YamlTierEditor.test.tsx does.
vi.mock("@codemirror/state", () => ({ EditorState: { create: vi.fn(() => ({})), Compartment: vi.fn() } }));
vi.mock("@codemirror/view", () => ({
  EditorView: Object.assign(
    vi.fn(() => ({ destroy: vi.fn(), dispatch: vi.fn(), state: { doc: { toString: () => "" } } })),
    {
      updateListener: { of: vi.fn(() => []) },
      theme: vi.fn(() => []),
      baseTheme: vi.fn(() => []),
    },
  ),
  keymap: Object.assign(vi.fn(() => []), { of: vi.fn(() => []) }),
  lineNumbers: vi.fn(() => []),
}));
vi.mock("@codemirror/lang-yaml", () => ({ yaml: vi.fn(() => []), yamlLanguage: {} }));
vi.mock("@codemirror/lint", () => ({ linter: vi.fn(() => vi.fn()), Diagnostic: {} }));
vi.mock("@codemirror/commands", () => ({ defaultKeymap: [] }));

// useOpenAPISchema drives both SpecEditorCard's own schema lookups (for
// ingressSpec/miteSpec YAML validation) and the inner ControllerSpecForm's
// Tier-1 schema — return the fixture synchronously for both.
vi.mock("../../api/openapiSchema", async () => {
  const actual = await vi.importActual<typeof import("../../api/openapiSchema")>("../../api/openapiSchema");
  return {
    useOpenAPISchema: () => ({ data: fixture, isLoading: false, error: null }),
    getControllerSpecSchema: actual.getControllerSpecSchema,
    getPodOverridesSchema: actual.getPodOverridesSchema,
    getIngressSpecSchema: actual.getIngressSpecSchema,
    getMiteSpecSchema: actual.getMiteSpecSchema,
  };
});

// Real YAML parsing everywhere, except for a sentinel token that lets a test
// simulate an unparseable tier without typing into the CodeMirror mock.
vi.mock("yaml", async (importOriginal) => {
  const actual = await importOriginal<typeof import("yaml")>();
  return {
    ...actual,
    stringify: actual.stringify,
    parse: (text: string, ...rest: unknown[]) => {
      if (typeof text === "string" && text.includes("__INVALID__")) {
        throw new Error("bad indentation");
      }
      return (actual.parse as (...a: unknown[]) => unknown)(text, ...rest);
    },
  };
});

const mockUpdateController = vi.fn();
const mockPreviewControllerOverlay = vi.fn();
vi.mock("../../api/client", async () => {
  const actual = await vi.importActual<typeof import("../../api/client")>("../../api/client");
  return {
    updateController: (...args: unknown[]) => mockUpdateController(...args),
    previewControllerOverlay: (...args: unknown[]) => mockPreviewControllerOverlay(...args),
    ControllerConflictError: actual.ControllerConflictError,
  };
});

import SpecEditorCard from "./SpecEditorCard";

function renderCard(props: Partial<React.ComponentProps<typeof SpecEditorCard>> = {}) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <ToastProvider>
        <SpecEditorCard cluster="core" ns="team-a" name="ci" canUpdate={true} {...props} />
      </ToastProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  mockUpdateController.mockReset();
  mockUpdateController.mockResolvedValue({});
  mockPreviewControllerOverlay.mockReset();
  mockPreviewControllerOverlay.mockResolvedValue({ merged: {}, diff: {}, warnings: [], baselineUsed: "live" });
});

describe("SpecEditorCard tabs", () => {
  it("renders an Ingress tab and a Mite sidecar tab alongside the existing tiers", () => {
    renderCard();
    expect(screen.getByText("Form")).toBeInTheDocument();
    expect(screen.getByText("Pod overrides")).toBeInTheDocument();
    expect(screen.getByText("Resource overlay")).toBeInTheDocument();
    expect(screen.getByText("Ingress")).toBeInTheDocument();
    expect(screen.getByText("Mite sidecar")).toBeInTheDocument();
  });

  it("shows the ingressSpec YAML tier when the Ingress tab is selected", () => {
    renderCard();
    fireEvent.click(screen.getByText("Ingress"));
    expect(screen.getByText("Ingress (YAML)")).toBeInTheDocument();
  });

  it("shows the miteSpec YAML tier when the Mite sidecar tab is selected", () => {
    renderCard();
    fireEvent.click(screen.getByText("Mite sidecar"));
    expect(screen.getByText("Mite sidecar (YAML)")).toBeInTheDocument();
  });

  it("pre-populates the ingressSpec tier from initialIngressSpec as YAML", () => {
    renderCard({ initialIngressSpec: { host: "ci.example.com", mode: "existing" } });
    fireEvent.click(screen.getByText("Ingress"));
    expect(screen.getByText("Ingress (YAML)")).toBeInTheDocument();
    // The editor itself is CodeMirror-mocked, so we only assert the tier
    // rendered without crashing when initial data is supplied.
  });
});

describe("SpecEditorCard save", () => {
  it("includes parsed ingressSpec and miteSpec in the patch sent to updateController", async () => {
    renderCard({
      initialIngressSpec: { host: "old.example.com" },
      initialMiteSpec: { image: "old-image:1" },
    });

    fireEvent.click(screen.getByText("Save spec"));

    await waitFor(() => expect(mockUpdateController).toHaveBeenCalledTimes(1));
    const [, , , patch] = mockUpdateController.mock.calls[0];
    // Both fields were untouched (initial values preserved via the YAML
    // textarea state seeded from initial*), so the patch should carry
    // through the parsed initial values rather than dropping them.
    expect(patch.spec).toHaveProperty("ingressSpec");
    expect(patch.spec).toHaveProperty("miteSpec");
    expect(patch.spec.ingressSpec).toEqual({ host: "old.example.com" });
    expect(patch.spec.miteSpec).toEqual({ image: "old-image:1" });
  });

  it("aborts the save and surfaces an error when a YAML tier does not parse", async () => {
    renderCard({ initialIngressSpec: { host: "__INVALID__" } });

    fireEvent.click(screen.getByText("Save spec"));

    // The whole save is abandoned — previously the unparseable tier was
    // silently dropped from the patch and the user still saw "Spec saved".
    await waitFor(() =>
      expect(screen.getByText(/Ingress YAML is invalid/)).toBeInTheDocument(),
    );
    expect(mockUpdateController).not.toHaveBeenCalled();
    expect(screen.queryByText("Spec saved")).not.toBeInTheDocument();
  });

  it("omits ingressSpec/miteSpec from the patch when neither was ever set", async () => {
    renderCard();

    fireEvent.click(screen.getByText("Save spec"));
    await waitFor(() => expect(mockUpdateController).toHaveBeenCalledTimes(1));
    const [, , , patch] = mockUpdateController.mock.calls[0];
    // Neither field had an initial value nor was edited — both stay absent
    // from the patch rather than being sent as null/empty.
    expect(patch.spec).not.toHaveProperty("ingressSpec");
    expect(patch.spec).not.toHaveProperty("miteSpec");
  });
});
