# Runner Tokens

`status.token` authenticates an `inbound` runner at the [agent door](../operations/runners.md#launcher-classes),
the public URL an external machine uses to reach its cell. It is the only credential the
machine holds. It is not a Jenkins agent secret, and the machine never obtains one.

An `ssh` runner has no token. The cell holds the SSH credential and dials the machine, so the
machine holds no Varroa credential of its own.

## What the token is for

The token names a runner and proves the holder is that runner. The door reads it from the URL
path, `https://<door host>/runners/<token>/`, and resolves it to the owning `Team`, then to that
team's current cell. On every connection attempt the door fetches a fresh Jenkins agent secret
from the cell and injects it into the handshake. The machine never sees that secret, and it
never needs to: the token in the printed launch command is the only value the launch command
carries.

## Reading the token

The BFF omits `status.token` from a runner's response unless the caller holds `runners:create`.
A caller with only `runners:read`, such as `varroa:developer` or `varroa:viewer`, sees every
other status field but not the token.

This is a BFF-level control, not a Kubernetes-level one. A Kubernetes user or service account
with direct read access on the `runners.varroa.dev` resource still reads the raw
`status.token`, because the apiserver applies no such redaction. Scope direct cluster access
accordingly.

## Rotating a token

There is no in-place rotation. The operator mints a token once, when it first reconciles an
`inbound` runner, and never replaces it. To rotate a token, delete the `Runner` and register it
again:

```bash
varroactl runner delete lin-1
varroactl runner register lin-1 --tenant payments --remote-fs /home/jenkins/agent
```

The new registration mints a new token and a new launch command. The old token stops resolving
as soon as the old object is gone.

## Door failure answers

| Status | Cause |
|---|---|
| `401` | The token names no runner, the runner has no token yet, or the token belongs to an `ssh` runner. |
| `404` | The request does not match one of the door's three paths. |
| `503` | The runner's team has no current cell placement. Retry; the agent's own retry loop handles this. |

A `401` carries the same body for every cause, so it reveals nothing about which runners exist.
The door never contacts a cell before it has resolved and checked the token.
