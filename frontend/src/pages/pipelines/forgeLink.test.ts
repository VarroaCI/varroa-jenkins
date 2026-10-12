import { describe, it, expect } from "vitest";
import { matchForgeShape, repositoryHref, fileHref, commitHref } from "./forgeLink";

describe("matchForgeShape", () => {
  it("recognizes the https form for both hosts", () => {
    expect(matchForgeShape("https://github.com/example/app")).toEqual({
      host: "github.com",
      owner: "example",
      repo: "app",
    });
    expect(matchForgeShape("https://gitlab.com/group/project")).toEqual({
      host: "gitlab.com",
      owner: "group",
      repo: "project",
    });
  });

  it("recognizes the ssh form for both hosts", () => {
    expect(matchForgeShape("ssh://git@github.com/example/app")).toEqual({
      host: "github.com",
      owner: "example",
      repo: "app",
    });
    expect(matchForgeShape("ssh://git@gitlab.com/group/project")).toEqual({
      host: "gitlab.com",
      owner: "group",
      repo: "project",
    });
  });

  it("recognizes the scp-like form for both hosts", () => {
    expect(matchForgeShape("git@github.com:example/app")).toEqual({
      host: "github.com",
      owner: "example",
      repo: "app",
    });
    expect(matchForgeShape("git@gitlab.com:group/project")).toEqual({
      host: "gitlab.com",
      owner: "group",
      repo: "project",
    });
  });

  it("strips one trailing .git", () => {
    expect(matchForgeShape("https://github.com/example/app.git")).toEqual({
      host: "github.com",
      owner: "example",
      repo: "app",
    });
    expect(matchForgeShape("git@github.com:example/app.git")).toEqual({
      host: "github.com",
      owner: "example",
      repo: "app",
    });
  });

  it("matches the host case-insensitively", () => {
    expect(matchForgeShape("https://GitHub.com/example/app")?.host).toBe("github.com");
  });

  it("rejects every other host", () => {
    expect(matchForgeShape("https://bitbucket.org/example/app")).toBeNull();
    expect(matchForgeShape("git@git.example.com:example/app")).toBeNull();
  });

  it("rejects a URL that fits none of the three forms", () => {
    expect(matchForgeShape("git://github.com/example/app")).toBeNull();
    expect(matchForgeShape("github.com/example/app")).toBeNull();
  });

  it("rejects a wrong segment count", () => {
    expect(matchForgeShape("https://github.com/example/app/sub")).toBeNull();
    expect(matchForgeShape("https://github.com/example")).toBeNull();
  });

  it("rejects an empty owner or repository segment", () => {
    expect(matchForgeShape("https://github.com//app")).toBeNull();
    expect(matchForgeShape("https://github.com/example/app/")).toBeNull();
  });

  it("rejects a query string or a fragment in the repository segment", () => {
    expect(matchForgeShape("https://github.com/example/app?ref=main")).toBeNull();
    expect(matchForgeShape("https://github.com/example/app#readme")).toBeNull();
  });
});

describe("repositoryHref", () => {
  it("builds the GitHub tree link", () => {
    expect(repositoryHref({ host: "github.com", owner: "example", repo: "casc-bundles" }, "9f1c2d3")).toBe(
      "https://github.com/example/casc-bundles/tree/9f1c2d3",
    );
  });

  it("builds the GitLab tree link", () => {
    expect(repositoryHref({ host: "gitlab.com", owner: "group", repo: "casc-bundles" }, "9f1c2d3")).toBe(
      "https://gitlab.com/group/casc-bundles/-/tree/9f1c2d3",
    );
  });
});

describe("fileHref", () => {
  it("builds the GitHub blob link", () => {
    expect(fileHref({ host: "github.com", owner: "example", repo: "app" }, "9f1c2d3", "Jenkinsfile")).toBe(
      "https://github.com/example/app/blob/9f1c2d3/Jenkinsfile",
    );
  });

  it("builds the GitLab blob link", () => {
    expect(fileHref({ host: "gitlab.com", owner: "group", repo: "app" }, "9f1c2d3", "ci/Jenkinsfile")).toBe(
      "https://gitlab.com/group/app/-/blob/9f1c2d3/ci/Jenkinsfile",
    );
  });
});

describe("commitHref", () => {
  it("builds a GitHub commit URL", () => {
    expect(commitHref({ host: "github.com", owner: "example", repo: "app" }, "abc123")).toBe(
      "https://github.com/example/app/commit/abc123",
    );
  });

  it("builds a GitLab commit URL", () => {
    expect(commitHref({ host: "gitlab.com", owner: "group", repo: "app" }, "abc123")).toBe(
      "https://gitlab.com/group/app/-/commit/abc123",
    );
  });
});
