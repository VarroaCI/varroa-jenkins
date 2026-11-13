// assistantConfigEdit holds the pure rules of the Assistant admin forms. It
// mirrors the rules of api/v1alpha1/assistantconfig_validation.go, so a fault
// shows before a save. The server stays the authority.
import type {
  AssistantConfigObject,
  AssistantConfigProvider,
  AssistantConfigSpec,
  DataSharingLevel,
} from "../../types/assistant";

export const MAX_PROVIDERS = 16;
export const MAX_MODELS = 32;
export const MAX_MODEL_LEN = 128;
export const MAX_CIDRS = 16;
export const MAX_BASE_URL_LEN = 2048;

const NAME_RE = /^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$/;
const BASE_RE = /^https?:\/\//;
// MAX_STEPS is the largest int32, the type of budgets.steps on the server.
const MAX_STEPS = 2_147_483_647;
const EXPIRY_RE = /^[1-9][0-9]*(m|h)$/;
const KINDS = ["anthropic", "openai-compatible"];
const TRUSTS = ["internal", "external"];
const AUTHS = ["apiKey", "none"];
const LEVELS: DataSharingLevel[] = ["none", "metadata", "logs"];
const LEVEL_RANK: Record<string, number> = { none: 0, metadata: 1, logs: 2 };

/** ProviderDraft holds the text of the provider form. */
export interface ProviderDraft {
  name: string;
  kind: string;
  baseURL: string;
  models: string;
  trust: string;
  auth: string;
  cidrs: string;
  port: string;
}

export type ProviderField = keyof ProviderDraft;
export type ProviderErrors = Partial<Record<ProviderField, string>>;

export const EMPTY_DRAFT: ProviderDraft = {
  name: "",
  kind: "openai-compatible",
  baseURL: "",
  models: "",
  trust: "external",
  auth: "apiKey",
  cidrs: "",
  port: "",
};

/** parseList splits a text box into its entries. Commas and white space both separate. */
export function parseList(text: string): string[] {
  return text.split(/[,\s]+/).map((s) => s.trim()).filter((s) => s !== "");
}

/** draftOf copies a stored provider into the form text. */
export function draftOf(p: AssistantConfigProvider): ProviderDraft {
  return {
    name: p.name,
    kind: p.kind,
    baseURL: p.baseURL ?? "",
    models: p.models.join("\n"),
    trust: p.trust,
    auth: p.auth ?? "apiKey",
    cidrs: (p.cidrs ?? []).join("\n"),
    port: p.port ? String(p.port) : "",
  };
}

/** providerOf turns a valid draft into the stored shape. Empty optional fields are left out. */
export function providerOf(d: ProviderDraft): AssistantConfigProvider {
  const p: AssistantConfigProvider = {
    name: d.name.trim(),
    kind: d.kind as AssistantConfigProvider["kind"],
    models: parseList(d.models),
    trust: d.trust as AssistantConfigProvider["trust"],
    auth: d.auth as AssistantConfigProvider["auth"],
  };
  const base = d.baseURL.trim();
  if (base !== "") p.baseURL = base;
  const cidrs = parseList(d.cidrs);
  if (cidrs.length > 0) p.cidrs = cidrs;
  const port = d.port.trim();
  if (port !== "") p.port = Number(port);
  return p;
}

/** isCIDR reports whether text is an IPv4 or IPv6 address with a prefix length. */
export function isCIDR(text: string): boolean {
  const slash = text.lastIndexOf("/");
  if (slash < 0) return false;
  const addr = text.slice(0, slash);
  const prefix = text.slice(slash + 1);
  if (!/^\d{1,3}$/.test(prefix)) return false;
  const bits = Number(prefix);
  if (isIPv4(addr)) return bits <= 32;
  if (isIPv6(addr)) return bits <= 128;
  return false;
}

function isIPv4(a: string): boolean {
  // An octet with a leading zero is refused, as net.ParseCIDR refuses it.
  const parts = a.split(".");
  return parts.length === 4 && parts.every((p) => /^(0|[1-9]\d{0,2})$/.test(p) && Number(p) <= 255);
}

function isIPv6(a: string): boolean {
  // A dotted IPv4 tail, such as ::ffff:192.0.2.0, counts as two groups.
  let text = a;
  const last = a.lastIndexOf(":");
  if (a.slice(last + 1).includes(".")) {
    if (last < 0 || !isIPv4(a.slice(last + 1))) return false;
    text = a.slice(0, last + 1) + "0:0";
  }
  if (!/^[0-9a-fA-F:]+$/.test(text) || !text.includes(":")) return false;
  const halves = text.split("::");
  if (halves.length > 2) return false;
  // An empty half holds no groups. A single leading or trailing colon leaves an
  // empty group, which fails the group check below.
  const groupsOf = (s: string) => (s === "" ? [] : s.split(":"));
  const groups = halves.flatMap(groupsOf);
  if (!groups.every((g) => /^[0-9a-fA-F]{1,4}$/.test(g))) return false;
  return halves.length === 2 ? groups.length <= 7 : groups.length === 8;
}

/** validateProvider checks one provider draft. takenNames lists the other providers' names. */
export function validateProvider(d: ProviderDraft, takenNames: string[]): ProviderErrors {
  const e: ProviderErrors = {};
  const name = d.name.trim();
  if (!NAME_RE.test(name)) {
    e.name = "Use up to 63 lowercase letters, digits, or hyphens. Start and end with a letter or digit.";
  } else if (takenNames.includes(name)) {
    e.name = "Another provider has this name.";
  }
  if (!KINDS.includes(d.kind)) e.kind = "Pick a kind.";
  const base = d.baseURL.trim();
  if (base !== "" && (!BASE_RE.test(base) || base.length > MAX_BASE_URL_LEN)) {
    e.baseURL = "The base URL starts with http:// or https://.";
  } else if (base === "" && d.kind === "openai-compatible") {
    e.baseURL = "An openai-compatible provider needs a base URL.";
  }
  const models = parseList(d.models);
  if (models.length < 1 || models.length > MAX_MODELS) {
    e.models = `List 1 to ${MAX_MODELS} models.`;
  } else if (models.some((m) => m.length > MAX_MODEL_LEN)) {
    e.models = `Each model name has at most ${MAX_MODEL_LEN} characters.`;
  }
  if (!TRUSTS.includes(d.trust)) e.trust = "Pick a trust level.";
  if (!AUTHS.includes(d.auth)) e.auth = "Pick an auth mode.";
  const cidrs = parseList(d.cidrs);
  if (cidrs.length > MAX_CIDRS) {
    e.cidrs = `List at most ${MAX_CIDRS} ranges.`;
  } else if (cidrs.some((c) => !isCIDR(c))) {
    e.cidrs = "Each range is an address with a prefix, such as 192.0.2.0/24.";
  }
  const port = d.port.trim();
  if (port !== "" && !(/^\d+$/.test(port) && Number(port) >= 1 && Number(port) <= 65535)) {
    e.port = "The port is 1 to 65535, or empty to use the port of the base URL.";
  }
  return e;
}

/** LimitsDraft holds the text of the limits form. */
export interface LimitsDraft {
  session: string;
  userDay: string;
  tenantDay: string;
  steps: string;
  approvalExpiry: string;
  externalDataSharing: string;
}

export type LimitsErrors = Partial<Record<keyof LimitsDraft, string>>;

export function limitsDraftOf(spec: AssistantConfigSpec): LimitsDraft {
  return {
    session: String(spec.budgets.session),
    userDay: String(spec.budgets.userDay),
    tenantDay: String(spec.budgets.tenantDay),
    steps: String(spec.budgets.steps),
    approvalExpiry: spec.approvalExpiry,
    externalDataSharing: spec.externalDataSharing,
  };
}

const COUNT_MESSAGE = "Enter a whole number of 0 or more. Zero uses the default.";

// isCount accepts a whole number that a JavaScript number holds exactly. The
// server stores budgets as 64-bit integers, so a larger value would round.
function isCount(text: string): boolean {
  const t = text.trim();
  return /^\d+$/.test(t) && Number.isSafeInteger(Number(t));
}

/** validateLimits checks the limits draft. Each budget is a whole number. Zero means the default, as the schema defines it. */
export function validateLimits(d: LimitsDraft): LimitsErrors {
  const e: LimitsErrors = {};
  if (!isCount(d.session)) e.session = COUNT_MESSAGE;
  if (!isCount(d.userDay)) e.userDay = COUNT_MESSAGE;
  if (!isCount(d.tenantDay)) e.tenantDay = COUNT_MESSAGE;
  if (!isCount(d.steps) || Number(d.steps.trim()) > MAX_STEPS) e.steps = `Enter a whole number from 0 to ${MAX_STEPS}. Zero uses the default.`;
  if (!EXPIRY_RE.test(d.approvalExpiry.trim())) e.approvalExpiry = "Use a whole number with m or h, such as 24h.";
  if (!LEVELS.includes(d.externalDataSharing as DataSharingLevel)) e.externalDataSharing = "Pick a level.";
  return e;
}

/** applyLimits returns the spec with the limits draft written in. Call it after validateLimits passes. */
export function applyLimits(spec: AssistantConfigSpec, d: LimitsDraft): AssistantConfigSpec {
  return {
    ...spec,
    budgets: {
      session: Number(d.session.trim()),
      userDay: Number(d.userDay.trim()),
      tenantDay: Number(d.tenantDay.trim()),
      steps: Number(d.steps.trim()),
    },
    approvalExpiry: d.approvalExpiry.trim(),
    externalDataSharing: d.externalDataSharing as DataSharingLevel,
  };
}

/** Outcome is the result of a spec change. A refusal carries the message to show. */
export type Outcome = { ok: true; spec: AssistantConfigSpec } | { ok: false; message: string };

/**
 * upsertProvider adds a provider, or replaces the provider named previousName.
 * A rename of the default provider moves the default with it. A new default
 * model must be one of the new provider's models.
 */
export function upsertProvider(spec: AssistantConfigSpec, p: AssistantConfigProvider, previousName?: string): Outcome {
  if (previousName === undefined) {
    if (spec.providers.length >= MAX_PROVIDERS) {
      return { ok: false, message: `The object holds at most ${MAX_PROVIDERS} providers.` };
    }
    if (spec.providers.length === 0) {
      return { ok: true, spec: { ...spec, providers: [p], defaultProvider: p.name, defaultModel: p.models[0] } };
    }
    return { ok: true, spec: { ...spec, providers: [...spec.providers, p] } };
  }
  const idx = spec.providers.findIndex((x) => x.name === previousName);
  if (idx < 0) return { ok: false, message: `Provider ${previousName} is not listed. Reload the page.` };
  const isDefault = spec.defaultProvider === previousName;
  if (isDefault && spec.defaultModel && !p.models.includes(spec.defaultModel)) {
    return { ok: false, message: `The default model ${spec.defaultModel} is not one of the models of this provider. Pick a new default first.` };
  }
  const providers = spec.providers.slice();
  providers[idx] = p;
  return { ok: true, spec: { ...spec, providers, defaultProvider: isDefault ? p.name : spec.defaultProvider } };
}

/**
 * removeProvider deletes a provider. Removing the default provider needs
 * newDefault, a listed provider that becomes the default with its first model.
 * Removing the last provider clears both defaults.
 */
export function removeProvider(spec: AssistantConfigSpec, name: string, newDefault?: string): Outcome {
  if (!spec.providers.some((p) => p.name === name)) {
    return { ok: false, message: `Provider ${name} is not listed. Reload the page.` };
  }
  const rest = spec.providers.filter((p) => p.name !== name);
  if (rest.length === 0) {
    return { ok: true, spec: { ...spec, providers: [], defaultProvider: "", defaultModel: "" } };
  }
  if (spec.defaultProvider !== name) {
    return { ok: true, spec: { ...spec, providers: rest } };
  }
  const next = rest.find((p) => p.name === newDefault);
  if (next === undefined) {
    return { ok: false, message: "Pick a new default provider in the same save." };
  }
  return { ok: true, spec: { ...spec, providers: rest, defaultProvider: next.name, defaultModel: next.models[0] } };
}

/**
 * confirmationFor lists the effects of a change on data sharing. A change with
 * no such effect returns an empty list and saves at once.
 */
export function confirmationFor(before: AssistantConfigSpec, after: AssistantConfigSpec): string[] {
  const out: string[] = [];
  for (const p of after.providers) {
    const old = before.providers.find((o) => o.name === p.name);
    if (p.trust === "internal" && old?.trust !== "internal") {
      out.push(`Provider ${p.name} becomes internal. An internal provider has no data sharing ceiling, so it receives the level of each user and tenant.`);
    }
  }
  // The operator removes the key of a provider that is no longer listed. A
  // rename therefore removes the key too.
  for (const old of before.providers) {
    const next = after.providers.find((p) => p.name === old.name);
    const wasKeyed = (old.auth ?? "apiKey") !== "none";
    const remainsKeyed = next !== undefined && (next.auth ?? "apiKey") !== "none";
    if (wasKeyed && !remainsKeyed) {
      const reason = next ? "no longer takes a key" : "is gone from this save";
      out.push(`Provider ${old.name} ${reason}, so its saved key is removed.`);
    }
  }
  const was = before.externalDataSharing || "metadata";
  const now = after.externalDataSharing || "metadata";
  if ((LEVEL_RANK[now] ?? 0) > (LEVEL_RANK[was] ?? 0)) {
    out.push(`External data sharing rises from ${was} to ${now}. External providers may then receive ${now} data.`);
  }
  return out;
}

/** withoutManagedFields copies the metadata of the read and drops the server-owned field managers. */
function withoutManagedFields(meta: AssistantConfigObject["metadata"]): AssistantConfigObject["metadata"] {
  const copy: Record<string, unknown> = { ...meta };
  delete copy.managedFields;
  return copy as AssistantConfigObject["metadata"];
}

/**
 * configBody builds the PUT body of the whole object. The metadata of the read
 * goes back unchanged, so labels, annotations, and owner references survive,
 * and its version fences the write. Empty defaults are left out, because
 * the schema refuses an empty default name.
 */
export function configBody(base: AssistantConfigObject, spec: AssistantConfigSpec): AssistantConfigObject {
  const { defaultProvider, defaultModel, ...rest } = spec;
  const out: AssistantConfigSpec = { ...rest };
  if (defaultProvider) out.defaultProvider = defaultProvider;
  if (defaultModel) out.defaultModel = defaultModel;
  return {
    apiVersion: base.apiVersion,
    kind: base.kind,
    metadata: withoutManagedFields(base.metadata),
    spec: out,
  };
}
