# Cross-Tenant Pipelines

A cross-tenant call reaches its target only through Varroa. The core `build` step cannot cross
a tenant boundary; it starts a job in the same tenant only. The `varroa` global variable carries
every cross-tenant call, and Varroa checks the target tenant's export grant on each one.

A call inside one tenant needs no export, even when the two jobs sit on different
[slots](../operations/tenant-slots.md). A `build job:` step that waits cannot wait on another
slot's run, so `varroa.build` is the supported waiting call there. Roll the recorder image before
the plugin image: the recorder accepts the forwarded key before the plugin sends it.

## The varroa global variable

`varroa` exposes five entry points. A cross-tenant call resolves the target tenant's own cell,
whatever cluster it runs in.

| Call | Returns | What it does |
|---|---|---|
| `varroa.build(tenant:, job:, params:, wait:, propagate:)` | a run handle | starts a build and, by default, waits for it |
| `varroa.lastSuccessful(tenant:, job:)` | a run handle, or `null` | reads the last successful run |
| `handle.copyArtifact(path:, target:)` | the copied file's path | copies one artifact from a run the caller already holds a handle for |
| `varroa.lock(name:, timeout:) { ... }` | the block's return value | acquires a fleet lock around the block |
| `varroa.summary(text)` | nothing | attaches one line of text to the calling run |

### Start a build on another tenant

```groovy
def run = varroa.build(
    tenant: 'platform',
    job: 'release',
    params: [version: '1.4.0'],
)
echo "Started ${run.tenant}/${run.job}, result: ${run.result}"
```

`wait` defaults to `true`. A waiting call polls the target run for up to 24 hours, and logs a
warning at 10 minutes. Wrap the call in the core `timeout` step for a shorter bound:

```groovy
timeout(time: 15, unit: 'MINUTES') {
    varroa.build(tenant: 'platform', job: 'release')
}
```

`propagate` defaults to `true` and fails the caller's build when the target run does not
succeed. Set `wait: false` to fire and move on: the handle starts in state `pending`, and
`handle.refresh()` reads the current state.

### Read the last successful run

```groovy
def last = varroa.lastSuccessful(tenant: 'platform', job: 'release')
if (last != null) {
    echo "Platform's last release: ${last.buildNumber}, result ${last.result}"
}
```

`lastSuccessful` returns `null` when the job has never succeeded.

### Copy an artifact from another tenant

```groovy
def run = varroa.build(tenant: 'platform', job: 'release')
run.copyArtifact(path: 'dist/app.tar.gz', target: 'artifacts/app.tar.gz')
```

A cross-tenant artifact read needs the S3 blob tier. Set `recordStore.blobs.mode` to an S3
driver, and give the agent egress to the blob endpoint. A Postgres blob tier answers with an error
naming the setting to change.

### Acquire a fleet lock

```groovy
varroa.lock(name: 'deploy-prod', timeout: 300) {
    sh 'deploy.sh'
}
```

The block runs only after the lock is held. The lock releases when the block ends, on success,
on failure, or on abort. See [The fleet lock](#the-fleet-lock) for the full contract.

## The run handle

`varroa.build` and `varroa.lastSuccessful` return a run handle. It carries `runId`, `tenant`,
`job`, `buildNumber`, `state`, `result`, `startedAt`, `endedAt`, `url`, and `artifacts`.

| `state` | Meaning |
|---|---|
| `pending` | Varroa accepted the request; the target build has not started |
| `queued` | the target build is queued |
| `running` | the target build is running |
| `completed` | the target build finished |
| `interrupted` | the target build was marked interrupted |

## Declare an upstream trigger

A job can start automatically after another tenant's job finishes. Declare a `varroaUpstream`
trigger, in a `Jenkinsfile` or in an items YAML entry:

```groovy
triggers {
    varroaUpstream(tenant: 'platform', job: 'release', result: 'SUCCESS')
}
```

`result` is a threshold: `SUCCESS`, `UNSTABLE`, or `FAILURE`. The default is `SUCCESS`, meaning
this result or better. A job declares exactly one `varroaUpstream` trigger. A job that needs
two producers calls `varroa.build` from each producer's pipeline instead.

## The export grant

A tenant exports nothing by default. Grant another tenant access on the producing tenant's
`Team`:

```yaml
apiVersion: varroa.dev/v1alpha1
kind: Team
metadata:
  name: platform
spec:
  namespaces: ["team-platform"]
  pipelines:
    exports:
      - job: "release/*"
        callers: ["payments", "search"]
      - locks: ["deploy-prod", "staging/*"]
        callers: ["payments"]
```

Each entry names exactly one of two kinds, a `job` glob or a `locks` glob, plus `callers`, the Team
names allowed to use it. A `job` glob matches the tenant's own job paths. A `locks` glob matches
its lock names, without the tenant prefix. Three metacharacters apply: `*` matches any characters
except `/`, `**` crosses `/`, and `?` matches one character.

An edit to `exports` takes one reconcile tick to reach the record store that enforces it. A caller
may see the old grant for a few seconds after a save.

An ungranted call fails with a message that starts "cross-tenant triggers go through Varroa."

## The dependency edge

A job's page shows its declared upstream and downstream cross-tenant relationships on the
**Triggers** card, as an **Upstream** row and a **Downstream** row. Each row names the other
tenant, the other job, and the result threshold, and marks a row **grant missing** when the
producer has not exported the job. Such an edge still shows; it never disappears.

The same data is available over the API:

```json
"dependencies": {
  "upstream":   [{"tenant": "platform", "job": "release", "result": "SUCCESS", "granted": true}],
  "downstream": [{"tenant": "search", "job": "rebuild", "result": "SUCCESS", "granted": false}]
}
```

## The fleet lock

`varroa.lock` names a lock in the form `<tenant>/<name>`. A bare name, with no `/`, qualifies to
the caller's own tenant. A caller reaches another tenant's lock namespace only by naming it with
that tenant's prefix, and only when the owning tenant exports it under `locks`.

A lock moves through three states.

| State | Meaning |
|---|---|
| `held` | one run holds the lock |
| `waiting` | one or more runs wait for the current holder |
| `releasing` | the holder's run was marked interrupted; the lock is not yet free |

A lock leaves `releasing` on one of two events: the holder's run reaches a terminal state, or an
operator confirms the holder's cell instance is gone. A `releasing` lock still blocks every
other caller. Neither event fires when a Jenkins process restarts inside its pod without a clean
shutdown. Delete the cell pod then: the operator confirms
that instance gone, and the lock releases.

### Fencing tokens

Every time a lock is acquired, Varroa mints a fencing token, an increasing number, fleet-wide. A
held lock exposes its token two ways inside the block: `varroa.lock.token`, and the environment
variable `VARROA_LOCK_TOKEN`.

Use the token to protect a resource the lock guards but Varroa does not touch directly, such as
a deployment target:

```groovy
varroa.lock(name: 'deploy-prod', timeout: 300) {
    sh 'deploy.sh --fence-token "$VARROA_LOCK_TOKEN"'
}
```

`deploy.sh` stores the highest token it has accepted for the target it protects, and refuses a
lower token. A holder can lose its lock without knowing it, after a slow network partition. Its
own token stays on any write it still sends, and the current holder's token is higher, so that late
write is refused.

## Timeout budgets

Four hops each carry their own timeout, from the plugin down to the record store. Each hop's
budget is shorter than the one outside it.

| Hop | Variable | Default |
|---|---|---|
| the plugin's connect | `VARROA_CROSSTENANT_CONNECT_TIMEOUT_MS` | 1000 ms |
| the plugin's read | `VARROA_CROSSTENANT_READ_TIMEOUT_MS` | 10000 ms |
| the mite's wait for a response | `VARROA_CROSSTENANT_MITE_TIMEOUT_MS` | 9500 ms |
| the gateway's wait for the record store | `VARROA_CROSSTENANT_GATEWAY_TIMEOUT_MS` | 9000 ms |

Raise the outer budget before an inner one. Varroa logs a warning, and keeps running, when a
budget is not shorter than the budget inside it.

## Placement requirement

Every cross-tenant call needs a cell role: the calling controller must carry a placement record
naming its tenant. A controller with no placement record is refused before Varroa checks any
grant.
