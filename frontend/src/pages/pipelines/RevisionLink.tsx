import { useEffect, useState } from "react";
import { Copy, Check } from "lucide-react";
import { Button } from "../../components/Button";
import { revisionHref } from "./forgeLink";
import styles from "./RevisionLink.module.css";

/** COPIED_NOTE_MS is how long the copy button shows its confirmation. */
const COPIED_NOTE_MS = 2000;

interface RevisionLinkProps {
  /** revision is the full commit id the run built. */
  revision: string;
  /** repoUrl is the run's repository URL, when Jenkins reported one. */
  repoUrl?: string;
  /** forge names the host kind of repoUrl: github, gitlab, or gitea. */
  forge?: string;
  /** length is the number of characters to show. */
  length: number;
}

/** RevisionLink shows a commit id. A repository on a known forge opens the
 * commit there, and so does a GitHub or GitLab URL with no forge. Any other
 * repository, or none, shows a button that copies the full id. */
export function RevisionLink({ revision, repoUrl, forge, length }: RevisionLinkProps) {
  const href = revisionHref(repoUrl, forge, revision);
  const [copied, setCopied] = useState(false);

  useEffect(() => {
    if (!copied) return;
    const timer = setTimeout(() => setCopied(false), COPIED_NOTE_MS);
    return () => clearTimeout(timer);
  }, [copied]);

  if (href) {
    return (
      <a
        href={href}
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
