import { useInfiniteQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { listRunDownstream } from "../../api/client";
import { Button } from "../../components/Button";
import { Card } from "../../components/Card";
import type { RunLink, RunLinkPage } from "../../types";
import { stripTenantFolder } from "./jobPath";
import styles from "./RunLinksCard.module.css";

const enc = (s: string) => encodeURIComponent(s);

function buildHref(tenant: string, jobPath: string, runId: string): string {
  return `/pipelines/${enc(tenant)}/${enc(jobPath)}/${enc(runId)}`;
}

/**
 * RunRef names one linked run as tenant/job #n. The job is the job path
 * without its tenant folder. A run with no run id shows as text. A run with
 * no job path shows as its tenant alone.
 */
function RunRef({ link }: { link: RunLink }) {
  const job = link.jobPath ? stripTenantFolder(link.tenant, link.jobPath) : "";
  const number = link.buildNumber ? ` #${link.buildNumber}` : "";
  const text = job ? `${link.tenant}/${job}${number}` : link.tenant;
  if (link.jobPath && link.runId) {
    return <Link to={buildHref(link.tenant, link.jobPath, link.runId)}>{text}</Link>;
  }
  return <span>{text}</span>;
}

function DownstreamItem({ link }: { link: RunLink }) {
  const state = [link.status, link.result].filter(Boolean).join(" · ");
  return (
    <li>
      <RunRef link={link} />
      {state && <span className={styles.state}> · {state}</span>}
    </li>
  );
}

/**
 * RunLinksCard shows the run that started this run and the runs it started.
 * The first downstream page comes with the run detail. Show more reads the
 * next page. The card renders nothing when neither link exists.
 */
export function RunLinksCard({
  runId,
  upstream,
  downstream,
}: {
  runId: string;
  upstream?: RunLink;
  downstream?: RunLinkPage;
}) {
  // The key holds the whole first page, so a refetched run detail with a
  // changed status or build number starts a fresh page chain. The cache
  // seeds from initialData only when the key is new.
  const firstKey = JSON.stringify(downstream ?? null);
  const extra = useInfiniteQuery({
    queryKey: ["run-downstream", runId, firstKey],
    queryFn: ({ pageParam }) => listRunDownstream(runId, pageParam),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (last: RunLinkPage) => last.nextCursor,
    initialData: downstream
      ? { pages: [downstream], pageParams: [undefined as string | undefined] }
      : undefined,
    enabled: false,
  });

  const pages = extra.data?.pages ?? (downstream ? [downstream] : []);
  const items = pages.flatMap((page) => page.items);
  const hasMore = extra.hasNextPage ?? false;

  if (!upstream && items.length === 0) return null;

  return (
    <Card title="Run links">
      <dl className={styles.kv}>
        {upstream && (
          <>
            <dt>Triggered by</dt>
            <dd>
              <RunRef link={upstream} />
            </dd>
          </>
        )}
        {items.length > 0 && (
          <>
            <dt>Downstream</dt>
            <dd>
              <ul className={styles.list}>
                {items.map((link, i) => (
                  <DownstreamItem key={link.runId ?? `${link.tenant}-${i}`} link={link} />
                ))}
              </ul>
              {hasMore && (
                <Button
                  type="button"
                  variant="ghost"
                  disabled={extra.isFetchingNextPage}
                  onClick={() => void extra.fetchNextPage()}
                >
                  Show more
                </Button>
              )}
            </dd>
          </>
        )}
      </dl>
    </Card>
  );
}
