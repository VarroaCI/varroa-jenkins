import { Component, type ReactNode } from "react";
import styles from "./ErrorBoundary.module.css";

interface Props {
  children: ReactNode;
}

interface State {
  error: Error | null;
}

export default class ErrorBoundary extends Component<Props, State> {
  state: State = { error: null };

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  render() {
    if (this.state.error) {
      return (
        <div className={styles.boundary}>
          <h2 className={styles.title}>
            Something went wrong
          </h2>
          <p className={styles.message}>
            {this.state.error.message}
          </p>
          <button
            onClick={() => this.setState({ error: null })}
            className={styles.retry}
          >
            Try again
          </button>
        </div>
      );
    }
    return this.props.children;
  }
}
