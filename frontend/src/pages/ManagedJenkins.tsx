import { Link, useParams } from "react-router-dom";
import { useController } from "../hooks/useControllers";
import { Card } from "../components/Card";
import { StatusPill } from "../components/StatusPill";
import { Pulse } from "../components/Pulse";
import styles from "./ManagedJenkins.module.css";
import { controllerRoute } from "../routing";

export default function ManagedJenkins() {
  const { cluster = "", namespace = "", name = "" } = useParams();
  const { data: ctrl, isLoading } = useController(cluster, namespace, name);

  if (isLoading) {
    return <div className={styles.page}><div className={styles.loading}>Loading...</div></div>;
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
