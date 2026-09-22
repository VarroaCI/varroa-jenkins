# Operate a Run

Hold a fleet lock across tenants, read why a build failed, attach a summary line to a run, and
call a job on another tenant.

## Prerequisite

These pages need the [build record store](../operations/build-records.md), the same way the
[Read and run pipelines](pipelines.md) tutorial does. Set `recordStore.enabled: true` in your
Helm values before you start.

## Fleet locks

A job that must not run twice at the same time takes a fleet lock. The `varroa.lock` step holds
the lock, and a second run waits for it. See
[Cross-tenant pipelines](../config/cross-tenant-pipelines.md) for the lock name form, the three
lock states, and the timeout budget. This page does not repeat them.

The **Locks** card on the team home lists the locks your tenant holds and the locks its runs wait
on. A row names the lock, the state, the holder, and the time since the wait or the acquisition
started. The card reads the locks again on a `lock` event from the run stream, at most once every
two seconds. It shows no queue position.

An operator opens `/locks` to list every lock of the cluster, across every tenant. That page needs
the `fleetlocks:read` verb, which `varroa:operator` and `varroa:admin` hold. The page polls and
opens no stream.

Both views are read-only, so neither page releases a lock. A stuck lock frees itself in three
ways:

- The holder's step releases the lock when its block ends.
- The holder's run reaches a terminal state.
- The Varroa operator confirms that the holder's cell instance is gone.

## Read a failure digest

A run that fails leads with a **Digest** card, above the **Pipeline** card. The card appears for a
run that carries a result, that did not succeed, and that started.

These runs carry a card: `UNSTABLE`, `FAILURE`, `ABORTED`, `NOT_BUILT`, and a started
`INTERRUPTED`. These runs carry no card: a run with no result yet, a successful run, a refused
trigger, and an interrupted run that never started.

The card shows up to four parts. Each part appears on its own.

| Part | Meaning |
|---|---|
| Stage | The name of the first stage that failed, in stage-start order |
| Error | That stage's own stored error text |
| Log tail | The last 50 log lines of that stage |
| Failed tests | The failed, skipped, and total counts of the run's tests |

The card uses the whole log for the log tail when no stage reports `FAILURE`. It uses the whole
log too when the stored log index names no stage. An aborted run and a run with no stages take this
fallback.

The card carries no log tail when the server cannot read the log. The **Log** tab then shows what
the store holds.

A record that holds none of the four parts still shows the card, with no part in it.

## Attach a summary line

A pipeline attaches one line of text to its own run. Call `varroa.summary` with the text:

```groovy
varroa.summary('deployed canary to 10%, holding for 1h')
```

The build page shows the line in its header. The line shows the latest call's text, so a second
call replaces the first. A run that never calls `varroa.summary` shows no line. An empty text
attaches nothing: the run keeps the last text that was not empty.

The call cuts the text at 512 bytes of UTF-8. The cut stops at the largest whole character at or
under that limit, so the stored text stays valid UTF-8. Text under the limit stays unchanged.

The call touches the run's own object and needs no network round trip. Call it in an early stage
to show the line while the run continues.

## Call a job on another tenant

Two pipelines tenants, a producer and a caller, connect through a grant and a trigger.

On the producer tenant's `Team`, export the job to the caller:

```yaml
spec:
  pipelines:
    exports:
      - job: "release"
        callers: ["payments"]
```

Apply the `Team` and wait about one reconcile tick for the grant to reach the record store.

On the caller tenant's job, declare a `varroaUpstream` trigger naming the producer:

```groovy
triggers {
    varroaUpstream(tenant: 'platform', job: 'release', result: 'SUCCESS')
}
```

Run the producer's job to success. The trigger starts the caller's job.

Open either job's page and check the **Triggers** card. The caller's job shows an **Upstream**
row naming the producer job; the producer's job shows a **Downstream** row naming the caller.
Neither row carries **grant missing**, because the export covers the call.

A pipeline can also call another tenant's job directly, without a declared trigger, using
`varroa.build`. A direct call runs once, on demand, and does not add a row to either job's
**Triggers** card. See [Cross-tenant pipelines](../config/cross-tenant-pipelines.md) for the
full grant syntax, the direct call, the fleet lock, and the timeout budgets.
