import { Link } from "react-router-dom";
import { stripTenantFolder } from "./jobPath";
import styles from "./JobBreadcrumbs.module.css";

const enc = (s: string) => encodeURIComponent(s);

/**
 * teamHomeHref builds the team home link for one folder crumb. The link
 * carries one "open" parameter per ancestor folder and one for the crumb's
 * own folder, so the team home opens unfolded at that folder. The tenant
 * crumb carries none.
 */
function teamHomeHref(team: string, openPaths: string[]): string {
  const base = `/pipelines/${enc(team)}`;
  if (openPaths.length === 0) return base;
  const query = openPaths.map((value) => `open=${enc(value)}`).join("&");
  return `${base}?${query}`;
}

/**
 * JobBreadcrumbs names the tenant, then every folder between the tenant
 * folder and the job, then the job. Without a current label, the job crumb
 * is the current page and is not a link. With a current label, the job crumb
 * links to the job page, and the label is the last crumb and the current
 * page. The label names a build or a test case.
 *
 * The first crumb carries the {team} URL segment, never the response's
 * tenant value: a controller tenant reports "<namespace>/<controller>"
 * there, and the crumb link needs the team segment.
 */
export default function JobBreadcrumbs({
  team,
  tenantFolder,
  jobPath,
  current,
}: {
  team: string;
  tenantFolder: string;
  jobPath: string;
  current?: string;
}) {
  const relative = stripTenantFolder(tenantFolder ?? "", jobPath);
  const segments = relative === "" ? [] : relative.split("/");
  const folders = segments.slice(0, -1);
  const jobName = segments.length > 0 ? segments[segments.length - 1] : jobPath;

  return (
    <nav aria-label="Job path" className={styles.trail}>
      <ol className={styles.list}>
        <li className={styles.crumb}>
          <Link to={teamHomeHref(team, [])}>{team}</Link>
        </li>
        {folders.map((segment, index) => {
          const openPaths = folders.slice(0, index + 1).map((_, i) => folders.slice(0, i + 1).join("/"));
          return (
            <li key={openPaths[openPaths.length - 1]} className={styles.crumb}>
              <span aria-hidden="true" className={styles.separator}>
                /
              </span>
              <Link to={teamHomeHref(team, openPaths)}>{segment}</Link>
            </li>
          );
        })}
        <li className={styles.crumb}>
          <span aria-hidden="true" className={styles.separator}>
            /
          </span>
          {current === undefined ? (
            <span aria-current="page" className={styles.current}>
              {jobName}
            </span>
          ) : (
            <Link to={`/pipelines/${enc(team)}/${enc(jobPath)}`}>{jobName}</Link>
          )}
        </li>
        {current !== undefined && (
          <li className={styles.crumb}>
            <span aria-hidden="true" className={styles.separator}>
              /
            </span>
            <span aria-current="page" className={styles.current}>
              {current}
            </span>
          </li>
        )}
      </ol>
    </nav>
  );
}
