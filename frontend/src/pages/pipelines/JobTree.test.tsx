import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import JobTree from "./JobTree";
import type { JobTreeNode } from "./jobTree";

// multibranchNode is a folder-relative multibranch item with no built
// branch yet, so it has no child rows.
const multibranchNode: JobTreeNode = {
  name: "app",
  path: "app",
  depth: 0,
  kind: "multibranch",
  children: [],
  jobCount: 0,
  runningCount: 0,
  branchCount: 0,
} as unknown as JobTreeNode;

describe("JobTree multibranch rows", () => {
  it("links the multibranch name to its job page with the tenant folder", () => {
    render(
      <MemoryRouter>
        <table>
          <tbody>
            <JobTree
              team="alpha"
              folder="alpha"
              rows={[multibranchNode]}
              openPaths={new Set()}
              onToggle={() => {}}
            />
          </tbody>
        </table>
      </MemoryRouter>,
    );

    const link = screen.getByRole("link", { name: "app" });
    expect(link.getAttribute("href")).toBe("/pipelines/alpha/alpha%2Fapp");
  });
});
