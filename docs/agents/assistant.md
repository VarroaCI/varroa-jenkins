# Assistant

The assistant is a chat surface in the Varroa BFF. It answers questions about
a run, a team, or a controller. It can propose a change. A change that writes
state waits for a person to approve it.

The assistant is off by default. An operator turns it on with the chart.

## Configure the assistant

The assistant needs the record store. Its session tables live in the record
store database.

Set these values to turn it on:

```yaml
recordStore:
  enabled: true
assistant:
  enabled: true
  providers:
    - name: primary
      kind: anthropic
      baseURL: https://models.example.net
      models: [model-a]
      trust: external
      default: true
      secretRef: assistant-primary-key
    - name: local
      kind: openai-compatible
      baseURL: http://models.internal:8000/v1
      models: [model-b]
      trust: internal
      secretRef: assistant-local-key
networkPolicy:
  enabled: true
  assistantEgress:
    cidrs: [10.20.30.0/24]
    ports: [443, 8000]
```

Each provider has these fields:

- `name` is a lowercase DNS label. It must be unique.
- `kind` is the wire protocol: `anthropic` or `openai-compatible`.
- `baseURL` is required for `openai-compatible`. It starts with `http://` or `https://`.
- `models` lists the model ids that a user may choose.
- `trust` is `internal` when the endpoint runs inside your network, and `external` otherwise.
- `default: true` marks the provider for new sessions. Exactly one provider sets it.
- `secretRef` names a Secret that holds the key under the `apiKey` key.

Rotate a key by updating the Secret. The BFF reads the key file on every call.
A restart is not needed.

`networkPolicy.assistantEgress.cidrs` must name the provider addresses when
`networkPolicy.enabled` is true. The render fails when it is empty.

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
| `assistant.providers` | `[]` | The model providers. |
| `assistant.externalDataSharing` | `metadata` | The highest data level for an `external` provider. |
| `assistant.budgets.session` | `200000` | Tokens for one session. |
| `assistant.budgets.userDay` | `1000000` | Tokens for one user in one UTC day. |
| `assistant.budgets.teamDay` | `2000000` | Tokens for one team in one UTC day. |
| `assistant.budgets.steps` | `32` | Provider calls for one session. |
| `assistant.approvalExpiry` | `24h` | How long an approval waits. |

## Use the assistant

Open the assistant from the top bar, or press `a` on any page. The palette
also has an "Ask the assistant" row. The button and the key appear only when
the assistant is on for you.

The drawer shows a context chip. The chip names the build, job, team, or
controller in view. The assistant reads that context when you start a
conversation. A follow-up message keeps the first context. Open a new
conversation to change it.

A tool call that changes something waits for approval. Approve or deny it in
the drawer. You can only decide an approval that you started.

On a failed build, select **Explain this failure**. The answer is posted as a
note on the build. Everyone who can read the build sees the note.

Your assistant settings are on the Profile page, in the Assistant tab. Your
session key is kept for this login only. It is not stored in the browser.

Agent rows appear in the activity feed under the Agents filter.

## Data levels

Each session has one data level. The level is the lowest of three limits:

- the user's level, which is `none`, `metadata`, or `logs`,
- the team's level, set by an admin for each tenant,
- the provider ceiling. An `internal` provider has no ceiling. An `external`
  provider has the `externalDataSharing` ceiling.

At level `none`, the session is skipped. At `metadata`, the model sees
run and job metadata. At `logs`, the model also sees the log tail.

A tool result is cut at 32768 bytes.

## Budgets and the kill switch

A session stops when a budget is spent. The stop writes an error event. The
day budget is checked before every provider call.

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
| `skipped` `data_sharing_none` | Session state | The data level is `none`. Raise the user or team level. |
| `skipped` `provider_unavailable` | Session state | The provider has no key or no model. Check the Secret. |
| `budget_session` | Error event | The session spent its tokens. Start a new session. |
| `budget_day` | Error event | The day budget is spent. |
| `steps` | Error event | The session reached its step limit. |
| `kill_switch` | Error event | The kill switch stopped the session. |
| `provider_error` | Error event | The provider call failed. Check the provider and its key. |
| `session_key_missing` | Error event | The user key cookie is missing. Store the key again. |
| `lease_lost` | Error event | Another replica stopped the run. Send a new message. |
| `assistant_disabled` | HTTP 503 | The assistant is off. |
| `assistant_key_unavailable` | HTTP 503 or 409 | No key is available for the provider. |
| `assistant_off` | HTTP 409 | The caller turned the assistant off. |
| `assistant_busy` | HTTP 409 or 429 | Too many sessions run, or the session changed. |
| `prompt_not_startable` | HTTP 403 | The prompt cannot start a session. |
| `kill_switch` | HTTP 409 | The kill switch blocks an approval. |
