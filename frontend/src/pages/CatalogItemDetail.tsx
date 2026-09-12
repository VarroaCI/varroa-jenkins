import { useEffect, useMemo, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { parse as parseYAML, stringify as stringifyYAML } from "yaml";
import type { RJSFSchema, UiSchema } from "@rjsf/utils";
import { getCatalogItem } from "../api/client";
import { clusterQuery } from "../routing";
import NoAccessibleClusters from "../components/NoAccessibleClusters";
import { useConfigurationCluster } from "../hooks/useConfigurationCluster";
import { VarroaForm } from "../components/specform/VarroaFormTheme";
import { templateValidator } from "../components/specform/templateValidator";
import type {
  CatalogItem,
  CatalogItemLockPins,
  CatalogVariable,
} from "../types";
import { VERDICT_LABEL, isWarningVerdict } from "../lib/compat";
import styles from "./CatalogItemDetail.module.css";

const TYPE_ICONS: Record<string, string> = {
  podtemplate: "━",
  plugin: "◆",
  item: "○",
  jcasc: "⚙",
  rbac: "⚇",
  "pipeline-template": "▶",
};

export default function CatalogItemDetail() {
  const { name, namespace } = useParams<{ name: string; namespace: string }>();
  const { cluster, ready } = useConfigurationCluster();
  const [item, setItem] = useState<CatalogItem | null>(null);
  // Lock pins are cluster state joined by the BFF at read time; they are
  // deliberately not stored on the item, where they would go stale.
  const [lockPins, setLockPins] = useState<CatalogItemLockPins[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    if (!cluster || !name || !namespace) return;
    setLoading(true);
    setError(null);
    getCatalogItem(cluster, namespace, name)
      .then((resp) => {
        setItem(resp.item);
        setLockPins(resp.lockPins ?? []);
      })
      .catch((e) => setError(e.message))
      .finally(() => setLoading(false));
  }, [cluster, name, namespace]);

  if (ready && !cluster) return <NoAccessibleClusters />;
  if (loading) {
    return <div className={styles.loadingBanner}>Loading item...</div>;
  }

  if (error) {
    return (
      <div>
        <Link to={`/catalog${clusterQuery(cluster)}`} className={styles.backLink}>
          &larr; Back to Catalog
        </Link>
        <div className={styles.errorBanner}>Error: {error}</div>
      </div>
    );
  }

  if (!item) return null;

  const spec = item.spec;
  const status = item.status;
  const vars: CatalogVariable[] = spec.variables || [];
  const closure = status?.closure ?? [];
  const compat = status?.compat ?? [];

  return (
    <div className={styles.page}>
      <Link to={`/catalog${clusterQuery(cluster)}`} className={styles.backLink}>
        &larr; Back to Catalog
      </Link>

      <div style={{ display: "flex", alignItems: "center", gap: 10, marginBottom: 4 }}>
        <span style={{ fontSize: 22 }}>{TYPE_ICONS[spec.type] || "○"}</span>
        <h2 className={styles.title}>{spec.displayName || item.metadata.name}</h2>
      </div>
      <p className={styles.subtitle}>
        Catalog item &middot; {spec.type}
      </p>

      {/* Validation badge */}
      {status && (
        <div style={{ marginBottom: 20 }}>
          {status.valid ? (
            <span className={styles.validBadge}>&#x2713; Valid</span>
          ) : (
            <span className={styles.invalidBadge}>
              &#x26A0; Invalid{status.message ? `: ${status.message}` : ""}
            </span>
          )}
        </div>
      )}

      {/* Details section */}
      <div className={styles.section}>
        <h3 className={styles.sectionTitle}>Details</h3>
        <div className={styles.detailGrid}>
          <span className={styles.detailLabel}>Name</span>
          <span className={styles.mono}>{item.metadata.name}</span>
          <span className={styles.detailLabel}>Namespace</span>
          <span>{item.metadata.namespace || "default"}</span>
          <span className={styles.detailLabel}>Source</span>
          <span>{spec.sourceRef}</span>
          <span className={styles.detailLabel}>Type</span>
          <span>{spec.type}</span>
          <span className={styles.detailLabel}>Path</span>
          <span className={styles.mono}>{spec.path}</span>
          {spec.version && (
            <>
              <span className={styles.detailLabel}>Version</span>
              <span className={styles.mono}>{spec.version}</span>
            </>
          )}
          {status?.contentHash && (
            <>
              <span className={styles.detailLabel}>Content Hash</span>
              <span className={styles.mono}>{status.contentHash.substring(0, 16)}...</span>
            </>
          )}
          {status?.observedRevision && (
            <>
              <span className={styles.detailLabel}>Revision</span>
              <span className={styles.mono}>{status.observedRevision}</span>
            </>
          )}
          <span className={styles.detailLabel}>Created</span>
          <span>
            {item.metadata.creationTimestamp
              ? new Date(item.metadata.creationTimestamp).toLocaleDateString()
              : "-"}
          </span>
        </div>
      </div>

      {/* Description */}
      {spec.description && (
        <div className={styles.section}>
          <h3 className={styles.sectionTitle}>Description</h3>
          <p style={{ fontSize: "0.92rem", lineHeight: 1.6, color: "var(--text-2)" }}>
            {spec.description}
          </p>
        </div>
      )}

      {/* Tags */}
      {spec.tags && spec.tags.length > 0 && (
        <div className={styles.section}>
          <h3 className={styles.sectionTitle}>Tags</h3>
          <div className={styles.tags}>
            {spec.tags.map((tag) => (
              <span key={tag} className={styles.tag}>{tag}</span>
            ))}
          </div>
        </div>
      )}

      {/* Variables */}
      {vars.length > 0 && (
        <div className={styles.section}>
          <h3 className={styles.sectionTitle}>
            Variables ({vars.length})
          </h3>
          {vars.map((v) => (
            <div key={v.name} className={styles.variableCard}>
              <div className={styles.variableName}>
                {v.name}
                {v.required && (
                  <span style={{ color: "var(--bad-text)", marginLeft: 4 }}>*</span>
                )}
                {v.type && (
                  <span className={styles.tag} style={{ marginLeft: 8 }}>{v.type}</span>
                )}
              </div>
              {v.default && (
                <div className={styles.variableMeta}>
                  Default: <span className={styles.mono}>{v.default}</span>
                </div>
              )}
              {v.description && (
                <div className={styles.variableDesc}>{v.description}</div>
              )}
              {v.allowedValues && v.allowedValues.length > 0 && (
                <div className={styles.variableMeta}>
                  Allowed: <span className={styles.mono}>{v.allowedValues.join(", ")}</span>
                </div>
              )}
            </div>
          ))}
        </div>
      )}

      {/* Pinned dependency closure. Every column is read straight from
          status.closure — the browser re-derives nothing, having neither the
          store annotations nor the solver. */}
      {closure.length > 0 && (
        <div className={styles.section}>
          <h3 className={styles.sectionTitle}>Pinned closure ({closure.length})</h3>
          <div className={styles.tableScroll}>
            <table className={styles.table}>
              <thead>
                <tr>
                  <th>Plugin</th>
                  <th>Version</th>
                  <th>Origin</th>
                  <th>From</th>
                  <th>Minimum</th>
                  {lockPins.map((lp) => (
                    <th key={lp.profile}>{lp.profile} lock</th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {closure.map((entry) => (
                  <tr key={entry.artifactId}>
                    <td className={styles.mono}>{entry.artifactId}</td>
                    <td className={styles.mono}>{entry.version}</td>
                    <td>{entry.direct ? "direct" : "transitive"}</td>
                    <td>{entry.provenance ?? "-"}</td>
                    <td className={styles.mono}>{entry.minimum || "-"}</td>
                    {lockPins.map((lp) => {
                      const pinned = lp.pins[entry.artifactId];
                      // No key means this profile\u2019s lock does not pin the
                      // plugin at all, which is distinct from pinning it equal.
                      if (pinned === undefined) {
                        return (
                          <td key={lp.profile} className={styles.lockUnpinned}>
                            not pinned
                          </td>
                        );
                      }
                      const diverges = pinned !== entry.version;
                      return (
                        <td
                          key={lp.profile}
                          className={diverges ? styles.lockDiverges : styles.mono}
                          title={
                            diverges
                              ? "This profile pins a different version; selecting this item will trip the provisioning plugin-version conflict check."
                              : undefined
                          }
                        >
                          {pinned}
                          {diverges ? " \u2260" : ""}
                        </td>
                      );
                    })}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}

      {/* Per-profile compatibility. Advisory in every case: nothing here blocks
          selection, composition, or provisioning. */}
      {compat.length > 0 && (
        <div className={styles.section}>
          <h3 className={styles.sectionTitle}>Compatibility ({compat.length} profiles)</h3>
          <p className={styles.advisoryNote}>
            Advisory only. These verdicts never block selecting this item into a bundle or
            provisioning a controller.
          </p>
          <div className={styles.tableScroll}>
            <table className={styles.table}>
              <thead>
                <tr>
                  <th>Profile</th>
                  <th>Jenkins</th>
                  <th>Verdict</th>
                  <th>Detail</th>
                </tr>
              </thead>
              <tbody>
                {compat.map((c) => (
                  <tr key={c.profile}>
                    <td>{c.profile}</td>
                    <td className={styles.mono}>{c.jenkinsVersion || "-"}</td>
                    <td>
                      <span className={styles.compatBadge} data-verdict={c.verdict}>
                        {isWarningVerdict(c.verdict) ? "\u26A0 " : ""}
                        {VERDICT_LABEL[c.verdict]}
                      </span>
                    </td>
                    <td className={styles.compatMessage}>{c.message || "-"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}

      {/* A pipeline-template item's content is a PipelineTemplate document,
          not items YAML (pipeline-template-catalog-items). Its dedicated
          card below replaces the generic raw-content preview. */}
      {spec.type === "pipeline-template" ? (
        <PipelineTemplateCard content={status?.content} />
      ) : (
        status?.content && (
          <div className={styles.section}>
            <h3 className={styles.sectionTitle}>Content</h3>
            <pre className={styles.yamlBlock}>{status.content}</pre>
          </div>
        )
      )}
    </div>
  );
}

/** The subset of a PipelineTemplate document (design D2) this page renders:
 * the header, the parameter form, and the marker YAML the form state
 * produces. Fields this page does not read (jenkinsfile, scmDefinition) are
 * left untyped here. */
interface PipelineTemplateDocument {
  metadata?: {
    name?: string;
    displayName?: string;
    description?: string;
  };
  spec?: {
    version?: string;
    parameters?: Record<string, unknown>;
    uiSchema?: Record<string, unknown>;
  };
}

/** parseTemplateDocument reads a pipeline-template item's raw YAML content
 * into a PipelineTemplateDocument, or null when the content is missing or
 * does not parse as a YAML mapping. */
function parseTemplateDocument(content: string | undefined): PipelineTemplateDocument | null {
  if (!content) return null;
  try {
    const parsed: unknown = parseYAML(content);
    if (!parsed || typeof parsed !== "object") return null;
    return parsed as PipelineTemplateDocument;
  } catch {
    return null;
  }
}

/** buildMarkerYaml renders the marker document (design D6) the current form
 * values produce for this template: the fixed apiVersion and kind, the
 * template's own name and version, and the entered values. `values` is
 * omitted from the document entirely when the form holds none, matching a
 * marker with no `values` key. */
function buildMarkerYaml(doc: PipelineTemplateDocument, values: Record<string, unknown>): string {
  const marker: Record<string, unknown> = {
    apiVersion: "varroa.dev/v1",
    kind: "Pipeline",
    template: doc.metadata?.name ?? "",
  };
  if (doc.spec?.version) marker.templateVersion = doc.spec.version;
  if (Object.keys(values).length > 0) marker.values = values;
  return stringifyYAML(marker);
}

/**
 * PipelineTemplateCard renders a pipeline-template item's detail: the
 * template header (metadata.displayName, spec.version,
 * metadata.description), a live form built from spec.parameters and
 * spec.uiSchema with the dedicated 2020-12 validator, and the marker YAML
 * the current form state produces, with a copy control (design D12).
 */
function PipelineTemplateCard({ content }: { content?: string }) {
  const doc = useMemo(() => parseTemplateDocument(content), [content]);
  const [values, setValues] = useState<Record<string, unknown>>({});
  const [copied, setCopied] = useState(false);

  // A different item's content replaces this one's entered values rather
  // than carrying them over.
  useEffect(() => {
    setValues({});
    setCopied(false);
  }, [content]);

  if (!doc) {
    return (
      <div className={styles.section}>
        <h3 className={styles.sectionTitle}>Template</h3>
        <div className={styles.errorBanner}>Unable to read the template document.</div>
      </div>
    );
  }

  const schema = (doc.spec?.parameters ?? {}) as RJSFSchema;
  const uiSchema = doc.spec?.uiSchema as UiSchema | undefined;
  const markerYaml = buildMarkerYaml(doc, values);

  async function copyMarker() {
    try {
      await navigator.clipboard.writeText(markerYaml);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    } catch {
      // The browser can deny clipboard access; the YAML stays on screen for
      // a manual copy.
    }
  }

  return (
    <div className={styles.section}>
      <h3 className={styles.sectionTitle}>Template</h3>
      <div className={styles.detailGrid}>
        <span className={styles.detailLabel}>Name</span>
        <span>{doc.metadata?.displayName || doc.metadata?.name}</span>
        {doc.spec?.version && (
          <>
            <span className={styles.detailLabel}>Version</span>
            <span className={styles.mono}>{doc.spec.version}</span>
          </>
        )}
      </div>
      {doc.metadata?.description && (
        <p style={{ fontSize: "0.92rem", lineHeight: 1.6, color: "var(--text-2)", marginTop: 8 }}>
          {doc.metadata.description}
        </p>
      )}

      <VarroaForm
        schema={schema}
        uiSchema={uiSchema}
        validator={templateValidator}
        formData={values}
        liveValidate
        onChange={(e) => setValues((e.formData as Record<string, unknown>) ?? {})}
      >
        {/* No submit button — the page has nothing to submit the form to. */}
        <div />
      </VarroaForm>

      <div className={styles.markerHeader}>
        <span className={styles.detailLabel}>.varroa/pipeline.yaml</span>
        <button type="button" className={styles.copyBtn} onClick={copyMarker}>
          {copied ? "Copied" : "Copy"}
        </button>
      </div>
      <pre className={styles.yamlBlock}>{markerYaml}</pre>
    </div>
  );
}
