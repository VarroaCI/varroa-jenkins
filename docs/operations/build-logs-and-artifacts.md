# Build Logs and Artifacts

The build record store can hold a Pipeline build's console log and archived artifacts outside
the controller. This needs the [build record store](build-records.md). Without the store, a
controller keeps both on local disk.

Log storage works under both blob tiers. Artifact storage needs the S3 tier and one more
plugin, `artifact-manager-s3`, pinned in the controller's `JenkinsVersionProfile`.

## The log path

A Pipeline step writes a line of console output. The chain from there to durable storage:

1. The Jenkins plugin buffers the line, assigns it a sequence number, and posts a chunk to the
   mite's loopback listener at `http://127.0.0.1:9095/v1/log`. The operator sets this URL on the
   Jenkins container only when the record store is enabled.
2. The mite retains the chunk in memory and forwards it to the gateway over the mite stream. It
   keeps the chunk until the gateway acknowledges it.
3. The gateway stamps the chunk with the cluster, namespace, and controller name it read from
   the mTLS connection. It checks that the run belongs to this controller, then appends the
   chunk to the blob store and to a per-chunk index.
4. The gateway acknowledges the chunk only after both writes land.

A build that outruns this chain loses no stored data. The mite's retained buffer applies
back pressure: at its bound, the mite answers `503` and the plugin retries with backoff. When the
gateway is busy, the mite re-sends the chunk later, and its `503` names the wait. Only the plugin's own per-build cap discards incoming
bytes, and it counts every byte it drops.

If the mite process restarts, its retained buffer is lost. The gateway detects the resulting
sequence gap and marks the run's log incomplete. See [Incomplete logs](#incomplete-logs) below.

A resumed build continues its sequence. After a Jenkins restart, it reads the plugin's log cache.
After a move to another cell, it reads the stored log.

## Live tail

A viewer of a running build gets a live tail, not a poll:

1. The dashboard asks the gateway to start a tail for the run. The gateway records a 90-second
   lease and pushes a tail-start control message to the mite.
2. The mite calls the Jenkins plugin's tail endpoint, which switches that build to a short flush
   interval.
3. While the lease is live, the gateway also publishes each stored chunk on a live subject. The
   BFF reads a missed live chunk from the blob store.
4. The dashboard renews the lease every 30 seconds while a viewer stays attached. After the last
   viewer detaches, the lease expires within 90 seconds. The mite tells the plugin to stop, and
   the build reverts to its normal flush interval.

## Postgres and S3 tier behaviour

`recordStore.blobs.mode` selects the tier for both logs and artifacts:

| Mode | Log chunks | Artifacts |
|---|---|---|
| `postgres` | Rows in `varroa.log_chunks`, no external egress. | Not supported, because `artifact-manager-s3` needs S3. The operator reports `ArtifactStorage` `False` with reason `LocalOnly`, and the controller keeps Jenkins' own local artifact manager. |
| `s3` | Objects in the configured bucket. | Objects in the same bucket, under a per-cell prefix. |

## Retention

Log and artifact retention follow the same `recordStore.retention.logs` and
`recordStore.retention.artifacts` dials as every other record-store content. See
[Retention](build-records.md#retention) in Build Records for the day-count format and the
month-boundary rounding rule.

## The per-cell artifact prefix

Every object a cell writes, log or artifact, carries a prefix built from three identity tokens.
The gateway or the operator reads them from the cell's own identity: the cluster name, the
controller's namespace, and the controller name.

Log chunks and their index live at fixed keys, `logs/<run id>` and `logindex/<run id>`, and need
no prefix of their own: the run id already makes them unique.

Artifacts live under:

```
artifacts/<cluster>/<namespace>/<cell>/<job full name>/<build number>/artifacts/<path>
```

A stash lands under the same prefix, at `<build number>/stashes/<name>.tgz`. Two controllers, or
the same controller name on two clusters, therefore never write the same key.

## Artifact credentials

See [Artifact credentials](artifact-credentials.md).

## Incomplete logs

A log's manifest can report `incomplete` true with a reason token:

| Token | Meaning |
|---|---|
| `plugin-drop` | The plugin discarded bytes at its own cap. |
| `terminal-response` | A chunk post ended on a permanent HTTP error. |
| `jenkins-restart` | A Jenkins restart ended the build; the plugin rebuilt its completion. |
| `rejected` | The gateway refused a chunk on the run-ownership check. |
| `gateway-gap` | The gateway observed a sequence it can never fill, or a moved build overlapped a lost cell's parts. |
| `finalize-deadline` | The finalization deadline passed with chunks unstored. |
| `not-finalized` | The recorder's sweep finalized a run that produced no final chunk. |
| `no-summary` | The run record carries no log summary. The read path serves every stored chunk. |

The record keeps the most precise reason: a plugin token, then `rejected` or `gateway-gap`, then
the last three tokens. Inside one rank, the first reason stays. A complete log clears only those
three.

## Agent-namespace egress

An agent uploads artifacts straight to the object store, not through the controller pod. An
agent namespace therefore needs its own egress rule to the blob endpoint. `networkPolicy`
templates in this chart cover only Varroa's own components in the release namespace; the agent
namespace's egress is configured separately. See
[Network policies](../install/network-policies.md).

## Known limits

- **Pipeline builds only.** The plugin's log-storage extension point applies to Pipeline builds.
  Freestyle and matrix builds keep Jenkins' own local console log.
- **A tenant move changes the prefix.** The artifact plugin holds one prefix per Jenkins
  instance, so a per-tenant prefix inside one cell is not possible. After a tenant moves to
  another cell, Jenkins' own artifact manager resolves only the runs the new cell wrote. The
  Varroa read path still resolves every earlier run, because it opens the recorded object key
  directly rather than recomputing one from the current prefix.
- **No cross-cell log read-back inside Jenkins.** Jenkins' own log viewer reads from a cell-local
  cache. A replaced pod loses that cache, and a build's log page inside Jenkins itself does not
  reach across cells. The Varroa read path always serves the full log from the blob store,
  regardless of which cell is live now.

## Troubleshooting

- Console output stops updating for a running build, and the dashboard tail shows no new lines:
  confirm a viewer is still attached and the tail lease is renewing. A lease with no renewal
  clears itself within 90 seconds and the build falls back to its normal flush interval, which is
  expected once every viewer has left.
- A build's log reads `incomplete` after it finished: find the manifest's `incompleteReason` in
  the [Incomplete logs](#incomplete-logs) table. `no-summary` usually means
  the cell was lost before it could report a finish. `gateway-gap` or `rejected` point at the
  gateway's own rejection counters.
- Artifacts do not appear in object storage, and the controller shows `ArtifactStorage`
  `LocalOnly`: read the condition message. It names a disabled S3 blob tier, a missing
  credential, a failed read, or a plugin set that does not pin `artifact-manager-s3`. See [Jenkins upgrades](jenkins-upgrades.md) and
  [Version profiles](../config/jenkins-versions.md) for pinning a plugin into a profile.
- The controller shows `ArtifactStorage` `PluginMissing` even though the bundle lists
  `artifact-manager-s3`: the condition reads the resolved `JenkinsVersionProfile`'s plugin set,
  not the bundle content. Pin the plugin in the profile that matches the controller's Jenkins
  version.
- An archived artifact cannot be resolved by `copyArtifacts` after a tenant moved to a new cell:
  expected. See [A tenant move changes the prefix](#known-limits) above. Use the Varroa API to
  read the artifact by its recorded key instead of a Jenkins-side copy.
- A build cannot bind the artifact credential id in its own pipeline, or a pipeline step that
  tries to read the `logs/` prefix or a prefix outside its own match fails: expected. No
  build receives the credential, and its policy limits it to its own prefix.
