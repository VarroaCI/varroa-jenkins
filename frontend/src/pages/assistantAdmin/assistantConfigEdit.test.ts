import { describe, it, expect } from "vitest";
import type { AssistantConfigProvider, AssistantConfigSpec } from "../../types/assistant";
import {
  EMPTY_DRAFT,
  applyLimits,
  configBody,
  confirmationFor,
  draftOf,
  isCIDR,
  limitsDraftOf,
  parseList,
  providerOf,
  removeProvider,
  upsertProvider,
  validateLimits,
  validateProvider,
} from "./assistantConfigEdit";

const smoke: AssistantConfigProvider = {
  name: "smoke", kind: "openai-compatible", baseURL: "http://192.0.2.10:8082/v1",
  models: ["varroa-smoke"], trust: "internal", auth: "none", cidrs: ["192.0.2.10/32"], port: 8082,
};
const backup: AssistantConfigProvider = {
  name: "backup", kind: "anthropic", models: ["model-c", "model-d"], trust: "external", auth: "apiKey",
};

function spec(over: Partial<AssistantConfigSpec> = {}): AssistantConfigSpec {
  return {
    providers: [smoke, backup],
    defaultProvider: "smoke",
    defaultModel: "varroa-smoke",
    externalDataSharing: "metadata",
    budgets: { session: 200000, userDay: 1000000, tenantDay: 2000000, steps: 32 },
    approvalExpiry: "24h",
    ...over,
  };
}

describe("parseList", () => {
  it("splits on commas and white space and drops empty entries", () => {
    expect(parseList(" a, b\nc  ,, ")).toEqual(["a", "b", "c"]);
    expect(parseList("   ")).toEqual([]);
  });
});

describe("isCIDR", () => {
  it("accepts IPv4 and IPv6 ranges with a valid prefix", () => {
    expect(isCIDR("192.0.2.0/24")).toBe(true);
    expect(isCIDR("192.0.2.1/32")).toBe(true);
    expect(isCIDR("2001:db8::/32")).toBe(true);
    expect(isCIDR("2001:db8:0:0:0:0:0:1/128")).toBe(true);
  });

  it("refuses a bad address, a bad octet, or a prefix out of range", () => {
    expect(isCIDR("192.0.2.0")).toBe(false);
    expect(isCIDR("192.0.2.256/24")).toBe(false);
    expect(isCIDR("192.0.2.0/33")).toBe(false);
    expect(isCIDR("2001:db8::/129")).toBe(false);
    expect(isCIDR("example.test/24")).toBe(false);
    expect(isCIDR("2001:db8::1::2/64")).toBe(false);
    expect(isCIDR(":1:2:3:4:5:6:7:8/64")).toBe(false);
    expect(isCIDR("1:2:3:4:5:6:7:8:/64")).toBe(false);
    expect(isCIDR("::/0")).toBe(true);
    expect(isCIDR("::ffff:192.0.2.0/120")).toBe(true);
    expect(isCIDR("::ffff:192.0.2.300/120")).toBe(false);
    expect(isCIDR("1.2.3.4:5/24")).toBe(false);
  });
});

describe("validateProvider", () => {
  const good = { ...EMPTY_DRAFT, name: "primary", kind: "anthropic", models: "model-a", cidrs: "192.0.2.0/24", port: "443" };

  it("accepts a complete draft", () => {
    expect(validateProvider(good, [])).toEqual({});
  });

  it("refuses a name that is not a DNS label or is already taken", () => {
    expect(validateProvider({ ...good, name: "Bad_Name" }, []).name).toBeTruthy();
    expect(validateProvider({ ...good, name: "-lead" }, []).name).toBeTruthy();
    expect(validateProvider({ ...good, name: "primary" }, ["primary"]).name).toBe("Another provider has this name.");
  });

  it("needs a base URL for openai-compatible and an http scheme when one is given", () => {
    const openai = { ...good, kind: "openai-compatible", baseURL: "" };
    expect(validateProvider(openai, []).baseURL).toBe("An openai-compatible provider needs a base URL.");
    expect(validateProvider({ ...good, baseURL: "ftp://x.test" }, []).baseURL).toBeTruthy();
    expect(validateProvider({ ...good, kind: "openai-compatible", baseURL: "https://models.test/v1" }, []).baseURL).toBeUndefined();
  });

  it("needs one to 32 models", () => {
    expect(validateProvider({ ...good, models: "" }, []).models).toBeTruthy();
    const many = Array.from({ length: 33 }, (_, i) => `m${i}`).join(",");
    expect(validateProvider({ ...good, models: many }, []).models).toBeTruthy();
  });

  it("checks each CIDR and the port range", () => {
    expect(validateProvider({ ...good, cidrs: "192.0.2.0/99" }, []).cidrs).toBeTruthy();
    expect(validateProvider({ ...good, port: "0" }, []).port).toBeTruthy();
    expect(validateProvider({ ...good, port: "65536" }, []).port).toBeTruthy();
    expect(validateProvider({ ...good, port: "" }, []).port).toBeUndefined();
  });
});

describe("providerOf and draftOf", () => {
  it("round-trips a stored provider and leaves empty optional fields out", () => {
    expect(providerOf(draftOf(smoke))).toEqual(smoke);
    const bare = providerOf({ ...EMPTY_DRAFT, name: "bare", kind: "anthropic", models: "m", trust: "external", auth: "apiKey" });
    expect(bare).toEqual({ name: "bare", kind: "anthropic", models: ["m"], trust: "external", auth: "apiKey" });
    expect("baseURL" in bare || "cidrs" in bare || "port" in bare).toBe(false);
  });
});

describe("upsertProvider", () => {
  it("appends a provider and keeps the default", () => {
    const out = upsertProvider(spec(), { ...backup, name: "third" });
    expect(out.ok && out.spec.providers.map((p) => p.name)).toEqual(["smoke", "backup", "third"]);
    expect(out.ok && out.spec.defaultProvider).toBe("smoke");
  });

  it("makes the first provider the default with its first model", () => {
    const out = upsertProvider(spec({ providers: [], defaultProvider: undefined, defaultModel: undefined }), backup);
    expect(out.ok && out.spec.defaultProvider).toBe("backup");
    expect(out.ok && out.spec.defaultModel).toBe("model-c");
  });

  it("moves the default with a rename of the default provider", () => {
    const out = upsertProvider(spec(), { ...smoke, name: "local" }, "smoke");
    expect(out.ok && out.spec.defaultProvider).toBe("local");
    expect(out.ok && out.spec.providers[0].name).toBe("local");
  });

  it("refuses to drop the default model from the default provider", () => {
    const out = upsertProvider(spec(), { ...smoke, models: ["other"] }, "smoke");
    expect(out.ok).toBe(false);
  });
});

describe("removeProvider", () => {
  it("hands the default to the named provider with its first model", () => {
    const out = removeProvider(spec(), "smoke", "backup");
    expect(out.ok && out.spec.providers.map((p) => p.name)).toEqual(["backup"]);
    expect(out.ok && out.spec.defaultProvider).toBe("backup");
    expect(out.ok && out.spec.defaultModel).toBe("model-c");
  });

  it("refuses to remove the default without a new default", () => {
    const out = removeProvider(spec(), "smoke");
    expect(out.ok).toBe(false);
  });

  it("keeps the default when another provider goes", () => {
    const out = removeProvider(spec(), "backup");
    expect(out.ok && out.spec.defaultProvider).toBe("smoke");
    expect(out.ok && out.spec.defaultModel).toBe("varroa-smoke");
  });

  it("clears both defaults when the last provider goes", () => {
    const out = removeProvider(spec({ providers: [smoke] }), "smoke");
    expect(out.ok && out.spec.providers).toEqual([]);
    expect(out.ok && out.spec.defaultProvider).toBe("");
    expect(out.ok && out.spec.defaultModel).toBe("");
  });
});

describe("confirmationFor", () => {
  it("names a provider that becomes internal", () => {
    const after = spec({ providers: [smoke, { ...backup, trust: "internal" }] });
    const msgs = confirmationFor(spec(), after);
    expect(msgs).toEqual([expect.stringContaining("Provider backup becomes internal.")]);
  });

  it("names a rise in external data sharing and nothing for a fall", () => {
    expect(confirmationFor(spec(), spec({ externalDataSharing: "logs" }))).toEqual([
      "External data sharing rises from metadata to logs. External providers may then receive logs data.",
    ]);
    expect(confirmationFor(spec(), spec({ externalDataSharing: "none" }))).toEqual([]);
  });

  it("names the saved key that a rename or a removal takes away", () => {
    const renamed = spec({ providers: [smoke, { ...backup, name: "backup-eu" }], defaultProvider: "smoke" });
    expect(confirmationFor(spec(), renamed)).toEqual(["Provider backup is gone from this save, so its saved key is removed."]);
    const dropped = spec({ providers: [smoke] });
    expect(confirmationFor(spec(), dropped)).toEqual(["Provider backup is gone from this save, so its saved key is removed."]);
    const unkeyed = spec({ providers: [smoke, backup], defaultProvider: "smoke" });
    expect(confirmationFor(unkeyed, spec({ providers: [smoke] }))).toHaveLength(1);
  });

  it("names the saved key that a switch to auth none takes away", () => {
    const off = spec({ providers: [smoke, { ...backup, auth: "none" }] });
    expect(confirmationFor(spec(), off)).toEqual(["Provider backup no longer takes a key, so its saved key is removed."]);
    const back = spec({ providers: [smoke, { ...backup, auth: "apiKey" }] });
    expect(confirmationFor(off, back)).toEqual([]);
  });

  it("treats an empty sharing level as metadata", () => {
    const unset = "" as AssistantConfigSpec["externalDataSharing"];
    expect(confirmationFor(spec({ externalDataSharing: unset }), spec({ externalDataSharing: "metadata" }))).toEqual([]);
  });
});

describe("configBody", () => {
  it("drops managedFields from the metadata of the read", () => {
    const base = {
      apiVersion: "varroa.dev/v1alpha1",
      kind: "AssistantConfig",
      metadata: { name: "varroa-assistant", resourceVersion: "9", managedFields: [{ manager: "x" }] } as { name: string; resourceVersion?: string },
      spec: spec(),
      status: { providers: [] },
    };
    const body = configBody(base, spec());
    expect(body.metadata).toEqual({ name: "varroa-assistant", resourceVersion: "9" });
    expect(body).not.toHaveProperty("status");
  });

  it("sends the read version and keeps the other metadata, and leaves an empty default out", () => {
    const base = {
      apiVersion: "varroa.dev/v1alpha1",
      kind: "AssistantConfig",
      metadata: { name: "varroa-assistant", resourceVersion: "9", labels: { owner: "ops" } } as { name: string; resourceVersion?: string },
      spec: spec(),
    };
    const body = configBody(base, spec({ providers: [], defaultProvider: "", defaultModel: "" }));
    expect(body.metadata).toEqual({ name: "varroa-assistant", resourceVersion: "9", labels: { owner: "ops" } });
    expect(body).not.toHaveProperty("status");
    expect("defaultProvider" in body.spec).toBe(false);
    expect("defaultModel" in body.spec).toBe(false);
  });
});

describe("limits", () => {
  it("requires each budget to be a whole number of at least 1", () => {
    const d = limitsDraftOf(spec());
    expect(validateLimits(d)).toEqual({});
    expect(validateLimits({ ...d, session: "0" })).toEqual({});
    expect(validateLimits({ ...d, session: "-1" }).session).toBeTruthy();
    expect(validateLimits({ ...d, steps: "1.5" }).steps).toBeTruthy();
    expect(validateLimits({ ...d, session: "9007199254740993" }).session).toBeTruthy();
    expect(validateLimits({ ...d, session: "9007199254740991" })).toEqual({});
    expect(validateLimits({ ...d, steps: "2147483647" })).toEqual({});
    expect(validateLimits({ ...d, steps: "2147483648" }).steps).toBeTruthy();
    expect(validateLimits({ ...d, tenantDay: "" }).tenantDay).toBeTruthy();
  });

  it("requires the approval expiry to be a number with m or h", () => {
    const d = limitsDraftOf(spec());
    expect(validateLimits({ ...d, approvalExpiry: "1d" }).approvalExpiry).toBeTruthy();
    expect(validateLimits({ ...d, approvalExpiry: "0h" }).approvalExpiry).toBeTruthy();
    expect(validateLimits({ ...d, approvalExpiry: "90m" }).approvalExpiry).toBeUndefined();
  });

  it("writes the draft into the spec as numbers", () => {
    const next = applyLimits(spec(), { ...limitsDraftOf(spec()), session: "5000", approvalExpiry: "2h", externalDataSharing: "logs" });
    expect(next.budgets.session).toBe(5000);
    expect(next.approvalExpiry).toBe("2h");
    expect(next.externalDataSharing).toBe("logs");
  });
});
