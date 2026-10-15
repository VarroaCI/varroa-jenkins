import { Link, useParams } from "react-router-dom";
import { useController } from "../hooks/useControllers";
import { Card } from "../components/Card";
import { StatusPill } from "../components/StatusPill";
import { Pulse } from "../components/Pulse";
import styles from "./ManagedJenkins.module.css";
import { controllerRoute } from "../routing";
import { usePermissions, canDoInNamespace } from "../hooks/usePermissions";

// pipelinesHref resolves the Pipelines surface link for a headless
// controller's card. A controller name is always the "team" URL segment
// the tenant resolver falls back to when no Team CRD claims it (the
// synthetic single-controller tenant), so the DTO's own name resolves a
// team; an absent name falls back to the Pipelines index.
function pipelinesHref(controllerName?: string): string {
  return controllerName ? `/pipelines/${encodeURIComponent(controllerName)}` : "/pipelines";
}

export default function ManagedJenkins() {
  const { cluster = "", namespace = "", name = "" } = useParams();
  const { data: ctrl, isLoading } = useController(cluster, namespace, name);
  const { data: permissions } = usePermissions();

  if (isLoading) {
    return <div className={styles.page}><div className={styles.loading}>Loading...</div></div>;
  }

  // Headless controller — the Jenkins web UI is blocked for every non-admin.
  // Neither an iframe nor a link-out is offered; route the caller to
  // Pipelines instead, and show the break-glass runbook only to a caller
  // who holds the verb.
  if (ctrl?.headless) {
    const canBreakGlass = canDoInNamespace(permissions, namespace, "controllers", "break-glass");
    return (
      <div className={styles.page}>
        <Link to={controllerRoute(cluster, namespace, name)} className={styles.back}>
          ← Back to controller
        </Link>

        <Card title="⬡ Headless controller">
          <div className={styles.emptyState}>
            <div className={styles.emptyTitle}>
              {name}
            </div>
            <div className={styles.emptyMsg}>
              This controller blocks the Jenkins web interface for every caller who is not a
              Jenkins administrator. Use the Pipelines surface to view jobs and runs.
            </div>
            <Link to={pipelinesHref(ctrl.name)} data-testid="pipelines-link">Open Pipelines</Link>
            {canBreakGlass && (
              <div className={styles.muted} data-testid="breakglass-runbook">
                <p>An administrator can open the Jenkins UI temporarily:</p>
                <pre className={styles.mono}>
{`varroactl break-glass ${name} -n ${namespace} --cluster ${cluster} --reason "<reason>"`}
                </pre>
                <p>Then run the printed kubectl port-forward command, and open the printed URL.</p>
              </div>
            )}
          </div>
        </Card>
      </div>
    );
  }

  // Path mode and endpoint available — embed Jenkins in an iframe.
  if (ctrl?.routingMode === "path" && ctrl?.endpoint) {
    return (
      <div className={styles.page}>
        <Link to={controllerRoute(cluster, namespace, name)} className={styles.back}>
          ← Back to controller
        </Link>
        <iframe
          src={ctrl.endpoint}
          title={`${name} Jenkins`}
          className={styles.iframe}
        />
      </div>
    );
  }

  // Subdomain mode with endpoint — show message and external link (no redirect).
  if (ctrl?.endpoint) {
    return (
      <div className={styles.page}>
        <Link to={controllerRoute(cluster, namespace, name)} className={styles.back}>
          ← Back to controller
        </Link>

        <Card title="⬡ Cannot embed">
          <div className={styles.emptyState}>
            <div className={styles.emptyTitle}>
              {name}
            </div>
            <div className={styles.emptyMsg}>
              This controller uses subdomain routing and cannot be embedded.
            </div>
            <a href={ctrl.endpoint} target="_blank" rel="noreferrer">
              Open Jenkins ↗
            </a>
          </div>
        </Card>
      </div>
    );
  }

  // Not yet provisioned — show controller status.
  return (
    <div className={styles.page}>
      <Link to={controllerRoute(cluster, namespace, name)} className={styles.back}>
        ← Back to controller
      </Link>

      <Card title="⬡ Controller not yet provisioned">
        <div className={styles.emptyState}>
          <div className={styles.emptyTitle}>
            {name}
          </div>
          <div className={styles.emptyMsg}>
            This controller is still provisioning or has no endpoint configured.
          </div>
          <StatusPill phase={ctrl?.phase || "Pending"} />
          <div className={styles.miteStatus}>
            <Pulse active={ctrl?.miteConnected ?? false} size={10} />{" "}
            <span className={styles.miteLabel}>
              mite {ctrl?.miteConnected ? "connected" : "disconnected"}
            </span>
          </div>
        </div>
      </Card>
    </div>
  );
}
