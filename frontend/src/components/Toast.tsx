import {
  useState,
  useCallback,
  useEffect,
  useRef,
  createContext,
  useContext,
  type ReactNode,
} from "react";
import styles from "./Toast.module.css";

interface ToastContextValue {
  toast: (message: string) => void;
}

const ToastContext = createContext<ToastContextValue | null>(null);

export function useToast(): ToastContextValue {
  const ctx = useContext(ToastContext);
  if (!ctx) throw new Error("useToast must be used within ToastProvider");
  return ctx;
}

let toastId = 0;

export function ToastProvider({ children }: { children: ReactNode }) {
  const [messages, setMessages] = useState<{ id: number; text: string }[]>([]);
  // Every pending dismissal timer, so unmount can cancel them. Without this a
  // timer outlives the component and calls setState on an unmounted tree — in
  // tests that lands after the DOM environment is gone and throws
  // "window is not defined", which surfaces as a flake on slow runners rather
  // than as the real bug it is.
  const timers = useRef<ReturnType<typeof setTimeout>[]>([]);

  useEffect(
    () => () => {
      timers.current.forEach(clearTimeout);
      timers.current = [];
    },
    []
  );

  const toast = useCallback((text: string) => {
    const id = ++toastId;
    setMessages((prev) => [...prev, { id, text }]);
    const handle = setTimeout(() => {
      timers.current = timers.current.filter((t) => t !== handle);
      setMessages((prev) => prev.filter((m) => m.id !== id));
    }, 2600);
    timers.current.push(handle);
  }, []);

  return (
    <ToastContext.Provider value={{ toast }}>
      {children}
      <div className={styles.container}>
        {messages.map((m) => (
          <div key={m.id} className={`${styles.toast} ${styles.show}`}>
            ✓ {m.text}
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  );
}
