# Build Claims

A claim is the admission check a build goes through before it runs. The Jenkins plugin asks the
mite for permission, the mite asks the gateway, and the gateway asks the build record store's
ledger. The ledger assigns the build number and makes the trigger idempotent, so a retried
webhook or a lost reply never starts a second build for the same event.

Build claims need the [build record store](build-records.md). A controller with the store
disabled runs builds the same way it always has, with no claim step.

## Turn on the gate

`recordStore.enabled` controls the gate for the whole install:

```yaml
recordStore:
  enabled: true
```

The operator reads its own `VARROA_CLAIM_GATE` environment variable, set from that value, and
stamps the same variable onto the Jenkins container of every controller it provisions: `1` when
the store is enabled, `0` when it is not. The Jenkins plugin emits build events and asks for
claims only when the variable is `1`. A controller already running rolls once when the variable
changes, because it is stamped on the pod template.

The mite's loopback listener runs on every controller, gated or not. It answers no request when
the plugin never calls it.

## Timeout budget

Four hops carry a budget each. Every budget shrinks going inward, so the hop closest to the
ledger answers first and every caller above it reads a structured decision instead of a bare
timeout.

| Hop | Budget | Override | Result on expiry |
|---|---|---|---|
| Plugin to mite, connect | 1 s | none | empty decision, code `UNAVAILABLE` |
| Plugin to mite, read | 5 s | `VARROA_CLAIM_TIMEOUT_MS` on the Jenkins container | empty decision, code `UNAVAILABLE` |
| Mite wait for the claim reply | 4.5 s | `MITE_CLAIM_TIMEOUT_MS` on the mite container | HTTP `504`, empty decision, code `UNAVAILABLE` |
| Gateway request to the recorder | 4 s | `VARROA_LEDGER_TIMEOUT_MS` on the gateway | claim reply with empty decision, code `UNAVAILABLE` |

The operator stamps the first two overrides together from its own `VARROA_CLAIM_TIMEOUT_MS`
value. Leave it unset and both containers use their built-in defaults, 5000 ms and 4500 ms. Set
it and the operator stamps that value on the Jenkins container and the value minus 500 ms on the
mite container. A value below 5000 ms is rejected. The operator stamps neither variable and logs
a warning instead. A smaller value would push the mite budget to or below the gateway's 4 s
default, breaking the shrink-inward rule.

Raise `VARROA_LEDGER_TIMEOUT_MS` only after raising `VARROA_CLAIM_TIMEOUT_MS` to match. Raising
the gateway budget alone makes the mite answer `504` while the gateway is still waiting, which
turns a slow recorder into a retry storm.

## The retry schedule and the queue delay

A trigger admits its queue item at once. The item shows up in the Jenkins queue immediately,
blocked with the cause "awaiting Varroa claim", while a background claimer asks for the claim.
The item runs only after the claimer receives a `granted` decision.

The claimer retries a failed or timed-out claim on this schedule: 1 s, 2 s, 5 s, 10 s, 30 s,
then every 60 s. It keeps retrying for one hour on one pending claim, then cancels the queue item
and logs a `WARNING`. Every attempt carries the same `run_id`, so a lost reply costs one retry
and never starts a second build.

## Reading a claim decision

| Decision | Meaning | Claimer action |
|---|---|---|
| `granted` | the ledger owns this run and assigned it a build number | apply the build number, start the build |
| `duplicate` | the intent key already holds a different `run_id`, or the ledger recorded this run's start | cancel the queue item, log one `INFO` line |
| `vetoed`, reason `NOT_READY` | the ledger is not ready to grant yet | retry on the backoff schedule |
| `vetoed`, any other reason | the ledger refuses this run permanently | cancel the queue item, log a `WARNING` |
| empty, with a `code` | the hop that answered could not reach the next one | retry on the backoff schedule |
| empty, with no `code` | not a claim answer | cancel the queue item, log a `WARNING` |

A missing `decision` key counts as empty. The mite omits the key from every failure body.

`duplicate` and `granted` are not the same outcome for a retried request. A retried request
reads `granted` again when it carries the same `intent_key` and the same `run_id` as an earlier
granted claim, and that run has not started. The ledger recognizes its own retried request.
`duplicate` means the intent key belongs to a different run entirely, for example two triggers
racing on the same commit. It also answers a repeat for a run whose start the ledger recorded.
Only a queued item claims, so that repeat comes from a redelivered command. A redelivery that
reaches another cell before the ledger records the start still reads `granted`.

A retried request from a new cell instance can carry a fresh build number. The ledger refreshes
the number when the run never started, the repeat comes from a different cell instance, and the
stored number is below that instance's next build number. A trigger lost to a cell roll
therefore restarts under the same run identifier. The new cell reads its next build number from
the ledger's watermark, and the repeat claim reports that number. The ledger then grants a
number the cell can apply. The old number stays burned. A repeat from the same cell instance
keeps its number: that instance's counter passes the stored number only by running the build.
The recorder drops a late `run.started` from the old cell for the old number and counts it in
`recorder_ingest_superseded_start_total`.

Webhook and SCM-poll triggers use the intent key `scm:<job-path>:<revision>`, read from the
revision Jenkins has recorded for the job at the moment the claim runs. A second delivery of the
same revision reuses the same key and reads `granted` again on its own run, or `duplicate` if a
different run already claimed it.

## `NUMBER_CONFLICT`

The ledger grants a build number that is not yet used locally. The claimer reads the number back
from Jenkins right after applying it. `NUMBER_CONFLICT` means that read-back found Jenkins
already past the granted number. The counter advanced between the claim request and the number
apply. A manual `Job.scheduleBuild2` call that bypassed the plugin in that window is one cause. A
restore from a backup that replayed old build history in that window is another. An advance
before a first claim causes no conflict: the claim reports the counter as
`jenkins_next_build_number`, and the ledger never grants a new number below it. Two repeat
claims are the exceptions. One from the same cell instance answers the stored number while the
ledger has not yet recorded the build's start. A second queue item for a run that instance
already built then reads `NUMBER_CONFLICT` and is cancelled, which keeps that run from building
twice. Once the ledger records the start, the repeat answers `duplicate` instead. One that names
no cell instance answers the stored number too, because the ledger cannot tell which instance
owns the run; a real cell always names its instance. Treat any other `NUMBER_CONFLICT` as a sign the Jenkins instance
and the ledger have diverged for that job. The retry schedule will not clear it.

## Symptoms of a recorder outage

During a recorder outage, every claim reads empty, `code` `UNAVAILABLE`. With no recorder
subscribed, the gateway answers at once ("no responders"). Otherwise it answers after its
`VARROA_LEDGER_TIMEOUT_MS` budget. The claimer keeps the item and retries for one hour.

Symptoms to check for:

- Jenkins queues fill with items blocked on "awaiting Varroa claim" that never start.
- The gateway logs `claim answered unavailable` at `WARN` for each failed claim, with the
  controller and the error text.
- The mite counters `varroa.mite.claims.failed` and `varroa.mite.claims.vetoed` rise.
- Build events keep publishing normally as long as NATS itself is healthy. They queue in the
  JetStream stream, so `recorder_ingest_backlog_msgs` and `recorder_ingest_backlog_bytes` (see
  [Build records](build-records.md)) rise during the outage and drain once it recovers.

A recorder back within the hour needs no action: the next retry reads `granted`. After one hour
the claimer cancels the item and logs a `WARNING`. A cancelled trigger needs a new build request.
The backstop round marks its run `TRIGGER_FAILED`, reason `CLAIM_EXPIRED`, 75 minutes after
acceptance. One round marks 500 runs at most and counts each in `recorder_claim_expired_total`. An item that waits that long before its first claim is
marked too, and its real build later replaces the row.
