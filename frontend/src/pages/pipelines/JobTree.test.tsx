import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import JobTree from "./JobTree";
import type { JobTreeNode } from "./jobTree";
import { createPipelineSummary } from "../../test/factories";

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

// organizationNode is a folder-relative organization folder with two
// repositories, which are its multibranch children.
const organizationNode = {
  name: "repos",
  path: "repos",
  depth: 0,
  kind: "organizationFolder",
  children: [],
  jobCount: 2,
  runningCount: 0,
  branchCount: 0,
  repositoryCount: 2,
} as unknown as JobTreeNode;

describe("JobTree organization folder rows", () => {
  it("links the organization folder name and counts its repositories", () => {
    render(
      <MemoryRouter>
        <table>
          <tbody>
            <JobTree
              team="alpha"
              folder="alpha"
              rows={[organizationNode]}
              openPaths={new Set()}
              onToggle={() => {}}
            />
          </tbody>
        </table>
      </MemoryRouter>,
    );

    const link = screen.getByRole("link", { name: "repos" });
    expect(link.getAttribute("href")).toBe("/pipelines/alpha/alpha%2Frepos");
    expect(screen.getByText("2 repositories")).toBeInTheDocument();
  });
});

// jobNode builds a "job" row node whose summary carries the pending flag.
function jobNode(path: string, pending: boolean): JobTreeNode {
  return {
    name: path.split("/").pop(),
    path,
    depth: 0,
    kind: "job",
    children: [],
    jobCount: 1,
    runningCount: 0,
    branchCount: 0,
    summary: createPipelineSummary({ jobPath: path, pending }),
  } as unknown as JobTreeNode;
}

function renderRows(rows: JobTreeNode[]) {
  render(
    <MemoryRouter>
      <table>
        <tbody>
          <JobTree team="alpha" folder="alpha" rows={rows} openPaths={new Set()} onToggle={() => {}} />
        </tbody>
      </table>
    </MemoryRouter>,
  );
}

describe("JobTree pending badge", () => {
  it("shows a Pending badge on a pending job row", () => {
    renderRows([jobNode("acme/app-a", true)]);
    expect(screen.getByText("Pending")).toBeInTheDocument();
  });

  it("shows no badge when pending is false", () => {
    renderRows([jobNode("acme/app-a", false)]);
    expect(screen.queryByText("Pending")).not.toBeInTheDocument();
  });

  it("shows a Pending badge on a pending multibranch node", () => {
    const node = {
      ...multibranchNode,
      summary: createPipelineSummary({ jobPath: "app", kind: "multibranch", pending: true }),
    } as unknown as JobTreeNode;
    renderRows([node]);
    expect(screen.getByText("Pending")).toBeInTheDocument();
  });
});
