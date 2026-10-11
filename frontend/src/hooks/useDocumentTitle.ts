import { useEffect } from "react";

/**
 * Sets document.title while the page is mounted. A null or empty title leaves
 * it unchanged. Unmount restores the title the page found.
 */
export function useDocumentTitle(title: string | null | undefined): void {
  useEffect(() => {
    if (!title) return;
    const previous = document.title;
    document.title = title;
    return () => {
      document.title = previous;
    };
  }, [title]);
}
