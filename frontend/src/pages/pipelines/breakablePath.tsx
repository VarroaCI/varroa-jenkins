import { Fragment, type ReactNode } from "react";

const SEPARATOR = /[/._]/;

/**
 * breakablePath returns the text with a line break opportunity after
 * each slash, dot, and underscore. A long path wraps at its separators
 * and does not widen its table column. The text content is unchanged.
 */
export function breakablePath(text: string): ReactNode {
  if (!SEPARATOR.test(text)) return text;
  const parts = text.split(/(?<=[/._])/);
  return parts.map((part, i) => (
    <Fragment key={i}>
      {part}
      {i < parts.length - 1 && <wbr aria-hidden="true" />}
    </Fragment>
  ));
}
