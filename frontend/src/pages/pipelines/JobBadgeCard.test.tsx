import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClientProvider } from "@tanstack/react-query";
import { createTestQueryClient } from "../../test/render-utils";
import { JobBadgeCard, badgeSnippet, badgeURL } from "./JobBadgeCard";

const mockGetJobBadge = vi.fn();
vi.mock("../../api/client", () => ({
  getJobBadge: (...args: unknown[]) => mockGetJobBadge(...args),
}));

const badgePath = "/badges/payments/tok123/app%2Fmain.svg";

beforeEach(() => {
  mockGetJobBadge.mockReset();
});

function renderCard(kind: "pipeline" | "multibranch" | "organizationFolder" = "pipeline") {
  const client = createTestQueryClient();
  return render(
    <QueryClientProvider client={client}>
      <JobBadgeCard tenant="payments" jobPath="payments/app/main" kind={kind} jobLabel="app/main" />
    </QueryClientProvider>,
  );
}

describe("JobBadgeCard", () => {
  it("shows the preview, the URL, and the Markdown snippet when the badge is on", async () => {
    mockGetJobBadge.mockResolvedValue({ enabled: true, path: badgePath });
    renderCard();

    expect(await screen.findByRole("img", { name: "build status of app/main" })).toBeInTheDocument();
    const url = `${window.location.origin}${badgePath}`;
    expect(screen.getByDisplayValue(url)).toBeInTheDocument();
    expect(screen.getByDisplayValue(badgeSnippet(url, window.location.href))).toBeInTheDocument();
    expect(mockGetJobBadge).toHaveBeenCalledWith("payments", "payments/app/main");
  });

  it("shows only the off note when the badge is off", async () => {
    mockGetJobBadge.mockResolvedValue({ enabled: false });
    renderCard();

    expect(
      await screen.findByText("Status badge: off for this tenant. A tenant admin turns it on in the tenant settings."),
    ).toBeInTheDocument();
    expect(screen.queryByText("Status badge")).not.toBeInTheDocument();
    expect(screen.queryByRole("img")).not.toBeInTheDocument();
    expect(screen.queryByDisplayValue(/badges\//)).not.toBeInTheDocument();
  });

  it("names the main branch for a multibranch job", async () => {
    mockGetJobBadge.mockResolvedValue({ enabled: true, path: badgePath });
    renderCard("multibranch");

    const img = await screen.findByRole("img", { name: "build status of app/main" });
    expect(img.getAttribute("src")).toBe(`${window.location.origin}${badgePath}?branch=main`);
    expect(screen.getByText(/The badge shows the last build of this branch/)).toBeInTheDocument();
  });

  it("renders nothing for an organization folder", () => {
    renderCard("organizationFolder");

    expect(screen.queryByText("Status badge")).not.toBeInTheDocument();
    expect(mockGetJobBadge).not.toHaveBeenCalled();
  });

  it("copies the snippet to the clipboard", async () => {
    mockGetJobBadge.mockResolvedValue({ enabled: true, path: badgePath });
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    renderCard();

    fireEvent.click(await screen.findByRole("button", { name: "Copy Markdown" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Copied" })).toBeInTheDocument());
    const url = `${window.location.origin}${badgePath}`;
    expect(writeText).toHaveBeenCalledWith(badgeSnippet(url, window.location.href));
  });
});

describe("badgeURL", () => {
  it("adds the branch query only when a branch is given", () => {
    expect(badgeURL("/badges/a/b/c.svg", null)).toBe(`${window.location.origin}/badges/a/b/c.svg`);
    expect(badgeURL("/badges/a/b/c.svg", "main")).toBe(`${window.location.origin}/badges/a/b/c.svg?branch=main`);
  });
});

describe("JobBadgeCard branch", () => {
  it("names the branch the reader types", async () => {
    mockGetJobBadge.mockResolvedValue({ enabled: true, path: badgePath });
    renderCard("multibranch");

    const img = await screen.findByRole("img", { name: "build status of app/main" });
    fireEvent.change(screen.getByRole("textbox", { name: /Branch/ }), { target: { value: "master" } });
    expect(img.getAttribute("src")).toBe(`${window.location.origin}${badgePath}?branch=master`);
  });
});
