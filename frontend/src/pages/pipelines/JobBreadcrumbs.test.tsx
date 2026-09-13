import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import JobBreadcrumbs from "./JobBreadcrumbs";

function renderTrail(team: string, tenantFolder: string, jobPath: string) {
  return render(
    <MemoryRouter>
      <JobBreadcrumbs team={team} tenantFolder={tenantFolder} jobPath={jobPath} />
    </MemoryRouter>,
  );
}

/** crumbTexts reads the trail in order. */
function crumbTexts(): string[] {
  const list = screen.getByRole("navigation", { name: "Job path" });
  return [...list.querySelectorAll("li")].map((item) => item.textContent?.replace("/", "") ?? "");
}

describe("JobBreadcrumbs", () => {
  it("names the tenant, then each folder, then the job", () => {
    renderTrail("payments", "payments", "payments/release/eu/deploy");
    expect(crumbTexts()).toEqual(["payments", "release", "eu", "deploy"]);
  });

  it("links a folder crumb to the team home with that folder and every ancestor unfolded", () => {
    renderTrail("payments", "payments", "payments/release/eu/deploy");
    const link = screen.getByRole("link", { name: "eu" });
    const href = link.getAttribute("href") ?? "";
    expect(href).toContain("open=release");
    expect(href).toContain("open=release%2Feu");
    expect(screen.getByRole("link", { name: "payments" })).toHaveAttribute("href", "/pipelines/payments");
  });

  it("marks the last crumb as the current page and renders it as text", () => {
    renderTrail("payments", "payments", "payments/release/eu/deploy");
    const current = screen.getByText("deploy");
    expect(current).toHaveAttribute("aria-current", "page");
    expect(screen.queryByRole("link", { name: "deploy" })).not.toBeInTheDocument();
  });

  it("names the controller first for a controller tenant whose folder is empty", () => {
    renderTrail("my-ctrl", "", "build/nightly");
    expect(crumbTexts()).toEqual(["my-ctrl", "build", "nightly"]);
  });

  it("shows two crumbs for a job that sits directly in the tenant folder", () => {
    renderTrail("payments", "payments", "payments/build");
    expect(crumbTexts()).toEqual(["payments", "build"]);
  });

  it("hides every separator from a screen reader", () => {
    renderTrail("payments", "payments", "payments/release/eu");
    const trail = screen.getByRole("navigation", { name: "Job path" });
    const separators = [...trail.querySelectorAll("span")].filter((s) => s.textContent === "/");
    expect(separators.length).toBeGreaterThan(0);
    for (const separator of separators) {
      expect(separator).toHaveAttribute("aria-hidden", "true");
    }
  });
});
