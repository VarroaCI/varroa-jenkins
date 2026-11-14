import { useId } from "react";
import { AlertTriangle, Ban, Info, SearchX } from "lucide-react";
import { useNavigate } from "react-router-dom";
import styles from "./RecoveryState.module.css";

export interface RecoveryStateProps {
  kind: "403" | "404" | "error" | "disabled";
  title: string;
  message: string;
  onRetry?: () => void;
  /** An inline block for a section of a page. It has no mark and no Back or Home. */
  compact?: boolean;
}

export function RecoveryState({ kind, title, message, onRetry, compact = false }: RecoveryStateProps) {
  const navigate = useNavigate();
  const id = useId();
  const Icon = kind === "403" ? Ban : kind === "404" ? SearchX : kind === "disabled" ? Info : AlertTriangle;
  const goBack = () => window.history.length > 1 ? navigate(-1) : navigate("/");
  if (compact) {
    return (
      <section className={styles.compact} aria-labelledby={id}>
        <Icon aria-hidden="true" size={20} strokeWidth={1.5} />
        <div className={styles.compactText}>
          <p id={id} className={styles.compactTitle}>{title}</p>
          <p className={styles.compactMessage}>{message}</p>
        </div>
        {onRetry && <div className={styles.actions}><button type="button" onClick={onRetry}>Retry</button></div>}
      </section>
    );
  }
  return (
    <section className={styles.state} aria-labelledby={id}>
      <div className={styles.mark} aria-hidden="true">V</div>
      <Icon aria-hidden="true" size={42} strokeWidth={1.5} />
      <h1 id={id}>{title}</h1>
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
export const RecordStoreDisabledPage = ({ compact = false }: { compact?: boolean }) => <RecoveryState kind="disabled" title="Pipeline data is off" message="The record store is not configured for this cluster, so build history is not available." compact={compact} />;

/**
 * Explains a 503 with the record_store_unreachable code: the store is
 * configured but did not answer. Retry asks again once it returns.
 */
export const RecordStoreUnreachablePage = ({ onRetry, compact = false }: { onRetry?: () => void; compact?: boolean }) => <RecoveryState kind="error" title="Pipeline data is unreachable" message="Varroa cannot reach the record store. Build history returns when the database is back." onRetry={onRetry} compact={compact} />;
