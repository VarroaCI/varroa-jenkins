import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { CommandPalette } from "./CommandPalette";

// Mock useNavigate so we can assert navigation calls
const mockNavigate = vi.fn();
vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual("react-router-dom");
  return { ...actual, useNavigate: () => mockNavigate };
});

// Mock the API call for search
const mockBffFetch = vi.fn();
vi.mock("../hooks/useApi", () => ({
  bffFetch: (...args: unknown[]) => mockBffFetch(...args),
}));

function createTestQueryClient() {
  return new QueryClient({
    defaultOptions: {
      queries: { retry: false, staleTime: 0, refetchOnWindowFocus: false },
      mutations: { retry: false },
    },
  });
}

function renderPalette() {
  const queryClient = createTestQueryClient();
  return render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <CommandPalette />
      </QueryClientProvider>
    </MemoryRouter>,
  );
}

describe("CommandPalette", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockBffFetch.mockReset();
  });

  it("is hidden initially (returns null, not in the DOM)", () => {
    const { container } = renderPalette();
    expect(container.innerHTML).toBe("");
  });

  it("opens when Ctrl+K / Meta+K keyboard shortcut is pressed", async () => {
    renderPalette();
    await userEvent.keyboard("{Meta>}k{/Meta}");
    expect(
      screen.getByPlaceholderText("Search controllers, groups, templates..."),
    ).toBeInTheDocument();
  });

  it("opens when Ctrl+K is used (fallback for non-Meta platforms)", async () => {
    renderPalette();
    await userEvent.keyboard("{Control>}k{/Control}");
    expect(
      screen.getByPlaceholderText("Search controllers, groups, templates..."),
    ).toBeInTheDocument();
  });

  it("closes when Escape key is pressed", async () => {
    renderPalette();
    // Open first
    await userEvent.keyboard("{Meta>}k{/Meta}");
    expect(
      screen.getByPlaceholderText("Search controllers, groups, templates..."),
    ).toBeInTheDocument();
    // Close with Escape
    await userEvent.keyboard("{Escape}");
    expect(
      screen.queryByPlaceholderText("Search controllers, groups, templates..."),
    ).not.toBeInTheDocument();
  });

  it("displays sections and groups of search results", async () => {
    mockBffFetch.mockResolvedValue({items: [
      { type: "controller", name: "jenkins-main", namespace: "default", link: "/controllers/default/jenkins-main" },
      { type: "group", name: "dev-team", namespace: "default", link: "/groups/default/dev-team" },
    ]});

    renderPalette();
    await userEvent.keyboard("{Meta>}k{/Meta}");

    const input = screen.getByPlaceholderText("Search controllers, groups, templates...");
    await userEvent.type(input, "jenkins");

    // Wait for results and check group labels
    expect(await screen.findByText("controllers")).toBeInTheDocument();
    expect(screen.getByText("groups")).toBeInTheDocument();
    expect(screen.getByText("jenkins-main")).toBeInTheDocument();
    expect(screen.getByText("dev-team")).toBeInTheDocument();
  });

  it("shows empty state when no results match the query", async () => {
    mockBffFetch.mockResolvedValue({items: []});

    renderPalette();
    await userEvent.keyboard("{Meta>}k{/Meta}");

    const input = screen.getByPlaceholderText("Search controllers, groups, templates...");
    await userEvent.type(input, "nonexistent");

    expect(await screen.findByText(/No results for/)).toBeInTheDocument();
    expect(screen.getByText(/nonexistent/)).toBeInTheDocument();
  });

  it("search input filters visible items as user types", async () => {
    mockBffFetch.mockImplementation((url: string) => {
      const q = new URL(url, "http://localhost").searchParams.get("q") ?? "";
      if (q === "jenkins-main") {
        return Promise.resolve({items: [
          { type: "controller", name: "jenkins-main", namespace: "default", link: "/controllers/default/jenkins-main" },
        ]});
      }
      if (q === "jenkins") {
        return Promise.resolve({items: [
          { type: "controller", name: "jenkins-main", namespace: "default", link: "/controllers/default/jenkins-main" },
          { type: "controller", name: "jenkins-dev", namespace: "default", link: "/controllers/default/jenkins-dev" },
        ]});
      }
      return Promise.resolve({items: []});
    });

    renderPalette();
    await userEvent.keyboard("{Meta>}k{/Meta}");

    const input = screen.getByPlaceholderText("Search controllers, groups, templates...");
    await userEvent.type(input, "jenkins");
    expect(await screen.findByText("jenkins-main")).toBeInTheDocument();
    expect(screen.getByText("jenkins-dev")).toBeInTheDocument();
  });

  it("arrow keys navigate through filtered results and Enter selects highlighted item", async () => {
    mockBffFetch.mockResolvedValue({items: [
      { type: "controller", name: "jenkins-main", namespace: "default", link: "/controllers/default/jenkins-main" },
      { type: "controller", name: "jenkins-dev", namespace: "default", link: "/controllers/default/jenkins-dev" },
    ]});

    renderPalette();
    await userEvent.keyboard("{Meta>}k{/Meta}");

    const input = screen.getByPlaceholderText("Search controllers, groups, templates...");
    await userEvent.type(input, "jenkins");
    await screen.findByText("jenkins-main");

    // Press ArrowDown to select second item, then Enter
    await userEvent.keyboard("{ArrowDown}{Enter}");

    // Should navigate to the second item's link
    expect(mockNavigate).toHaveBeenCalledWith("/controllers/default/jenkins-dev");
  });

  it("pressing Enter on first item (default selected) navigates to it", async () => {
    mockBffFetch.mockResolvedValue({items: [
      { type: "controller", name: "jenkins-main", namespace: "default", link: "/controllers/default/jenkins-main" },
    ]});

    renderPalette();
    await userEvent.keyboard("{Meta>}k{/Meta}");

    const input = screen.getByPlaceholderText("Search controllers, groups, templates...");
    await userEvent.type(input, "jenkins");
    await screen.findByText("jenkins-main");

    await userEvent.keyboard("{Enter}");

    expect(mockNavigate).toHaveBeenCalledWith("/controllers/default/jenkins-main");
  });

  it("closes the palette after navigating to a result", async () => {
    mockBffFetch.mockResolvedValue({items: [
      { type: "controller", name: "jenkins-main", namespace: "default", link: "/controllers/default/jenkins-main" },
    ]});

    renderPalette();
    await userEvent.keyboard("{Meta>}k{/Meta}");

    const input = screen.getByPlaceholderText("Search controllers, groups, templates...");
    await userEvent.type(input, "jenkins");
    await screen.findByText("jenkins-main");

    await userEvent.keyboard("{Enter}");

    // Palette should be closed
    expect(
      screen.queryByPlaceholderText("Search controllers, groups, templates..."),
    ).not.toBeInTheDocument();
  });

  it("navigates when clicking a result item", async () => {
    mockBffFetch.mockResolvedValue({items: [
      { type: "controller", name: "jenkins-main", namespace: "default", link: "/controllers/default/jenkins-main" },
    ]});

    renderPalette();
    await userEvent.keyboard("{Meta>}k{/Meta}");

    const input = screen.getByPlaceholderText("Search controllers, groups, templates...");
    await userEvent.type(input, "jenkins");

    const resultBtn = await screen.findByText("jenkins-main");
    await userEvent.click(resultBtn);

    expect(mockNavigate).toHaveBeenCalledWith("/controllers/default/jenkins-main");
  });

  it("toggles closed when pressing Meta+K again", async () => {
    renderPalette();
    // Open
    await userEvent.keyboard("{Meta>}k{/Meta}");
    expect(
      screen.getByPlaceholderText("Search controllers, groups, templates..."),
    ).toBeInTheDocument();

    // Toggle closed
    await userEvent.keyboard("{Meta>}k{/Meta}");
    expect(
      screen.queryByPlaceholderText("Search controllers, groups, templates..."),
    ).not.toBeInTheDocument();
  });
});
