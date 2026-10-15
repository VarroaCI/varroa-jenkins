import { describe, it, expect } from "vitest";
import {
  matchForgeShape,
  repositoryHref,
  fileHref,
  commitHref,
  libraryHref,
  repositoryBase,
  revisionHref,
  forgeFileHref,
  forgeTreeHref,
} from "./forgeLink";

describe("libraryHref", () => {
  it("links a github library at its commit and at its version", () => {
    expect(libraryHref("https://github.com/o/r.git", "github", "abc123", "1.0.0")).toBe(
      "https://github.com/o/r/commit/abc123",
    );
    expect(libraryHref("https://github.com/o/r", "github", undefined, "1.0.0")).toBe(
      "https://github.com/o/r/tree/1.0.0",
    );
  });

  it("links a gitlab library through the dash segment", () => {
    expect(libraryHref("https://gitlab.com/o/r", "gitlab", "abc123")).toBe("https://gitlab.com/o/r/-/commit/abc123");
    expect(libraryHref("https://gitlab.com/o/r", "gitlab", undefined, "v2")).toBe("https://gitlab.com/o/r/-/tree/v2");
  });

  it("links a gitea library through the commit and src paths", () => {
    expect(libraryHref("https://git.example.com/t/lib", "gitea", "abc123")).toBe(
      "https://git.example.com/t/lib/commit/abc123",
    );
    expect(libraryHref("https://git.example.com/t/lib", "gitea", undefined, "main")).toBeNull();
    expect(repositoryBase("https://git.example.com/t/lib.git/")).toBe("https://git.example.com/t/lib");
  });

  it("maps the ssh and scp forms to https", () => {
    expect(libraryHref("ssh://git@github.com/o/r.git", "github", "abc")).toBe("https://github.com/o/r/commit/abc");
    expect(libraryHref("git@github.com:o/r.git", "github", "abc")).toBe("https://github.com/o/r/commit/abc");
  });

  it("strips userinfo from an https URL", () => {
    expect(libraryHref("https://user:tok@github.com/o/r", "github", "abc")).toBe("https://github.com/o/r/commit/abc");
  });

  it("answers null for an unknown forge, an empty URL, or a query", () => {
    expect(libraryHref("https://example.org/o/r", undefined, "abc")).toBeNull();
    expect(libraryHref("https://example.org/o/r", "bitbucket", "abc")).toBeNull();
    expect(libraryHref("", "github", "abc")).toBeNull();
    expect(libraryHref("https://github.com/o/r?x=1", "github", "abc")).toBeNull();
    expect(libraryHref("https://github.com/o/r", "github")).toBeNull();
  });

  it("keeps a branch with a slash as path segments", () => {
    expect(libraryHref("https://github.com/o/r", "github", undefined, "feature/x y")).toBe(
      "https://github.com/o/r/tree/feature/x%20y",
    );
  });

  it("answers null for a URL with fewer than two path segments", () => {
    expect(repositoryBase("https://github.com/only")).toBeNull();
  });
});

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

describe("revisionHref", () => {
  it("links a Gitea repository through the commit path", () => {
    expect(revisionHref("https://gitea.example.com/acme/app", "gitea", "abc123")).toBe(
      "https://gitea.example.com/acme/app/commit/abc123",
    );
  });

  it("reads a Gitea scp-like remote as its https base", () => {
    expect(revisionHref("git@gitea.example.com:acme/app.git", "gitea", "abc123")).toBe(
      "https://gitea.example.com/acme/app/commit/abc123",
    );
  });

  it("uses the dash segment for a self-hosted GitLab with a forge", () => {
    expect(revisionHref("https://gitlab.example.org/g/app", "gitlab", "abc123")).toBe(
      "https://gitlab.example.org/g/app/-/commit/abc123",
    );
  });

  it("answers null for an unknown forge on an unknown host", () => {
    expect(revisionHref("https://git.example.org/x/y", undefined, "abc123")).toBeNull();
  });

  it("links a github.com repository with no forge through the shape", () => {
    expect(revisionHref("https://github.com/example/app", undefined, "abc123")).toBe(
      "https://github.com/example/app/commit/abc123",
    );
  });

  it("answers null without a URL or a revision", () => {
    expect(revisionHref(undefined, "gitea", "abc123")).toBeNull();
    expect(revisionHref("https://gitea.example.com/o/r", "gitea", "")).toBeNull();
  });
});

describe("forgeFileHref", () => {
  const sha = "abc123";
  it("links a Gitea file through the src commit path", () => {
    expect(forgeFileHref("https://gitea.example.com/acme/app", "gitea", sha, "ci/Jenkinsfile")).toBe(
      `https://gitea.example.com/acme/app/src/commit/${sha}/ci/Jenkinsfile`,
    );
  });

  it("links a GitLab and a GitHub file through their blob paths", () => {
    expect(forgeFileHref("https://gitlab.example.com/acme/app", "gitlab", sha, "Jenkinsfile")).toBe(
      `https://gitlab.example.com/acme/app/-/blob/${sha}/Jenkinsfile`,
    );
    expect(forgeFileHref("https://github.com/acme/app", "github", sha, "Jenkinsfile")).toBe(
      `https://github.com/acme/app/blob/${sha}/Jenkinsfile`,
    );
  });

  it("encodes each path segment", () => {
    expect(forgeFileHref("https://gitea.example.com/acme/app", "gitea", sha, "ci/my file")).toBe(
      `https://gitea.example.com/acme/app/src/commit/${sha}/ci/my%20file`,
    );
  });

  it("falls back to the GitHub shape with no forge, and answers null for an unknown host", () => {
    const url = "https://github.com/acme/app";
    expect(forgeFileHref(url, undefined, sha, "Jenkinsfile")).toBe(
      fileHref(matchForgeShape(url)!, sha, "Jenkinsfile"),
    );
    expect(forgeFileHref("https://git.example.com/acme/app", undefined, sha, "Jenkinsfile")).toBeNull();
  });

  it("answers null when the commit or the path is missing", () => {
    expect(forgeFileHref("https://gitea.example.com/acme/app", "gitea", undefined, "Jenkinsfile")).toBeNull();
    expect(forgeFileHref("https://gitea.example.com/acme/app", "gitea", sha, "")).toBeNull();
  });
});

describe("forgeTreeHref", () => {
  const sha = "abc123";
  it("links each forge at a revision through its tree path", () => {
    expect(forgeTreeHref("https://gitea.example.com/acme/app", "gitea", sha)).toBe(
      `https://gitea.example.com/acme/app/src/commit/${sha}`,
    );
    expect(forgeTreeHref("https://gitlab.example.com/acme/app", "gitlab", sha)).toBe(
      `https://gitlab.example.com/acme/app/-/tree/${sha}`,
    );
    expect(forgeTreeHref("https://github.com/acme/app", "github", sha)).toBe(
      `https://github.com/acme/app/tree/${sha}`,
    );
  });

  it("falls back to the GitHub shape with no forge, and answers null for an unknown host", () => {
    expect(forgeTreeHref("https://github.com/acme/app", undefined, sha)).toBe(
      `https://github.com/acme/app/tree/${sha}`,
    );
    expect(forgeTreeHref("https://git.example.com/acme/app", undefined, sha)).toBeNull();
  });

  it("answers null when the URL or the revision is missing", () => {
    expect(forgeTreeHref(undefined, "gitea", sha)).toBeNull();
    expect(forgeTreeHref("https://gitea.example.com/acme/app", "gitea", "")).toBeNull();
  });
});
