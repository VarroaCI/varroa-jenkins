import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { JenkinsLinkRedirect } from "./JenkinsLinkRedirect";

const mockGetLink = vi.fn();
vi.mock("../api/client", () => ({
  getControllerJenkinsLink: (...args: unknown[]) => mockGetLink(...args),
}));

// Shows the location the router lands on after the redirect.
function Landed() {
  const loc = useLocation();
  return <div data-testid="landed">{loc.pathname}</div>;
}

function renderAt(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <Routes>
        <Route path="headless/:cluster/:namespace/:controller/*" element={<JenkinsLinkRedirect />} />
        <Route path="pipelines/*" element={<Landed />} />
        <Route path="controllers/:cluster/:namespace/:name" element={<div>controller</div>} />
      </Routes>
    </MemoryRouter>,
  );
}

describe("JenkinsLinkRedirect", () => {
  beforeEach(() => {
    mockGetLink.mockReset();
  });

  it("navigates to the link the BFF returns", async () => {
    mockGetLink.mockResolvedValue({ link: "/pipelines/team-a/team-a%2Fapp/run-1" });
    renderAt("/headless/core/team-a/ci/job/team-a/job/app/12/display/redirect");

    await waitFor(() =>
      expect(screen.getByTestId("landed").textContent).toBe("/pipelines/team-a/team-a%2Fapp/run-1"),
    );
    expect(mockGetLink).toHaveBeenCalledWith(
      "core",
      "team-a",
      "ci",
      "job/team-a/job/app/12/display/redirect",
    );
  });

  it("keeps an encoded %252F in the request path", async () => {
    mockGetLink.mockResolvedValue({ link: "/pipelines/t/job" });
    renderAt("/headless/core/t/r/job/t/job/r/job/feature%252Fx/3/");

    await waitFor(() => expect(mockGetLink).toHaveBeenCalled());
    const pathArg = mockGetLink.mock.calls[0][3] as string;
    expect(pathArg).toBe("job/t/job/r/job/feature%252Fx/3/");
  });

  it("shows the not-found state when the BFF has no match", async () => {
    mockGetLink.mockRejectedValue(new Error("not found"));
    renderAt("/headless/core/team-a/ci/job/missing/");

    expect(await screen.findByText("Build page not found")).toBeTruthy();
    const link = screen.getByRole("link", { name: "Open the controller" });
    expect(link.getAttribute("href")).toBe("/controllers/core/team-a/ci");
  });
});
