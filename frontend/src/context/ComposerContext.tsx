import { createContext, useContext, useReducer, useEffect, useCallback, useRef, type ReactNode } from "react";
import type { ComposedItemRef, ComposedBundleSpec } from "../types";

const STORAGE_KEY = "varroa_composer_draft";

/** Stable identity for a staged itemRef. Namespace-aware so same-named items from
 *  different namespaces are distinct entries; unset-namespace refs (loaded legacy
 *  specs) keep their bare-name identity. */
export const itemRefKey = (ref: { name: string; namespace?: string }): string =>
  ref.namespace ? `${ref.namespace}/${ref.name}` : ref.name;

/** Drop later duplicates (by itemRefKey), keeping first occurrence. ADD_ITEM
 *  already enforces uniqueness incrementally; this guards the bulk paths (LOAD,
 *  localStorage hydration) where items arrive as a whole list — a duplicated
 *  itemRef in a server spec or persisted draft would otherwise break React keys,
 *  index-based reordering, and be written back to the bundle on save. */
const dedupeItems = (items: ComposedItemRef[]): ComposedItemRef[] => {
  const seen = new Set<string>();
  return items.filter((i) => {
    const key = itemRefKey(i);
    if (seen.has(key)) return false;
    seen.add(key);
    return true;
  });
};

interface ComposerState {
  items: ComposedItemRef[];
  variables: Record<string, string>;
  // For an edit draft: the bundle resourceVersion this draft was seeded from.
  // Used to detect a stale draft (the bundle changed server-side since the draft
  // was written) so the editor can re-seed from the current bundle instead of
  // silently editing an outdated snapshot. Unset for the create flow.
  baseVersion?: string;
  // The cluster this draft targets. All preview/create/update/validate calls a
  // composer session makes are addressed to this cluster so config authored in
  // the composer lands on the intended (possibly hive) cluster, not always the
  // core. Unset falls back to "core" at the call site.
  cluster?: string;
}

type ComposerAction =
  | { type: "ADD_ITEM"; item: ComposedItemRef }
  | { type: "REMOVE_ITEM"; ref: { name: string; namespace?: string } }
  | { type: "REORDER_ITEMS"; from: number; to: number }
  | { type: "SET_VARIABLE"; name: string; value: string }
  | { type: "SET_CLUSTER"; cluster: string }
  | { type: "CLEAR" }
  | { type: "LOAD"; state: ComposerState };

function reducer(state: ComposerState, action: ComposerAction): ComposerState {
  switch (action.type) {
    case "ADD_ITEM":
      if (state.items.some((i) => itemRefKey(i) === itemRefKey(action.item))) return state;
      return { ...state, items: [...state.items, action.item] };
    case "REMOVE_ITEM":
      return { ...state, items: state.items.filter((i) => itemRefKey(i) !== itemRefKey(action.ref)) };
    case "REORDER_ITEMS": {
      const items = [...state.items];
      const [moved] = items.splice(action.from, 1);
      if (!moved) return state;
      items.splice(action.to, 0, moved);
      return { ...state, items };
    }
    case "SET_VARIABLE":
      return { ...state, variables: { ...state.variables, [action.name]: action.value } };
    case "SET_CLUSTER":
      // Idempotent: return the same state when unchanged so a caller syncing the
      // cluster on every render (CatalogBrowser) can't drive a re-render loop.
      if (state.cluster === action.cluster) return state;
      return { ...state, cluster: action.cluster };
    case "CLEAR":
      // Preserve the target cluster across a clear so the composer keeps
      // authoring against the same cluster the user selected.
      return { items: [], variables: {}, cluster: state.cluster };
    case "LOAD":
      return { ...action.state, items: dedupeItems(action.state.items) };
    default:
      return state;
  }
}

function loadDraft(key: string): ComposerState {
  try {
    const raw = localStorage.getItem(key);
    if (raw) {
      const s = JSON.parse(raw) as ComposerState;
      return { ...s, items: dedupeItems(s.items ?? []), variables: s.variables ?? {} };
    }
  } catch {
    // ignore
  }
  return { items: [], variables: {} };
}

function saveDraft(key: string, state: ComposerState) {
  try {
    localStorage.setItem(key, JSON.stringify(state));
  } catch {
    // ignore
  }
}

/** Whether a *non-empty* persisted draft exists under the given key. An empty
 *  draft ({items:[],variables:{}}) counts as absent, so a cleared session
 *  re-seeds from the bundle on the next visit rather than staying blank. */
export function hasDraft(key: string): boolean {
  try {
    const raw = localStorage.getItem(key);
    if (!raw) return false;
    const s = JSON.parse(raw) as ComposerState;
    return (s.items?.length ?? 0) > 0 || Object.keys(s.variables ?? {}).length > 0;
  } catch {
    return false;
  }
}

/** The bundle resourceVersion a persisted edit draft was seeded from, or
 *  undefined if there is no draft (or it predates version stamping). Lets the
 *  editor decide whether a hydrated draft is still based on the current bundle. */
export function draftBaseVersion(key: string): string | undefined {
  try {
    const raw = localStorage.getItem(key);
    if (!raw) return undefined;
    const s = JSON.parse(raw) as ComposerState;
    return s.baseVersion;
  } catch {
    return undefined;
  }
}

interface ComposerContextValue {
  items: ComposedItemRef[];
  variables: Record<string, string>;
  /** Cluster this composer session targets (undefined => core at the call site). */
  cluster?: string;
  addItem: (item: ComposedItemRef) => void;
  removeItem: (ref: { name: string; namespace?: string }) => void;
  reorderItem: (from: number, to: number) => void;
  setVar: (name: string, value: string) => void;
  /** Set the cluster this composer session authors against. */
  setCluster: (cluster: string) => void;
  /** Atomically replace the composer contents (used to seed the editor from a
   *  freshly-loaded bundle). `baseVersion` stamps the bundle version this seed
   *  came from so a later visit can detect a stale draft; `cluster` records the
   *  cluster the bundle lives on. */
  load: (items: ComposedItemRef[], variables: Record<string, string>, baseVersion?: string, cluster?: string) => void;
  clear: () => void;
  hasItem: (ref: { name: string; namespace?: string }) => boolean;
  toSpec: (displayName: string) => ComposedBundleSpec;
  clearPersisted: () => void;
}

const ComposerContext = createContext<ComposerContextValue | null>(null);

export function ComposerProvider({
  children,
  storageKey = STORAGE_KEY,
}: {
  children: ReactNode;
  // Which localStorage key to persist the draft under. `null` = in-memory only
  // (no read or write). Defaults to the shared create-draft key.
  storageKey?: string | null;
}) {
  const [state, dispatch] = useReducer(reducer, storageKey, (key) =>
    key ? loadDraft(key) : { items: [], variables: {} },
  );
  const skipNextPersist = useRef(false);

  // Persist to localStorage on every change
  useEffect(() => {
    if (!storageKey) return;
    if (skipNextPersist.current) {
      skipNextPersist.current = false;
      return;
    }
    const isPristine =
      state.items.length === 0 &&
      Object.keys(state.variables).length === 0 &&
      !state.baseVersion &&
      !state.cluster;
    if (isPristine) return;
    saveDraft(storageKey, state);
  }, [state, storageKey]);

  const addItem = useCallback((item: ComposedItemRef) => {
    dispatch({ type: "ADD_ITEM", item });
  }, []);

  const removeItem = useCallback((ref: { name: string; namespace?: string }) => {
    dispatch({ type: "REMOVE_ITEM", ref });
  }, []);

  const reorderItem = useCallback((from: number, to: number) => {
    dispatch({ type: "REORDER_ITEMS", from, to });
  }, []);

  const setVar = useCallback((name: string, value: string) => {
    dispatch({ type: "SET_VARIABLE", name, value });
  }, []);

  const setCluster = useCallback((cluster: string) => {
    dispatch({ type: "SET_CLUSTER", cluster });
  }, []);

  const load = useCallback(
    (items: ComposedItemRef[], variables: Record<string, string>, baseVersion?: string, cluster?: string) => {
      dispatch({ type: "LOAD", state: { items, variables, baseVersion, cluster } });
    },
    [],
  );

  const clear = useCallback(() => {
    dispatch({ type: "CLEAR" });
  }, []);

  // Clear in-memory state AND remove the persisted draft (used on intentional
  // cancel/save so a stale draft is not resurrected on the next visit).
  const clearPersisted = useCallback(() => {
    skipNextPersist.current = true;
    if (storageKey) {
      try {
        localStorage.removeItem(storageKey);
      } catch {
        // ignore
      }
    }
    dispatch({ type: "CLEAR" });
  }, [storageKey]);

  const hasItem = useCallback(
    (ref: { name: string; namespace?: string }) =>
      state.items.some((i) => itemRefKey(i) === itemRefKey(ref)),
    [state.items],
  );

  const toSpec = useCallback(
    (displayName: string): ComposedBundleSpec => {
      return {
        displayName,
        inputs: state.items.map((ref) => ({ itemRef: ref })),
        variables: Object.keys(state.variables).length > 0 ? state.variables : undefined,
      };
    },
    [state.items, state.variables],
  );

  const value: ComposerContextValue = {
    items: state.items,
    variables: state.variables,
    cluster: state.cluster,
    addItem,
    removeItem,
    reorderItem,
    setVar,
    setCluster,
    load,
    clear,
    hasItem,
    toSpec,
    clearPersisted,
  };

  return (
    <ComposerContext.Provider value={value}>
      {children}
    </ComposerContext.Provider>
  );
}

export function useComposer(): ComposerContextValue {
  const ctx = useContext(ComposerContext);
  if (!ctx) throw new Error("useComposer must be used within ComposerProvider");
  return ctx;
}
