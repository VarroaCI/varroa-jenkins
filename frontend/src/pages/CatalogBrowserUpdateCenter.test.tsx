import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithProviders } from "../test/render-utils";
import CatalogBrowser from "./CatalogBrowser";

const mockNavigate = vi.fn();
vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual("react-router-dom");
  return { ...actual, useNavigate: () => mockNavigate };
});

const mockUseCatalogItems = vi.fn();
vi.mock("../hooks/useCatalog", () => ({
  useCatalogItems: () => mockUseCatalogItems(),
  useCatalogSources: () => ({ data: { items: [] }, isLoading: false, error: null }),
  useComposedBundles: () => ({ data: { items: [] }, isLoading: false, error: null }),
}));

/** A derived update-center item. */
const ucItem = (
  pluginName: string,
  version: string,
  overrides?: Record<string, unknown>,
) => ({
  name: `uc-${pluginName}-${version.replace(/\./g, "-")}-abcdef0123`,
  namespace: "varroa-system",
  sourceRef: "varroa-update-center",
  type: "plugin",
  pluginName,
  version,
  displayName: pluginName,
  valid: true,
  tags: ["update-center"],
  ...overrides,
});

const gitItem = (name: string) => ({
  name,
  namespace: "default",
  sourceRef: "platform-catalog",
  type: "jcasc",
  displayName: `Display ${name}`,
  valid: true,
});

function renderWith(items: unknown[]) {
  mockUseCatalogItems.mockReturnValue({ data: { items }, isLoading: false, error: null });
  return renderWithProviders(<CatalogBrowser />);
}

describe("CatalogBrowser — update center group", () => {
  beforeEach(() => {
    mockNavigate.mockReset();
    mockUseCatalogItems.mockReset();
    localStorage.clear();
  });

  it("renders update-center-backed items in their own group", () => {
    renderWith([ucItem("acme-widget", "1.2.0"), gitItem("shared-jcasc")]);

    expect(screen.getByText("Update Center")).toBeInTheDocument();
    // Both still render; the update-center item is inside the group section.
    expect(screen.getByText("acme-widget")).toBeInTheDocument();
    expect(screen.getByText("Display shared-jcasc")).toBeInTheDocument();
  });

  it("does not render the group when no derived items exist", () => {
    renderWith([gitItem("shared-jcasc")]);
    expect(screen.queryByText("Update Center")).not.toBeInTheDocument();
  });

  it("collapses multiple stored versions into one entry with a version selector", () => {
    renderWith([
      ucItem("acme-widget", "1.2.0"),
      ucItem("acme-widget", "1.3.0"),
      ucItem("acme-widget", "1.10.0"),
    ]);

    // One row, not three.
    expect(screen.getAllByText("acme-widget")).toHaveLength(1);

    const select = screen.getByLabelText("Version for acme-widget") as HTMLSelectElement;
    expect(within(select).getAllByRole("option")).toHaveLength(3);
    // Highest by the comparator, not lexically: 1.10.0 beats 1.3.0.
    expect(select.value).toBe("1.10.0");
  });

  it("shows a plain version label when only one version is stored", () => {
    renderWith([ucItem("acme-widget", "1.2.0")]);
    expect(screen.queryByLabelText("Version for acme-widget")).not.toBeInTheDocument();
    expect(screen.getByText("v1.2.0")).toBeInTheDocument();
  });

  it("switching the version selector switches which item the row represents", async () => {
    const user = userEvent.setup();
    renderWith([
      ucItem("acme-widget", "1.2.0", { description: "old build" }),
      ucItem("acme-widget", "1.3.0", { description: "new build" }),
    ]);

    expect(screen.getByText("new build")).toBeInTheDocument();
    await user.selectOptions(screen.getByLabelText("Version for acme-widget"), "1.2.0");
    expect(screen.getByText("old build")).toBeInTheDocument();
    expect(screen.queryByText("new build")).not.toBeInTheDocument();
  });

  it.each([
    ["core-too-old", "Needs newer Jenkins"],
    ["dep-below-minimum", "Dependency below minimum"],
    ["lock-too-old", "Lock too old"],
  ])("renders a badge for the %s verdict", (verdict, label) => {
    renderWith([
      ucItem("acme-widget", "1.2.0", {
        compat: [{ profile: "lts", verdict, message: "why" }],
      }),
    ]);
    expect(screen.getByText(new RegExp(label))).toBeInTheDocument();
  });

  it("shows the worst verdict across profiles", () => {
    renderWith([
      ucItem("acme-widget", "1.2.0", {
        compat: [
          { profile: "a", verdict: "lock-too-old" },
          { profile: "b", verdict: "core-too-old" },
          { profile: "c", verdict: "compatible" },
        ],
      }),
    ]);
    expect(screen.getByText(/Needs newer Jenkins/)).toBeInTheDocument();
    expect(screen.queryByText(/Lock too old/)).not.toBeInTheDocument();
  });

  it.each([["compatible"], ["unknown"]])(
    "renders no warning badge for the %s verdict",
    (verdict) => {
      renderWith([
        ucItem("acme-widget", "1.2.0", { compat: [{ profile: "lts", verdict }] }),
      ]);
      expect(screen.queryByText(/Needs newer Jenkins/)).not.toBeInTheDocument();
      expect(screen.queryByText(/Lock too old/)).not.toBeInTheDocument();
      expect(screen.queryByText(/Dependency below minimum/)).not.toBeInTheDocument();
    },
  );

  // The visual expression of D6: derivability blocks, compatibility advises.
  it("a warning badge never disables selection", async () => {
    const user = userEvent.setup();
    renderWith([
      ucItem("acme-widget", "1.2.0", {
        compat: [{ profile: "lts", verdict: "core-too-old", message: "needs 2.555.1" }],
      }),
    ]);

    const addButton = screen.getByRole("button", { name: /Add to bundle/ });
    expect(addButton).not.toBeDisabled();
    await user.click(addButton);
    expect(screen.getByText(/Added/)).toBeInTheDocument();
  });
});
