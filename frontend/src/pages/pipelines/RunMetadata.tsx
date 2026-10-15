import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { getRun } from "../../api/client";
import type { ChangeRequest, RunDetail } from "../../types";
import { formatDuration } from "./JobTree";
import { libraryHref } from "./forgeLink";
import { RevisionLink } from "./RevisionLink";
import {
  MASKED_PARAM,
  QUEUE_WAIT_SHOWN_MS,
  TRIGGER_LABEL,
  changeRequestLabel,
  queueWaitMs,
  safeWebUrl,
  sortedParams,
} from "./runMeta";
import styles from "./RunMetadata.module.css";

const enc = (s: string) => encodeURIComponent(s);

interface RunMetadataProps {
  run: RunDetail;
  team?: string;
  job?: string;
}

/** RunMetadata renders the facts above a build: the meta line, the replay
 * origin, the parameters, and the run's definition. Each row hides itself
 * when the record carries no value for it. */
export function RunMetadata({ run, team, job }: RunMetadataProps) {
  const wait = queueWaitMs(run);
  const params = sortedParams(run.params);
  const hasJobLink = !!team && !!job;

  return (
    <>
      <div className={styles.meta}>
        {run.branch &&
          (hasJobLink ? (
            <Link to={`/pipelines/${enc(team!)}/${enc(job!)}?branch=${enc(run.branch)}`}>{run.branch}</Link>
          ) : (
            <span>{run.branch}</span>
          ))}
        {run.scmRevision && <RevisionLink revision={run.scmRevision} repoUrl={run.scmUrl} forge={run.scmForge} length={8} />}
        {run.actor && (
          <span title={run.triggerSource ? `Triggered by ${TRIGGER_LABEL[run.triggerSource]}` : undefined}>
            {run.actor}
          </span>
        )}
        {run.cause && <span>{run.cause}</span>}
        {run.startedAt && <span>{new Date(run.startedAt).toLocaleString()}</span>}
        {typeof run.durationMs === "number" && <span>{formatDuration(run.durationMs)}</span>}
        {wait !== undefined && wait > QUEUE_WAIT_SHOWN_MS && <span>Waited {formatDuration(wait)} in queue</span>}
      </div>
      {run.changeRequest && <ChangeRequestRow changeRequest={run.changeRequest} />}
      {run.replayOf && <ReplayOrigin replayOf={run.replayOf} team={team} job={job} />}
      {run.restartOf && run.restartStage && (
        <RestartOrigin restartOf={run.restartOf} stage={run.restartStage} team={team} job={job} />
      )}
      {params.length > 0 && (
        <div className={styles.row}>
          <span className={styles.rowLabel}>Parameters</span>
          {params.map(([name, value]) => (
            <span key={name} className={styles.mono}>
              {name}={value === MASKED_PARAM ? "••••" : value}
            </span>
          ))}
        </div>
      )}
      {run.source &&
        !!(run.source.definitionPath || run.source.libraries?.length || run.source.podTemplates?.length) && (
          <div className={styles.row}>
            <span className={styles.rowLabel}>Ran with</span>
            {run.source.definitionPath && <span className={styles.mono}>{run.source.definitionPath}</span>}
            {(run.source.libraries ?? []).map((lib, i) => {
              const text =
                (lib.version ? `${lib.name}@${lib.version}` : lib.name) +
                (lib.revision ? ` ${lib.revision.slice(0, 8)}` : "");
              const href = libraryHref(lib.repositoryUrl, lib.repositoryForge, lib.revision, lib.version);
              return href ? (
                <a
                  key={`lib-${i}-${lib.name}`}
                  href={href}
                  target="_blank"
                  rel="noreferrer"
                  title={lib.repositoryUrl}
                >
                  {text}
                </a>
              ) : (
                <span key={`lib-${i}-${lib.name}`}>{text}</span>
              );
            })}
            {(run.source.podTemplates ?? []).map((pod, i) => (
              <span key={`pod-${i}-${pod.name}`}>{pod.image ? `${pod.name} (${pod.image})` : pod.name}</span>
            ))}
          </div>
        )}
    </>
  );
}

/** RestartOrigin links the build a restart reran from, and names the stage. It
 * reads that build to learn its number. Until the read answers, or when it
 * fails, the row names an earlier build without a number. */
function RestartOrigin({
  restartOf,
  stage,
  team,
  job,
}: {
  restartOf: string;
  stage: string;
  team?: string;
  job?: string;
}) {
  const origin = useQuery({
    queryKey: ["run-restart-origin", restartOf],
    queryFn: () => getRun(restartOf),
    enabled: !!restartOf,
    retry: false,
    staleTime: Infinity,
  });
  const number = origin.data?.buildNumber;
  const from = number ? `#${number}` : "an earlier build";
  const text = `Restarted from ${from} at stage ${stage}`;
  const linked = !!team && !!job;

  return (
    <div className={styles.row}>
      {linked ? (
        <Link to={`/pipelines/${enc(team!)}/${enc(job!)}/${enc(restartOf)}`}>{text}</Link>
      ) : (
        <span>{text}</span>
      )}
    </div>
  );
}

/** ChangeRequestRow names the pull request a multibranch run builds. It links
 * the request only when the URL is a web link. */
function ChangeRequestRow({ changeRequest }: { changeRequest: ChangeRequest }) {
  const label = changeRequestLabel(changeRequest);
  const href = safeWebUrl(changeRequest.url);

  return (
    <div className={styles.row}>
      <span className={styles.rowLabel}>Pull request</span>
      {href ? (
        <a href={href} target="_blank" rel="noreferrer">
          {label}
        </a>
      ) : (
        <span>{label}</span>
      )}
    </div>
  );
}

/** ReplayOrigin links the build a replay copied. It reads that build to
 * learn its number. Until the read answers, or when it fails, the row names
 * an earlier build without a number. */
function ReplayOrigin({ replayOf, team, job }: { replayOf: string; team?: string; job?: string }) {
  const origin = useQuery({
    queryKey: ["run-replay-origin", replayOf],
    queryFn: () => getRun(replayOf),
    enabled: !!replayOf,
    retry: false,
    staleTime: Infinity,
  });
  const number = origin.data?.buildNumber;
  const text = number ? `Replay of #${number}` : "Replay of an earlier build";
  const linked = !!team && !!job;

  return (
    <div className={styles.row}>
      {linked ? (
        <Link to={`/pipelines/${enc(team!)}/${enc(job!)}/${enc(replayOf)}`}>{text}</Link>
      ) : (
        <span>{text}</span>
      )}
    </div>
  );
}
