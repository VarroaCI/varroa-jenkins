import { AlertTriangle, Ban, SearchX } from "lucide-react";
import { useNavigate } from "react-router-dom";
import styles from "./RecoveryState.module.css";

export interface RecoveryStateProps {
  kind: "403" | "404" | "error";
  title: string;
  message: string;
  onRetry?: () => void;
}

export function RecoveryState({ kind, title, message, onRetry }: RecoveryStateProps) {
  const navigate = useNavigate();
  const Icon = kind === "403" ? Ban : kind === "404" ? SearchX : AlertTriangle;
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
export const GenericErrorPage = ({ onRetry }: { onRetry?: () => void }) => <RecoveryState kind="error" title="Unable to load page" message="Varroa could not load this page." onRetry={onRetry} />;
