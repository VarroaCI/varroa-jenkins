# <img src="assets/logo.svg" width="56" height="56" alt="Varroa" align="top"/> Varroa

Jenkins pipelines without a Jenkins server to keep alive.

[![PR](https://github.com/varroaci/varroa-jenkins/actions/workflows/pr.yaml/badge.svg)](https://github.com/varroaci/varroa-jenkins/actions/workflows/pr.yaml)

Every Jenkins team knows the one server that must never go down. Varroa removes it. A
team declares that it wants pipelines, and its Jenkinsfiles run on shared, replaceable
Jenkins processes that Varroa creates and discards. Build history lives outside Jenkins.
A build that is running when its host disappears picks up on the next one.

The same control plane also runs classic Jenkins controllers as Kubernetes resources,
on one cluster or many, with single sign-on, federated RBAC, pinned plugins, an
air-gapped update center, scale-to-zero, and an MCP endpoint for AI agents.

## Pipelines

A `Team` with a `pipelines` block is a pipelines tenant. Varroa places it on a shared
Jenkins that matches the requested profile and creates its jobs from the named bundle.

```yaml
apiVersion: varroa.dev/v1alpha1
kind: Team
metadata:
  name: payments
spec:
  namespaces: ["team-payments"]
  pipelines:
    profile: jenkins-lts
    agentNamespace: team-payments
    composedBundleRef:
      name: payments-pipelines
      namespace: team-payments
```

What the team gets:

- **No server to own.** Varroa runs the Jenkins behind the pipelines and replaces it on
  its own. Nobody configures it, backs it up, or restarts it. See
  [Cells](docs/operations/cells.md).
- **History outside Jenkins.** Runs, stages, tests, logs, and artifacts live in Postgres
  and object storage, and the pipelines page reads them from there. See
  [Build records](docs/operations/build-records.md).
- **Builds that survive the host.** A running build streams its state off the Jenkins
  process every few seconds. A replacement resumes it from its last saved point, and only
  the work since that point runs again. No shared filesystem and no clustered JVM are
  involved. See
  [Pipeline mesh](docs/operations/pipeline-mesh.md).
- **One build-number ledger.** A replaced Jenkins never reuses a number, and two copies
  never run the same build. See [Build claims](docs/operations/build-claims.md).
- **Fleet-wide locks and cross-team builds.** The `varroa` global variable adds
  `varroa.lock`, `varroa.build`, and artifact copies across teams, each checked against
  the target team's export grant. See
  [Cross-tenant pipelines](docs/config/cross-tenant-pipelines.md).
- **Your own machines.** A team attaches a workstation, build box, or VM it owns as an
  agent for its pipelines alone. See [External runners](docs/operations/runners.md).

A Jenkinsfile needs no change. Scripted, declarative, multibranch, shared libraries, and
Kubernetes agents work as they do today.

```groovy
pipeline {
  agent { label 'kubernetes' }
  stages {
    stage('Deploy') {
      steps {
        script {
          varroa.lock(name: 'deploy-production') {
            sh './deploy.sh'
          }
        }
      }
    }
  }
}
```

Start with [Read and run pipelines](docs/tutorials/pipelines.md), then
[Move a Jenkinsfile onto a cell](docs/tutorials/move-to-a-cell.md).

## Adoption path

Each row is optional and independent. Adopting one does not require the next.

| Situation | Resource |
|---|---|
| A team wants pipelines, not a Jenkins | A [`Team`](docs/operations/multi-tenancy.md) with a `pipelines` block. |
| A team wants its own Jenkins | A [`Controller`](docs/tutorials/first-controller.md). It uses the built-in starter bundle. |
| A team wants its own config | A [`ComposedBundle`](docs/config/composed-bundles.md) pointed at their git repo. |
| Several teams, one baseline | [Catalog items](docs/config/casc-catalog.md), shared fragments composed in order. |
| Deliberate upgrades | [`JenkinsVersionProfile`](docs/config/jenkins-versions.md), a pinned core plus plugin lock. |
| Changes that must land safely | [Rollout waves](docs/operations/rollout-waves.md). |
| No egress allowed | The [update center](docs/install/air-gapped.md). |
| More than one cluster | [Multi-cluster](docs/install/multi-cluster.md). |
| AI agents driving the platform | The [MCP endpoint](docs/agents/overview.md). |

## Install

Varroa is deployed with Helm on Kubernetes v1.30 or later. For evaluation you need a
default StorageClass. For other people you also need `cert-manager`, a domain name, and
an OIDC identity provider. [Prerequisites](docs/install/prerequisites.md) splits the two.

```bash
helm install varroa oci://ghcr.io/varroaci/charts/varroa --version 0.1.0 \
  --namespace varroa-system \
  --create-namespace \
  --values values.yaml
```

[Installing with Helm](docs/install/helm-install.md) covers the values, and
[Build records](docs/operations/build-records.md) covers the storage that pipelines need.
The smallest controller Varroa accepts names no bundle and no version:

```yaml
apiVersion: varroa.dev/v1alpha1
kind: Controller
metadata:
  name: demo
  namespace: varroa
spec:
  namespace: varroa
```

## How it works

Varroa is a Kubernetes operator with a gateway, an HTTP API, a dashboard, and a
recorder. A **mite** sidecar in every Jenkins pod holds a long-lived gRPC stream to the
gateway over mTLS and applies the desired state the operator publishes through NATS:
JCasC configuration, plugin manifests, and RBAC. The recorder owns build history in
Postgres and object storage. A Jenkins plugin, **VarroaSecurityRealm**, validates the
single sign-on token and the operator-signed mite token offline, so no Jenkins holds a
credential at rest.

Read the [Operator Handbook](docs/README.md) for the full picture:
[architecture](docs/architecture/overview.md), [authentication](docs/security/authentication.md),
[RBAC](docs/security/varroa-rbac.md), [AI agents](docs/agents/overview.md), and the
[API reference](docs/api-reference.md).

## Development

```bash
make localdev        # full stack on a disposable kind cluster, browser-ready
make build           # operator, gateway, BFF, mite, update center, recorder
make build-cli       # bin/varroactl
make test            # all Go tests with race detector
make lint            # golangci-lint
cd frontend && npm ci && npm run dev   # dashboard at :3000
```

`make localdev` stands up the whole stack at `https://app.varroa.localtest.me`. Sign in
with `admin` and `password`. See [AGENTS.md](AGENTS.md) for the codebase map.

## Contributing

Issues and pull requests are welcome. Varroa is AGPL-3.0 with a commercial license
available. See [CLA.md](CLA.md) and [COMMERCIAL-LICENSE.md](COMMERCIAL-LICENSE.md).
