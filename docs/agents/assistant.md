# Assistant

The assistant is a chat surface in the Varroa BFF. It answers questions about
a run, a tenant, or a controller. It can propose a change. A change that writes
state waits for a person to approve it.

The assistant is off by default. An operator turns it on with the chart.

## The AssistantConfig object

The provider list, the default provider and model, the budgets, the approval
expiry, and the external data ceiling are fields of one cluster-scoped object,
`AssistantConfig` named `varroa-assistant`. An `assistant:admin` holder reads it
with `GET /api/v1/assistant/config` and writes it with `PUT /api/v1/assistant/config`.
The MCP tools `get_assistant_config` and `update_assistant_config` do the same.

A write sends the whole object, with its `metadata.resourceVersion`. A stale
version answers `409`. A write with no version only creates the object. An object
that does not exist reads as the default spec: no provider and the default limits.

The runtime follows the object. A change applies to the next request, message,
or run. No restart is needed. A run that is in progress keeps the budgets it
started with. An approval expiry change applies to approvals made after the change.

## Configure the assistant

The assistant needs the record store. Its session tables live in the record
store database. When the record store does not answer, the assistant stays on. Its calls answer 503 with the code `record_store_unreachable` until the store answers again. Each store call stops after 8 seconds. The Administration page shows a Retry button until then. A restart is not necessary.

Set these values to turn it on:

```yaml
recordStore:
  enabled: true
assistant:
  enabled: true
networkPolicy:
  enabled: true
```

Then set the providers in the object. Each provider with `cidrs` gets an egress
rule. See "Egress" below.

Then write the object `varroa-assistant`:

```yaml
apiVersion: varroa.dev/v1alpha1
kind: AssistantConfig
metadata:
  name: varroa-assistant
spec:
  providers:
    - name: primary
      kind: anthropic
      models: [model-a]
      trust: external
      auth: apiKey
    - name: local
      kind: openai-compatible
      baseURL: http://models.internal:8000/v1
      models: [model-b]
      trust: internal
      auth: none
  defaultProvider: primary
  defaultModel: model-a
  externalDataSharing: metadata
  budgets: {session: 200000, userDay: 1000000, tenantDay: 2000000, steps: 32}
  approvalExpiry: 24h
```

Each provider has these fields:

- `name` is a lowercase DNS label. It must be unique.
- `kind` is the wire protocol: `anthropic` or `openai-compatible`.
- `baseURL` is required for `openai-compatible`. It starts with `http://` or `https://`.
- `models` lists the model ids that a user may choose. It needs at least one.
- `trust` is `internal` when the endpoint runs inside your network, and `external` otherwise.
- `auth` is `apiKey` (the default) when the provider needs a key, and `none` when it does not.
- `cidrs` lists the address ranges of the provider. The operator opens egress to
  these ranges only.
- `port` is the TCP port of the egress rule. Zero takes the port of `baseURL`.

Exactly one provider is the default. `defaultProvider` names it, and
`defaultModel` must be one of its models. A new session uses your saved
provider. When your saved provider is not listed, the session uses the default
provider and model. A Tenant agent that names no provider uses the default too.

The API refuses a write that removes the default provider without naming a new one.

### Provider keys

Send a key to the API. A key write needs `assistant:admin`:

- `PUT /api/v1/assistant/providers/{name}/key` with the body `{"apiKey": "<key>"}`
  stores the key. The reply is 204 and has no body.
- `DELETE /api/v1/assistant/providers/{name}/key` removes the key.

The provider must be listed in the object and must have `auth: apiKey`. The
BFF sends the key to the operator as a core NATS request. The operator writes
the key into the Secret `varroa-assistant-keys`, one data key for each provider
name. No stream, log, event, or reply holds the key. The BFF and the Tenant
agent runner read the mounted file on each call, so a new key takes effect
within about one minute.

The operator owns the Secret. It removes each key whose provider is not listed,
or whose provider has `auth: none`. A Secret that you create by hand is adopted
on the next pass. Its keys for listed providers stay.

### External Postgres

When you run an external Postgres, the session role needs write access to
the four agent tables:

```sql
GRANT INSERT, UPDATE ON varroa.agent_sessions, varroa.agent_session_events,
  varroa.agent_approvals, varroa.agent_settings TO <client role>;
```

### Values

| Value | Default | Meaning |
|---|---|---|
| `assistant.enabled` | `false` | Turns the runtime on. |

The chart fails the render when `assistant.providers`, `assistant.budgets`,
`assistant.approvalExpiry`, `assistant.externalDataSharing`, or
`networkPolicy.assistantEgress` is set. Set those fields in the `AssistantConfig`
object instead.

### Egress

With `networkPolicy.enabled` true, the operator renders the policy
`varroa-assistant-egress` in the release namespace. The policy selects the BFF
and the Tenant agent runner. It allows egress to each provider `cidrs` on the
provider port, and nothing else. It holds no rule for a provider with no
`cidrs`.

The condition `EgressReady` on the object shows the result. It is `False` with
the reason `ProviderEgressMissing` when a provider has no `cidrs`. The
Administration page shows each provider's rule and the condition.

The defaults of the object are: `externalDataSharing` `metadata`, `budgets.session`
`200000`, `budgets.userDay` `1000000`, `budgets.tenantDay` `2000000`,
`budgets.steps` `32`, and `approvalExpiry` `24h`.

## Use the assistant

Open the assistant from the top bar, or press `a` on any page. The palette
also has an "Ask the assistant" row. The button and the key appear only when
the assistant is on for you.

The drawer shows a context chip. The chip names the build, job, tenant, or
controller in view. The assistant reads that context when you start a
conversation. A follow-up message keeps the first context. Open a new
conversation to change it.

When a conversation cannot take another message, the drawer shows **Start a new conversation**. Select it, then select **Send**. Your message goes to a new conversation for this page.

When the assistant is off for your account, the drawer shows a link to your assistant settings. When too many sessions run, the drawer shows a busy message and opens the session list.

Select **Sessions** to see your recent conversations. Each row shows your first message, the context, the state, and the age. On a phone, the drawer opens full screen.

The drawer footer shows the session state and its prompt and completion token counts. When the provider reports no usage, Varroa estimates the counts and marks them as estimated. A skipped session also shows why it did not run. While a session runs, the footer shows **Stop**. Select it to cancel the session.

Select **Close** to close the drawer. Press Escape while focus is inside the drawer to close it too. Focus then returns to the control that opened the drawer. When that control is gone, focus moves to the top-bar button. When you close the drawer with the top-bar button, focus stays on that button.

On a screen 1024 px or wider, drag the left edge of the drawer to change its width. You can also focus the edge and use the arrow keys. Home and End set the smallest and the largest width. The browser keeps the width for your next visit.

Your message shows in the transcript at once. Focus moves to the message box. A status line under it shows what the assistant does now. It thinks, it runs a named tool, or it waits for your approval. The answer text then appears at a steady rate. A conversation that you open again shows all of its text at once. When your system asks for reduced motion, the text and the status line do not move.

The assistant writes Markdown. Tables, task lists, and strikethrough also show. A wide table scrolls inside the drawer.

A tool call that changes something waits for approval. Approve or deny it in
the drawer. The call row opens while it waits. You can only decide an approval that you started.
After you decide, the transcript stays in place. The call row shows your decision, and the answer continues under it.

On a failed build, select **Explain this failure**. The answer is posted as a
note on the build. Everyone who can read the build sees the note.

Your assistant settings are on the Profile page, in the Assistant tab. Your
session key is kept for this login only. It is not stored in the browser.

When an admin removes your saved provider, the Assistant tab shows a notice. Pick a provider or Default, then save.
The API refuses a model that your saved provider does not list. A model needs a provider.
When the provider list does not load, the Assistant tab shows a notice.

Agent rows appear in the activity feed under the Agents filter.

The tenant home card starts a proposal. See [Propose a pipeline](../tutorials/propose-a-pipeline.md).

## Data levels

Each session has one data level. The level is the lowest of three limits:

- the user's level, which is `none`, `metadata`, or `logs`,
- the tenant's level, set by an admin for each tenant,
- the provider ceiling. An `internal` provider has no ceiling. An `external`
  provider has the `externalDataSharing` ceiling.

At level `none`, the session is skipped. At `metadata`, the model sees
run and job metadata. At `logs`, the model also sees the log tail.

A tool result is cut at 32768 bytes.

## System prompt

The system prompt carries the [operator guide](guide.md). The guide costs prompt tokens on every step, and the session budget counts them. The `anthropic` adapter marks the tool list, the system prompt, and the latest message for the provider prompt cache. The session budget counts cached tokens at their full count.

## Budgets and the kill switch

A session stops when a budget is spent. The stop writes an error event. The
day budget is checked before every provider call.

The Administration page saves the defaults with `PUT /api/v1/assistant/config`.
An empty default model uses the first model of the default provider.
The API refuses a default model that the default provider does not list.

An admin sets the kill switch with `PUT /api/v1/assistant/settings`. While it
is on:

- new sessions are stored as `skipped` with the reason `kill_switch`,
- a running session stops before its next provider call,
- no approval runs a tool.

## Approvals

The assistant does not run a write tool on its own. A write tool call parks
the session in `waiting_approval`. The approval names the tool, its
arguments, and its target.

A person approves or denies the call. An approval runs the tool as that
person, with that person's grants. A denial writes an error result and runs
nothing. When every approval of a session has a result, the session resumes.

An approval that is not decided in the approval expiry window expires.
The session then ends.

When a session ends before an approval has a decision, the approval becomes `denied`. A cancel records the person who cancels. The session does not resume.

A decision on an expired approval answers HTTP 409 with `approval_expired`.

## Read another person's session

The owner of an assistant session reads the full transcript. An admin with
`assistant:admin` reads every session. The admin sees each tool call, its name,
and its arguments. The admin does not see the content of a tool result. A tool
result holds data that the owner's grants fetched. The API marks each such
result as withheld, and the transcript shows a notice. The assistant text of
each turn stays visible to every reader who may view the session.

## Keys

A user may store a provider key for the browser session. The key is sealed
into an `HttpOnly` cookie. The cookie is `SameSite=Strict`, is scoped to the
API path, and has no lifetime. Logout clears it. An API key caller has no
login session, so it cannot store a key.

A session that uses a chart key reads the key from its Secret. A session that
uses a user key reads the cookie. If the cookie is missing, the session ends
with `session_key_missing`.

## Skip reasons and errors

| Code | Where | Meaning and action |
|---|---|---|
| `skipped` `budget_day` | Session state | The user spent the day budget. Wait for the UTC day to end. |
| `skipped` `kill_switch` | Session state | An admin paused the assistant. Ask an admin to resume it. |
| `skipped` `data_sharing_none` | Session state | The data level is `none`. Raise the user or tenant level. |
| `skipped` `provider_unavailable` | Session state | The provider has no key or no model. Check the Secret. |
| `budget_session` | Error event | The session spent its tokens. Start a new session. |
| `budget_day` | Error event | The day budget is spent. |
| `steps` | Error event | The session reached its step limit. |
| `kill_switch` | Error event | The kill switch stopped the session. |
| `provider_error` | Error event | The provider call failed. Check the provider and its key. |
| `session_key_missing` | Error event | The user key cookie is missing. Store the key again. |
| `lease_lost` | Error event | Another replica stopped the run. An approved tool may have run. Send a new message. |
| `data_level_lowered` | Error event | The data level of the session dropped below its start level. Start a new conversation. |
| `data_level_unchecked` | Error event | The data level could not be read on resume. No model call ran. Start a new conversation. |
| `assistant_disabled` | HTTP 503 | The assistant is off, or the record store schema is too old. Turn on the assistant, or update the recorder. |
| `record_store_unreachable` | HTTP 503 | The record store does not answer. Retry when it answers. |
| `assistant_key_unavailable` | HTTP 503 or 409 | No key is available for the provider. |
| `assistant_off` | HTTP 409 | The caller turned the assistant off. |
| `assistant_busy` | HTTP 409 or 429 | Too many sessions run, or the session changed. |
| `prompt_not_startable` | HTTP 403 | The prompt cannot start a session. |
| `kill_switch` | HTTP 409 | The kill switch blocks an approval. |
| `approval_expired` | HTTP 409 | The approval expired before the decision. Ask again in a new session. |
| `tenant_agent_session` | HTTP 409 | A Tenant agent session takes no messages. |
