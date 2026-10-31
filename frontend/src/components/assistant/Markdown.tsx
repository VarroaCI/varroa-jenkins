// Markdown is the only renderer of model and note text. It never sets raw
// HTML: skipHtml drops it, and images are removed. A link to another page
// opens in a new tab. A single tilde stays text, because answers use it for
// an approximate value.
import { memo } from "react";
import ReactMarkdown, { type Components, type Options } from "react-markdown";
import remarkGfm from "remark-gfm";
import styles from "./Markdown.module.css";

const REMARK_PLUGINS: Options["remarkPlugins"] = [[remarkGfm, { singleTilde: false }]];

const COMPONENTS: Components = {
  a: ({ node: _node, href, children, ...rest }) =>
    href?.startsWith("#") ? (
      <a href={href} {...rest}>{children}</a>
    ) : (
      <a href={href} target="_blank" rel="noopener noreferrer" {...rest}>{children}</a>
    ),
  table: ({ node: _node, children, ...rest }) => (
    <div className={styles.tableScroll} tabIndex={0} role="region" aria-label="Table">
      <table {...rest}>{children}</table>
    </div>
  ),
};

/** Markdown renders model and note text. It renders again only when the text changes. */
export const Markdown = memo(function Markdown({ text }: { text: string }) {
  return (
    <div className={styles.markdown}>
      <ReactMarkdown
        skipHtml
        disallowedElements={["img"]}
        unwrapDisallowed
        remarkPlugins={REMARK_PLUGINS}
        components={COMPONENTS}
      >
        {text}
      </ReactMarkdown>
    </div>
  );
});
