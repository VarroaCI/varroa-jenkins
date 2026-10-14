/** ForgeMatch names one recognized repository host and its two path
 * segments. */
export interface ForgeMatch {
  host: "github.com" | "gitlab.com";
  owner: string;
  repo: string;
}

/** matchForgeShape recognizes a GitHub or GitLab repository URL in one of
 * three forms (https, ssh://, or scp-like git@host:owner/repo), after
 * stripping one trailing ".git". Returns null for every other host or
 * shape, including a URL with more or fewer than two path segments. */
export function matchForgeShape(repoURL: string): ForgeMatch | null {
  const stripped = repoURL.replace(/\.git$/, "");
  let host = "";
  let rest = "";
  const httpsMatch = stripped.match(/^https:\/\/([^/]+)\/(.+)$/);
  const sshMatch = stripped.match(/^ssh:\/\/git@([^/]+)\/(.+)$/);
  const scpMatch = stripped.match(/^git@([^:]+):(.+)$/);
  if (httpsMatch) [, host, rest] = httpsMatch;
  else if (sshMatch) [, host, rest] = sshMatch;
  else if (scpMatch) [, host, rest] = scpMatch;
  else return null;

  if (host.toLowerCase() !== "github.com" && host.toLowerCase() !== "gitlab.com") return null;
  // Exactly two segments, both non-empty: no trailing slash, no extra
  // path depth, no query string or fragment folded into <repo>.
  const segments = rest.split("/");
  if (segments.length !== 2 || !segments[0] || !segments[1] || /[?#]/.test(segments[1])) return null;
  return { host: host.toLowerCase() as "github.com" | "gitlab.com", owner: segments[0], repo: segments[1] };
}

/** repositoryHref builds the "browse at a revision" link for a forge match. */
export function repositoryHref(match: ForgeMatch, revision: string): string {
  return match.host === "github.com"
    ? `https://github.com/${match.owner}/${match.repo}/tree/${revision}`
    : `https://gitlab.com/${match.owner}/${match.repo}/-/tree/${revision}`;
}

/** fileHref builds the "view this file at a revision" link for a forge match. */
export function fileHref(match: ForgeMatch, revision: string, path: string): string {
  return match.host === "github.com"
    ? `https://github.com/${match.owner}/${match.repo}/blob/${revision}/${path}`
    : `https://gitlab.com/${match.owner}/${match.repo}/-/blob/${revision}/${path}`;
}

/** commitHref builds the "view this commit" link for a forge match. */
export function commitHref(match: ForgeMatch, sha: string): string {
  return match.host === "github.com"
    ? `https://github.com/${match.owner}/${match.repo}/commit/${sha}`
    : `https://gitlab.com/${match.owner}/${match.repo}/-/commit/${sha}`;
}

/** ForgeKind names a forge the plugin can classify for a library repository. */
export type ForgeKind = "github" | "gitlab" | "gitea";

/** repositoryBase turns a repository URL into a browsable https base with no
 * userinfo, no ".git" suffix, and no trailing slash. Answers null for a
 * query, a fragment, an unknown shape, or fewer than two path segments. */
export function repositoryBase(url: string): string | null {
  const value = url.trim();
  if (!value || /[?#]/.test(value)) return null;
  let host = "";
  let path = "";
  let scheme = "https";
  const webMatch = value.match(/^(https?):\/\/(?:[^@/]*@)?([A-Za-z0-9.-]+(?::\d+)?)\/(.+)$/i);
  const sshMatch = value.match(/^ssh:\/\/(?:[^@/]+@)?([A-Za-z0-9.-]+)(?::\d+)?\/(.+)$/i);
  const scpMatch = value.match(/^[^@/:]+@([A-Za-z0-9.-]+):(?!\/\/)(.+)$/);
  if (webMatch) {
    [, scheme, host, path] = webMatch;
    scheme = scheme.toLowerCase();
  } else if (sshMatch) {
    [, host, path] = sshMatch;
  } else if (scpMatch) {
    [, host, path] = scpMatch;
  } else {
    return null;
  }
  const segments = path.replace(/\/+$/, "").replace(/\.git$/, "").split("/");
  if (segments.length < 2) return null;
  if (segments.some((s) => !s || s === "." || s === ".." || /\s/.test(s))) return null;
  return `${scheme}://${host}/${segments.join("/")}`;
}

/** revisionHref links a run's own commit to its repository. A known forge
 * builds the commit path directly, so a Gitea or Forgejo host links. With no
 * known forge, the URL shape must name GitHub or GitLab. Answers null for a
 * missing URL or revision, or an unparsable URL. */
export function revisionHref(repoUrl?: string, forge?: string, sha?: string): string | null {
  if (!repoUrl || !sha) return null;
  if (forge === "github" || forge === "gitlab" || forge === "gitea") {
    const base = repositoryBase(repoUrl);
    if (!base) return null;
    const dash = forge === "gitlab" ? "/-" : "";
    return `${base}${dash}/commit/${encodeURIComponent(sha)}`;
  }
  const match = matchForgeShape(repoUrl);
  return match ? commitHref(match, sha) : null;
}

/** libraryHref links a shared library to its repository. It targets the
 * commit when a revision is known, and the version otherwise. Answers null
 * for an unknown forge, an unparsable URL, or a library with neither a
 * revision nor a version. */
export function libraryHref(
  repositoryUrl?: string,
  forge?: string,
  revision?: string,
  version?: string,
): string | null {
  if (!repositoryUrl) return null;
  if (forge !== "github" && forge !== "gitlab" && forge !== "gitea") return null;
  const base = repositoryBase(repositoryUrl);
  if (!base) return null;
  const dash = forge === "gitlab" ? "/-" : "";
  if (revision) {
    return revisionHref(repositoryUrl, forge, revision);
  }
  // Gitea needs the ref kind (branch or tag) in its src path, and the record
  // does not carry it. A Gitea library with no revision therefore shows text.
  if (version && forge !== "gitea") {
    const ref = version.split("/").map(encodeURIComponent).join("/");
    return `${base}${dash}/tree/${ref}`;
  }
  return null;
}
