import { describe, it, expect } from "vitest";
import { overlayFieldErrorFrom, overlayJenkinsImage } from "./overlay";

describe("overlayJenkinsImage", () => {
  it("returns the image for the jenkins container", () => {
    const yaml = `
spec:
  template:
    spec:
      containers:
        - name: jenkins
          image: myrepo/jenkins:2.600
        - name: sidecar
          image: busybox
`;
    expect(overlayJenkinsImage(yaml)).toBe("myrepo/jenkins:2.600");
  });

  it("returns null when there is no jenkins container", () => {
    const yaml = `
spec:
  template:
    spec:
      containers:
        - name: sidecar
          image: busybox
`;
    expect(overlayJenkinsImage(yaml)).toBeNull();
  });

  it("returns null when the jenkins container has no image", () => {
    const yaml = `
spec:
  template:
    spec:
      containers:
        - name: jenkins
          resources: {}
`;
    expect(overlayJenkinsImage(yaml)).toBeNull();
  });

  it("returns null for unparseable YAML", () => {
    expect(overlayJenkinsImage("::: not: [valid")).toBeNull();
  });

  it("returns null for empty / whitespace input", () => {
    expect(overlayJenkinsImage("")).toBeNull();
    expect(overlayJenkinsImage("   ")).toBeNull();
    expect(overlayJenkinsImage(undefined)).toBeNull();
  });
});

describe("overlayFieldErrorFrom", () => {
  it("maps a resourceOverlay prefix to its editor field and keeps the whole reason", () => {
    expect(
      overlayFieldErrorFrom(
        "resourceOverlay.statefulSet: malformed YAML: error converting YAML to JSON: yaml: line 1: did not find expected key",
      ),
    ).toEqual({
      field: "statefulSet",
      message: "malformed YAML: error converting YAML to JSON: yaml: line 1: did not find expected key",
    });
  });

  it("maps the service and ingress prefixes", () => {
    expect(overlayFieldErrorFrom("resourceOverlay.service: bad")).toEqual({ field: "service", message: "bad" });
    expect(overlayFieldErrorFrom("resourceOverlay.ingress: bad")).toEqual({ field: "ingress", message: "bad" });
  });

  it("maps podOverrides and compile podOverrides to the podOverrides field", () => {
    expect(overlayFieldErrorFrom("podOverrides: invalid patch")).toEqual({
      field: "podOverrides",
      message: "invalid patch",
    });
    expect(overlayFieldErrorFrom("compile podOverrides: bad env")).toEqual({
      field: "podOverrides",
      message: "bad env",
    });
  });

  it("returns null when the prefix names no editor field", () => {
    for (const input of [
      "invalid body: json: cannot unmarshal string into Go struct field previewRequest.podOverrides.env of type []v1.EnvVar",
      "constructor: x",
      "forbidden",
      "",
      "podOverrides: ",
    ]) {
      expect(overlayFieldErrorFrom(input)).toBeNull();
    }
  });
});
