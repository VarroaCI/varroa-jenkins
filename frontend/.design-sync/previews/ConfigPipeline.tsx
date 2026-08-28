import { ConfigPipeline } from "varroa-frontend";

const noop = () => {};

const convergedCtrl = {
  appliedBundleHash: "a1b2c3d",
  desiredStateHash: "a1b2c3d",
  lastApplyResult: {
    succeeded: true,
    hash: "a1b2c3d",
    timestamp: "2026-06-17T10:42:12Z",
    sections: [
      { name: "jcasc", ok: true },
      { name: "plugins", ok: true },
      { name: "rbac", ok: true },
    ],
  },
  applyHistory: [
    {
      hash: "a1b2c3d",
      timestamp: "2026-06-17T10:42:12Z",
      succeeded: true,
      sections: [{ name: "jcasc", ok: true }, { name: "plugins", ok: true }, { name: "rbac", ok: true }],
    },
    {
      hash: "f4e5d6c",
      timestamp: "2026-06-17T09:18:03Z",
      succeeded: false,
      sections: [{ name: "jcasc", ok: false }, { name: "plugins", ok: true }],
    },
  ],
  rollout: { blocked: false, paused: false },
  liveDrift: { detected: false, liveConfigHash: "a1b2c3d" },
  miteConnected: true,
  jenkinsVersion: "2.452.3",
  miteVersion: "1.4.0",
  lastSeen: "2026-06-17T10:42:30Z",
  certExpiry: "2026-09-15T00:00:00Z",
};

export function Converged() {
  return (
    <div style={{ maxWidth: 760 }}>
      <ConfigPipeline
        ctrl={convergedCtrl}
        bundleResolvedHash="a1b2c3d"
        runningBuilds={2}
        onReload={noop}
        onSafeRestart={noop}
        onReprovision={noop}
      />
    </div>
  );
}

const pendingCtrl = {
  ...convergedCtrl,
  appliedBundleHash: "f4e5d6c",
  desiredStateHash: "a1b2c3d",
  liveDrift: { detected: true, liveConfigHash: "9a8b7c6" },
};

export function PendingDrift() {
  return (
    <div style={{ maxWidth: 760 }}>
      <ConfigPipeline
        ctrl={pendingCtrl}
        bundleResolvedHash="a1b2c3d"
        runningBuilds={0}
        onReload={noop}
        onSafeRestart={noop}
        onReprovision={noop}
      />
    </div>
  );
}
