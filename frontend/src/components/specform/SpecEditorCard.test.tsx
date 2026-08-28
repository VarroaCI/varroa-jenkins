import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor, act, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ToastProvider } from "../Toast";
import fixture from "../../api/__fixtures__/openapi.json";

// CodeMirror requires browser APIs not available in jsdom — mock it exactly
// like YamlTierEditor.test.tsx does, but capture the doc-change listener so a
// test can simulate typing into a YAML tier (the real EditorView is replaced).
// EditorState.create also records the doc it was handed, so a test can assert
// what a freshly-mounted tier was seeded with (e.g. after a save rebase).
let yamlChangeListener: ((u: {
  docChanged: boolean;
  state: { doc: { toString(): string } };
}) => void) | null = null;
let mountedYamlDoc = "";
vi.mock("@codemirror/state", () => ({
  EditorState: {
    create: vi.fn((cfg: { doc?: string }) => {
      mountedYamlDoc = cfg?.doc ?? "";
      return {};
    }),
    Compartment: vi.fn(),
  },
}));
vi.mock("@codemirror/view", () => ({
  EditorView: Object.assign(
    class MockEditorView {
      constructor() {
        return { destroy: vi.fn(), dispatch: vi.fn(), state: { doc: { toString: () => "" } } };
      }
    },
    {
      updateListener: {
        of: (fn: (u: {
          docChanged: boolean;
          state: { doc: { toString(): string } };
        }) => void) => {
          yamlChangeListener = fn;
          return [];
        },
      },
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
import { ControllerConflictError } from "../../api/client";
import type { ControllerSpec } from "../../types";

type CardProps = React.ComponentProps<typeof SpecEditorCard>;

// Render the card with a query client + toast provider, and expose a `rerender`
// that swaps props while keeping the same component instance — this is how a
// test simulates a background refetch (a new `spec` object) or an identity
// change.
function renderCard(props: Partial<CardProps> = {}) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const base: CardProps = { cluster: "core", ns: "team-a", name: "ci", canUpdate: true };
  const view = render(
    <QueryClientProvider client={qc}>
      <ToastProvider>
        <SpecEditorCard {...base} {...props} />
      </ToastProvider>
    </QueryClientProvider>,
  );
  return {
    ...view,
    qc,
    rerender: (next: Partial<CardProps>) =>
      view.rerender(
        <QueryClientProvider client={qc}>
          <ToastProvider>
            <SpecEditorCard {...base} {...next} />
          </ToastProvider>
        </QueryClientProvider>,
      ),
  };
}

// Simulate typing into whichever YAML tier is currently mounted (CodeMirror is
// mocked, so this drives its captured doc-change listener straight to the
// tier's onChange).
function typeYaml(text: string) {
  act(() => {
    yamlChangeListener?.({ docChanged: true, state: { doc: { toString: () => text } } });
  });
}

// The doc a freshly-mounted YAML tier was seeded with (via EditorState.create)
// — used to assert what a tier's text is after a rebase remounts it.
function yamlMountedDoc(): string {
  return mountedYamlDoc;
}

function classNameInput(): HTMLInputElement {
  // className's field label is the curated ui:title "Controller class".
  return screen.getByLabelText(/controller class/i) as HTMLInputElement;
}

// Create a `resources.limits.<key>` row via the map editor's Add control and
// rename it to `key` (the key input commits on blur). Used by both the gate
// tests and the section-7 patch tests.
async function addMapRow(key: string): Promise<void> {
  const limitsField = screen.getByText("Limits").closest(".rjsf-field") as HTMLElement;
  fireEvent.click(within(limitsField).getByRole("button", { name: "+ Add" }));
  await waitFor(() =>
    expect(document.getElementById("root_resources_limits_newKey-key")).not.toBeNull(),
  );
  const newKey = document.getElementById("root_resources_limits_newKey-key") as HTMLInputElement;
  fireEvent.change(newKey, { target: { value: key } });
  fireEvent.blur(newKey);
  await waitFor(() =>
    expect(document.getElementById(`root_resources_limits_${key}`)).not.toBeNull(),
  );
}

async function setMapValue(key: string, value: string): Promise<void> {
  const input = document.getElementById(`root_resources_limits_${key}`) as HTMLInputElement;
  fireEvent.change(input, { target: { value } });
}

beforeEach(() => {
  yamlChangeListener = null;
  mountedYamlDoc = "";
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

describe("SpecEditorCard hydration", () => {
  it("renders the controller's stored curated values (hydration actually happened)", async () => {
    renderCard({
      spec: {
        className: "my-class",
        persistence: { size: "20Gi" },
        backupSpec: { enabled: true, retentionDays: 7, schedule: "0 1 * * *" },
      },
    });
    await waitFor(() => expect(classNameInput().value).toBe("my-class"));
    expect(classNameInput().value).toBe("my-class");
  });

  it("hydrates the YAML tiers from spec so an untouched save sends nothing for them", async () => {
    renderCard({
      spec: {
        className: "a",
        podOverrides: { jvmOpts: "-Xmx1g" },
        ingressSpec: { host: "ci.example.com" },
        miteSpec: { image: "mite:1.2" },
        resourceOverlay: { statefulSet: "spec:\n  replicas: 2\n" },
      },
    });
    await waitFor(() => expect(classNameInput().value).toBe("a"));
    // No tier was edited → the whole editor is pristine.
    fireEvent.click(screen.getByText("Save spec"));
    await waitFor(() => expect(mockUpdateController).not.toHaveBeenCalled());
  });

  it("renders hydrated map rows that arrive after mount via a props update (Part 1 regression)", async () => {
    // The card ALWAYS mounts the form with {} and hydrates it in a useEffect,
    // so the map rows arrive via a later props update, not at mount. RJSF's
    // ObjectField seeds its additional-property row order once at mount and
    // never re-syncs it; without the formHydrationKey remount these rows would
    // be invisible — the feature did not work. This is the probe: value at
    // mount renders, value arriving after mount must too.
    renderCard({
      spec: {
        className: "a",
        resources: { limits: { cpu: "500m", memory: "1Gi" } },
      },
    });
    await waitFor(() => expect(classNameInput().value).toBe("a"));

    // Key inputs for the hydrated rows are present and carry the keys…
    const cpuKey = document.getElementById(
      "root_resources_limits_cpu-key",
    ) as HTMLInputElement | null;
    const memoryKey = document.getElementById(
      "root_resources_limits_memory-key",
    ) as HTMLInputElement | null;
    expect(cpuKey).not.toBeNull();
    expect(memoryKey).not.toBeNull();
    expect(cpuKey?.value).toBe("cpu");
    expect(memoryKey?.value).toBe("memory");
    // …and the value widgets render the stored quantities.
    expect(screen.getByDisplayValue("500m")).toBeInTheDocument();
    expect(screen.getByDisplayValue("1Gi")).toBeInTheDocument();
  });

  it("does not remount the curated form on a keystroke (focus retained while typing)", async () => {
    // The remount key must change ONLY on hydration. Keying the form on the
    // draft itself (or any keystroke-derived value) would remount on every
    // change and drop focus while the user types.
    renderCard({ spec: { className: "a", resources: { limits: { cpu: "500m" } } } });
    await waitFor(() => expect(classNameInput().value).toBe("a"));
    const firstNode = classNameInput();
    fireEvent.change(firstNode, { target: { value: "ab" } });
    // The very same DOM node is still mounted — the keystroke did not remount
    // the form (only hydration bumps the key).
    expect(classNameInput()).toBe(firstNode);
  });

  it("does not remount the curated form when a refetch leaves its hydrated value identical", async () => {
    // A focused-but-untyped field is still a PRISTINE tier, so a background
    // refetch re-hydrates it. When the hydrated value is deep-equal to the
    // current draft (only a YAML tier moved), a remount would drop focus for
    // no reason — the remount key must NOT bump on an identical value.
    const { rerender } = renderCard({
      spec: {
        className: "a",
        resources: { limits: { cpu: "500m" } },
        podOverrides: { jvmOpts: "-Xmx1g" },
      },
    });
    await waitFor(() => expect(classNameInput().value).toBe("a"));
    const firstNode = classNameInput();

    // Refetch: only podOverrides (a YAML tier) changed; the curated value is
    // identical. rerender flushes the hydration effect before returning.
    rerender({
      spec: {
        className: "a",
        resources: { limits: { cpu: "500m" } },
        podOverrides: { jvmOpts: "-Xmx2g" },
      },
    });

    // The same DOM node survived the re-hydration — no remount on an identical
    // draft (the 7.2 hydrated rows are still rendered).
    expect(classNameInput()).toBe(firstNode);
    expect(screen.getByDisplayValue("500m")).toBeInTheDocument();
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

  it("issues no request when the save has no differences (8.4)", async () => {
    renderCard();
    fireEvent.click(screen.getByText("Save spec"));
    // Nothing was hydrated and nothing was edited: a pristine editor must not
    // PATCH at all — previously an empty {spec:{}} patch was sent.
    await waitFor(() => expect(mockUpdateController).not.toHaveBeenCalled());
  });

  it("emits an explicit null for a cleared field (8.2)", async () => {
    renderCard({ spec: { className: "a" } });
    await waitFor(() => expect(classNameInput().value).toBe("a"));

    fireEvent.change(classNameInput(), { target: { value: "" } });
    fireEvent.click(screen.getByText("Save spec"));

    await waitFor(() => expect(mockUpdateController).toHaveBeenCalledTimes(1));
    const [, , , patch] = mockUpdateController.mock.calls[0];
    expect(patch.spec).toEqual({ className: null });
  });

  it("leaves an untouched hydrated field (resources) out of the patch", async () => {
    renderCard({
      spec: { className: "a", resources: { limits: { cpu: "100m" }, requests: { memory: "1Gi" } } },
    });
    await waitFor(() => expect(classNameInput().value).toBe("a"));

    fireEvent.change(classNameInput(), { target: { value: "b" } });
    fireEvent.click(screen.getByText("Save spec"));

    await waitFor(() => expect(mockUpdateController).toHaveBeenCalledTimes(1));
    const [, , , patch] = mockUpdateController.mock.calls[0];
    // Only the edited field is sent — resources now renders editable rows via
    // the map editor but is untouched, so it is neither re-sent nor nulled.
    expect(patch.spec).toEqual({ className: "b" });
    expect(patch.spec).not.toHaveProperty("resources");
  });

  it("preserves an existing endpoint value when an unrelated Tier-1 field is edited (5.3)", async () => {
    // endpoint has zero Go readers/writers and the frontend ControllerSpec type
    // has dropped it, but a Controller created before the field went dead may
    // still carry it in the CRD — simulate that legacy value arriving on the
    // projected spec (hence the cast).
    const spec = {
      className: "a",
      endpoint: "https://ci.example.com/jenkins",
    } as ControllerSpec;
    renderCard({ spec });
    await waitFor(() => expect(classNameInput().value).toBe("a"));

    fireEvent.change(classNameInput(), { target: { value: "b" } });
    fireEvent.click(screen.getByText("Save spec"));

    await waitFor(() => expect(mockUpdateController).toHaveBeenCalledTimes(1));
    const [, , , patch] = mockUpdateController.mock.calls[0];
    // endpoint is excluded (EXCLUDED_FROM_TIER1), so stripExcluded keeps it
    // out of both the hydration snapshot and the curated draft — it never
    // reaches diffValues and is never emitted as a spurious removal. The
    // outgoing PATCH must contain no endpoint key, so the existing value is
    // preserved rather than deleted.
    expect(patch.spec).toEqual({ className: "b" });
    expect(patch.spec).not.toHaveProperty("endpoint");
  });

  it("surfaces unappliedRemovals as a non-blocking notice naming the field(s)", async () => {
    // A blocked removal: the save succeeded (200) but another manager still
    // owns spec.version, so the server reports it in unappliedRemovals.
    mockUpdateController.mockResolvedValue({
      unappliedRemovals: [{ field: "spec.version" }],
    });
    renderCard({ spec: { className: "a" } });
    await waitFor(() => expect(classNameInput().value).toBe("a"));
    fireEvent.change(classNameInput(), { target: { value: "b" } });

    fireEvent.click(screen.getByText("Save spec"));
    await waitFor(() =>
      expect(screen.getByText(/spec\.version could not be removed/)).toBeInTheDocument(),
    );
    // The unqualified success message is replaced, not shown alongside it.
    expect(screen.queryByText("Spec saved")).not.toBeInTheDocument();
    expect(mockUpdateController).toHaveBeenCalledTimes(1);
  });

  it("shows the unqualified success when no removal was blocked", async () => {
    mockUpdateController.mockResolvedValue({});
    renderCard({ spec: { className: "a" } });
    await waitFor(() => expect(classNameInput().value).toBe("a"));
    fireEvent.change(classNameInput(), { target: { value: "b" } });

    fireEvent.click(screen.getByText("Save spec"));
    await waitFor(() => expect(screen.getByText(/Spec saved/)).toBeInTheDocument());
    expect(screen.queryByText(/could not be removed/)).not.toBeInTheDocument();
    expect(mockUpdateController).toHaveBeenCalledTimes(1);
  });

  it("rebases the baseline from the returned controller after a successful save", async () => {
    mockUpdateController.mockResolvedValue({ spec: { className: "b" } });
    renderCard({ spec: { className: "a" } });
    await waitFor(() => expect(classNameInput().value).toBe("a"));
    fireEvent.change(classNameInput(), { target: { value: "b" } });

    fireEvent.click(screen.getByText("Save spec"));
    await waitFor(() => expect(screen.getByText(/Spec saved/)).toBeInTheDocument());

    // The baseline was rebased to the response (className: "b"), so the draft
    // "b" now equals it. A second save with no further edits must issue NO
    // request — had the baseline stayed "a", it would diff and send again.
    fireEvent.click(screen.getByText("Save spec"));
    await waitFor(() => expect(mockUpdateController).toHaveBeenCalledTimes(1));
  });
});

describe("SpecEditorCard YAML formatting preservation", () => {
  // After a successful save the tier is rebased from the response. Re-stringifying
  // the server's JSON would silently drop the user's comments/quoting, so the
  // rebase must keep the existing text when it parses to the same value. The
  // editor is CodeMirror-mocked and never re-syncs its view, so to read the
  // rebased text we switch away from the tier and back — that remounts the
  // editor, which seeds itself from the current tier text (EditorState.create).
  it("keeps the user's comments when the response value matches the edit", async () => {
    mockUpdateController.mockResolvedValue({
      spec: { podOverrides: { replicas: 4 } },
    });
    renderCard({ initialPodOverrides: { replicas: 3 } });
    await waitFor(() => expect(classNameInput().value).toBe(""));

    fireEvent.click(screen.getByText("Pod overrides"));
    typeYaml("# keep me\nreplicas: 4");
    fireEvent.click(screen.getByText("Save spec"));
    await waitFor(() => expect(mockUpdateController).toHaveBeenCalledTimes(1));

    // Re-mount the tier to read its current text after the rebase.
    fireEvent.click(screen.getByText("Form"));
    fireEvent.click(screen.getByText("Pod overrides"));
    expect(yamlMountedDoc()).toContain("# keep me");
    expect(yamlMountedDoc()).toContain("replicas: 4");
  });

  it("replaces the text when the response value differs from the edit", async () => {
    mockUpdateController.mockResolvedValue({
      spec: { podOverrides: { replicas: 5 } },
    });
    renderCard({ initialPodOverrides: { replicas: 3 } });
    await waitFor(() => expect(classNameInput().value).toBe(""));

    fireEvent.click(screen.getByText("Pod overrides"));
    typeYaml("# keep me\nreplicas: 4");
    fireEvent.click(screen.getByText("Save spec"));
    await waitFor(() => expect(mockUpdateController).toHaveBeenCalledTimes(1));

    fireEvent.click(screen.getByText("Form"));
    fireEvent.click(screen.getByText("Pod overrides"));
    // The server's value actually differs, so the tier converges to it —
    // the comment and the stale value are gone.
    expect(yamlMountedDoc()).not.toContain("# keep me");
    expect(yamlMountedDoc().trim()).toBe("replicas: 5");
  });
});

describe("SpecEditorCard refetch during editing", () => {
  it("preserves a dirty draft on a background refetch and updates the baseline (8.3)", async () => {
    const { rerender } = renderCard({
      spec: {
        className: "a",
        podOverrides: { jvmOpts: "-Xmx1g" },
        ingressSpec: { host: "old.example.com" },
      },
    });
    await waitFor(() => expect(classNameInput().value).toBe("a"));

    // Dirty the podOverrides YAML tier.
    fireEvent.click(screen.getByText("Pod overrides"));
    typeYaml("jvmOpts: -Xmx999m");

    // Background refetch: new spec object, same controller identity. The dirty
    // podOverrides draft must survive; the pristine ingressSpec draft must be
    // rehydrated to the new baseline.
    rerender({
      spec: {
        className: "b",
        podOverrides: { jvmOpts: "-Xmx2g" },
        ingressSpec: { host: "new.example.com" },
      },
    });

    fireEvent.click(screen.getByText("Save spec"));
    await waitFor(() => expect(mockUpdateController).toHaveBeenCalledTimes(1));
    const [, , , patch] = mockUpdateController.mock.calls[0];
    // The dirty draft is sent as the user typed it, not the refetched value.
    expect(patch.spec.podOverrides).toEqual({ jvmOpts: "-Xmx999m" });
    // The pristine tier was rehydrated to the refetched baseline, so it diffs
    // clean and is NOT sent as a spurious revert to "old".
    expect(patch.spec).not.toHaveProperty("ingressSpec");
  });

  it("an edit made while a save is in flight survives the rebase (8.5)", async () => {
    let resolveSave: (v: { spec: Record<string, unknown> }) => void = () => {};
    mockUpdateController.mockImplementation(
      () => new Promise<{ spec: Record<string, unknown> }>((res) => { resolveSave = res; }),
    );
    renderCard({ spec: { className: "a" } });
    await waitFor(() => expect(classNameInput().value).toBe("a"));

    fireEvent.change(classNameInput(), { target: { value: "b" } });
    fireEvent.click(screen.getByText("Save spec"));
    await waitFor(() => expect(mockUpdateController).toHaveBeenCalledTimes(1));

    // Edit the same tier while the save is in flight.
    fireEvent.change(classNameInput(), { target: { value: "c" } });

    // The server applied "b" and returns it as the new spec.
    await act(async () => {
      resolveSave({ spec: { className: "b" } });
    });
    await waitFor(() => expect(screen.getByText(/Spec saved/)).toBeInTheDocument());

    // The mid-flight edit survives the rebase.
    expect(classNameInput().value).toBe("c");
  });

  it("diffs a dirty tier against its own hydration snapshot, not the live baseline (finding 2)", async () => {
    const { rerender } = renderCard({
      spec: {
        className: "a",
        backupSpec: { enabled: true, retentionDays: 7 },
      },
    });
    await waitFor(() => expect(classNameInput().value).toBe("a"));

    // The user edits className in the curated (form) tier.
    fireEvent.change(classNameInput(), { target: { value: "b" } });

    // Background refetch: the operator changes a DIFFERENT field in the same
    // curated tier (backupSpec.retentionDays). The live baseline advances, but
    // the dirty form tier must keep the snapshot it was hydrated from — a save
    // diffing against the live baseline would see the stale retentionDays=7 as
    // a local edit and re-send it, silently reverting the operator's change.
    rerender({
      spec: {
        className: "a",
        backupSpec: { enabled: true, retentionDays: 30 },
      },
    });

    fireEvent.click(screen.getByText("Save spec"));
    await waitFor(() => expect(mockUpdateController).toHaveBeenCalledTimes(1));
    const [, , , patch] = mockUpdateController.mock.calls[0];
    // ONLY the user's edit is sent; the server-changed field is never
    // mentioned (and never reverted to the stale value).
    expect(patch.spec).toEqual({ className: "b" });
    expect(patch.spec).not.toHaveProperty("backupSpec");
  });

  it("never re-emits a field another writer added to a dirty tier as a removal (finding 3)", async () => {
    // The controller originally has NO podOverrides — the podOverrides tier
    // hydrates with a legitimately-undefined snapshot.
    const { rerender } = renderCard({ spec: { className: "a" } });
    await waitFor(() => expect(classNameInput().value).toBe("a"));

    // Dirty the podOverrides tier. Its snapshot is `undefined`, but it IS
    // recorded — a save must not read "recorded as undefined" as "never
    // hydrated".
    fireEvent.click(screen.getByText("Pod overrides"));
    typeYaml("jvmOpts: -Xmx999m");

    // Background refetch: another writer has ADDED podOverrides with a field
    // the draft does not have. The dirty tier's draft is preserved, so its
    // snapshot must stay "recorded as undefined" rather than falling back to
    // the live baseline's NEW podOverrides.
    rerender({
      spec: {
        className: "a",
        podOverrides: { jvmOpts: "-Xmx2g", resources: { requests: { memory: "2Gi" } } },
      },
    });

    fireEvent.click(screen.getByText("Save spec"));
    await waitFor(() => expect(mockUpdateController).toHaveBeenCalledTimes(1));
    const [, , , patch] = mockUpdateController.mock.calls[0];
    // Exactly the user's edit — the other writer's `resources` field is NOT
    // re-emitted as an explicit null removal.
    expect(patch.spec.podOverrides).toEqual({ jvmOpts: "-Xmx999m" });
    expect(patch.spec.podOverrides).not.toHaveProperty("resources");
  });

  it("a non-conflict save failure leaves baseline and drafts unchanged (8.8)", async () => {
    mockUpdateController.mockRejectedValue(new Error("boom"));
    renderCard({ spec: { className: "a" } });
    await waitFor(() => expect(classNameInput().value).toBe("a"));
    fireEvent.change(classNameInput(), { target: { value: "b" } });

    fireEvent.click(screen.getByText("Save spec"));
    await waitFor(() => expect(screen.getByText(/Failed: boom/)).toBeInTheDocument());

    // Draft kept the edit; baseline unchanged — a save with no further edits
    // would still send { className: "b" } (it was never rebased).
    expect(classNameInput().value).toBe("b");
  });

  it("preserves an in-flight key rename across a refetch that changes another curated field (finding 2)", async () => {
    // Key text lives only in MapEntry local state until blur, so a typed-but-
    // unblurred key never changes specValue and never bumps the form draft
    // version — the card thinks the tier is pristine. A refetch that changes
    // any OTHER curated field therefore makes hydrateTier see a different
    // value, bump formHydrationKey, remount the form, and drop the typed key
    // AND focus. An ordinary value widget's edits ARE lifted immediately, so
    // the existing focus test cannot catch this.
    const { rerender } = renderCard({
      spec: { className: "a", resources: { limits: { cpu: "500m" } } },
    });
    await waitFor(() => expect(classNameInput().value).toBe("a"));

    // Start renaming cpu → "mem" WITHOUT blurring (the rename commits on blur).
    const cpuKey = document.getElementById(
      "root_resources_limits_cpu-key",
    ) as HTMLInputElement;
    fireEvent.change(cpuKey, { target: { value: "mem" } });
    cpuKey.focus();
    expect(cpuKey.value).toBe("mem");

    // Background refetch: the operator changes className, a different curated
    // field. A remount here would reset the key input to the committed "cpu"
    // and steal focus.
    rerender({
      spec: { className: "b", resources: { limits: { cpu: "500m" } } },
    });

    // The SAME key input survives — no remount — and still shows the typed key.
    const after = document.getElementById(
      "root_resources_limits_cpu-key",
    ) as HTMLInputElement;
    expect(after).toBe(cpuKey);
    expect(after.value).toBe("mem");
    expect(after).toHaveFocus();
  });
});

describe("SpecEditorCard conflict", () => {
  it("Reload resets the baseline and every tier from freshly fetched data (8.6)", async () => {
    mockUpdateController.mockRejectedValue(
      new ControllerConflictError([{ field: "spec.className", message: "owned elsewhere" }]),
    );
    const { rerender } = renderCard({
      spec: {
        className: "a",
        podOverrides: { jvmOpts: "-Xmx1g" },
        ingressSpec: { host: "old.example.com" },
      },
    });
    await waitFor(() => expect(classNameInput().value).toBe("a"));

    // Dirty two tiers, then force the conflict.
    fireEvent.change(classNameInput(), { target: { value: "b" } });
    fireEvent.click(screen.getByText("Pod overrides"));
    typeYaml("jvmOpts: -Xmx999m");
    fireEvent.click(screen.getByText("Save spec"));
    await waitFor(() => expect(screen.getByText("Reload latest")).toBeInTheDocument());

    // Reload then refetch with fresh data.
    fireEvent.click(screen.getByText("Reload latest"));
    rerender({
      spec: {
        className: "z",
        podOverrides: { jvmOpts: "-Xmx9g" },
        ingressSpec: { host: "new.example.com" },
      },
    });
    fireEvent.click(screen.getByText("Form"));
    await waitFor(() => expect(classNameInput().value).toBe("z"));

    // Every tier was reset to the fresh baseline: a save with no edits issues
    // no request (the conflict attempt was the only call so far).
    fireEvent.click(screen.getByText("Save spec"));
    await waitFor(() => expect(mockUpdateController).toHaveBeenCalledTimes(1));
  });

  it("Reload resets drafts from the query cache even when refetch data is identical", async () => {
    // The structural-sharing case: a refetch whose data equals what we already
    // have reuses the same `spec` reference, so the hydration effect never
    // re-runs. Reload must therefore hydrate from the query cache directly.
    mockUpdateController.mockRejectedValue(
      new ControllerConflictError([{ field: "spec.className", message: "owned elsewhere" }]),
    );
    const { qc } = renderCard({
      spec: { className: "a", podOverrides: { jvmOpts: "-Xmx1g" } },
    });
    await waitFor(() => expect(classNameInput().value).toBe("a"));
    fireEvent.change(classNameInput(), { target: { value: "b" } });
    fireEvent.click(screen.getByText("Save spec"));
    await waitFor(() => expect(screen.getByText("Reload latest")).toBeInTheDocument());

    qc.setQueryData(["controller", "core", "team-a", "ci"], {
      spec: { className: "z", podOverrides: { jvmOpts: "-Xmx9g" } },
    });
    fireEvent.click(screen.getByText("Reload latest"));
    // No re-render with a new spec object — the cache hydration alone resets.
    await waitFor(() => expect(classNameInput().value).toBe("z"));

    // Every tier is pristine against the fresh baseline: a save issues no
    // request (the conflict attempt was the only call).
    fireEvent.click(screen.getByText("Save spec"));
    await waitFor(() => expect(mockUpdateController).toHaveBeenCalledTimes(1));
  });

  it("Override resends the CAPTURED patch with force, not current state (8.7)", async () => {
    mockUpdateController
      .mockRejectedValueOnce(new ControllerConflictError([{ field: "spec.className", message: "owned elsewhere" }]))
      .mockResolvedValueOnce({
        spec: { className: "b", podOverrides: { jvmOpts: "-Xmx999m" } },
      });
    renderCard({
      spec: { className: "a", podOverrides: { jvmOpts: "-Xmx1g" } },
    });
    await waitFor(() => expect(classNameInput().value).toBe("a"));

    fireEvent.change(classNameInput(), { target: { value: "b" } });
    fireEvent.click(screen.getByText("Pod overrides"));
    typeYaml("jvmOpts: -Xmx999m");
    fireEvent.click(screen.getByText("Save spec"));
    await waitFor(() => expect(screen.getByText("Override anyway")).toBeInTheDocument());

    // The user edits again while the conflict dialog is open — Override must
    // ignore this and resend the captured patch.
    fireEvent.click(screen.getByText("Form"));
    fireEvent.change(classNameInput(), { target: { value: "c" } });
    fireEvent.click(screen.getByText("Override anyway"));

    await waitFor(() => expect(mockUpdateController).toHaveBeenCalledTimes(2));
    const [, , , overridePatch, opts] = mockUpdateController.mock.calls[1];
    // The captured configuration, not the current draft ("c").
    expect(overridePatch).toEqual({
      spec: { className: "b", podOverrides: { jvmOpts: "-Xmx999m" } },
    });
    expect(opts).toEqual({ force: true });
  });
});

describe("SpecEditorCard validation gate", () => {
  // The curated tier's live validation (§6.1) gates Save by CONTAINMENT against
  // the curated patch (§6.2): an invalid field blocks only when its path is at
  // or below a path actually being sent. An error whose path does not resolve
  // to a real form value blocks unconditionally — fail closed (§6.3).
  //
  // RJSF 6.7.1 seeds additional-property row order once at mount, so a form
  // must mount with its real data for hydrated map rows to render. The card
  // handles that by remounting the form on hydration (formHydrationKey), so a
  // hydrated `resources.limits.cpu` DOES render here. The gate tests below
  // still drive a concrete map path through the Add control + key rename —
  // the resulting formData and error path are identical to an edit on a
  // hydrated row, so the gate is proven the same way.

  it("6.3a-1: type 12qq as resources.limits.cpu, Save is not called and a field-level error is visible", async () => {
    renderCard({ spec: { className: "a" } });
    await waitFor(() => expect(classNameInput().value).toBe("a"));

    await addMapRow("cpu");
    await setMapValue("cpu", "12qq");

    // A field-level error renders under the cpu input (RJSF's own plumbing).
    await waitFor(() =>
      expect(document.getElementById("root_resources_limits_cpu__error")).not.toBeNull(),
    );
    // The Save button is disabled and the adjacent message names the path (§6.4).
    expect(screen.getByText("Save spec")).toBeDisabled();
    expect(
      screen.getByText(/Cannot save: invalid value at resources\.limits\.cpu/),
    ).toBeInTheDocument();

    fireEvent.click(screen.getByText("Save spec"));
    expect(mockUpdateController).not.toHaveBeenCalled();
  });

  it('6.3a-2: add a map entry and Save without editing it — not called (the newKey/"New Value" guard)', async () => {
    renderCard({ spec: { className: "a" } });
    await waitFor(() => expect(classNameInput().value).toBe("a"));

    const limitsField = screen.getByText("Limits").closest(".rjsf-field") as HTMLElement;
    fireEvent.click(within(limitsField).getByRole("button", { name: "+ Add" }));

    // RJSF's add default (`newKey` / "New Value") is invalid against the
    // quantity pattern by construction — an unedited entry must not save.
    await waitFor(() => expect(screen.getByText("Save spec")).toBeDisabled());
    expect(
      screen.getByText(/Cannot save: invalid value at resources\.limits\.newKey/),
    ).toBeInTheDocument();
    fireEvent.click(screen.getByText("Save spec"));
    expect(mockUpdateController).not.toHaveBeenCalled();
  });

  it("6.3a-3: untouched invalid resources.limits.cpu + className-only edit — Save IS called (field-level, not tier-level)", async () => {
    renderCard({ spec: { className: "a", resources: { limits: { cpu: "12qq" } } } });
    await waitFor(() => expect(classNameInput().value).toBe("a"));

    fireEvent.change(classNameInput(), { target: { value: "b" } });
    fireEvent.click(screen.getByText("Save spec"));

    // The className edit surfaces the full-form live validation, so the
    // pre-existing invalid cpu error IS known to the gate — yet it is NOT in
    // the curated patch (only className is), so it must not block. A tier-level
    // dirty-and-invalid rule would wrongly block here; this distinguishes the
    // field-level rule from a tier-level one.
    await waitFor(() => expect(mockUpdateController).toHaveBeenCalledTimes(1));
    const [, , , patch] = mockUpdateController.mock.calls[0];
    expect(patch.spec).toEqual({ className: "b" });
  });

  it("6.3a-4: invalid value under the dotted key nvidia.com/gpu — Save is not called (does-not-resolve)", async () => {
    // The ControllerSpec ResourceRequirements type only names cpu/memory, so a
    // domain-qualified extended-resource key needs a cast to reach the form.
    const spec = {
      className: "a",
      resources: { limits: { "nvidia.com/gpu": "12qq" } },
    } as unknown as ControllerSpec;
    renderCard({ spec });
    await waitFor(() => expect(classNameInput().value).toBe("a"));

    // Editing an unrelated field surfaces the full-form live validation, which
    // reports the invalid value at the mangled path .resources.limits.nvidia.com~1gpu.
    // That path never resolves against the real key nvidia.com/gpu, so the
    // does-not-resolve rule blocks even though the invalid value sits outside
    // the curated patch.
    fireEvent.change(classNameInput(), { target: { value: "b" } });

    await waitFor(() => expect(screen.getByText("Save spec")).toBeDisabled());
    expect(
      screen.getByText(/Cannot save: invalid value at resources\.limits\.nvidia\.com~1gpu/),
    ).toBeInTheDocument();
    fireEvent.click(screen.getByText("Save spec"));
    expect(mockUpdateController).not.toHaveBeenCalled();
  });

  it("6.2: whole-array replacement invalid at a child path blocks (containment, not equality)", async () => {
    renderCard({
      spec: { className: "a", rbacSpec: { groups: [{ name: "dev", role: "admin" }] } },
    });
    await waitFor(() => expect(classNameInput().value).toBe("a"));

    // diffValues emits a changed array wholesale, so the patch carries the path
    // rbacSpec.groups while the error sits at rbacSpec.groups.0.role — an
    // equality test would miss it; containment must not.
    const role = document.getElementById("root_rbacSpec_groups_0_role") as HTMLInputElement;
    fireEvent.change(role, { target: { value: "bogus" } });

    await waitFor(() => expect(screen.getByText("Save spec")).toBeDisabled());
    expect(
      screen.getByText(/Cannot save: invalid value at rbacSpec\.groups\.0\.role/),
    ).toBeInTheDocument();
    fireEvent.click(screen.getByText("Save spec"));
    expect(mockUpdateController).not.toHaveBeenCalled();
  });

  it("fixing the invalid field re-enables Save", async () => {
    renderCard({ spec: { className: "a" } });
    await waitFor(() => expect(classNameInput().value).toBe("a"));

    await addMapRow("cpu");
    await setMapValue("cpu", "12qq");
    await waitFor(() => expect(screen.getByText("Save spec")).toBeDisabled());

    await setMapValue("cpu", "700m");
    await waitFor(() => expect(screen.getByText("Save spec")).toBeEnabled());
    expect(
      screen.queryByText(/Cannot save: invalid value at resources\.limits\.cpu/),
    ).not.toBeInTheDocument();

    fireEvent.click(screen.getByText("Save spec"));
    await waitFor(() => expect(mockUpdateController).toHaveBeenCalledTimes(1));
  });

  it("6.3a-5: clearing a hydrated map value to an empty string blocks Save (cleared value is still sent)", async () => {
    // Review claim to pin down: TextField maps "" → undefined, so clearing a
    // map value was claimed to produce NO error, let the gate pass, and send
    // {resources:{limits:{}}} with a success toast. Measured reality: RJSF
    // keeps the empty string in formData, so the quantity pattern still fails,
    // the error path RESOLVES (cpu is present, with ""), and it is at/below a
    // path the curated patch actually carries — the gate blocks.
    renderCard({
      spec: { className: "a", resources: { limits: { cpu: "500m" } } },
    });
    await waitFor(() => expect(classNameInput().value).toBe("a"));

    // Clear the hydrated cpu value.
    const cpu = document.getElementById("root_resources_limits_cpu") as HTMLInputElement;
    fireEvent.change(cpu, { target: { value: "" } });

    // A field-level error renders and the gate blocks — the empty string IS in
    // the curated patch (draft cpu:"" vs snapshot cpu:"500m"), so this is the
    // "touched an invalid field" case, not the untouched-exemption.
    await waitFor(() =>
      expect(document.getElementById("root_resources_limits_cpu__error")).not.toBeNull(),
    );
    expect(screen.getByText("Save spec")).toBeDisabled();
    expect(
      screen.getByText(/Cannot save: invalid value at resources\.limits\.cpu/),
    ).toBeInTheDocument();
    fireEvent.click(screen.getByText("Save spec"));
    expect(mockUpdateController).not.toHaveBeenCalled();
  });

  it("6.3a-6: a mount-time unresolved-path error blocks even when only a YAML tier is edited (fail closed)", async () => {
    // §9 fail-closed: the dotted key nvidia.com/gpu reports at the mangled path
    // .resources.limits.nvidia.com~1gpu, which never resolves to a real form
    // value — such an error blocks UNCONDITIONALLY, even when the user never
    // touches the curated form. The error is computed by RJSF at form mount
    // (liveValidate), so it must reach the gate without any curated onChange.
    const spec = {
      className: "a",
      resources: { limits: { "nvidia.com/gpu": "12qq" } },
      podOverrides: { jvmOpts: "-Xmx1g" },
    } as unknown as ControllerSpec;
    renderCard({ spec });
    await waitFor(() => expect(classNameInput().value).toBe("a"));

    // Edit ONLY a YAML tier; the curated form is never touched.
    fireEvent.click(screen.getByText("Pod overrides"));
    typeYaml("jvmOpts: -Xmx999m");

    // The unresolved error blocks unconditionally, before any save attempt.
    expect(screen.getByText("Save spec")).toBeDisabled();
    expect(
      screen.getByText(/Cannot save: invalid value at resources\.limits\.nvidia\.com~1gpu/),
    ).toBeInTheDocument();
    fireEvent.click(screen.getByText("Save spec"));
    expect(mockUpdateController).not.toHaveBeenCalled();
  });
});

describe("SpecEditorCard section 7 request-body tests", () => {
  // Section 7 asserts REQUEST BODIES, not rendered output — driving
  // SpecEditorCard, which owns the patch. 7.4–7.7c live in the
  // "SpecEditorCard validation gate" block above; this block covers the rest.
  // 7.9 lives in mapForm.test.tsx (the readonly map has no card-level path).

  it("7.1: adds cpu to a spec with no resources → patch.spec is exactly {resources:{limits:{cpu:\"500m\"}}}", async () => {
    renderCard({ spec: { className: "a" } });
    await waitFor(() => expect(classNameInput().value).toBe("a"));

    await addMapRow("cpu");
    await setMapValue("cpu", "500m");

    fireEvent.click(screen.getByText("Save spec"));
    await waitFor(() => expect(mockUpdateController).toHaveBeenCalledTimes(1));
    const [, , , patch] = mockUpdateController.mock.calls[0];
    expect(patch.spec).toEqual({ resources: { limits: { cpu: "500m" } } });
  });

  it("7.2: clearing (removing) a hydrated resources.limits.memory → exactly {resources:{limits:{memory:null}}}", async () => {
    // Depends on Part 1: the memory row must be RENDERED (it arrives via
    // hydration, after mount) for its Remove control to exist.
    renderCard({
      spec: { className: "a", resources: { limits: { cpu: "100m", memory: "512Mi" } } },
    });
    await waitFor(() => expect(classNameInput().value).toBe("a"));

    const remove = document.getElementById(
      "root_resources_limits_memory__remove",
    ) as HTMLButtonElement;
    expect(remove).not.toBeNull();
    fireEvent.click(remove);

    // Removing the entry deletes the key from the draft, and diffValues emits
    // an explicit null for the removed key.
    fireEvent.click(screen.getByText("Save spec"));
    await waitFor(() => expect(mockUpdateController).toHaveBeenCalledTimes(1));
    const [, , , patch] = mockUpdateController.mock.calls[0];
    expect(patch.spec).toEqual({ resources: { limits: { memory: null } } });
  });

  it("7.3: editing className only → patch.spec has no resources key", async () => {
    renderCard({
      spec: { className: "a", resources: { limits: { cpu: "100m" } } },
    });
    await waitFor(() => expect(classNameInput().value).toBe("a"));

    fireEvent.change(classNameInput(), { target: { value: "b" } });
    fireEvent.click(screen.getByText("Save spec"));
    await waitFor(() => expect(mockUpdateController).toHaveBeenCalledTimes(1));
    const [, , , patch] = mockUpdateController.mock.calls[0];
    expect(patch.spec).toEqual({ className: "b" });
    expect(patch.spec).not.toHaveProperty("resources");
  });

  it("7.7b: untouched invalid curated value + edit ONLY a YAML tier → updateController IS called", async () => {
    renderCard({
      spec: {
        className: "a",
        resources: { limits: { cpu: "12qq" } },
        podOverrides: { jvmOpts: "-Xmx1g" },
      },
    });
    await waitFor(() => expect(classNameInput().value).toBe("a"));

    // Surface the invalid curated value to the gate WITHOUT editing it: change
    // className and revert it. The live-validation error for cpu stays known,
    // while the curated patch returns to NO_CHANGE (nothing curated is sent).
    fireEvent.change(classNameInput(), { target: { value: "b" } });
    fireEvent.change(classNameInput(), { target: { value: "a" } });

    // Edit ONLY a YAML tier.
    fireEvent.click(screen.getByText("Pod overrides"));
    typeYaml("jvmOpts: -Xmx999m");

    fireEvent.click(screen.getByText("Save spec"));
    // The YAML edit is sent — the invalid curated value is not in the patch,
    // so the §2 field-level gate must NOT block (pairs with 7.6).
    await waitFor(() => expect(mockUpdateController).toHaveBeenCalledTimes(1));
    const [, , , patch] = mockUpdateController.mock.calls[0];
    expect(patch.spec).toEqual({ podOverrides: { jvmOpts: "-Xmx999m" } });
    expect(patch.spec).not.toHaveProperty("resources");
  });

  it("7.7d: a non-suggested key (hugepages-2Mi) with a valid value submits normally", async () => {
    renderCard({ spec: { className: "a" } });
    await waitFor(() => expect(classNameInput().value).toBe("a"));

    // The datalist suggests cpu/memory/ephemeral-storage — hugepages-2Mi is
    // NOT among them, yet it must be typable and submit (3.4: suggest without
    // restricting).
    await addMapRow("hugepages-2Mi");
    await setMapValue("hugepages-2Mi", "512Mi");

    fireEvent.click(screen.getByText("Save spec"));
    await waitFor(() => expect(mockUpdateController).toHaveBeenCalledTimes(1));
    const [, , , patch] = mockUpdateController.mock.calls[0];
    expect(patch.spec).toEqual({ resources: { limits: { "hugepages-2Mi": "512Mi" } } });
  });

  it("7.8a: renaming a key onto an existing sibling is rejected — key unchanged, error shown, form data unchanged", async () => {
    renderCard({
      spec: { className: "a", resources: { limits: { cpu: "100m", memory: "512Mi" } } },
    });
    await waitFor(() => expect(classNameInput().value).toBe("a"));

    const memoryKey = document.getElementById(
      "root_resources_limits_memory-key",
    ) as HTMLInputElement;
    expect(memoryKey).not.toBeNull();
    fireEvent.change(memoryKey, { target: { value: "cpu" } });
    fireEvent.blur(memoryKey);

    // Rejected: the input reverts and a row-local error is shown.
    expect(memoryKey.value).toBe("memory");
    expect(screen.getByText('"cpu" is already set')).toBeInTheDocument();

    // Form data unchanged: an unrelated className edit saves without touching
    // resources (the rejected rename altered nothing).
    fireEvent.change(classNameInput(), { target: { value: "b" } });
    fireEvent.click(screen.getByText("Save spec"));
    await waitFor(() => expect(mockUpdateController).toHaveBeenCalledTimes(1));
    const [, , , patch] = mockUpdateController.mock.calls[0];
    expect(patch.spec).toEqual({ className: "b" });
    expect(patch.spec).not.toHaveProperty("resources");
  });

  it("7.8b: renaming a key to an empty string is rejected — key unchanged, error shown", async () => {
    renderCard({
      spec: { className: "a", resources: { limits: { cpu: "100m", memory: "512Mi" } } },
    });
    await waitFor(() => expect(classNameInput().value).toBe("a"));

    const memoryKey = document.getElementById(
      "root_resources_limits_memory-key",
    ) as HTMLInputElement;
    fireEvent.change(memoryKey, { target: { value: "" } });
    fireEvent.blur(memoryKey);

    expect(memoryKey.value).toBe("memory");
    expect(screen.getByText("Key cannot be empty")).toBeInTheDocument();
  });

  it("7.8c: a valid rename moves the entry and removes the old key", async () => {
    renderCard({
      spec: { className: "a", resources: { limits: { cpu: "100m", memory: "512Mi" } } },
    });
    await waitFor(() => expect(classNameInput().value).toBe("a"));

    const memoryKey = document.getElementById(
      "root_resources_limits_memory-key",
    ) as HTMLInputElement;
    fireEvent.change(memoryKey, { target: { value: "ephemeral-storage" } });
    fireEvent.blur(memoryKey);
    await waitFor(() =>
      expect(document.getElementById("root_resources_limits_ephemeral-storage")).not.toBeNull(),
    );

    fireEvent.click(screen.getByText("Save spec"));
    await waitFor(() => expect(mockUpdateController).toHaveBeenCalledTimes(1));
    const [, , , patch] = mockUpdateController.mock.calls[0];
    // The old key is emitted as an explicit null (removal) and the new key
    // carries the value that moved with the row.
    expect(patch.spec).toEqual({
      resources: { limits: { memory: null, "ephemeral-storage": "512Mi" } },
    });
  });

  it("7.11a: ingress mode omitted from a YAML-tier edit (previously path) emits mode: null and is rejected", async () => {
    // The mode field is immutable after create (oldSelf rule, types.go:353),
    // so the server rejects any mode change — here, its explicit null.
    mockUpdateController.mockRejectedValue(
      new Error('cannot set field "spec.ingressSpec.mode": value is immutable'),
    );
    renderCard({
      spec: { className: "x", ingressSpec: { host: "ci.example.com", mode: "path" } },
    });
    await waitFor(() => expect(classNameInput().value).toBe("x"));

    fireEvent.click(screen.getByText("Ingress"));
    // Omit mode entirely — it was previously "path".
    typeYaml("host: ci.example.com");

    fireEvent.click(screen.getByText("Save spec"));
    await waitFor(() => expect(mockUpdateController).toHaveBeenCalledTimes(1));
    const [, , , patch] = mockUpdateController.mock.calls[0];
    // "Omission preserves" is FALSE: diffValues emits an explicit null for an
    // absent key.
    expect(patch.spec.ingressSpec).toEqual({ mode: null });
    // The immutable-field change is rejected, and the failure names the field.
    await waitFor(() => expect(screen.getByText(/ingressSpec\.mode/)).toBeInTheDocument());
  });

  it("7.11b: ingress mode left unchanged in a YAML-tier edit emits no mode key and succeeds", async () => {
    renderCard({
      spec: { className: "x", ingressSpec: { host: "ci.example.com", mode: "path" } },
    });
    await waitFor(() => expect(classNameInput().value).toBe("x"));

    fireEvent.click(screen.getByText("Ingress"));
    // Edit host only; keep mode as-is.
    typeYaml("host: new.example.com\nmode: path");

    fireEvent.click(screen.getByText("Save spec"));
    await waitFor(() => expect(mockUpdateController).toHaveBeenCalledTimes(1));
    const [, , , patch] = mockUpdateController.mock.calls[0];
    expect(patch.spec.ingressSpec).toEqual({ host: "new.example.com" });
    expect(patch.spec.ingressSpec).not.toHaveProperty("mode");
  });

  it("7.11c: ingress mode changed via the YAML tier is rejected with a field-level error naming ingressSpec.mode", async () => {
    mockUpdateController.mockRejectedValue(
      new Error("failed to apply spec: spec.ingressSpec.mode is immutable after create"),
    );
    renderCard({
      spec: { className: "x", ingressSpec: { host: "ci.example.com", mode: "path" } },
    });
    await waitFor(() => expect(classNameInput().value).toBe("x"));

    fireEvent.click(screen.getByText("Ingress"));
    typeYaml("host: ci.example.com\nmode: subdomain");

    fireEvent.click(screen.getByText("Save spec"));
    await waitFor(() => expect(mockUpdateController).toHaveBeenCalledTimes(1));
    const [, , , patch] = mockUpdateController.mock.calls[0];
    expect(patch.spec.ingressSpec.mode).toBe("subdomain");
    // The rejection names the offending field rather than failing opaquely.
    await waitFor(() => expect(screen.getByText(/ingressSpec\.mode/)).toBeInTheDocument());
  });
});
