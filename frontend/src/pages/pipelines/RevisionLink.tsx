import { useEffect, useState } from "react";
import { Copy, Check } from "lucide-react";
import { Button } from "../../components/Button";
import { commitHref, matchForgeShape } from "./forgeLink";
import styles from "./RevisionLink.module.css";

/** COPIED_NOTE_MS is how long the copy button shows its confirmation. */
const COPIED_NOTE_MS = 2000;

interface RevisionLinkProps {
  /** revision is the full commit id the run built. */
  revision: string;
  /** repoUrl is the run's repository URL, when Jenkins reported one. */
  repoUrl?: string;
  /** length is the number of characters to show. */
  length: number;
}

/** RevisionLink shows a commit id. A GitHub or GitLab repository opens the
 * commit on the forge. Any other repository, or none, shows a button that
 * copies the full id. */
export function RevisionLink({ revision, repoUrl, length }: RevisionLinkProps) {
  const match = repoUrl ? matchForgeShape(repoUrl) : null;
  const [copied, setCopied] = useState(false);

  useEffect(() => {
    if (!copied) return;
    const timer = setTimeout(() => setCopied(false), COPIED_NOTE_MS);
    return () => clearTimeout(timer);
  }, [copied]);

  if (match) {
    return (
      <a
        href={commitHref(match, revision)}
        target="_blank"
        rel="noreferrer"
        className={styles.mono}
        title={revision}
      >
        {revision.slice(0, length)}
      </a>
    );
  }

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(revision);
      setCopied(true);
    } catch {
      setCopied(false);
    }
  };

  return (
    <span className={styles.wrap}>
      <span className={styles.mono} title={revision}>
        {revision.slice(0, length)}
      </span>
      <Button
        variant="ghost"
        size="sm"
        aria-label={`Copy commit ${revision}`}
        onClick={copy}
      >
        {copied ? <Check size={12} aria-hidden="true" /> : <Copy size={12} aria-hidden="true" />}
        {copied ? " Copied" : ""}
      </Button>
    </span>
  );
}
