import { KVGrid } from "varroa-frontend";

export function ControllerDetails() {
  return (
    <div style={{ maxWidth: 460 }}>
      <KVGrid
        items={[
          { key: "Namespace", value: "jenkins" },
          { key: "Image", value: "jenkins/jenkins:2.452.3-lts" },
          { key: "Replicas", value: "1 / 1 ready" },
          { key: "Phase", value: "Connected" },
          { key: "Group", value: "platform" },
          { key: "Bundle", value: "platform-base@a1b2c3d" },
        ]}
      />
    </div>
  );
}
