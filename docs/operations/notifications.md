# Notifications

A pipelines tenant can declare a completion webhook. When a run of the
tenant completes, the build record store sends a signed JSON payload to a
URL that you choose. A tool outside Jenkins receives the same completion
signal that Varroa records.

The record store sends the webhook. The cell does not. A cell never holds
the signing key.

## Declare the webhook

Set `spec.pipelines.notifications.webhook` on the Team:

```yaml
apiVersion: varroa.dev/v1alpha1
kind: Team
metadata:
  name: payments
spec:
  namespaces: [payments]
  pipelines:
    profile: lts-2.479
    agentNamespace: payments
    notifications:
      webhook:
        url: https://hooks.example.com/varroa
        secretRef:
          name: payments-webhook
          key: hmac-key
```

| Field | Meaning |
|---|---|
| `url` | The receiver endpoint. It starts with `https://` or `http://`. Use `https://` for a receiver outside the cluster. |
| `secretRef.name` | The name of a Secret in the tenant's agent namespace. |
| `secretRef.key` | The data key that holds the signing key. The default is `hmac-key`. |

Create the Secret in the tenant's agent namespace. The value is the
signing key. Use a long random value.

```bash
kubectl -n payments create secret generic payments-webhook \
  --from-literal=hmac-key="$(openssl rand -hex 32)"
```

The Team status reports the webhook in its `Notifications` condition:

- `True` with reason `WebhookReady` means the operator read the key.
- `False` with reason `WebhookSecretMissing` means the Secret or the key
  is missing, or the key is empty. No delivery starts while the condition
  is `False`.

A tenant administrator cannot change `notifications`. The operator
administrator sets it.

## Which runs send a webhook

The record store sends one webhook for each completion of a run that has a
result. A redelivered completion sends nothing new.

The backstop can also end a run with the synthetic result `INTERRUPTED`.
That end sends one delivery with `synthetic: true`. A later real completion
sends its own delivery, which names the synthetic one in `supersedes`. A
real delivery always wins for a `run_id`.

## Payload

The body is a JSON object:

```json
{
  "event": "run.completed",
  "delivery_id": "0190f6c2-6d1e-7a3b-9c2e-5f0a4b8d1e27",
  "cluster": "core",
  "tenant": "payments",
  "job": "release/deploy",
  "run_id": "0190f6c2-6a10-7d44-8e0b-2c9f1a7b3d55",
  "number": 42,
  "result": "SUCCESS",
  "started_at": "2026-10-11T09:14:02Z",
  "completed_at": "2026-10-11T09:16:40Z",
  "duration_ms": 158000,
  "scm_revision": "9f2c1ab",
  "url": "https://varroa.example.com/pipelines/payments/release%2Fdeploy/0190f6c2-6a10-7d44-8e0b-2c9f1a7b3d55"
}
```

| Field | Always present | Meaning |
|---|---|---|
| `event` | yes | The event name. It is `run.completed`. |
| `delivery_id` | yes | A unique identifier for this delivery. A retry sends the same value. |
| `cluster`, `tenant` | yes | The cluster and the tenant of the run. |
| `job` | yes | The job path of the run. |
| `run_id` | yes | The run identifier. |
| `number` | no | The build number. Absent when the run has none. |
| `result` | yes | The Jenkins result, for example `SUCCESS`, `UNSTABLE`, `FAILURE`, or `ABORTED`. |
| `started_at` | no | The start time. Absent when the record has none. |
| `completed_at` | yes | The completion time. |
| `duration_ms` | yes | The run duration in milliseconds. Zero when unknown. |
| `scm_revision` | no | The source revision, when the run reported one. |
| `rejected_signature` | no | The script signature that the sandbox refused. Present only when `result` is `FAILURE` and a refused signature failed the run. |
| `url` | no | A link to the run page in the dashboard. Absent when the dashboard URL is not set. |
| `synthetic` | no | `true` for a synthetic delivery. |
| `interrupted_reason` | no | The mark reason, for example `start-lost`. |
| `supersedes` | no | The `delivery_id` of the synthetic delivery that this completion replaces. |

## Headers

Each request carries four headers:

| Header | Value |
|---|---|
| `X-Varroa-Signature` | `v1=` followed by a hex HMAC-SHA256 digest. |
| `X-Varroa-Timestamp` | The Unix time, in seconds, when the request was signed. |
| `X-Varroa-Delivery` | The same value as `delivery_id`. |
| `X-Varroa-Event` | `run.completed`. |

The signed input is the timestamp, a period, and the exact request body:

```text
HMAC-SHA256(key, "<X-Varroa-Timestamp>." + <raw request body>)
```

Use the raw body bytes. Do not parse and re-encode the JSON before you
verify the signature.

### Verify a delivery

This Python example checks the signature and rejects old timestamps:

```python
import hashlib
import hmac
import time


def verify(key: bytes, body: bytes, signature: str, timestamp: str, max_age: int = 300) -> bool:
    if abs(time.time() - int(timestamp)) > max_age:
        return False
    digest = hmac.new(key, timestamp.encode() + b"." + body, hashlib.sha256).hexdigest()
    return hmac.compare_digest("v1=" + digest, signature)
```

Store the key in your own secret store. Read the delivery identifier
from `X-Varroa-Delivery`, and ignore a delivery that you already handled.

## Delivery and retries

The record store sends the request with a 10-second timeout. A `2xx`
response marks the delivery `delivered`. Any other response, or a timeout,
counts as a failed attempt. A redirect is not followed. A `3xx` response
counts as a failed attempt.

The record store makes up to six attempts. It waits between attempts:

| After attempt | Wait before the next attempt |
|---|---|
| 1 | 10 seconds |
| 2 | 30 seconds |
| 3 | 2 minutes |
| 4 | 10 minutes |
| 5 | 30 minutes |

After the sixth failed attempt, the delivery state becomes `failed`. The
record store then writes one activity event with type
`tenant.webhook_failed` and severity `error`. The event names the tenant,
the run, the delivery identifier, and the last error. It appears in the
activity feed.

Delivery is at least once. A receiver can get the same delivery more than
once, for example after a timeout that hides a success. Use
`X-Varroa-Delivery` to ignore the repeat.

The record store refuses to send to any address that is not global unicast.
Loopback, link-local, group, and unspecified addresses are not global unicast. A receiver must use an address that the record
store can reach.

A delivery records the Team incarnation that owned the run when the cell
claimed it. When you delete a Team and create a new one with the same name,
the record store drops any pending delivery of the old Team. The new Team's
receiver gets no deliveries for the old Team's runs.

Finished delivery rows stay for seven days, then the maintenance round
deletes them.

## Network egress

The recorder sends the webhook from its own pod. With `networkPolicy.enabled`
set, the recorder NetworkPolicy blocks that traffic by default. Enable the
egress rule for the receiver:

```yaml
networkPolicy:
  enabled: true
  recorderWebhookEgress:
    enabled: true
    cidrs: ["203.0.113.0/24"]
    ports: [443]
```

The rule is off by default. Enabled with an empty `cidrs` list, the render
fails. Set `["0.0.0.0/0"]` to allow every address. Pin the CIDR to your
receiver's address range when you can.

## Dashboard link

The recorder reads `VARROA_DASHBOARD_URL` from `auth.dashboardUrl`. The
payload `url` field prefixes this value to the run page path. Set
`auth.dashboardUrl` to the public address of the dashboard. When the value
is empty, the payload carries no `url` field.

## Slack and email

A pipeline step sends a Slack message or an email. The `slack`, `email-ext`,
and `mailer` plugins provide the steps. Varroa pins all three in every cell.

### Slack

Create a folder credential of type `secretText` that holds the Slack bot
token. Call `slackSend` in the pipeline:

```groovy
slackSend(channel: '#releases', message: "Build ${env.BUILD_NUMBER} passed",
          tokenCredentialId: 'slack-token', botUser: true)
```

The cell must reach Slack on port 443. Allow the egress in
`networkPolicy.cellIsolation.extraEgress` in the chart values.

### Email

Set the SMTP server on the `CellPool`:

```yaml
apiVersion: varroa.dev/v1alpha1
kind: CellPool
metadata:
  name: lts-2-555
spec:
  profileRef: lts-2-555
  mail:
    host: smtp.example.com
    port: 587
    security: starttls
    from: ci@example.com
    replyTo: ops@example.com
    credentialRef: pool-mail
```

| Field | Meaning |
|---|---|
| `host` | The SMTP server name. Required. |
| `port` | The SMTP server port. The default is 587. |
| `security` | `none`, `starttls`, or `tls`. The default is `starttls`. |
| `from` | The sender address. Required. |
| `replyTo` | The reply-to address. Optional. |
| `defaultSuffix` | The domain that a bare recipient name gets. Optional. |
| `credentialRef` | The Secret that holds the login. Optional. |

Create the login Secret in the operator namespace. Label it, so the
operator reads it. The Secret needs the keys `username` and `password`.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: pool-mail
  namespace: varroa-system
  labels:
    varroa.dev/pool-mail-credential: "true"
stringData:
  username: ci
  password: <the SMTP password>
```

The operator writes the login only into the cell's desired-state command.
The cell's configuration map never holds it. A cell that renders no mail
setting shows the `PoolMail` condition on its controller. The condition
names the fault, such as a missing Secret.

Use the `mail` step in a pipeline to send through the pool login:

```groovy
post {
  failure {
    mail to: 'team@example.com', subject: "${env.JOB_NAME} failed",
         body: "See ${env.BUILD_URL}"
  }
}
```

The `emailext` step does not use the pool login. The email-ext plugin
sends without a login. Use a relay that accepts mail from the cell
without a login, or use the `mail` step.

Removing `spec.mail` renders nothing. The last mail setting stays in the
cell's configuration until you recreate the cell.

## Related pages

- [Build records](build-records.md) describes the record store.
- [Pipeline mesh](pipeline-mesh.md) describes how runs move between cells.
