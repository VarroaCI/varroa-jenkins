// Fixture for extractTS. A host in a comment never counts.
// see https://comment-host.io/a
/* block https://comment-host.io/b */
/**
 * JSDoc https://comment-host.io/c
 */
const double = "https://double-host.io/x";
const single = 'https://single-host.io/x';
const staticT = `https://static-host.io/${a}`;
const dynHost = `https://${h}/p`;
const dynSub = `https://${s}.dyn-host.io`;
const nested = `x${c ? "https://nested-host.io" : `y${"https://deep-host.io"}`}`;
const re = /"https:\/\/regex-host.io/;
const after = 'https://after-regex-host.io';
const div = a / b / 2;
const next = "https://after-div-host.io";
const el = <a href="https://jsx-attr-host.io">
  <p>Visit https://jsx-text-host.io now</p>
  <p>Don't</p>
  {"https://after-jsx-host.io"}
</a>;
