# Build Claims

A claim is the admission check a build goes through before it runs. The Jenkins plugin asks the
mite for permission, the mite asks the gateway, and the gateway asks the build record store's
ledger. The ledger assigns the build number and makes the trigger idempotent, so a retried
webhook or a lost reply never starts a second build for the same event.

Build claims need the [build record store](build-records.md). A controller with the store
disabled runs builds with no claim step.

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

## Timeout budget

Five hops carry a budget each. Every budget shrinks going inward, so the hop closest to the
ledger answers first and every caller above it reads a structured decision instead of a bare
timeout.

| Hop | Budget | Override | Result on expiry |
|---|---|---|---|
| Plugin to mite, connect | 1 s | none | empty decision, code `UNAVAILABLE` |
| Plugin to mite, read | 5 s | `VARROA_CLAIM_TIMEOUT_MS` on the Jenkins container | empty decision, code `UNAVAILABLE` |
| Mite wait for the claim reply | 4.5 s, never over 9 s | `MITE_CLAIM_TIMEOUT_MS` on the mite container | HTTP `504`, empty decision, code `UNAVAILABLE` |
| Mite wait for a run-state ack (restart-copy chunk, or the final chunk of a mesh file) | 4 s by default, derived from `MITE_CLAIM_TIMEOUT_MS` | none | HTTP `503`, reason `ack-timeout`, the plugin retries |
| Gateway request to the recorder | 4 s | `VARROA_LEDGER_TIMEOUT_MS` on the gateway | claim reply with empty decision, code `UNAVAILABLE` |
| Recorder database work for one claim | 3.5 s | none | claim reply with empty decision, code `UNAVAILABLE` |

The mite wait is never more than 9 s, because the loopback listener stops each reply after 10 s. A larger budget logs one warning on the mite, and the mite waits 9 s.

The mite counts each wait from the moment it receives the request. A slow send to the gateway shortens the wait.

The operator stamps the first two overrides together from its own `VARROA_CLAIM_TIMEOUT_MS`
value. Leave it unset and both containers use their built-in defaults, 5000 ms and 4500 ms. Set
it and the operator stamps that value on the Jenkins container and the value minus 500 ms on the
mite container. A value below 5000 ms is rejected. The operator then stamps neither variable and logs a warning.

The run-state ack wait derives from the mite budget. It applies to a restart-copy chunk and to the final chunk of each mesh file. The wait is the budget minus 500 ms. It is never under half the budget and never over 9 s. The plugin uses the same `VARROA_CLAIM_TIMEOUT_MS` read budget for those posts. Set the budget on the operator, not through `podOverrides`. If you override it through `podOverrides`, also set `MITE_CLAIM_TIMEOUT_MS` to 500 ms less.

Keep `VARROA_LEDGER_TIMEOUT_MS` under 9 s.

Raise `VARROA_LEDGER_TIMEOUT_MS` only after raising `VARROA_CLAIM_TIMEOUT_MS` to match. Raising
the gateway budget alone makes the mite answer `504` while the gateway is still waiting, which
turns a slow recorder into a retry storm.

## The retry schedule and the queue delay

A trigger admits its queue item at once, blocked with the cause "awaiting Varroa claim", while a
background claimer asks for the claim.
The item runs only after the claimer receives a `granted` decision.

The claimer retries a failed or timed-out claim on this schedule: 1 s, 2 s, 5 s, 10 s, 30 s,
then every 60 s. The claimer retries a `NOT_READY` veto at most every 10 seconds. It keeps the
full schedule for transport failures. After one hour it cancels the queue item and logs a
`WARNING`. A claim waits for its next attempt off the claimer threads, so a refused claim does not
delay other jobs.

## Reading a claim decision

| Decision | Meaning | Claimer action |
|---|---|---|
| `granted` | the ledger owns this run and assigned it a build number | apply the build number, start the build |
| `duplicate` | the intent key already holds a different `run_id`, or the ledger recorded this run's start | cancel the queue item, log one `INFO` line |
| `vetoed` | a policy refused the claim | see the reason table |
| empty, with a `code` | the hop that answered could not reach the next one | retry on the backoff schedule |
| empty, with no `code` | not a claim answer | cancel the queue item, log a `WARNING` |

A missing `decision` key counts as empty. The mite omits the key from every failure body.

| Reason | Cause | Claimer action |
|---|---|---|
| `INVALID_KEY` | the key or the request does not parse | cancel |
| `FENCED` | the cell holds no member of the tenant | cancel |
| `CROSS_TENANT` | the trigger names a job of another tenant | cancel |
| `NOT_READY` | the placement is missing or behind, or another pod holds the grant | retry at most every 10 s |
| `NOT_HOME` | a standby or a slot that is not the home | forward to the veto's `home_slot`; cancel a replay or a restart |

A cancel logs the `WARNING` `claim refused for job <job>, run <run_id>: <reason>: <message>`. The recorder
logs each veto at `INFO` as `claim refused`, with `reason_code`, `tenant`, `run_id`, `job_path`,
`cell`, and `message`. It counts each veto in `recorder_claims_vetoed_total`, labelled `reason`.

A cancel on `INVALID_KEY`, `FENCED`, `CROSS_TENANT`, or `THROTTLED` ends a Varroa-started run at
once. The recorder marks it `TRIGGER_FAILED` with the veto code before it replies. A cancel on
`NOT_HOME` does the same for a replay or a restart. A Jenkins-native trigger gets no run record,
and `NOT_READY` marks nothing.

A retried request with the same `intent_key` and `run_id` reads `granted` again while the run has
not started. A `duplicate` means the intent key belongs to a different run, for example two
triggers racing on the same commit. It also answers a redelivered repeat for a run that started or
that another pod marked.

Before a granted item leaves the queue, the plugin sends a start mark on the claim route. This costs one
round trip per build. The mark fails closed: with no `granted` answer, the build stays
queued and claims again. A repeat from a new pod reads `NOT_READY` while the old pod can still mark.

The ledger refreshes
the number when the run never started, the repeat comes from a different cell instance, and the
stored number is below that instance's next build number. A trigger lost to a cell roll
therefore restarts under the same run identifier and a number the new cell can apply. A repeat from the same cell instance
keeps its number: that instance's counter passes the stored number only by running the build.
The recorder drops a late `run.started` from the old cell for the old number and counts it in
`recorder_ingest_superseded_start_total`.

Webhook and SCM-poll triggers use the intent key `scm:<job-path>:<revision>`, read from the
revision Jenkins has recorded for the job at the moment the claim runs. A second delivery of the
same revision reuses the same key and reads `granted` again on its own run, or `duplicate` if a
different run already claimed it.

## Build numbers in a burst

The plugin applies one grant of a job at a time. The next grant waits until the last grant's run
starts, or for 60 seconds. A run that starts under another number than its grant logs a
`BuildNumberGate` `WARNING`, and `recorder_ingest_number_mismatch_total` counts it.

The number Jenkins used is the truth. The recorder moves it from an unstarted grant to the started
run, and the other run gets a fresh number on its next claim.

## `NUMBER_CONFLICT`

`NUMBER_CONFLICT` means the job's counter already passed the grant. The plugin keeps the item and
claims again with `number_conflict=<grant>`. The ledger then grants a fresh number, and the old
number stays burned.

## Symptoms of a recorder outage

During a recorder outage, every claim reads empty, `code` `UNAVAILABLE`. With no recorder
subscribed, the gateway answers at once ("no responders"). Otherwise it answers after its
`VARROA_LEDGER_TIMEOUT_MS` budget. The claimer keeps the item and retries for one hour.

Symptoms:

- Jenkins queues fill with items blocked on "awaiting Varroa claim" that never start.
- The gateway logs `claim answered unavailable` at `WARN` for each failed claim, with the
  controller and the error text.
- The mite counters `varroa.mite.claims.failed` and `varroa.mite.claims.vetoed` rise.
- Build events queue in the JetStream stream while NATS is healthy, so `recorder_ingest_backlog_msgs` and `recorder_ingest_backlog_bytes` (see
  [Build records](build-records.md)) rise during the outage and drain once it recovers.

A recorder back within the hour needs no action. After one hour
the claimer cancels the item and logs a `WARNING` that names the last answer. A cancelled trigger needs a new build request.
The backstop round marks its run `TRIGGER_FAILED`, reason `CLAIM_EXPIRED`, 75 minutes after
acceptance. One round marks 500 runs at most and counts each in `recorder_claim_expired_total`. An item that waits that long before its first claim is
marked too, and its real build later replaces the row.

A granted run whose build never started ends the same way with reason `CLAIM_ABANDONED`. The
counter carries the attribute `reason`.
