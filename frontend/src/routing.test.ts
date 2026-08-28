import { describe, expect, it } from "vitest";
import { clusterQuery, configurationLink, controllerRoute, withCluster } from "./routing";

describe("routing helpers", () => {
  it("always includes and encodes the selected cluster", () => {
    expect(clusterQuery("core")).toBe("?cluster=core");
    expect(clusterQuery("edge/a")).toBe("?cluster=edge%2Fa");
  });

  it("preserves existing query parameters", () => {
    expect(withCluster("/catalog?q=agents", "edge/a")).toBe("/catalog?q=agents&cluster=edge%2Fa");
    expect(configurationLink("/catalog/bundles", "core")).toBe("/catalog/bundles?cluster=core");
  });

  it("encodes every controller identity segment once", () => {
    expect(controllerRoute("edge/a", "team one", "controller#1")).toBe(
      "/controllers/edge%2Fa/team%20one/controller%231",
    );
  });
});
