# Tenant webhooks

A pipelines tenant receives forge webhooks through its own door. The door is served from the
BFF public host. It is outside `/api/v1` and outside login.

## The door URL

```
/hibernation/<token>/teams/<team>/queue/<path>
```

`<token>` addresses the tenant. `<path>` selects the forge and the Jenkins endpoint. Any path
that the door does not serve answers `404`.

Two credentials guard the door. Varroa mints each once, and never regenerates them:

- `status.pipelines.webhookToken` goes in the URL. Only the `Team` object holds it:

  ```bash
  kubectl get team payments -o jsonpath='{.status.pipelines.webhookToken}'
  ```

- `status.pipelines.webhookSecret` proves that the forge sent the body. Set it as the signing
  secret in the forge.

## Forges and paths

| Forge | Path | Verified by | Job trigger |
|---|---|---|---|
| GitHub | `github-webhook/` | `X-Hub-Signature-256` (HMAC-SHA256) | `githubPush` |
| Gitea or Forgejo | `gitea-webhook/post` | `X-Gitea-Signature` (or `X-Forgejo-Signature`), HMAC-SHA256 over the bare hex digest | Gitea SCM source: a `multibranch` or `organizationFolder` item |
| Gitea or Forgejo | `generic-webhook-trigger/invoke?token=<team>:<name>` | `X-Gitea-Signature` (or `X-Forgejo-Signature`), HMAC-SHA256 over the bare hex digest | `genericTrigger` on a `pipeline` item |
| GitLab | `generic-webhook-trigger/invoke?token=<team>:<name>` | `X-Gitlab-Token` | `genericTrigger` |
| Generic | `generic-webhook-trigger/invoke?token=<team>:<name>` | `X-Varroa-Token` | `genericTrigger` |

The door identifies the forge from the request headers. A request with `X-Gitea-Event` or
`X-Forgejo-Event` is Gitea or Forgejo. A request with `X-GitHub-Event` is GitHub. A request with
`X-Gitlab-Event` is GitLab. Any other request is generic.

GitHub works without extra setup.

## The token fence

The door checks each delivery against the tenant before it publishes the delivery. The
checks run in this order:

1. The door token must match the tenant token. A mismatch answers `401`.
2. The forge signature must verify against the tenant secret. A mismatch answers `400`.
3. The path must match the forge, and the token fence must hold. A failure answers `400`.

The fence has three rules:

- A `github-webhook/` path needs a GitHub delivery. A GitLab or generic delivery on that path
  answers `400`.
- A `gitea-webhook/` path needs a Gitea or Forgejo delivery. Any other delivery on that path
  answers `400`.
- A `generic-webhook-trigger/invoke` path needs a `token` query value that starts with
  `<team>:` and has at least one character after the colon. Otherwise the door answers `400`.

The generic path accepts a delivery from any forge, but the forge signature must verify. So a
Gitea or Forgejo delivery can use `gitea-webhook/` or `generic-webhook-trigger/invoke`. It cannot
use `github-webhook/`.

The fence keeps one tenant from starting another tenant's jobs. Many tenants share a cell, and
the generic trigger plugin searches every job on the controller for a matching token. A
Kubernetes name never contains `:`, so a `<team>:` prefix names exactly one tenant.

## Set up a GitLab project

1. Open the project. Go to **Settings > Webhooks**.
2. Set **URL** to the door URL. Add the job token as the `token` query value:

   ```text
   https://<host>/hibernation/<token>/teams/payments/queue/generic-webhook-trigger/invoke?token=payments:deploy
   ```

3. Set **Secret token** to the `webhookSecret` value from the `Team` status.
4. Select the events to send, then save the webhook.

GitLab sends the secret in `X-Gitlab-Token`. The door verifies it and does not forward it.

## Set up a Forgejo or Gitea webhook

Select the path from the job trigger:

- For a `multibranch` or `organizationFolder` item, use `gitea-webhook/post`.
- For a `pipeline` item with a `genericTrigger`, use
  `generic-webhook-trigger/invoke?token=<team>:<name>`.

| Field | Value |
|---|---|
| Target URL | `https://<host>/hibernation/<token>/teams/<team>/queue/<path>` |
| Content type | `application/json` |
| Secret | The value of `status.pipelines.webhookSecret` |
| Events | Push, and pull request for a source job |

The forge signs the body with the secret. The door verifies the signature.

### One hook for an organization folder

Add the hook to the organization, not to each repository. One organization hook on
`gitea-webhook/post` reaches an `organizationFolder` item for every repository of the
organization. A delivery for a new repository adds its project to the folder.

### Source setup

A Gitea source also needs these parts. This page does not repeat them:

- A server entry for the forge. On a cell, set `CellPool.spec.giteaServers`. See
  [Commit status](commit-status.md#forgejo-and-gitea).
- A folder credential with an access token. The token needs the `write:repository` and
  `read:organization` scopes. See [Credentials and webhook](commit-status.md#credentials-and-webhook).
- The item. An `organizationFolder` item needs a `giteaNavigator`.
  See [Gitea and Forgejo sources](../config/items.md#gitea-and-forgejo-sources).

## Send a generic delivery

A generic sender signs the body with `X-Varroa-Token`. The value is the `webhookSecret`:

```bash
curl -i -X POST \
  -H "X-Varroa-Token: $SECRET" \
  -H "Content-Type: application/json" \
  --data '{"ref":"refs/heads/main"}' \
  "https://<host>/hibernation/<token>/teams/payments/queue/generic-webhook-trigger/invoke?token=payments:deploy"
```

Expect `202 Accepted`. Then the `payments` folder starts the job, if its `genericTrigger`
matches the request.

## Declare a generic trigger in items

A job starts on a generic delivery when its `items.yaml` declares a `genericTrigger`. The
`token` must equal the value that the door call carries in `?token=`. It must also start with
the team name and a colon. Varroa rejects a tenant manifest with another prefix:

```yaml
triggers:
  - genericTrigger:
      token: payments:deploy
      regexpFilterText: $ref
      regexpFilterExpression: "^refs/heads/main$"
      genericVariables:
        - key: ref
          value: $.ref
```

The `genericVariables` entries set build variables from the request body. The `expressionType`
is `JSONPath` (the default) or `XPath`. A header value is not extractable from `items.yaml` today.

The job page **Triggers** card shows **Webhook: Yes** for a job that declares a `genericTrigger`.
The card does not show the token.

## Pins for air-gapped installs

The baseline lock pins `generic-webhook-trigger` and `github-checks`. An air-gapped install must
pre-seed both plugins in the update center. See [Update center](update-center.md).

## Buffering

A delivery buffers for up to one hour, so a tenant that moves cells in that hour still
receives it. After that, only the forge's retry recovers it.

## Verify

A `202` response means that the door buffered the delivery. It does not mean that a build
started. A build starts only when the job trigger matches.

This GitHub delivery checks the signature and the path:

```bash
curl -i -X POST \
  -H "X-GitHub-Event: push" \
  -H "X-Hub-Signature-256: sha256=$(openssl dgst -sha256 -hmac "$SECRET" -hex body.json | cut -d' ' -f2)" \
  --data-binary @body.json \
  "https://<host>/hibernation/<token>/teams/payments/queue/github-webhook/"
```

Expect `202 Accepted`, then a build in the `payments` folder. A push of an already built
revision starts no build.
