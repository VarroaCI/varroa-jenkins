import { useMemo, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { CircleAlert, CircleHelp, CircleX, PackageX, Unplug } from "lucide-react";
import { abortRunInput, getUpdateCenterStatus, proceedRunInput, rerunRun } from "../../api/client";
import { useToast } from "../../components/Toast";
import { StatusPill } from "../../components/StatusPill";
import { runHref } from "../../components/runs/runRowText";
import { useClusters } from "../../hooks/useClusters";
import { useControllers } from "../../hooks/useControllers";
import { controllerRoute } from "../../routing";
import { usePipelinesOverview } from "../pipelines/usePipelinesOverview";
import {
  ATTENTION_ROWS,
  attentionBreakdown,
  attentionCount,
  attentionItems,
  clusterDetail,
  clusterTitle,
  controllerDetail,
  failingDetail,
  failingTitle,
  inputDetail,
  inputTitle,
  updateCenterTitle,
  type AttentionItem,
} from "./attentionItems";
import { useNow } from "./useNow";
import styles from "./AttentionPanel.module.css";

export interface AttentionPanelProps {
  /** Whether the caller reads pipelines. It gates the overview read and its rows. */
  canPipelines: boolean;
  /** Whether the caller reads controllers. It gates the controller and cluster reads. */
  canControllers: boolean;
  /** Whether the caller is an administrator. It gates the update center row. */
  isAdmin: boolean;
}

/**
 * AttentionPanel lists the items that need a person: runs that wait for input,
 * failing jobs, clusters that stopped reporting, controllers that need a look,
 * and the update center for an administrator. Each part reads on its own, so a
 * failed read keeps the rows of the other parts. It renders nothing when no
 * item needs attention.
 */
export function AttentionPanel({ canPipelines, canControllers, isAdmin }: AttentionPanelProps) {
  const overview = usePipelinesOverview(canPipelines);
  const controllers = useControllers(canControllers);
  const clusters = useClusters(canControllers);
  const updateCenter = useQuery({
    queryKey: ["updatecenter-status"],
    queryFn: getUpdateCenterStatus,
    enabled: isAdmin,
    staleTime: 300_000,
    retry: false,
  });
  const now = useNow(30_000);
  const [expanded, setExpanded] = useState(false);
  const queryClient = useQueryClient();
  const { toast } = useToast();

  const ov = overview.data;
  const items = useMemo(
    () =>
      attentionItems({
        waitingInputs: ov?.waitingInputs ?? [],
        failingJobs: ov?.failingJobs ?? [],
        clusters: clusters.data ?? [],
        controllers: controllers.data ?? [],
        updateCenterUnservable: updateCenter.data?.enabled ? (updateCenter.data.unservableCount ?? 0) : 0,
      }),
    [ov, clusters.data, controllers.data, updateCenter.data],
  );

  const invalidate = () => void queryClient.invalidateQueries({ queryKey: ["pipelines-overview"] });
  const proceed = useMutation({
    mutationFn: (v: { runId: string; inputId: string }) => proceedRunInput(v.runId, v.inputId, {}),
    onSuccess: () => {
      toast("Proceed requested");
      invalidate();
    },
    onError: () => toast("Proceed failed"),
  });
  const abort = useMutation({
    mutationFn: (v: { runId: string; inputId: string }) => abortRunInput(v.runId, v.inputId),
    onSuccess: () => {
      toast("Abort requested");
      invalidate();
    },
    onError: () => toast("Abort failed"),
  });
  const rerun = useMutation({
    mutationFn: (runId: string) => rerunRun(runId),
    onSuccess: () => {
      toast("Re-run requested");
      invalidate();
    },
    onError: () => toast("Re-run failed"),
  });

  // A run is busy while one of its requests is in flight, and its buttons stay disabled.
  const busy = new Set<string>();
  if (proceed.isPending && proceed.variables) busy.add(proceed.variables.runId);
  if (abort.isPending && abort.variables) busy.add(abort.variables.runId);
  if (rerun.isPending && rerun.variables) busy.add(rerun.variables);

  if (items.length === 0) return null;

  const cut = ov?.moreWaitingInputs === true || ov?.moreFailingJobs === true;
  const shown = expanded ? items : items.slice(0, ATTENTION_ROWS);

  return (
    <section className={styles.panel} aria-labelledby="overview-attention">
      <div className={styles.head}>
        <h2 id="overview-attention" className={styles.title}>
          Needs attention <span className={styles.count}>{attentionCount(items, cut)}</span>
        </h2>
        <span className={styles.breakdown}>{attentionBreakdown(items)}</span>
      </div>
      <ul className={styles.list}>
        {shown.map((item) => (
          <li key={item.key} className={styles.row}>
            <AttentionRow
              item={item}
              now={now}
              busy={busy}
              onProceed={(runId, inputId) => proceed.mutate({ runId, inputId })}
              onAbort={(runId, inputId) => abort.mutate({ runId, inputId })}
              onRerun={(runId) => rerun.mutate(runId)}
            />
          </li>
        ))}
      </ul>
      {items.length > ATTENTION_ROWS && (
        <button type="button" className={styles.more} onClick={() => setExpanded((e) => !e)}>
          {expanded ? "Show fewer" : `Show all ${items.length}`}
        </button>
      )}
      {ov?.moreWaitingInputs && (
        <p className={styles.note}>
          <Link to="/pipelines?status=WAITING_INPUT">More runs wait for input on the Pipelines page.</Link>
        </p>
      )}
      {ov?.moreFailingJobs && (
        <p className={styles.note}>
          <Link to="/pipelines?result=FAILURE">More failing jobs are on the Pipelines page.</Link>
        </p>
      )}
    </section>
  );
}

interface AttentionRowProps {
  item: AttentionItem;
  now: number;
  busy: Set<string>;
  onProceed: (runId: string, inputId: string) => void;
  onAbort: (runId: string, inputId: string) => void;
  onRerun: (runId: string) => void;
}

function AttentionRow({ item, now, busy, onProceed, onAbort, onRerun }: AttentionRowProps) {
  switch (item.kind) {
    case "input": {
      const { run, input, inputCount } = item.waiting;
      // An input with parameters, or a run with two inputs, is answered on its build page only.
      const canAct = run.canWrite && input.parameterCount === 0 && inputCount === 1;
      const inFlight = busy.has(run.runId);
      return (
        <Row tone="warn" icon={<CircleHelp size={16} />} title={inputTitle(item.waiting)} detail={inputDetail(item.waiting, now)}>
          {canAct && (
            <button type="button" className={styles.btnPrimary} disabled={inFlight} onClick={() => onProceed(run.runId, input.id)}>
              {input.ok || "Proceed"}
            </button>
          )}
          {canAct && (
            <button type="button" className={styles.btn} disabled={inFlight} onClick={() => onAbort(run.runId, input.id)}>
              Abort
            </button>
          )}
          <Link className={styles.btnGhost} to={runHref(run.tenant, run.jobPath ?? "", run.runId)}>
            Open
          </Link>
        </Row>
      );
    }
    case "failing": {
      const { run } = item.job;
      const inFlight = busy.has(run.runId);
      return (
        <Row tone="bad" icon={<CircleX size={16} />} title={failingTitle(item.job)} detail={failingDetail(item.job, now)}>
          <Link className={styles.btn} to={runHref(run.tenant, run.jobPath ?? "", run.runId)}>
            Open log
          </Link>
          {run.canWrite && (
            <button type="button" className={styles.btnGhost} disabled={inFlight} onClick={() => onRerun(run.runId)}>
              Run again
            </button>
          )}
        </Row>
      );
    }
    case "cluster":
      return (
        <Row tone="bad" icon={<Unplug size={16} />} title={clusterTitle(item.cluster)} detail={clusterDetail(item.cluster, now)}>
          <Link className={styles.btnGhost} to="/clusters">
            Open
          </Link>
        </Row>
      );
    case "controller": {
      const c = item.controller;
      return (
        <Row
          tone="bad"
          icon={<CircleAlert size={16} />}
          title={
            <>
              {c.name} <StatusPill phase={c.phase} attention={c.attention} size="sm" />
            </>
          }
          detail={`${c.cluster} · ns/${c.namespace} · ${controllerDetail(c)}`}
        >
          <Link className={styles.btnGhost} to={controllerRoute(c.cluster, c.namespace, c.name)}>
            Open
          </Link>
        </Row>
      );
    }
    case "updateCenter":
      return (
        <Row tone="warn" icon={<PackageX size={16} />} title={updateCenterTitle(item.unservable)}>
          <Link className={styles.btnGhost} to="/administration/update-center">
            Open
          </Link>
        </Row>
      );
  }
}

interface RowProps {
  tone: "warn" | "bad";
  icon: ReactNode;
  title: ReactNode;
  detail?: string;
  children?: ReactNode;
}

/** Row lays out one attention row: a badge, the title and detail, and its actions. */
function Row({ tone, icon, title, detail, children }: RowProps) {
  return (
    <>
      <span className={styles.badge} data-tone={tone} aria-hidden="true">
        {icon}
      </span>
      <div className={styles.text}>
        <div className={styles.rowTitle}>{title}</div>
        {detail && <div className={styles.rowDetail}>{detail}</div>}
      </div>
      <div className={styles.actions}>{children}</div>
    </>
  );
}
