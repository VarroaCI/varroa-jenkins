# Headless controllers

A headless controller blocks its Jenkins web UI for everyone except an admin. Use it when
users should reach jobs only through the Pipelines page.

## What headless mode is

Headless mode is a Jenkins-side filter, backed by an operator flag. A cell controller is
always headless. A one-tenant controller opts in with `spec.headless: true`.

## What a non-admin user loses

A non-admin loses the Jenkins web UI:

- Job pages
- Build pages
- The script console
- The plugin manager
- System configuration

The [Pipelines page](../tutorials/pipelines.md) still works for jobs, builds, logs, and
artifacts. Inbound and outbound agents, webhooks, health probes, and the activity feed keep
working.

## Infrastructure allowlist

The Jenkins filter keeps these paths open on every headless controller, for every caller.

| Match | Path | Keeps working |
|---|---|---|
| prefix | `/login` | Kubernetes probes; break-glass sign-in |
| prefix | `/logout` | Break-glass sign-out |
| exact | `/j_spring_security_check` | Form-based sign-in |
| prefix | `/crumbIssuer/` | The mite and the API server |
| prefix | `/instance-identity/` | Inbound agent trust setup |
| prefix | `/tcpSlaveAgentListener/` | JNLP agents |
| prefix | `/wsagents/` | WebSocket agents |
| prefix | `/jnlpJars/` | The agent JAR download |
| prefix `/computer/`, suffix `/slave-agent.jnlp` or `/jenkins-agent.jnlp` | Agent connection setup |
| prefix | `/github-webhook/` | Forge webhooks and webhook replay |
| prefix | `/gitea-webhook/` | Gitea and Forgejo webhooks and webhook replay |
| exact | `/generic-webhook-trigger/invoke` | Forge webhooks and webhook replay |
| prefix | `/varroa-activity/` | The activity drain |
| prefix | `/varroa-run` | The pipeline run trigger |
| prefix | `/prometheus/` | Health probes and metrics |
| prefix | `/static/`, `/adjuncts/`, `/images/`, `/css/`, `/scripts/`, `/userContent/` | Page assets |
| exact | `/favicon.ico`, `/robots.txt` | Browser requests |

The filter allows no other path. It never grants a Jenkins permission on an allowed path. The
authorization strategy still applies there.

A headless tenant answers an `input` step from the Pipelines build page. The mite sends the
answer over the Jenkins loopback inside the pod, so the request never leaves the pod. The
allowlist above already carries the `/varroa-run` prefix. This action needs no new allowlist
entry and no ingress rule change.

## Make a controller headless

Add `spec.headless: true` to a one-tenant controller.

```yaml
apiVersion: varroa.dev/v1alpha1
kind: Controller
metadata:
  name: build
  namespace: jenkins-platform
spec:
  headless: true
```

```bash
kubectl apply -f controller.yaml
kubectl get controller build -n jenkins-platform \
  -o jsonpath='{.status.conditions[?(@.type=="NoExternalURL")]}{"\n"}'
```

The condition reads `True` with reason `Headless` once the roll finishes.

Set `spec.headless: false` to reverse it. `spec.headless` has no effect on a cell: a cell is
always headless.

## What the operator removes

The operator deletes the controller's Ingress. It creates no new Ingress while the controller
stays headless.

It keeps the cluster-IP Service. Agents and in-cluster callers still reach Jenkins through it.

It sets the `NoExternalURL` condition to `True` with reason `Headless`. The message names the
Pipelines page and the break-glass command.

## The two break-glass grants

Break-glass access needs two separate grants:

| Grant | Where it lives | What it lets you do |
|---|---|---|
| The `controllers/break-glass` API verb | A Varroa role | Request a break-glass token |
| The `hudson.model.Hudson.Administer` Jenkins permission | A Jenkins role assignment | Use the token once Jenkins receives it |

Only the built-in `admin` Varroa role holds the API verb. See [Varroa RBAC](varroa-rbac.md) for
how to grant it to a custom role.

Holding only the API verb still gets a token. Jenkins denies the session until the caller also
holds `hudson.model.Hudson.Administer`.

## Break-glass runbook

Run these commands in order:

1. `varroactl break-glass <namespace>/<name> --reason "<why>"`
2. Run the `kubectl port-forward` command the previous step prints, in another terminal.
3. Open the proxy URL the first command prints, in a browser.
4. Work in Jenkins. The command exits when the token expires or you press Ctrl-C. The proxy
   stops with it.

The browser never holds a Jenkins credential. The proxy strips every inbound `Authorization`
and `Cookie` header before it forwards a request.

See [varroactl](../varroactl.md#reach-a-headless-controller) for every flag.

## Audit events

Every break-glass grant produces one of three activity events.

| Type | Emitted by | Repeat limit |
|---|---|---|
| `controller.breakglass.granted` | The API server, on every grant | None |
| `controller.breakglass.session` | Jenkins, on the first admin request of a session | One per admin every 30 minutes |
| `controller.breakglass.script` | Jenkins, on a sensitive path such as `/scriptText` or `/manage` | One per admin and path every 60 seconds |

Read them with:

```bash
varroactl activity --controller <name>
```

See [Observability](../operations/observability.md#use-activity-history) for the full command
and the record-store prerequisite.

## Troubleshooting

| Response | Cause | Action |
|---|---|---|
| `503`, "Jenkins is starting" | Jenkins has not finished booting | Wait, then retry |
| `403`, names a missing realm | The bundle never installed the Varroa security realm | Set `spec.headless: false` to recover, then fix the bundle |
| `403`, names `hudson.model.Hudson.Administer` | The caller holds the API verb but not the Jenkins permission | Grant the Jenkins permission, or use the Pipelines page instead |
| `409` from the break-glass endpoint | The controller is not headless | Connect to it directly; it needs no grant |
| `400` from the break-glass endpoint | The controller runs on a cluster other than core | Break-glass reaches only the core cluster |
| `404` on a redirect link | The controller is headless and hibernated | Expected. Use break-glass instead of the redirect link |

## Limitations

Headless mode does not remove Kubernetes access. Anyone who can read the pod or edit
`spec.headless` still reaches Jenkins.

Break-glass reaches only the core cluster. A controller on another cluster has no break-glass
path.

A Jenkins page that embeds an absolute in-cluster URL in its body does not render correctly
through the break-glass proxy. The proxy rewrites redirect headers, not page bodies.

A controller whose bundle never installs the Varroa security realm denies every request. Set
`spec.headless: false` to recover it, then fix the bundle.

## Location URL and Jenkins links

The location URL of a headless controller is the dashboard door, not the in-cluster Service
address:

```text
https://<dashboard>/headless/<cluster>/<namespace>/<controller>/
```

Every Jenkins link that the controller emits uses this origin. `BUILD_URL` and `JENKINS_URL`
in a build point to the door too. A link to a Jenkins item opens the matching Varroa page.
For example, `…/headless/core/team-a/ci/job/team-a/job/app/12/display/redirect` opens
`/pipelines/team-a/team-a%2Fapp/<runId>`.

The door resolves a link only when a recorded run matches the item path. The record store must be
enabled for the link to resolve. Without a match, the door answers "Build page not found".

Pods of a headless controller also receive `KUBERNETES_JENKINS_URL` with the in-cluster Service
address. A cloud without an explicit `jenkinsUrl` keeps its agents inside the cluster.

The operator reads the dashboard origin from the OIDC redirect URL. The BFF reads it from
`auth.dashboardUrl`. Set both to the same host. A member cluster with no dashboard URL keeps
the in-cluster location URL.

Changing the location URL rolls each headless controller once.
