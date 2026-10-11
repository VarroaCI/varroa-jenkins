import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes, type InitialEntry } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import TestCaseHistory, { caseKeyFromPath } from "./TestCaseHistory";
import { ApiError } from "../../hooks/useApi";
import type { TestCaseOutcome } from "../../types";

const mockGetTestCaseHistory = vi.fn();
const mockGetPipelineJob = vi.fn();

vi.mock("../../api/client", async () => {
  const actual = await vi.importActual<typeof import("../../api/client")>("../../api/client");
  return {
    ...actual,
    getTestCaseHistory: (...args: unknown[]) => mockGetTestCaseHistory(...args),
    getPipelineJob: (...args: unknown[]) => mockGetPipelineJob(...args),
  };
});

const CASE_URL = "/pipelines/payments/checkout-api%2Fmain/tests/com.example.FooTest%2Fbar";

function renderPage(entry: InitialEntry = CASE_URL) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={[entry]}>
        <Routes>
          <Route path="pipelines/:team/:job/tests/:case" element={<TestCaseHistory />} />
          <Route path="pipelines/:team/:job/:run" element={<div>build page</div>} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

function outcome(overrides: Partial<TestCaseOutcome> = {}): TestCaseOutcome {
  return {
    runId: "run-2",
    buildNumber: 42,
    suite: "unit",
    status: "FAILED",
    durationMs: 2000,
    errorDetails: "expected 1 but was 2",
    createdAt: new Date(Date.now() - 5 * 60_000).toISOString(),
    result: "FAILURE",
    runLink: "/pipelines/payments/checkout-api%2Fmain/run-2",
    ...overrides,
  };
}

beforeEach(() => {
  vi.clearAllMocks();
  mockGetPipelineJob.mockResolvedValue({ jobPath: "checkout-api/main", tenantFolder: "", tenant: "payments" });
});

describe("TestCaseHistory", () => {
  it("names the class and the test from the link state, and lists one row per outcome", async () => {
    mockGetTestCaseHistory.mockResolvedValue({
      items: [outcome(), outcome({ runId: "run-1", buildNumber: 41, runLink: "/pipelines/payments/checkout-api%2Fmain/run-1", errorDetails: undefined, durationMs: undefined })],
    });
    renderPage({ pathname: CASE_URL, state: { className: "com.example.FooTest", name: "bar" } });

    expect(await screen.findByText("com.example.FooTest")).toBeInTheDocument();
    expect(within(screen.getByRole("heading", { level: 1 })).getByText("bar")).toBeInTheDocument();
    expect(mockGetTestCaseHistory).toHaveBeenCalledWith("payments", "checkout-api/main", "com.example.FooTest/bar");

    const rows = screen.getAllByRole("row").slice(1);
    expect(rows).toHaveLength(2);
    const first = within(rows[0]).getAllByRole("cell").map((c) => c.textContent);
    expect(first).toEqual(["#42", "FAILED", "unit", "2s", "5m ago", "expected 1 but was 2"]);
    expect(within(rows[0]).getByRole("link", { name: "#42" })).toHaveAttribute(
      "href",
      "/pipelines/payments/checkout-api%2Fmain/run-2",
    );
    // A value the record omits renders nothing.
    const second = within(rows[1]).getAllByRole("cell").map((c) => c.textContent);
    expect(second[3]).toBe("");
    expect(second[5]).toBe("");

    expect(screen.getByText(/retention window/)).toBeInTheDocument();
    expect(screen.queryByRole("img")).toBeNull();
  });

  it("renders the trail with the test as the current crumb", async () => {
    mockGetPipelineJob.mockResolvedValue({
      jobPath: "payments/checkout-api/main",
      tenantFolder: "payments",
      tenant: "payments",
    });
    mockGetTestCaseHistory.mockResolvedValue({ items: [outcome()] });
    renderPage({ pathname: CASE_URL, state: { className: "com.example.FooTest", name: "bar" } });

    const trail = await screen.findByRole("navigation", { name: "Job path" });
    expect(within(trail).getByRole("link", { name: "main" })).toHaveAttribute(
      "href",
      "/pipelines/payments/payments%2Fcheckout-api%2Fmain",
    );
    expect(within(trail).getByText("bar")).toHaveAttribute("aria-current", "page");
  });

  it("keeps the job link when the job read fails", async () => {
    mockGetPipelineJob.mockRejectedValue(new ApiError(500, "boom"));
    // A failed job read never blocks the page.
    mockGetTestCaseHistory.mockResolvedValue({ items: [outcome()] });
    renderPage({ pathname: CASE_URL, state: { className: "com.example.FooTest", name: "bar" } });

    expect(await screen.findByRole("link", { name: "checkout-api/main" })).toHaveAttribute(
      "href",
      "/pipelines/payments/checkout-api%2Fmain",
    );
    expect(screen.queryByRole("navigation", { name: "Job path" })).toBeNull();
  });

  it("sets the document title from the test, the job, and the team", async () => {
    mockGetTestCaseHistory.mockResolvedValue({ items: [outcome()] });
    renderPage({ pathname: CASE_URL, state: { className: "com.example.FooTest", name: "bar" } });

    await waitFor(() => expect(document.title).toBe("bar · checkout-api/main · payments"));
  });

  it("names the case by its key when the URL carries no link state", async () => {
    mockGetTestCaseHistory.mockResolvedValue({ items: [outcome()] });
    renderPage();

    expect(await screen.findByRole("heading", { name: "com.example.FooTest/bar" })).toBeInTheDocument();
  });

  it("passes a case key that holds an encoded slash back unchanged", async () => {
    mockGetTestCaseHistory.mockResolvedValue({ items: [] });
    renderPage("/pipelines/payments/main/tests/a%252Fb%2Fc");

    await screen.findByText(/holds no outcome/);
    expect(mockGetTestCaseHistory).toHaveBeenCalledWith("payments", "main", "a%2Fb/c");
  });

  it("shows no outcome row for an empty history", async () => {
    mockGetTestCaseHistory.mockResolvedValue({ items: [] });
    renderPage({ pathname: CASE_URL, state: { className: "com.example.FooTest", name: "bar" } });

    expect(await screen.findByText("The record store holds no outcome for this test case.")).toBeInTheDocument();
    expect(within(screen.getByRole("heading", { level: 1 })).getByText("bar")).toBeInTheDocument();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("shows the not-found state after a 404", async () => {
    mockGetTestCaseHistory.mockRejectedValue(new ApiError(404, "not found"));
    renderPage();

    expect(await screen.findByText("Not found")).toBeInTheDocument();
  });

  it("shows the forbidden state after a 403", async () => {
    mockGetTestCaseHistory.mockRejectedValue(new ApiError(403, "forbidden"));
    renderPage();

    expect(await screen.findByText("Access denied")).toBeInTheDocument();
  });

  it("explains a disabled record store after a 503", async () => {
    mockGetTestCaseHistory.mockRejectedValue(new ApiError(503, "record store not configured"));
    renderPage();

    expect(await screen.findByText("Pipeline data is off")).toBeInTheDocument();
  });

  it("shows the generic error state after any other failure", async () => {
    mockGetTestCaseHistory.mockRejectedValue(new ApiError(500, "boom"));
    renderPage();

    expect(await screen.findByText("Unable to load page")).toBeInTheDocument();
  });
});

describe("caseKeyFromPath", () => {
  it("decodes the last segment once", () => {
    expect(caseKeyFromPath("/pipelines/t/j/tests/a%252Fb%2Fc")).toBe("a%2Fb/c");
  });

  it("answers undefined for a malformed segment", () => {
    expect(caseKeyFromPath("/pipelines/t/j/tests/%E0%A4%A")).toBeUndefined();
  });

  it("answers undefined for an empty path", () => {
    expect(caseKeyFromPath("/")).toBeUndefined();
  });
});
