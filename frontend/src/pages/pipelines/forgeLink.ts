/** ForgeMatch names one recognized repository host and its two path
 * segments. */
interface ForgeMatch {
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
