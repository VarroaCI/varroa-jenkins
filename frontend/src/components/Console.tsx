import { useEffect, useRef } from "react";
import styles from "./Console.module.css";

interface LogLine {
  timestamp: string;
  level: "INFO" | "WARN" | "ERROR" | "DEBUG" | "OK";
  source: string;
  message: string;
}

interface ConsoleProps {
  lines?: LogLine[];
  autoScroll?: boolean;
  maxHeight?: number;
}

const levelClass = (lvl: string): string => {
  switch (lvl) {
    case "INFO": return styles.INFO;
    case "WARN": return styles.WARN;
    case "ERROR": return styles.ERROR;
    case "DEBUG": return styles.DEBUG;
    case "OK": return styles.OK;
    default: return "";
  }
};

export function Console({ lines = [], autoScroll = true, maxHeight = 430 }: ConsoleProps) {
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (autoScroll && ref.current) {
      ref.current.scrollTop = ref.current.scrollHeight;
    }
  }, [lines, autoScroll]);

  return (
    <div className={styles.console} style={{ maxHeight }} ref={ref}>
      {lines.map((line, i) => (
        <div key={i} className={styles.logline}>
          <span className={styles.ts}>{line.timestamp}</span>
          <span className={`${styles.lv} ${levelClass(line.level)}`}>{line.level}</span>
          <span style={{ color: "var(--text-2)" }}>[{line.source}]</span>
          <span>{line.message}</span>
        </div>
      ))}
    </div>
  );
}
