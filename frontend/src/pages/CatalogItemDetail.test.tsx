import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import CatalogItemDetail from "./CatalogItemDetail";

vi.mock("../hooks/useConfigurationCluster", () => ({
  useConfigurationCluster: () => ({ cluster: "core", entry: null, ready: true }),
}));

const mockNavigate = vi.fn();
vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual("react-router-dom");
  return {
    ...actual,
    useNavigate: () => mockNavigate,
    useParams: () => ({ name: "test-item", namespace: "default" }),
  };
});

const mockGetCatalogItem = vi.fn();
vi.mock("../api/client", () => ({
  listClusters: () => Promise.resolve([{ name: "core", core: true, healthy: true, state: "active" }]),
  getCatalogItem: (...args: unknown[]) => mockGetCatalogItem(...args),
}));

/**
 * The get route returns the item wrapped alongside the per-profile lock-pin
 * projection the BFF joins at read time.
 */
const detailResponse = (item: unknown, lockPins: unknown[] = []) => ({ item, lockPins });

// Wrapper with MemoryRouter so Link components render correctly
function renderWithRouter(ui: React.ReactElement) {
  return render(<MemoryRouter>{ui}</MemoryRouter>);
}

const catalogItem = (name: string, overrides?: Record<string, unknown>) => ({
  metadata: {
    name,
    namespace: "default",
    creationTimestamp: "2024-06-01T00:00:00Z",
  },
  spec: {
    sourceRef: "test-source",
    type: "jcasc",
    displayName: "Test Catalog Item",
    description: "A test catalog item description",
    path: "test/casc.yaml",
    version: "1.2.3",
    tags: ["networking", "security"],
    variables: [
      { name: "LOG_LEVEL", default: "INFO", description: "Logging level", required: false },
      { name: "PORT", required: true },
    ],
    ...(overrides?.spec as Record<string, unknown> ?? {}),
  },
  status: {
    valid: true,
    contentHash: "abc123deadbeef4567",
    content: "jenkins:\n  systemMessage: test\n",
    observedRevision: "main",
  },
  ...overrides,
});

function renderPage() {
  return renderWithRouter(<CatalogItemDetail />);
}

describe("CatalogItemDetail", () => {
  beforeEach(() => {
    mockGetCatalogItem.mockReset();
  });

  describe("Loading state", () => {
    it("shows a loading banner when the item is loading", () => {
      // Never resolve the promise
      mockGetCatalogItem.mockReturnValue(new Promise(() => {}));
      renderPage();
      expect(screen.getByText("Loading item...")).toBeInTheDocument();
    });
  });

  describe("Error state", () => {
    it("shows an error banner with the error message", async () => {
      mockGetCatalogItem.mockRejectedValue(new Error("Item not found"));
      renderPage();

      await waitFor(() => {
        expect(screen.getByText(/Error: Item not found/)).toBeInTheDocument();
      });
    });

    it("shows a back link to the catalog on error", async () => {
      mockGetCatalogItem.mockRejectedValue(new Error("Not found"));
      renderPage();

      await waitFor(() => {
        const backLink = screen.getByText(/Back to Catalog/);
        expect(backLink).toBeInTheDocument();
        expect(backLink.closest("a")).toHaveAttribute("href", "/catalog?cluster=core");
      });
    });
  });

  describe("Happy path", () => {
    it("renders the item name and type", async () => {
      mockGetCatalogItem.mockResolvedValue(detailResponse(catalogItem("test-item")));
      renderPage();

      await waitFor(() => {
        expect(screen.getByText("Test Catalog Item")).toBeInTheDocument();
        expect(screen.getByText(/Catalog item · jcasc/)).toBeInTheDocument();
      });
    });

    it("renders a valid badge for valid items", async () => {
      mockGetCatalogItem.mockResolvedValue(detailResponse(catalogItem("test-item")));
      renderPage();

      await waitFor(() => {
        expect(screen.getByText(/Valid/)).toBeInTheDocument();
      });
    });

    it("renders an invalid badge for invalid items", async () => {
      mockGetCatalogItem.mockResolvedValue(detailResponse(
        catalogItem("test-item", {
          status: { valid: false, message: "Missing required field" },
        }),
      ));
      renderPage();

      await waitFor(() => {
        expect(screen.getByText(/Invalid: Missing required field/)).toBeInTheDocument();
      });
    });

    it("renders the detail grid with Name, Namespace, Source, Type, Path, Version, Created", async () => {
      mockGetCatalogItem.mockResolvedValue(detailResponse(catalogItem("test-item")));
      renderPage();

      await waitFor(() => {
        expect(screen.getByText("test-item")).toBeInTheDocument();
        expect(screen.getByText("default")).toBeInTheDocument();
        expect(screen.getByText("test-source")).toBeInTheDocument();
        expect(screen.getByText("jcasc")).toBeInTheDocument();
        expect(screen.getByText("test/casc.yaml")).toBeInTheDocument();
        expect(screen.getByText("1.2.3")).toBeInTheDocument();
        expect(screen.getByText(/content hash/i)).toBeInTheDocument();
        expect(screen.getByText(/abc123deadbeef/)).toBeInTheDocument();
        expect(screen.getByText(/revision/i)).toBeInTheDocument();
        expect(screen.getByText("main")).toBeInTheDocument();
      });
    });

    it("renders the description section", async () => {
      mockGetCatalogItem.mockResolvedValue(detailResponse(catalogItem("test-item")));
      renderPage();

      await waitFor(() => {
        expect(screen.getByText("Description")).toBeInTheDocument();
        expect(
          screen.getByText("A test catalog item description"),
        ).toBeInTheDocument();
      });
    });

    it("renders the tags section when tags exist", async () => {
      mockGetCatalogItem.mockResolvedValue(detailResponse(catalogItem("test-item")));
      renderPage();

      await waitFor(() => {
        expect(screen.getByText("Tags")).toBeInTheDocument();
        expect(screen.getByText("networking")).toBeInTheDocument();
        expect(screen.getByText("security")).toBeInTheDocument();
      });
    });

    it("renders the variables section with name, default, description, and required marker", async () => {
      mockGetCatalogItem.mockResolvedValue(detailResponse(catalogItem("test-item")));
      renderPage();

      await waitFor(() => {
        expect(screen.getByText(/Variables \(2\)/)).toBeInTheDocument();
        expect(screen.getByText("LOG_LEVEL")).toBeInTheDocument();
        // Default value text — it's a span inside the variableMeta div, check via class or text
        expect(screen.getByText(/Default:/)).toBeInTheDocument();
        expect(screen.getByText("INFO")).toBeInTheDocument();
        expect(screen.getByText("PORT")).toBeInTheDocument();
        // Required marker
        expect(screen.getByText("*")).toBeInTheDocument();
      });
    });

    it("renders the content section when status.content exists", async () => {
      mockGetCatalogItem.mockResolvedValue(detailResponse(catalogItem("test-item")));
      renderPage();

      await waitFor(() => {
        expect(screen.getByText("Content")).toBeInTheDocument();
        expect(screen.getByText(/jenkins:/)).toBeInTheDocument();
      });
    });

    it("does not render sections when data is missing", async () => {
      mockGetCatalogItem.mockResolvedValue(detailResponse(
        catalogItem("test-item", {
          spec: {
            sourceRef: "test-source",
            type: "jcasc",
            path: "test.yaml",
          },
          status: { valid: true },
        }),
      ));
      renderPage();

      await waitFor(() => {
        // No description, no tags, no variables, no content
        expect(screen.queryByText("Description")).not.toBeInTheDocument();
        expect(screen.queryByText("Tags")).not.toBeInTheDocument();
        expect(screen.queryByText("Variables")).not.toBeInTheDocument();
        expect(screen.queryByText("Content")).not.toBeInTheDocument();
      });
    });

    it("renders a back link to the catalog", async () => {
      mockGetCatalogItem.mockResolvedValue(detailResponse(catalogItem("test-item")));
      renderPage();

      await waitFor(() => {
        const backLink = screen.getByText(/Back to Catalog/);
        expect(backLink).toBeInTheDocument();
        expect(backLink.closest("a")).toHaveAttribute("href", "/catalog?cluster=core");
      });
    });
  });
});

describe("CatalogItemDetail — pipeline-template item", () => {
  const templateYaml = [
    "apiVersion: varroa.dev/v1",
    "kind: PipelineTemplate",
    "metadata:",
    "  name: go-service",
    "  displayName: Go Service",
    "  description: Standard Go microservice pipeline.",
    "spec:",
    '  version: "2.1.0"',
    "  parameters:",
    "    $schema: https://json-schema.org/draft/2020-12/schema",
    "    type: object",
    "    additionalProperties: false",
    "    properties:",
    "      goVersion:",
    "        type: string",
    "        title: Go version",
    '        enum: ["1.22", "1.23", "1.24"]',
    '        default: "1.24"',
    "    required:",
    "      - goVersion",
    "  uiSchema:",
    "    goVersion:",
    "      ui:widget: select",
    "  jenkinsfile: |",
    "    pipeline { agent any }",
    "",
  ].join("\n");

  const pipelineTemplateItem = () =>
    catalogItem("go-service-template", {
      spec: {
        sourceRef: "test-source",
        type: "pipeline-template",
        displayName: "Go Service Template",
        path: "pipeline-templates/go-service.yaml",
      },
      status: { valid: true, content: templateYaml },
    });

  beforeEach(() => {
    mockGetCatalogItem.mockReset();
    Object.assign(navigator, { clipboard: { writeText: vi.fn().mockResolvedValue(undefined) } });
  });

  it("renders the template header from the document's metadata and spec", async () => {
    mockGetCatalogItem.mockResolvedValue(detailResponse(pipelineTemplateItem()));
    renderPage();

    await waitFor(() => {
      expect(screen.getByText("Template")).toBeInTheDocument();
      expect(screen.getByText("Go Service")).toBeInTheDocument();
      expect(screen.getByText("2.1.0")).toBeInTheDocument();
      expect(screen.getByText("Standard Go microservice pipeline.")).toBeInTheDocument();
    });
  });

  it("renders no generic Content section for a pipeline-template item", async () => {
    mockGetCatalogItem.mockResolvedValue(detailResponse(pipelineTemplateItem()));
    renderPage();

    await waitFor(() => expect(screen.getByText("Template")).toBeInTheDocument());
    expect(screen.queryByText("Content")).not.toBeInTheDocument();
  });

  it("renders a live form built from spec.parameters and spec.uiSchema", async () => {
    mockGetCatalogItem.mockResolvedValue(detailResponse(pipelineTemplateItem()));
    renderPage();

    await waitFor(() => expect(screen.getByText("Go version")).toBeInTheDocument());
    expect(screen.getByRole("combobox")).toBeInTheDocument();
  });

  it("renders the generated marker YAML naming the template and version", async () => {
    mockGetCatalogItem.mockResolvedValue(detailResponse(pipelineTemplateItem()));
    renderPage();

    await waitFor(() => expect(screen.getByText(/template: go-service/)).toBeInTheDocument());
    expect(screen.getByText(/templateVersion: 2\.1\.0/)).toBeInTheDocument();
  });

  it("updates the generated marker YAML as the form changes", async () => {
    mockGetCatalogItem.mockResolvedValue(detailResponse(pipelineTemplateItem()));
    renderPage();

    await waitFor(() => expect(screen.getByRole("combobox")).toBeInTheDocument());
    fireEvent.change(screen.getByRole("combobox"), { target: { value: "1.23" } });

    await waitFor(() => expect(screen.getByText(/goVersion: "?1\.23"?/)).toBeInTheDocument());
  });

  it("copies the generated marker YAML to the clipboard", async () => {
    mockGetCatalogItem.mockResolvedValue(detailResponse(pipelineTemplateItem()));
    renderPage();

    await waitFor(() => expect(screen.getByText(/template: go-service/)).toBeInTheDocument());
    fireEvent.click(screen.getByRole("button", { name: "Copy" }));

    await waitFor(() => expect(screen.getByRole("button", { name: "Copied" })).toBeInTheDocument());
    expect(navigator.clipboard.writeText).toHaveBeenCalledWith(expect.stringContaining("template: go-service"));
  });

  it("shows an error banner when the template content does not parse", async () => {
    mockGetCatalogItem.mockResolvedValue(detailResponse(
      catalogItem("broken-template", {
        spec: { sourceRef: "test-source", type: "pipeline-template", path: "pipeline-templates/broken.yaml" },
        status: { valid: false, content: "not: [valid" },
      }),
    ));
    renderPage();

    await waitFor(() => expect(screen.getByText("Unable to read the template document.")).toBeInTheDocument());
  });
});
