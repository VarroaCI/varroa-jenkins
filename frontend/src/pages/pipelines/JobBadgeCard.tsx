import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { getJobBadge } from "../../api/client";
import { Button } from "../../components/Button";
import { Card } from "../../components/Card";
import type { PipelineItemKind } from "../../types";
import styles from "./JobBadgeCard.module.css";

interface JobBadgeCardProps {
  tenant: string;
  /** The full job path, tenant folder included. */
  jobPath: string;
  kind: PipelineItemKind;
  /** The job path without its tenant folder, for the label. */
  jobLabel: string;
}

/**
 * badgeURL is the absolute URL of the badge. The path is host-root, so the
 * card joins it to the window origin. A multibranch job names its branch in
 * the query; a null branch leaves the query off.
 */
export function badgeURL(path: string, branch: string | null): string {
  const url = new URL(path, window.location.origin).toString();
  return branch === null ? url : `${url}?branch=${encodeURIComponent(branch)}`;
}

/** badgeSnippet is the Markdown that embeds the badge and links to the job. */
export function badgeSnippet(url: string, pageURL: string): string {
  return `[![build status](<${url}>)](<${pageURL}>)`;
}

/**
 * JobBadgeCard shows the anonymous build badge of one job. It renders
 * nothing for an organization folder, while it loads, and on a read error.
 * A job whose tenant has the badge off sees a note only.
 */
export function JobBadgeCard({ tenant, jobPath, kind, jobLabel }: JobBadgeCardProps) {
  const [copied, setCopied] = useState(false);
  // The branch is typed by the reader. The branch names differ by repository
  // (main, master, trunk), so the card cannot guess one.
  const [branch, setBranch] = useState("main");
  const query = useQuery({
    queryKey: ["pipelines", tenant, "badge", jobPath],
    queryFn: () => getJobBadge(tenant, jobPath),
    enabled: kind !== "organizationFolder",
  });

  if (kind === "organizationFolder" || query.isLoading || query.isError || !query.data) {
    return null;
  }

  const badge = query.data;
  if (!badge.enabled || !badge.path) {
    return (
      <Card title="Status badge">
        <p className={styles.note}>
          The anonymous badge is off for this tenant. A tenant admin turns it on in the tenant settings.
        </p>
      </Card>
    );
  }

  const url = badgeURL(badge.path, kind === "multibranch" ? branch.trim() : null);
  const snippet = badgeSnippet(url, window.location.href);

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(snippet);
      setCopied(true);
    } catch {
      setCopied(false);
    }
  };

  return (
    <Card title="Status badge">
      <div className={styles.body}>
        <img className={styles.preview} src={url} alt={`build status of ${jobLabel}`} />
        <label className={styles.field}>
          <span>Badge URL</span>
          <input readOnly value={url} onFocus={(e) => e.currentTarget.select()} />
        </label>
        <label className={styles.field}>
          <span>Markdown</span>
          <input readOnly value={snippet} onFocus={(e) => e.currentTarget.select()} />
        </label>
        <div className={styles.actions}>
          <Button size="sm" onClick={copy}>
            {copied ? "Copied" : "Copy Markdown"}
          </Button>
        </div>
        {kind === "multibranch" && (
          <label className={styles.field}>
            <span>Branch</span>
            <input value={branch} onChange={(e) => setBranch(e.target.value)} />
            <span className={styles.note}>The badge shows the last build of this branch.</span>
          </label>
        )}
      </div>
    </Card>
  );
}
