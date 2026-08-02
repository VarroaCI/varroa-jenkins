import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, act } from "@testing-library/react";
import { renderWithProviders } from "../test/render-utils";
import { useComposer, ComposerProvider } from "./ComposerContext";

// ---- Mocks ----
// renderWithProviders includes ComposerProvider, so useAuth queries fire via
// AuthProvider.  Give them a harmless default so they don't error.
const mockBffFetch = vi.fn();
vi.mock("../hooks/useApi", () => ({
  bffFetch: (...args: unknown[]) => mockBffFetch(...args),
  getToken: vi.fn(() => null),
  logout: vi.fn(),
}));

beforeEach(() => {
  localStorage.clear();
  mockBffFetch.mockResolvedValue({});
});

// ---- Test consumer component ----

function ComposerTestComponent() {
  const composer = useComposer();
  return (
    <div>
      <span data-testid="items-count">{composer.items.length}</span>
      <span data-testid="vars-count">{Object.keys(composer.variables).length}</span>
      <ul data-testid="items-list">
        {composer.items.map((item) => (
          <li key={item.name} data-testid={`item-${item.name}`}>
            {item.name}
          </li>
        ))}
      </ul>
      <span data-testid="has-item-a">{String(composer.hasItem({ name: "item-a" }))}</span>
      <span data-testid="has-item-b">{String(composer.hasItem({ name: "item-b" }))} </span>

      <button
        data-testid="add-item-a"
        onClick={() => composer.addItem({ name: "item-a" })}
      >
        Add A
      </button>
      <button
        data-testid="add-item-b"
        onClick={() => composer.addItem({ name: "item-b" })}
      >
        Add B
      </button>
      <button data-testid="remove-item-a" onClick={() => composer.removeItem({ name: "item-a" })}>
        Remove A
      </button>
      <span data-testid="has-a-theme">{String(composer.hasItem({ name: "theme", namespace: "a" }))}</span>
      <span data-testid="has-b-theme">{String(composer.hasItem({ name: "theme", namespace: "b" }))}</span>
      <button data-testid="add-a-theme" onClick={() => composer.addItem({ name: "theme", namespace: "a" })}>
        Add a/theme
      </button>
      <button data-testid="add-b-theme" onClick={() => composer.addItem({ name: "theme", namespace: "b" })}>
        Add b/theme
      </button>
      <button data-testid="remove-a-theme" onClick={() => composer.removeItem({ name: "theme", namespace: "a" })}>
        Remove a/theme
      </button>
      <button
        data-testid="reorder-0-1"
        onClick={() => composer.reorderItem(0, 1)}
      >
        Reorder 0 to 1
      </button>
      <button data-testid="set-var" onClick={() => composer.setVar("key1", "val1")}>
        Set Var
      </button>
      <button data-testid="clear" onClick={() => composer.clear()}>
        Clear
      </button>
      <button data-testid="clear-persisted" onClick={() => composer.clearPersisted()}>
        Clear persisted
      </button>
      <button
        data-testid="load-state"
        onClick={() => composer.load([{ name: "loaded-item" }], {}, "42", "hive")}
      >
        Load state
      </button>
      <span data-testid="spec-json">{JSON.stringify(composer.toSpec("Test Display"))}</span>
    </div>
  );
}

// ---- Tests ----

describe("ComposerContext", () => {
  it("does not create a localStorage key for a pristine mount", () => {
    render(<ComposerProvider storageKey="draft-key"><ComposerTestComponent /></ComposerProvider>);

    expect(localStorage.getItem("draft-key")).toBeNull();
  });

  it("persists CLEAR after load with empty items and the cluster retained", () => {
    render(<ComposerProvider storageKey="draft-key"><ComposerTestComponent /></ComposerProvider>);

    act(() => {
      screen.getByTestId("load-state").click();
      screen.getByTestId("clear").click();
    });

    expect(JSON.parse(localStorage.getItem("draft-key")!)).toMatchObject({
      items: [],
      variables: {},
      cluster: "hive",
    });
  });

  it("clearPersisted removes the key and does not recreate it after effects flush", async () => {
    localStorage.setItem(
      "draft-key",
      JSON.stringify({ items: [{ name: "existing" }], variables: {}, cluster: "hive" }),
    );
    render(<ComposerProvider storageKey="draft-key"><ComposerTestComponent /></ComposerProvider>);

    act(() => {
      screen.getByTestId("clear-persisted").click();
    });
    await act(async () => {});

    expect(localStorage.getItem("draft-key")).toBeNull();
  });

  it("persists cluster and baseVersion when loading a draft", () => {
    render(<ComposerProvider storageKey="draft-key"><ComposerTestComponent /></ComposerProvider>);

    act(() => {
      screen.getByTestId("load-state").click();
    });

    expect(JSON.parse(localStorage.getItem("draft-key")!)).toMatchObject({
      cluster: "hive",
      baseVersion: "42",
    });
  });

  it("resumes persistence after clearPersisted when the composer is mutated", () => {
    localStorage.setItem(
      "draft-key",
      JSON.stringify({ items: [{ name: "existing" }], variables: {}, cluster: "hive" }),
    );
    render(<ComposerProvider storageKey="draft-key"><ComposerTestComponent /></ComposerProvider>);

    act(() => {
      screen.getByTestId("clear-persisted").click();
    });
    act(() => {
      screen.getByTestId("add-item-a").click();
    });

    expect(JSON.parse(localStorage.getItem("draft-key")!)).toMatchObject({
      items: [{ name: "item-a" }],
      cluster: "hive",
    });
  });

  it("provides initial state with empty items and empty variables", () => {
    renderWithProviders(<ComposerTestComponent />);

    expect(screen.getByTestId("items-count")).toHaveTextContent("0");
    expect(screen.getByTestId("vars-count")).toHaveTextContent("0");
  });

  it("addItem adds an item, duplicate is ignored", () => {
    renderWithProviders(<ComposerTestComponent />);

    act(() => {
      screen.getByTestId("add-item-a").click();
    });
    expect(screen.getByTestId("items-count")).toHaveTextContent("1");
    expect(screen.getByTestId("has-item-a")).toHaveTextContent("true");

    // Adding the same item again is a no-op.
    act(() => {
      screen.getByTestId("add-item-a").click();
    });
    expect(screen.getByTestId("items-count")).toHaveTextContent("1");
  });

  it("removeItem removes an item by name", () => {
    renderWithProviders(<ComposerTestComponent />);

    act(() => {
      screen.getByTestId("add-item-a").click();
      screen.getByTestId("add-item-b").click();
    });
    expect(screen.getByTestId("items-count")).toHaveTextContent("2");

    act(() => {
      screen.getByTestId("remove-item-a").click();
    });
    expect(screen.getByTestId("items-count")).toHaveTextContent("1");
    expect(screen.getByTestId("has-item-a")).toHaveTextContent("false");
    expect(screen.getByTestId("has-item-b")).toHaveTextContent("true");
  });

  it("stages same-named items from different namespaces as distinct entries and removes only the target", () => {
    renderWithProviders(<ComposerTestComponent />);

    act(() => {
      screen.getByTestId("add-a-theme").click();
      screen.getByTestId("add-b-theme").click();
    });
    expect(screen.getByTestId("items-count")).toHaveTextContent("2");
    expect(screen.getByTestId("has-a-theme")).toHaveTextContent("true");
    expect(screen.getByTestId("has-b-theme")).toHaveTextContent("true");

    act(() => {
      screen.getByTestId("remove-a-theme").click();
    });
    expect(screen.getByTestId("items-count")).toHaveTextContent("1");
    expect(screen.getByTestId("has-a-theme")).toHaveTextContent("false");
    expect(screen.getByTestId("has-b-theme")).toHaveTextContent("true");
  });

  it("reorderItem moves an item from one index to another", () => {
    renderWithProviders(<ComposerTestComponent />);

    act(() => {
      screen.getByTestId("add-item-a").click();
      screen.getByTestId("add-item-b").click();
    });

    // Default order: [item-a, item-b]
    const listBefore = screen.getByTestId("items-list");
    expect(listBefore.children[0]).toHaveTextContent("item-a");
    expect(listBefore.children[1]).toHaveTextContent("item-b");

    act(() => {
      screen.getByTestId("reorder-0-1").click();
    });

    // After reorder: [item-b, item-a]
    const listAfter = screen.getByTestId("items-list");
    expect(listAfter.children[0]).toHaveTextContent("item-b");
    expect(listAfter.children[1]).toHaveTextContent("item-a");
  });

  it("setVar sets a variable", () => {
    renderWithProviders(<ComposerTestComponent />);

    act(() => {
      screen.getByTestId("set-var").click();
    });

    expect(screen.getByTestId("vars-count")).toHaveTextContent("1");
  });

  it("clear resets everything", () => {
    renderWithProviders(<ComposerTestComponent />);

    act(() => {
      screen.getByTestId("add-item-a").click();
      screen.getByTestId("add-item-b").click();
      screen.getByTestId("set-var").click();
    });

    expect(screen.getByTestId("items-count")).toHaveTextContent("2");
    expect(screen.getByTestId("vars-count")).toHaveTextContent("1");

    act(() => {
      screen.getByTestId("clear").click();
    });

    expect(screen.getByTestId("items-count")).toHaveTextContent("0");
    expect(screen.getByTestId("vars-count")).toHaveTextContent("0");
  });

  it("hasItem returns boolean for item presence", () => {
    renderWithProviders(<ComposerTestComponent />);

    expect(screen.getByTestId("has-item-a")).toHaveTextContent("false");

    act(() => {
      screen.getByTestId("add-item-a").click();
    });

    expect(screen.getByTestId("has-item-a")).toHaveTextContent("true");
  });

  it("toSpec returns a ComposedBundleSpec with inputs and variables", () => {
    renderWithProviders(<ComposerTestComponent />);

    act(() => {
      screen.getByTestId("add-item-a").click();
      screen.getByTestId("add-item-b").click();
      screen.getByTestId("set-var").click();
    });

    const specText = screen.getByTestId("spec-json").textContent;
    const spec = JSON.parse(specText!);

    expect(spec).toMatchObject({
      displayName: "Test Display",
      inputs: [{ itemRef: { name: "item-a" } }, { itemRef: { name: "item-b" } }],
      variables: { key1: "val1" },
    });
    expect(spec.items).toBeUndefined();
  });

  it("storageKey=null provider does not read or write localStorage", () => {
    // Set up pre-existing localStorage state to prove it's not read
    localStorage.setItem("varroa_composer_draft", JSON.stringify({ items: [{ name: "stale-item" }], variables: { x: "y" } }));

    render(
      <ComposerProvider storageKey={null}>
        <ComposerTestComponent />
      </ComposerProvider>
    );

    // Should NOT have loaded the stale item
    expect(screen.getByTestId("items-count")).toHaveTextContent("0");
    expect(screen.getByTestId("vars-count")).toHaveTextContent("0");

    // Add an item — localStorage should remain untouched
    act(() => {
      screen.getByTestId("add-item-a").click();
    });
    expect(screen.getByTestId("items-count")).toHaveTextContent("1");

    // localStorage should still contain the original stale data, not the new state
    const stored = localStorage.getItem("varroa_composer_draft");
    expect(stored).toBe(JSON.stringify({ items: [{ name: "stale-item" }], variables: { x: "y" } }));
  });

  it("default provider reads from localStorage and persists changes", () => {
    localStorage.setItem("varroa_composer_draft", JSON.stringify({ items: [{ name: "preloaded-item" }], variables: {} }));

    render(
      <ComposerProvider>
        <ComposerTestComponent />
      </ComposerProvider>
    );

    // Should have loaded the pre-existing item
    expect(screen.getByTestId("items-count")).toHaveTextContent("1");
    expect(screen.getByTestId("has-item-a")).toHaveTextContent("false");
  });

  it("load() replaces state atomically and de-dupes items by itemRefKey", () => {
    function LoadTestComponent() {
      const composer = useComposer();
      return (
        <div>
          <span data-testid="items-count">{composer.items.length}</span>
          <span data-testid="items-names">{composer.items.map((i) => i.name).join(",")}</span>
          <button
            data-testid="load"
            onClick={() =>
              composer.load(
                [
                  { name: "dup" },
                  { name: "unique" },
                  { name: "dup" }, // same key as first — must be dropped
                  { name: "dup", namespace: "other" }, // different key — kept
                ],
                {},
                "42",
              )
            }
          >
            Load
          </button>
        </div>
      );
    }
    render(
      <ComposerProvider storageKey={null}>
        <LoadTestComponent />
      </ComposerProvider>,
    );

    act(() => {
      screen.getByTestId("load").click();
    });
    expect(screen.getByTestId("items-count")).toHaveTextContent("3");
    expect(screen.getByTestId("items-names")).toHaveTextContent("dup,unique,dup");
  });

  it("de-dupes items when hydrating a persisted draft", () => {
    localStorage.setItem(
      "varroa_composer_draft",
      JSON.stringify({ items: [{ name: "item-a" }, { name: "item-a" }, { name: "item-b" }], variables: {} }),
    );

    render(
      <ComposerProvider>
        <ComposerTestComponent />
      </ComposerProvider>,
    );

    expect(screen.getByTestId("items-count")).toHaveTextContent("2");
    expect(screen.getByTestId("has-item-a")).toHaveTextContent("true");
    expect(screen.getByTestId("has-item-b")).toHaveTextContent("true");
  });

  it("throws when used outside ComposerProvider", () => {
    const errSpy = vi.spyOn(console, "error").mockImplementation(() => {});

    function BadComponent() {
      let msg = "";
      try {
        useComposer();
      } catch (e) {
        msg = (e as Error).message;
      }
      return <div data-testid="err">{msg}</div>;
    }

    // Render WITHOUT ComposerProvider by using bare render.
    render(<BadComponent />);

    expect(screen.getByTestId("err")).toHaveTextContent(
      "useComposer must be used within ComposerProvider",
    );
    errSpy.mockRestore();
  });
});
