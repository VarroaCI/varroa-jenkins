import { describe, it, expect } from "vitest";
import { overlayJenkinsImage } from "./overlay";

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
