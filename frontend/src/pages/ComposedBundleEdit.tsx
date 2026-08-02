import { useEffect, useMemo, useRef, useState } from "react";
import { useParams, Link, Navigate } from "react-router-dom";
import { getComposedBundle } from "../api/client";
import { usePermissions, canDoAnywhere } from "../hooks/usePermissions";
import { clusterQuery } from "../routing";
import { ComposerProvider, useComposer, hasDraft, draftBaseVersion } from "../context/ComposerContext";
import CatalogBrowser from "./CatalogBrowser";
import type { ComposedBundle, GitBundleSource } from "../types";
import styles from "./ComposedBundleEdit.module.css";
import NoAccessibleClusters from "../components/NoAccessibleClusters";
import { useConfigurationCluster } from "../hooks/useConfigurationCluster";

export interface EditTarget {
  namespace: string;
  name: string;
  baseBundle: ComposedBundle;
  gitInputs: GitBundleSource[];
}

/** localStorage key for an in-progress edit of a specific bundle. Cluster- and
 *  bundle-scoped so editing bundle A never bleeds into bundle B, same-named
 *  bundles on different clusters stay distinct, and it never clobbers the
 *  create-draft key. */
function editDraftKey(cluster: string, namespace: string, name: string): string {
  return `varroa_composer_edit_${cluster}_${namespace}_${name}`;
}

/**
 * Outer component: provides a bundle-scoped, persistent composer session so
 * in-progress edits survive navigation/refresh without touching the create-draft.
 */
export default function ComposedBundleEdit() {
  const { namespace = "", name = "" } = useParams();
  const { cluster, ready } = useConfigurationCluster();
  if (ready && !cluster) return <NoAccessibleClusters />;
  if (!cluster) return null;
  const storageKey = editDraftKey(cluster, namespace, name);
  return (
    // key: React Router reuses this element across edit routes for different
    // bundles (and clusters), so the provider's reducer state and the inner
    // component's one-shot refs (seededRef/hadDraftRef/draftBaseRef) would
    // otherwise carry the previous session into the new one. Keying by storageKey
    // (which folds in the cluster) remounts the whole composer session when the
    // target bundle OR cluster changes, forcing a fresh re-seed/re-preview
    // against the switched cluster's catalog.
    <ComposerProvider key={storageKey} storageKey={storageKey}>
      <ComposedBundleEditInner cluster={cluster} namespace={namespace} name={name} storageKey={storageKey} />
    </ComposerProvider>
  );
}

function ComposedBundleEditInner({
  cluster,
  namespace,
  name,
  storageKey,
}: {
  cluster: string;
  namespace: string;
  name: string;
  storageKey: string;
}) {
  const { data: perms, isLoading: permsLoading } = usePermissions();
  const composer = useComposer();

  const [bundle, setBundle] = useState<ComposedBundle | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const seededRef = useRef(false);
  // Whether a persisted edit draft already existed when this mount began (the
  // provider has hydrated it), plus the bundle version it was seeded from. A
  // draft is only trustworthy if it was based on the *current* bundle version;
  // otherwise the bundle changed server-side since the draft was written and we
  // must re-seed rather than silently edit an outdated snapshot (issue #261).
  const hadDraftRef = useRef(hasDraft(storageKey));
  const draftBaseRef = useRef(draftBaseVersion(storageKey));

  useEffect(() => {
    if (!name || !namespace) return;
    setLoading(true);
    setError(null);
    getComposedBundle(cluster, namespace, name)
      .then(setBundle)
      .catch((e: Error) => setError(e.message))
      .finally(() => setLoading(false));
  }, [cluster, name, namespace]);

  // Seed composer from the freshly-loaded bundle (one-shot). Keep a persisted
  // draft ONLY when it was seeded from the current bundle version — a draft based
  // on a superseded version is stale (e.g. the bundle was edited elsewhere while
  // this draft sat in localStorage) and must be discarded so the save reflects
  // the actual bundle rather than an outdated snapshot (issue #261). Seeding is a
  // single atomic LOAD so it never clears then re-adds through an empty state.
  useEffect(() => {
    if (!bundle || seededRef.current) return;
    seededRef.current = true;
    const bundleVersion = bundle.metadata?.resourceVersion ?? "";
    const draftIsCurrent = hadDraftRef.current && draftBaseRef.current === bundleVersion;
    if (draftIsCurrent) return;
    const items = (bundle.spec.inputs ?? []).flatMap((inp) => (inp.itemRef ? [inp.itemRef] : []));
    composer.load(items, bundle.spec.variables ?? {}, bundleVersion, cluster);
  }, [bundle, composer, cluster]);

  // Build editTarget (memoized on bundle identity)
  const editTarget = useMemo<EditTarget | null>(() => {
    if (!bundle) return null;
    const gitInputs = (bundle.spec.inputs ?? []).flatMap((i) => (i.gitSource ? [i.gitSource] : []));
    return { namespace, name, baseBundle: bundle, gitInputs };
  }, [bundle, namespace, name]);

  // Permission gate (placed after all hooks to keep hook order stable). Wait for
  // permissions to resolve before deciding, so we don't redirect during loading.
  if (permsLoading) {
    return (
      <div className={styles.page}>
        <div className={styles.loadingBanner}>Loading...</div>
      </div>
    );
  }
  if (!canDoAnywhere(perms, "composedbundles", "update")) {
    return <Navigate to={`/catalog/bundles/${namespace}/${name}${clusterQuery(cluster)}`} replace />;
  }

  if (loading) {
    return (
      <div className={styles.page}>
        <div className={styles.loadingBanner}>Loading bundle...</div>
      </div>
    );
  }

  if (error || !bundle) {
    return (
      <div className={styles.page}>
        <Link to={`/catalog/bundles${clusterQuery(cluster)}`} className={styles.backLink}>
          &larr; Back to Bundles
        </Link>
        <div className={styles.errorBanner}>
          {error ? `Failed to load: ${error}` : "Bundle not found"}
        </div>
      </div>
    );
  }

  return <CatalogBrowser editTarget={editTarget} />;
}
