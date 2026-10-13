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
on the core or on a member cluster.

| Call | Returns | What it does |
|---|---|---|
| `varroa.build(tenant:, job:, params:, wait:, propagate:)` | a run handle | starts a build and, by default, waits for it |
| `varroa.lastSuccessful(tenant:, job:)` | a run handle, or `null` | reads the last successful run |
| `handle.copyArtifact(path:, target:)` | the copied file's path | copies one artifact from a run the caller already holds a handle for |
| `varroa.lock(name:, tenant:, timeout:) { ... }` | the block's return value | acquires a fleet lock around the block |
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
succeed. A run the record store refuses ends `TRIGGER_FAILED`, and the failure names the
refusal code, such as `(FENCED: <text>)`. Set `wait: false` to fire and move on: the handle
starts in state `pending`. `handle.refresh()` reads the current state into a new handle and
does not change the old one. Assign the result to read it:

```groovy
def run = varroa.build(tenant: 'platform', job: 'release', wait: false)
run = run.refresh()
echo "Platform release state: ${run.state}"
```

### Read the last successful run

```groovy
def last = varroa.lastSuccessful(tenant: 'platform', job: 'release')
if (last != null) {
    echo "Platform's last release: ${last.buildNumber}, result ${last.result}"
}
```

`lastSuccessful` returns `null` when the job has never succeeded. A refused grant fails the step.

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
on failure, or on abort. Add `tenant:` to take a lock that another tenant owns:

```groovy
varroa.lock(name: 'deploy-prod', tenant: 'platform') {
    sh 'deploy.sh'
}
```

See [The fleet lock](#the-fleet-lock) for the full contract.

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
trigger in the job's items YAML entry:

```yaml
- kind: pipeline
  name: integration
  triggers:
    - varroaUpstream:
        tenant: platform
        job: release
        result: SUCCESS
  definition:
    script: echo integration
```

A `Jenkinsfile` `triggers {}` block also works, but only the items form
[survives a cell restart](items.md#schedule-a-job).

`result` is a threshold: `SUCCESS`, `UNSTABLE`, or `FAILURE`. The default is `SUCCESS`, meaning
this result or better. A job declares one `varroaUpstream` trigger. A job that needs
two producers calls `varroa.build` from each producer's pipeline instead.

The trigger reaches the subscriber job's home slot.

A new trigger goes live only after the subscriber's cell exports its schedules, within ten
minutes. A producer run that completes before that export starts no subscriber build.

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
    profile: jenkins-lts
    agentNamespace: team-platform
    exports:
      - job: "release/*"
        callers: ["payments", "search"]
      - locks: ["deploy-prod", "staging-*"]
        callers: ["payments"]
```

Each entry names exactly one of two kinds, a `job` glob or a `locks` glob, plus `callers`, the Team
names allowed to use it. `callers` is required and holds at least one Team name. A `job` glob matches the tenant's own job paths. A `locks` glob matches
the bare names of the tenant's own locks. Three metacharacters apply: `*` matches any characters
except `/`, `**` crosses `/`, and `?` matches one character.

An `exports` edit reaches the enforcing record store within one reconcile tick.

An ungranted call fails with a message that starts "cross-tenant triggers go through Varroa."

## The dependency edge

A job's page shows its declared upstream and downstream cross-tenant relationships on the
**Triggers** card, as an **Upstream** row and a **Downstream** row. Each row names the other
tenant, the other job, and the result threshold, and marks a row **grant missing** when the
producer has not exported the job.

The API carries the same data:

```json
"dependencies": {
  "upstream":   [{"tenant": "platform", "job": "release", "jobPath": "platform/release", "result": "SUCCESS", "granted": true, "lastRun": {"tenant": "platform", "jobPath": "platform/release", "runId": "0196f6b2-0000-7000-8000-000000000001", "buildNumber": 12}}],
  "downstream": [{"tenant": "search", "job": "rebuild", "jobPath": "search/rebuild", "result": "SUCCESS", "granted": false}]
}
```

`job` is the name in the other tenant's folder. `jobPath` is the full job path, and each row
links to it. `lastRun` is the newest run on the edge within 30 days. It is absent when no such
run exists.

## Run links

A run that another run started names that run as its **Triggered by** link. The build page
shows the link in the **Run links** card. A build that the run started appears under
**Downstream**, newest first. The first 20 downstream runs come with the run. Read the rest
with `GET /runs/{run}/downstream`, which takes `limit` and `cursor`.

The recorder links a run only to an upstream run that the recorder itself triggered, or to a run of the same tenant. A build that names another upstream run gets no **Triggered by** link.

A link to a run that the reader may not read keeps the tenant name alone. It has no job path,
run id, or status. `get_run` carries the same `upstream` and `downstream` fields.

## The fleet lock

Each tenant owns a lock namespace. `varroa.lock` names a lock with two arguments:

| Argument | Meaning |
|---|---|
| `name` | the bare lock name, with no `/` |
| `tenant` | the tenant that owns the lock. Omit it for the caller's own tenant. |

A caller takes another tenant's lock only when that tenant
exports the name under `locks`.

The lock list shows each lock as the owner tenant, a `/`, and the bare name. A controller tenant
is `<namespace>/<controller>`, so its lock `deploy-prod` shows as `team-pay/ci/deploy-prod`.

Inside the block, `varroa.lock.name` reads the bare name, and `varroa.lock.tenant` reads the owner
tenant. Both read `null` outside a block.

The step handles each recorder refusal as follows.

| Reason | Step behavior |
|---|---|
| `FENCED`, `CROSS_TENANT`, `NOT_HOME`, `INVALID_NAME`, `RUN_ENDED` | fails the build with the recorder's message, on an acquire or a renew. A renew failure also stops the block. `RUN_ENDED` answers an acquire for a run that already ended. |
| `NOT_READY`, `THROTTLED` | keeps polling, because the refusal is temporary |

A lock moves through three states.

| State | Meaning |
|---|---|
| `held` | one run holds the lock |
| `waiting` | runs wait for the holder; a row leaves when its run ends |
| `releasing` | the holder's run was marked interrupted (the move takes at most one minute); the lock is not yet free |

A lock leaves `releasing` when the holder's run reaches a terminal state, when the operator
confirms the holder's cell instance gone, or on a
[force release](../tutorials/run-operations.md#fleet-locks). After a move, the old pod is confirmed once gone. A
`releasing` lock blocks every other caller. A Jenkins restart inside its pod fires neither event.
Restart that cell to release the lock.

### Fencing tokens

Each acquire mints a fencing token, a number that increases fleet-wide. The block reads it as
`varroa.lock.token` or `$VARROA_LOCK_TOKEN`.

Use the token to protect a resource the lock guards, such as a deployment target:

```groovy
varroa.lock(name: 'deploy-prod', timeout: 300) {
    sh 'deploy.sh --fence-token "$VARROA_LOCK_TOKEN"'
}
```

`deploy.sh` stores the highest token it accepted and refuses a lower one. A holder can lose its
lock without knowing it, after a network partition. Its late write carries the old, lower token,
so the target refuses it.

After a Jenkins restart, the block waits up to 5 minutes for the recorder to confirm the lock. A
lost or unconfirmed lock stops it. An agent process the block started runs until its step stops.

## Step failures

A failed call fails its step with `<step>: <code>: <text>`.

| Code | Cause |
|---|---|
| `UNAVAILABLE` | a hop did not answer |
| `NOT_FOUND` | unknown run or artifact path |
| `INTERNAL` | any other recorder fault |
| a veto reason, for example `CROSS_TENANT` | a refusal; the text is the veto message |

## Timeout budgets

Each of four hops carries its own timeout. Each hop's budget is shorter than the one outside it.

| Hop | Variable | Default |
|---|---|---|
| the plugin's connect | `VARROA_CROSSTENANT_CONNECT_TIMEOUT_MS` | 1000 ms |
| the plugin's read | `VARROA_CROSSTENANT_READ_TIMEOUT_MS` | 10000 ms |
| the mite's wait for a response | `VARROA_CROSSTENANT_MITE_TIMEOUT_MS` | 9500 ms |
| the gateway's wait for the record store | `VARROA_CROSSTENANT_GATEWAY_TIMEOUT_MS` | 9000 ms |

Raise the outer budget before an inner one. Varroa logs a warning when a budget is not shorter
than the budget inside it.

## Placement requirement

Every cross-tenant call needs a cell role: the calling controller must carry a placement record
naming its tenant. A controller with no placement record is refused before Varroa checks any
grant.
