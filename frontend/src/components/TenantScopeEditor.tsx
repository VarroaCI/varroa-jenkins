import { useId } from "react";
import { useQuery } from "@tanstack/react-query";
import { listPipelines, listTenants } from "../api/client";
import { jobGlobMatches } from "../lib/jobglob";
import { stripTenantFolder } from "../pages/pipelines/jobPath";
import type { TenantScope } from "../types/auth";
import styles from "./TenantScopeEditor.module.css";

/**
 * validateTenantScope reports every refused shape the CRD rejects for the
 * tenants dimension, except the one-dimension rule, which the form applies
 * by disabling a control. The API server's own rule stays the authority;
 * this check only stops a submit that would certainly fail.
 */
export function validateTenantScope(tenants: TenantScope[] | undefined): string[] {
  if (tenants === undefined) return [];
  const errors: string[] = [];
  if (tenants.length === 0) {
    errors.push("A tenants list must name at least one tenant. Remove the list to leave the scope wide.");
  }
  for (const entry of tenants) {
    if (entry.name.trim() === "") errors.push("A tenant entry must name a tenant.");
    else if (entry.name.includes("/")) errors.push(`The tenant name ${entry.name} must not hold "/".`);
    if (entry.jobs === undefined) continue;
    if (entry.jobs.length === 0) {
      errors.push("An omitted jobs list covers every job. Add a pattern, or remove the list.");
    }
    for (const pattern of entry.jobs) {
      if (pattern.trim() === "") errors.push("A job pattern must not be empty.");
      else if (pattern.startsWith("/")) {
        errors.push(`The pattern ${pattern} must not start with "/". A pattern is folder-relative.`);
      }
    }
  }
  return errors;
}

/** entryErrors reports the refusals of one entry, for display beside it. */
function entryErrors(entry: TenantScope): string[] {
  return validateTenantScope([entry]);
}

/**
 * JobMatchPreview counts the jobs one entry's patterns match. The count
 * names the editing caller's own view: the read is narrowed by the editing
 * caller's grants, never by the subject's. A read the server refuses states
 * that the job list is not available and blocks nothing.
 */
function JobMatchPreview({ tenantName, patterns }: { tenantName: string; patterns: string[] }) {
  const usable = patterns.filter((pattern) => pattern.trim() !== "");
  const { data, isError, isLoading } = useQuery({
    queryKey: ["pipelines", tenantName],
    queryFn: () => listPipelines(tenantName),
    enabled: tenantName.trim() !== "" && usable.length > 0,
    retry: false,
  });

  if (tenantName.trim() === "" || usable.length === 0) return null;
  if (isError) return <p className={styles.previewUnavailable}>The job list is not available.</p>;
  if (isLoading || !data) return <p className={styles.previewUnavailable}>Counting the matching jobs…</p>;

  const total = data.items.length;
  const matched = data.items.filter((item) => {
    const relative = stripTenantFolder(data.tenantFolder ?? "", item.jobPath);
    return usable.some((pattern) => jobGlobMatches(pattern, relative));
  }).length;

  return (
    <p className={styles.preview}>
      matches {matched} of {total} jobs you can see
    </p>
  );
}

/**
 * TenantScopeEditor owns the whole tenants dimension of a role binding
 * scope: the entry list, the tenant name control, the job pattern list, the
 * grammar help, the refusals, and the match preview.
 */
export default function TenantScopeEditor({
  value,
  onChange,
  disabled,
  disabledReason,
}: {
  value: TenantScope[] | undefined;
  onChange: (next: TenantScope[] | undefined) => void;
  disabled?: boolean;
  disabledReason?: string;
}) {
  const listId = useId();
  const tenantsQuery = useQuery({ queryKey: ["tenants"], queryFn: listTenants, retry: false });
  const entries = value ?? [];

  const replace = (index: number, entry: TenantScope) => {
    const next = entries.map((current, i) => (i === index ? entry : current));
    onChange(next);
  };

  const addEntry = () => onChange([...entries, { name: "" }]);
  const removeEntry = (index: number) => {
    const next = entries.filter((_, i) => i !== index);
    onChange(next.length > 0 ? next : undefined);
  };

  return (
    <div className={styles.editor}>
      <p className={styles.hint}>
        A tenant scope narrows a binding to one pipelines tenant. A job pattern is a Varroa rule. It
        narrows what Varroa shows and what Varroa starts. It changes nothing inside Jenkins.
      </p>
      {disabled && disabledReason && <p className={styles.disabledNote}>{disabledReason}</p>}

      <datalist id={listId}>
        {(tenantsQuery.data ?? []).map((tenant) => (
          <option key={tenant.name} value={tenant.name} />
        ))}
      </datalist>

      {entries.map((entry, index) => (
        <div key={index} className={styles.entry}>
          <div>
            <label className={styles.label} htmlFor={`${listId}-name-${index}`}>
              Tenant
            </label>
            <div className={styles.entryHead}>
              <input
                id={`${listId}-name-${index}`}
                className={styles.input}
                list={listId}
                value={entry.name}
                disabled={disabled}
                placeholder="payments"
                onChange={(e) => replace(index, { ...entry, name: e.target.value })}
              />
              <button
                type="button"
                className={styles.removeBtn}
                aria-label={`Remove tenant entry ${index + 1}`}
                disabled={disabled}
                onClick={() => removeEntry(index)}
              >
                &times;
              </button>
            </div>
            <p className={styles.hint}>A tenant that does not exist yet is accepted.</p>
          </div>

          {entry.jobs === undefined ? (
            <>
              <p className={styles.hint}>Every job of this tenant is covered.</p>
              <button
                type="button"
                className={styles.addBtn}
                disabled={disabled}
                onClick={() => replace(index, { ...entry, jobs: [] })}
              >
                + Narrow to job patterns
              </button>
            </>
          ) : (
            <div>
              <span className={styles.label}>Job patterns</span>
              {entry.jobs.map((pattern, patternIndex) => (
                <div key={patternIndex} className={styles.patternRow}>
                  <input
                    className={styles.input}
                    value={pattern}
                    disabled={disabled}
                    aria-label={`Job pattern ${patternIndex + 1} for entry ${index + 1}`}
                    placeholder="release/*"
                    onChange={(e) =>
                      replace(index, {
                        ...entry,
                        jobs: (entry.jobs ?? []).map((p, i) => (i === patternIndex ? e.target.value : p)),
                      })
                    }
                  />
                  <button
                    type="button"
                    className={styles.removeBtn}
                    aria-label={`Remove job pattern ${patternIndex + 1} for entry ${index + 1}`}
                    disabled={disabled}
                    onClick={() =>
                      replace(index, {
                        ...entry,
                        jobs: (entry.jobs ?? []).filter((_, i) => i !== patternIndex),
                      })
                    }
                  >
                    &times;
                  </button>
                </div>
              ))}
              <button
                type="button"
                className={styles.addBtn}
                disabled={disabled}
                onClick={() => replace(index, { ...entry, jobs: [...(entry.jobs ?? []), ""] })}
              >
                + Add pattern
              </button>
              <p className={styles.grammar}>
                <code>*</code> matches a run of characters that holds no <code>/</code>.{" "}
                <code>**</code> matches a run across <code>/</code>. <code>?</code> matches one
                character. A pattern is folder-relative, and never starts with <code>/</code>.
              </p>
              <JobMatchPreview tenantName={entry.name} patterns={entry.jobs} />
            </div>
          )}

          {entryErrors(entry).map((message) => (
            <p key={message} className={styles.error}>
              {message}
            </p>
          ))}
        </div>
      ))}

      {entries.length === 0 && value !== undefined && (
        <p className={styles.error}>
          A tenants list must name at least one tenant. Remove the list to leave the scope wide.
        </p>
      )}

      <button type="button" className={styles.addBtn} disabled={disabled} onClick={addEntry}>
        + Add tenant
      </button>
    </div>
  );
}
