// Markdown is the only renderer of model and note text. It never sets raw
// HTML: skipHtml drops it, images are removed, and links open in a new tab.
import type { ComponentPropsWithoutRef } from "react";
import ReactMarkdown from "react-markdown";
import styles from "./Markdown.module.css";

export function Markdown({ text }: { text: string }) {
  return (
    <div className={styles.markdown}>
      <ReactMarkdown
        skipHtml
        disallowedElements={["img"]}
        unwrapDisallowed
        components={{
          a: ({ href, children, ...rest }: ComponentPropsWithoutRef<"a">) => (
            <a href={href} target="_blank" rel="noopener noreferrer" {...rest}>
              {children}
            </a>
          ),
        }}
      >
        {text}
      </ReactMarkdown>
    </div>
  );
}
