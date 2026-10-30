import { AlertTriangle, Ban, Info, SearchX } from "lucide-react";
import { useNavigate } from "react-router-dom";
import styles from "./RecoveryState.module.css";

export interface RecoveryStateProps {
  kind: "403" | "404" | "error" | "disabled";
  title: string;
  message: string;
  onRetry?: () => void;
}

export function RecoveryState({ kind, title, message, onRetry }: RecoveryStateProps) {
  const navigate = useNavigate();
  const Icon = kind === "403" ? Ban : kind === "404" ? SearchX : kind === "disabled" ? Info : AlertTriangle;
  const goBack = () => window.history.length > 1 ? navigate(-1) : navigate("/");
  return (
    <section className={styles.state} aria-labelledby="recovery-title">
      <div className={styles.mark} aria-hidden="true">V</div>
      <Icon aria-hidden="true" size={42} strokeWidth={1.5} />
      <h1 id="recovery-title">{title}</h1>
      <p>{message}</p>
      <div className={styles.actions}>
        {onRetry && <button type="button" onClick={onRetry}>Retry</button>}
        <button type="button" onClick={goBack}>Back</button>
        <button type="button" onClick={() => navigate("/")}>Home</button>
      </div>
    </section>
  );
}

export const ForbiddenPage = () => <RecoveryState kind="403" title="Access denied" message="You do not have access to this page." />;
export const NotFoundPage = () => <RecoveryState kind="404" title="Not found" message="We could not find that page." />;
/**
 * Explains a Tenant that has no pipelines. A caller who can read the Tenant sees
 * this page. Other callers see Not found.
 */
export const NoPipelinesPage = () => <RecoveryState kind="disabled" title="No pipelines" message="This tenant has no pipelines. An administrator turns them on with the Tenant's spec.pipelines field." />;
export const GenericErrorPage =({ onRetry }: { onRetry?: () => void }) => <RecoveryState kind="error" title="Unable to load page" message="Varroa could not load this page." onRetry={onRetry} />;

/**
 * Explains a 503 caused by an unconfigured record store, per design D12: the
 * page states that pipeline data is off rather than reading as a failure.
 */
export const RecordStoreDisabledPage = () => <RecoveryState kind="disabled" title="Pipeline data is off" message="The record store is not configured for this cluster, so build history is not available." />;

/**
 * Explains a 503 with the record_store_unreachable code: the store is
 * configured but did not answer. Retry asks again once it returns.
 */
export const RecordStoreUnreachablePage = ({ onRetry }: { onRetry?: () => void }) => <RecoveryState kind="error" title="Pipeline data is unreachable" message="Varroa cannot reach the record store. Build history returns when the database is back." onRetry={onRetry} />;
