import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import CatalogItemDetail from "./CatalogItemDetail";

vi.mock("../hooks/useConfigurationCluster", () => ({
  useConfigurationCluster: () => ({ cluster: "core", ready: true, clusters: [], error: null }),
}));

vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual("react-router-dom");
  return {
    ...actual,
    useParams: () => ({ name: "uc-acme-widget-1-2-0-abc", namespace: "varroa-system" }),
  };
});

const mockGetCatalogItem = vi.fn();
vi.mock("../api/client", () => ({
  getCatalogItem: (...args: unknown[]) => mockGetCatalogItem(...args),
}));

const derivedItem = (status: Record<string, unknown>) => ({
  metadata: { name: "uc-acme-widget-1-2-0-abc", namespace: "varroa-system" },
  spec: {
    sourceRef: "varroa-update-center",
    type: "plugin",
    displayName: "Acme Widget",
    version: "1.2.0",
    path: "uc://acme-widget@1.2.0",
  },
  status: { valid: true, ...status },
});

function renderPage() {
  return render(
    <MemoryRouter>
      <CatalogItemDetail />
    </MemoryRouter>,
  );
}

/** Returns the table under the section whose heading matches. */
async function tableUnder(heading: RegExp): Promise<HTMLTableElement> {
  const title = await screen.findByText(heading);
  const section = title.closest("div");
  if (!section) throw new Error("no section around the heading");
  return within(section as HTMLElement).getByRole("table") as HTMLTableElement;
}

describe("CatalogItemDetail — closure and compatibility", () => {
  beforeEach(() => {
    mockGetCatalogItem.mockReset();
  });

  it("renders every closure column from status.closure", async () => {
    mockGetCatalogItem.mockResolvedValue({
      item: derivedItem({
        closure: [
          { artifactId: "acme-widget", version: "1.2.0", direct: true, provenance: "store" },
          {
            artifactId: "mailer",
            version: "2.0",
            direct: true,
            provenance: "store",
            minimum: "2.0",
          },
          { artifactId: "deep", version: "9.9", provenance: "lock", minimum: "1.0" },
        ],
      }),
      lockPins: [],
    });

    renderPage();
    const table = await tableUnder(/Pinned closure \(3\)/);

    const rows = within(table).getAllByRole("row");
    // header + 3 entries
    expect(rows).toHaveLength(4);

    const mailerRow = rows.find((r) => within(r).queryByText("mailer"))!;
    const mailerCells = within(mailerRow).getAllByRole("cell").map((c) => c.textContent);
    // artifactId, version, direct/transitive, provenance, effective minimum.
    expect(mailerCells).toEqual(["mailer", "2.0", "direct", "store", "2.0"]);

    const deepRow = rows.find((r) => within(r).queryByText("deep"))!;
    expect(within(deepRow).getByText("transitive")).toBeInTheDocument();
    expect(within(deepRow).getByText("lock")).toBeInTheDocument();
    expect(within(deepRow).getByText("1.0")).toBeInTheDocument();
  });

  it("renders a lock-pin column per profile and marks divergences", async () => {
    mockGetCatalogItem.mockResolvedValue({
      item: derivedItem({
        closure: [
          { artifactId: "acme-widget", version: "1.2.0", direct: true, provenance: "store" },
          { artifactId: "mailer", version: "2.0", provenance: "store", minimum: "2.0" },
        ],
      }),
      lockPins: [
        { profile: "lts", pins: { mailer: "1.0" } },
        { profile: "weekly", pins: { mailer: "2.0", "acme-widget": "1.2.0" } },
      ],
    });

    renderPage();
    const table = await tableUnder(/Pinned closure/);

    expect(within(table).getByText("lts lock")).toBeInTheDocument();
    expect(within(table).getByText("weekly lock")).toBeInTheDocument();

    const rows = within(table).getAllByRole("row");
    const mailerRow = rows.find((r) => within(r).queryByText("mailer"))!;
    // lts pins 1.0 against a selected 2.0 — divergent, marked.
    expect(within(mailerRow).getByText(/1\.0 ≠/)).toBeInTheDocument();
    // weekly pins the same version — present, not marked.
    expect(within(mailerRow).queryByText(/2\.0 ≠/)).not.toBeInTheDocument();

    // A plugin a lock does not mention is distinct from an equal pin.
    const widgetRow = rows.find((r) => within(r).queryByText("acme-widget"))!;
    expect(within(widgetRow).getByText("not pinned")).toBeInTheDocument();
  });

  it("renders every profile's verdict and message", async () => {
    mockGetCatalogItem.mockResolvedValue({
      item: derivedItem({
        compat: [
          {
            profile: "lts",
            jenkinsVersion: "2.555.3",
            verdict: "core-too-old",
            message: "acme-widget@1.2.0 requires core 2.999.9",
          },
          { profile: "weekly", jenkinsVersion: "2.570", verdict: "compatible" },
          { profile: "old", verdict: "unknown", message: "cannot be judged" },
        ],
      }),
      lockPins: [],
    });

    renderPage();
    const table = await tableUnder(/Compatibility \(3 profiles\)/);

    expect(within(table).getByText(/Needs newer Jenkins/)).toBeInTheDocument();
    expect(within(table).getByText("Compatible")).toBeInTheDocument();
    expect(within(table).getByText("Unknown")).toBeInTheDocument();
    expect(within(table).getByText(/requires core 2\.999\.9/)).toBeInTheDocument();
    expect(within(table).getByText("2.555.3")).toBeInTheDocument();
    // A profile with no message renders a placeholder rather than blank.
    expect(within(table).getAllByText("-").length).toBeGreaterThan(0);
  });

  it("states that verdicts are advisory", async () => {
    mockGetCatalogItem.mockResolvedValue({
      item: derivedItem({ compat: [{ profile: "lts", verdict: "core-too-old" }] }),
      lockPins: [],
    });
    renderPage();
    await waitFor(() =>
      expect(screen.getByText(/never block selecting this item/)).toBeInTheDocument(),
    );
  });

  it("renders neither table for an item with no closure and no verdicts", async () => {
    mockGetCatalogItem.mockResolvedValue({ item: derivedItem({}), lockPins: [] });
    renderPage();
    await waitFor(() => expect(screen.getByText("Acme Widget")).toBeInTheDocument());
    expect(screen.queryByText(/Pinned closure/)).not.toBeInTheDocument();
    expect(screen.queryByText(/Compatibility/)).not.toBeInTheDocument();
  });
});
