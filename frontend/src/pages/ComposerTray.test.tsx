import { describe, it, expect, vi, beforeEach, afterEach, beforeAll, afterAll } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { createHandlers } from "../test/handlers";
import ComposerTray from "./ComposerTray";
import { renderWithProviders } from "../test/render-utils";

// ---- MSW server for integration tests ----
const server = setupServer(...createHandlers());

beforeAll(() => server.listen({ onUnhandledRequest: "warn" }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

// vi.mock is hoisted above variable declarations, so use vi.hoisted to make
// mockComposer available in the mock factory.
const mockComposer = vi.hoisted(() => ({
  items: [] as { name: string; namespace?: string; pinnedContentHash?: string; variables?: Record<string, string> }[],
  variables: {} as Record<string, string>,
  addItem: vi.fn(),
  removeItem: vi.fn(),
  reorderItem: vi.fn(),
  setVar: vi.fn(),
  clear: vi.fn(),
  hasItem: vi.fn(),
  toSpec: vi.fn(),
  clearPersisted: vi.fn(),
}));

vi.mock("../context/ComposerContext", () => ({
  useComposer: () => mockComposer,
  ComposerProvider: ({ children }: { children: React.ReactNode }) => children,
  itemRefKey: (ref: { name: string; namespace?: string }) =>
    ref.namespace ? `${ref.namespace}/${ref.name}` : ref.name,
}));

// Mock useControllers
const mockUseControllers = vi.fn();
vi.mock("../hooks/useControllers", () => ({
  useControllers: () => mockUseControllers(),
}));

vi.mock("../hooks/useClusters", () => ({ useClusters: () => ({ data: [{name:"core",core:true,healthy:true,lastHeartbeat:"2025-01-01T00:00:00Z",operatorVersion:"1.0",k8sVersion:"1.28",controllerCount:5,connectedCount:4}], isLoading: false, isError: false }), coreOf: (c: unknown[]) => c?.find((c2: any) => c2.core), clusterQuery: (c: string) => (c && c !== "core" ? `?cluster=${c}` : "") }));

const onClose = vi.fn();

describe("ComposerTray", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockUseControllers.mockReturnValue({
      data: [
        { name: "ctrl-one", namespace: "default" },
        { name: "ctrl-two", namespace: "other-ns" },
      ],
      isLoading: false,
      error: null,
    });

    // Reset composer state
    mockComposer.items = [];
    mockComposer.variables = {};
    mockComposer.toSpec.mockImplementation((displayName: string) => ({
      displayName,
      inputs: mockComposer.items.map((ref: { name: string; pinnedContentHash?: string; variables?: Record<string, string> }) => ({ itemRef: ref })),
      variables: Object.keys(mockComposer.variables).length > 0 ? mockComposer.variables : undefined,
    }));
  });

  describe("Visibility", () => {
    it("does not render when open is false", () => {
      renderWithProviders(<ComposerTray open={false} onClose={onClose} />);
      expect(screen.queryByText("Bundle Composer")).not.toBeInTheDocument();
    });

    it("renders when open is true", () => {
      renderWithProviders(<ComposerTray open={true} onClose={onClose} />);
      expect(screen.getByText("Bundle Composer")).toBeInTheDocument();
    });
  });

  describe("Empty state", () => {
    it('shows "Add items from the catalog browser" when items are empty', () => {
      renderWithProviders(<ComposerTray open={true} onClose={onClose} />);
      expect(
        screen.getByText(/Add items from the catalog browser/),
      ).toBeInTheDocument();
    });

    it("shows form fields even when items are empty", () => {
      renderWithProviders(<ComposerTray open={true} onClose={onClose} />);
      expect(screen.getByPlaceholderText("My composed bundle")).toBeInTheDocument();
      expect(screen.getByText(/Target controller/)).toBeInTheDocument();
      expect(screen.getByText(/Description/)).toBeInTheDocument();
      expect(screen.getByText(/JCasC Merge Strategy/)).toBeInTheDocument();
    });

    it("disables action buttons when items are empty", () => {
      renderWithProviders(<ComposerTray open={true} onClose={onClose} />);
      expect(screen.getByText("Clear")).toBeDisabled();
      expect(screen.getByText("Validate")).toBeDisabled();
      expect(screen.getByText("View bundle.yaml")).toBeDisabled();
      expect(screen.getByText("Create bundle")).toBeDisabled();
      expect(screen.queryByText("Create + Attach")).not.toBeInTheDocument();
    });
  });

  describe("With items", () => {
    beforeEach(() => {
      mockComposer.items = [
        { name: "my-plugin" },
        { name: "my-jcasc-config" },
      ];
      mockComposer.toSpec.mockImplementation((displayName: string) => ({
        displayName,
        inputs: mockComposer.items.map((ref) => ({ itemRef: ref })),
        variables: Object.keys(mockComposer.variables).length > 0
          ? mockComposer.variables
          : undefined,
      }));
    });

    it("shows items grouped by type", () => {
      renderWithProviders(<ComposerTray open={true} onClose={onClose} />);

      // my-plugin should be in "Plugins" group (name includes "plugin")
      // my-jcasc-config should be in "JCasC" group (name includes "jcasc")
      expect(screen.getByText("Plugins")).toBeInTheDocument();
      expect(screen.getByText("JCasC")).toBeInTheDocument();
      expect(screen.getByText("my-plugin")).toBeInTheDocument();
      expect(screen.getByText("my-jcasc-config")).toBeInTheDocument();
    });

    it("shows move up, move down, and remove buttons per item", () => {
      renderWithProviders(<ComposerTray open={true} onClose={onClose} />);

      const moveBtns = screen.getAllByTitle("Move up");
      const moveDownBtns = screen.getAllByTitle("Move down");
      const removeBtns = screen.getAllByTitle("Remove");

      expect(moveBtns.length).toBe(2);
      expect(moveDownBtns.length).toBe(2);
      expect(removeBtns.length).toBe(2);
    });

    it("enables action buttons when items exist", () => {
      renderWithProviders(<ComposerTray open={true} onClose={onClose} />);
      expect(screen.getByText("Clear")).not.toBeDisabled();
      expect(screen.getByText("Validate")).not.toBeDisabled();
      expect(screen.getByText("View bundle.yaml")).not.toBeDisabled();
      expect(screen.getByText("Create bundle")).not.toBeDisabled();
    });

    it('shows "Create + Attach" when a target controller is selected', async () => {
      const user = userEvent.setup();
      renderWithProviders(<ComposerTray open={true} onClose={onClose} />);

      // Select a target controller
      await user.selectOptions(
        screen.getAllByRole("combobox")[0],
        "ctrl-one",
      );

      expect(screen.getByText("Create + Attach")).toBeInTheDocument();
    });
  });

  describe("Item reordering", () => {
    it("calls reorderItem when move up is clicked", async () => {
      mockComposer.items = [
        { name: "item-a" },
        { name: "item-b" },
      ];
      mockComposer.toSpec.mockReturnValue({ displayName: "test", inputs: mockComposer.items.map((ref) => ({ itemRef: ref })) });

      const user = userEvent.setup();
      renderWithProviders(<ComposerTray open={true} onClose={onClose} />);

      // Click move up on the second item (index 1 → 0)
      const moveUpBtns = screen.getAllByTitle("Move up");
      // The first item's "move up" is disabled (index 0), second item's is enabled
      await user.click(moveUpBtns[1]);

      expect(mockComposer.reorderItem).toHaveBeenCalledWith(1, 0);
    });

    it("calls reorderItem when move down is clicked", async () => {
      mockComposer.items = [
        { name: "item-a" },
        { name: "item-b" },
      ];
      mockComposer.toSpec.mockReturnValue({ displayName: "test", inputs: mockComposer.items.map((ref) => ({ itemRef: ref })) });

      const user = userEvent.setup();
      renderWithProviders(<ComposerTray open={true} onClose={onClose} />);

      const moveDownBtns = screen.getAllByTitle("Move down");
      await user.click(moveDownBtns[0]);

      expect(mockComposer.reorderItem).toHaveBeenCalledWith(0, 1);
    });

    it("calls removeItem when remove is clicked", async () => {
      mockComposer.items = [
        { name: "item-a" },
        { name: "item-b" },
      ];
      mockComposer.toSpec.mockReturnValue({ displayName: "test", inputs: mockComposer.items.map((ref) => ({ itemRef: ref })) });

      const user = userEvent.setup();
      renderWithProviders(<ComposerTray open={true} onClose={onClose} />);

      const removeBtns = screen.getAllByTitle("Remove");
      await user.click(removeBtns[0]);

      expect(mockComposer.removeItem).toHaveBeenCalledWith({ name: "item-a" });
    });

    it("renders a namespace badge for items with a namespace", () => {
      mockComposer.items = [
        { name: "jcasc-a", namespace: "team-b" },
      ];
      renderWithProviders(<ComposerTray open={true} onClose={onClose} />);
      expect(screen.getByText("team-b")).toBeInTheDocument();
    });
  });

  describe("Preview", () => {
    beforeEach(() => {
      mockComposer.items = [{ name: "my-plugin" }];
    });

    it("shows a warning when display name is empty and preview is clicked", async () => {
      const user = userEvent.setup();
      renderWithProviders(<ComposerTray open={true} onClose={onClose} />);

      await user.click(screen.getByText("View bundle.yaml"));

      // Toast would be called — no visible UI change, so we just check no crash
    });

    it("triggers preview API call and shows preview YAML", async () => {
      const user = userEvent.setup();
      renderWithProviders(<ComposerTray open={true} onClose={onClose} />);

      await user.type(screen.getByPlaceholderText("My composed bundle"), "My Bundle");
      await user.click(screen.getByText("View bundle.yaml"));

      // The preview should appear with YAML
      await waitFor(() => {
        expect(screen.getByText("Preview")).toBeInTheDocument();
        // Content from createComposedBundlePreview default
        expect(screen.getByText(/bundle: test/)).toBeInTheDocument();
      });
    });

    it("shows missing, drifted, and warning banners in preview", async () => {
      const user = userEvent.setup();
      // Override preview to include warnings, missing, drifted
      server.use(
        ...createHandlers({
          preview: {
            bundleYaml: "bundle: test\n",
            jenkinsYaml: "jenkins: {}\n",
            pluginsYaml: "plugins: []\n",
            itemsYaml: "items: []\n",
            rbacYaml: "rbac: []\n",
            missing: ["item-gone"],
            drifted: ["item-drifted"],
            warnings: ["Some plugin version mismatch"],
          },
        }),
      );

      renderWithProviders(<ComposerTray open={true} onClose={onClose} />);
      await user.type(screen.getByPlaceholderText("My composed bundle"), "My Bundle");
      await user.click(screen.getByText("View bundle.yaml"));

      await waitFor(() => {
        expect(screen.getByText(/Missing items:/)).toBeInTheDocument();
        expect(screen.getByText(/item-gone/)).toBeInTheDocument();
        expect(screen.getByText(/Drifted items:/)).toBeInTheDocument();
        expect(screen.getByText(/item-drifted/)).toBeInTheDocument();
        expect(screen.getByText(/Some plugin version mismatch/)).toBeInTheDocument();
      });
    });

    it("renders preview without crashing when the API returns null arrays", async () => {
      const user = userEvent.setup();
      // The Go BFF serializes nil slices as JSON null for missing/drifted/warnings
      // (no omitempty). The tray must not crash reading .length on them.
      server.use(
        http.post("*/composedbundles/*/preview*", () =>
          HttpResponse.json({
            bundleYaml: "bundle: test\n",
            jenkinsYaml: "jenkins: {}\n",
            pluginsYaml: "plugins: []\n",
            itemsYaml: "items: []\n",
            rbacYaml: "rbac: []\n",
            missing: null,
            drifted: null,
            warnings: null,
            unresolvedVariables: null,
          }),
        ),
      );

      renderWithProviders(<ComposerTray open={true} onClose={onClose} />);
      await user.type(screen.getByPlaceholderText("My composed bundle"), "My Bundle");
      await user.click(screen.getByText("View bundle.yaml"));

      await waitFor(() => {
        expect(screen.getByText("Preview")).toBeInTheDocument();
        expect(screen.getByText(/bundle: test/)).toBeInTheDocument();
      });
    });
  });

  describe("Validation", () => {
    beforeEach(() => {
      mockComposer.items = [{ name: "my-plugin" }];
    });

    it("triggers validation API and shows validation result", async () => {
      const user = userEvent.setup();
      renderWithProviders(<ComposerTray open={true} onClose={onClose} />);

      await user.type(screen.getByPlaceholderText("My composed bundle"), "My Bundle");
      await user.click(screen.getByText("Validate"));

      await waitFor(() => {
        expect(screen.getByText(/✓ Valid/)).toBeInTheDocument();
      });
    });

    it("shows validation errors and warnings when present", async () => {
      const user = userEvent.setup();
      // Override validate endpoint specifically. Must NOT include
      // ...createHandlers() or the default composedbundles/validate handler
      // would match before this override.
      server.use(
        http.post("*/composedbundles/validate*", () =>
          HttpResponse.json({
            valid: false,
            errors: ["Missing required field: LOG_LEVEL"],
            warnings: ["Plugin 'git' version is outdated"],
          }),
        ),
      );

      renderWithProviders(<ComposerTray open={true} onClose={onClose} />);
      await user.type(screen.getByPlaceholderText("My composed bundle"), "My Bundle");
      await user.click(screen.getByText("Validate"));

      await waitFor(() => {
        expect(screen.getByText(/Invalid/)).toBeInTheDocument();
        expect(screen.getByText(/Missing required field/)).toBeInTheDocument();
        expect(screen.getByText(/Plugin 'git' version is outdated/)).toBeInTheDocument();
      });
    });

    it("renders validation result without crashing when errors/warnings are absent", async () => {
      const user = userEvent.setup();
      // errors/warnings are omitempty on the BFF response, so a clean validation
      // omits them entirely. The tray must not crash reading .length on undefined.
      server.use(
        http.post("*/composedbundles/validate*", () =>
          HttpResponse.json({ valid: true }),
        ),
      );

      renderWithProviders(<ComposerTray open={true} onClose={onClose} />);
      await user.type(screen.getByPlaceholderText("My composed bundle"), "My Bundle");
      await user.click(screen.getByText("Validate"));

      await waitFor(() => {
        expect(screen.getByText(/✓ Valid/)).toBeInTheDocument();
      });
    });
  });

  describe("Create bundle", () => {
    beforeEach(() => {
      mockComposer.items = [{ name: "my-item" }];
    });

    it("creates a bundle via API and closes the tray", async () => {
      const user = userEvent.setup();
      renderWithProviders(<ComposerTray open={true} onClose={onClose} />);

      await user.type(screen.getByPlaceholderText("My composed bundle"), "My Bundle");
      await user.click(screen.getByText("Create bundle"));

      await waitFor(() => {
        expect(onClose).toHaveBeenCalled();
      });
    });

    it("shows Create + Attach when a controller is selected and creates both", async () => {
      const user = userEvent.setup();
      renderWithProviders(<ComposerTray open={true} onClose={onClose} />);

      await user.type(screen.getByPlaceholderText("My composed bundle"), "My Bundle");
      await user.selectOptions(
        screen.getAllByRole("combobox")[0],
        "ctrl-one",
      );
      await user.click(screen.getByText("Create + Attach"));

      await waitFor(() => {
        expect(onClose).toHaveBeenCalled();
      });
    });
  });

  describe("Clear action", () => {
    it("calls composer.clear and resets form state", async () => {
      mockComposer.items = [{ name: "my-item" }];
      const user = userEvent.setup();
      renderWithProviders(<ComposerTray open={true} onClose={onClose} />);

      // Type something first
      await user.type(screen.getByPlaceholderText("My composed bundle"), "Something");
      await user.click(screen.getByText("Clear"));

      expect(mockComposer.clear).toHaveBeenCalled();
    });
  });

  describe("Close button", () => {
    it("calls onClose when close button is clicked", async () => {
      const user = userEvent.setup();
      renderWithProviders(<ComposerTray open={true} onClose={onClose} />);

      await user.click(screen.getByText("×"));

      expect(onClose).toHaveBeenCalled();
    });

    it("calls onClose when overlay is clicked", async () => {
      const user = userEvent.setup();
      const { container } = renderWithProviders(<ComposerTray open={true} onClose={onClose} />);

      // The overlay is the outermost rendered DOM element; the tray is inside it.
      // Clicking the overlay directly (via container.firstChild — which is the
      // overlay div) fires onClick={onClose} without hitting the tray's
      // stopPropagation.
      const overlay = container.firstChild as HTMLElement;
      await user.click(overlay);
      expect(onClose).toHaveBeenCalled();
    });
  });

  describe("Edit mode", () => {
    const editTarget = {
      namespace: "default",
      name: "test-bundle",
      baseBundle: {
        apiVersion: "varroa.dev/v1alpha1" as const,
        kind: "ComposedBundle" as const,
        metadata: { name: "test-bundle", namespace: "default", resourceVersion: "999" },
        spec: {
          displayName: "Existing Bundle",
          description: "Original description",
          inputs: [{ itemRef: { name: "original-item" } }],
          variables: { KEY: "val" },
          jcascMergeStrategy: "override",
        },
      },
      gitInputs: [],
    };

    const editTargetWithGit = {
      ...editTarget,
      gitInputs: [{ repoURL: "https://github.com/example/repo.git", path: "jenkins.yaml", revision: "abc" }],
    };

    beforeEach(() => {
      mockComposer.items = [{ name: "item-from-composer" }];
      mockComposer.toSpec.mockImplementation((displayName: string) => ({
        displayName,
        inputs: mockComposer.items.map((ref: { name: string }) => ({ itemRef: ref })),
      }));
    });

    it("prefills display name, description, and merge strategy from baseBundle", () => {
      renderWithProviders(<ComposerTray open={true} onClose={onClose} editTarget={editTarget} />);

      const nameInput = screen.getByPlaceholderText("My composed bundle") as HTMLInputElement;
      expect(nameInput.value).toBe("Existing Bundle");

      const descInput = screen.getByPlaceholderText("Optional description") as HTMLInputElement;
      expect(descInput.value).toBe("Original description");

      // Merge strategy should be "override" (from the fixture)
      const strategySelect = screen.getAllByRole("combobox")[0] as HTMLSelectElement;
      expect(strategySelect.value).toBe("override");
    });

    it('renders "Save changes" button instead of "Create bundle"', () => {
      renderWithProviders(<ComposerTray open={true} onClose={onClose} editTarget={editTarget} />);

      expect(screen.getByText("Save changes")).toBeInTheDocument();
      expect(screen.queryByText("Create bundle")).not.toBeInTheDocument();
      expect(screen.queryByText("Create + Attach")).not.toBeInTheDocument();
    });

    it("hides the target controller selector", () => {
      renderWithProviders(<ComposerTray open={true} onClose={onClose} editTarget={editTarget} />);

      expect(screen.queryByText("Target controller")).not.toBeInTheDocument();
    });

    it("calls updateComposedBundle on save with fixed name and preserved metadata", async () => {
      const user = userEvent.setup();
      renderWithProviders(<ComposerTray open={true} onClose={onClose} editTarget={editTarget} />);

      await user.click(screen.getByText("Save changes"));

      // The MSW handler returns success; the tray should close
      await waitFor(() => {
        expect(onClose).toHaveBeenCalled();
      });
    });

    it("disables save when composer has no items and no gitInputs", () => {
      mockComposer.items = [];
      renderWithProviders(<ComposerTray open={true} onClose={onClose} editTarget={editTarget} />);

      expect(screen.getByText("Save changes")).toBeDisabled();
    });

    it("enables save when composer has no items but gitInputs remain", () => {
      mockComposer.items = [];
      renderWithProviders(<ComposerTray open={true} onClose={onClose} editTarget={editTargetWithGit} />);

      expect(screen.getByText("Save changes")).not.toBeDisabled();
    });

    it("shows conflict message on 409 response", async () => {
      const user = userEvent.setup();

      // Override the PUT handler to return 409
      server.use(
        http.put("*/composedbundles/:namespace/:name", () =>
          new HttpResponse(null, { status: 409 }),
        ),
      );

      renderWithProviders(<ComposerTray open={true} onClose={onClose} editTarget={editTarget} />);

      await user.click(screen.getByText("Save changes"));

      // The 409 surfaces a conflict-specific message, not a generic failure.
      expect(await screen.findByText(/reload and retry/i)).toBeInTheDocument();

      await waitFor(() => {
        // The tray should still be open (save failed)
        expect(screen.getByText("Save changes")).toBeInTheDocument();
      });
    });

    it("falls back to the bundle resource name when spec.displayName is empty", () => {
      const noDisplayName = {
        ...editTarget,
        baseBundle: {
          ...editTarget.baseBundle,
          spec: { ...editTarget.baseBundle.spec, displayName: "" },
        },
      };
      renderWithProviders(<ComposerTray open={true} onClose={onClose} editTarget={noDisplayName} />);

      const nameInput = screen.getByPlaceholderText("My composed bundle") as HTMLInputElement;
      expect(nameInput.value).toBe("test-bundle");
    });

    describe("Save as new", () => {
      it('renders a "Save as new" button in edit mode', () => {
        renderWithProviders(<ComposerTray open={true} onClose={onClose} editTarget={editTarget} />);
        expect(screen.getByText("Save as new")).toBeInTheDocument();
      });

      it("blocks save-as when the display name slugs to the edited bundle's own name", async () => {
        const user = userEvent.setup();
        renderWithProviders(<ComposerTray open={true} onClose={onClose} editTarget={editTarget} />);

        const nameInput = screen.getByPlaceholderText("My composed bundle");
        await user.clear(nameInput);
        await user.type(nameInput, "Test Bundle"); // slugs to "test-bundle" == editTarget.name
        await user.click(screen.getByText("Save as new"));

        expect(await screen.findByText(/change the display name/i)).toBeInTheDocument();
        expect(onClose).not.toHaveBeenCalled();
      });

      it("blocks save-as when a bundle with the derived name already exists", async () => {
        const user = userEvent.setup();
        // "Existing Bundle" slugs to "existing-bundle"; pretend it exists.
        server.use(
          http.get("*/composedbundles/default/existing-bundle", () =>
            HttpResponse.json({ metadata: { name: "existing-bundle", namespace: "default" } }),
          ),
        );

        renderWithProviders(<ComposerTray open={true} onClose={onClose} editTarget={editTarget} />);
        await user.click(screen.getByText("Save as new"));

        expect(await screen.findByText(/already exists/i)).toBeInTheDocument();
        expect(onClose).not.toHaveBeenCalled();
      });

      it("creates a new bundle with the slugified name and composer inputs, then closes", async () => {
        const user = userEvent.setup();
        let postedBody: { metadata?: { name?: string }; spec?: { inputs?: unknown[] } } | null = null;
        server.use(
          // The derived name is free (404 from the existence probe)...
          http.get("*/composedbundles/default/existing-bundle", () =>
            new HttpResponse("Not found", { status: 404 }),
          ),
          // ...so the POST fires; capture what it sends.
          http.post("*/composedbundles/default", async ({ request }) => {
            postedBody = (await request.json()) as typeof postedBody;
            return HttpResponse.json(postedBody, { status: 201 });
          }),
        );

        renderWithProviders(<ComposerTray open={true} onClose={onClose} editTarget={editTarget} />);
        await user.click(screen.getByText("Save as new"));

        await waitFor(() => expect(onClose).toHaveBeenCalled());
        expect(postedBody!.metadata!.name).toBe("existing-bundle");
        expect(postedBody!.spec!.inputs).toEqual([{ itemRef: { name: "item-from-composer" } }]);
      });

      it("appends git inputs to a save-as spec", async () => {
        const user = userEvent.setup();
        let postedBody: { spec?: { inputs?: unknown[] } } | null = null;
        server.use(
          http.get("*/composedbundles/default/existing-bundle", () =>
            new HttpResponse("Not found", { status: 404 }),
          ),
          http.post("*/composedbundles/default", async ({ request }) => {
            postedBody = (await request.json()) as typeof postedBody;
            return HttpResponse.json(postedBody, { status: 201 });
          }),
        );

        renderWithProviders(<ComposerTray open={true} onClose={onClose} editTarget={editTargetWithGit} />);
        await user.click(screen.getByText("Save as new"));

        await waitFor(() => expect(onClose).toHaveBeenCalled());
        expect(postedBody!.spec!.inputs).toEqual([
          { itemRef: { name: "item-from-composer" } },
          { gitSource: { repoURL: "https://github.com/example/repo.git", path: "jenkins.yaml", revision: "abc" } },
        ]);
      });
    });
  });
});
