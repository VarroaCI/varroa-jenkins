# <img src="assets/logo.svg" width="56" height="56" alt="Varroa" align="top"/> Varroa

Jenkins has evolved.

[![PR](https://github.com/varroaci/varroa-jenkins/actions/workflows/pr.yaml/badge.svg)](https://github.com/varroaci/varroa-jenkins/actions/workflows/pr.yaml)

Think GitHub Actions, with a stateless Jenkins engine underneath. Varroa runs your
Jenkinsfiles on Jenkins processes it creates and replaces. Build history lives outside
them, and a running build carries on when its Jenkins dies. Need a Jenkins of your own?
Varroa runs classic controllers as Kubernetes resources too, and one MCP endpoint lets AI
agents drive all of it.

## Pipelines

A team declares pipelines and names its Jenkinsfiles. Varroa runs them on a Jenkins it
creates and replaces on its own. Build history lives outside that Jenkins. When its host
is lost, a running build continues on the next one, with no shared disk and no cluster
of JVMs.

Start with [Read and run pipelines](docs/tutorials/pipelines.md), then
[Move a Jenkinsfile onto a cell](docs/tutorials/move-to-a-cell.md). The handbook covers
[build records](docs/operations/build-records.md), [how a build survives](docs/operations/pipeline-mesh.md),
[cross-team calls and locks](docs/config/cross-tenant-pipelines.md), and
[your own machines as agents](docs/operations/runners.md).

## AI agents

One MCP endpoint covers the fleet and every Jenkins in it. An agent can create a
controller, compose its configuration, run a job, and read the log. Each call carries the
agent's own identity, so Jenkins applies its normal authorization.

See the [agents overview](docs/agents/overview.md) and [agent identity](docs/agents/identity.md).

## Classic Jenkins

A `Controller` resource is one Jenkins with its own home. Configuration comes from git,
sign-on from your identity provider, and permissions from cluster-wide roles. An idle
controller scales to zero and wakes on the next request.

```yaml
apiVersion: varroa.dev/v1alpha1
kind: Controller
metadata:
  name: demo
  namespace: varroa
spec:
  namespace: varroa
```

Start with [your first controller](docs/tutorials/first-controller.md). The handbook
covers [composed bundles](docs/config/composed-bundles.md),
[version profiles](docs/config/jenkins-versions.md), [rollout waves](docs/operations/rollout-waves.md),
the [air-gapped update center](docs/install/air-gapped.md), and
[multi-cluster](docs/install/multi-cluster.md).

## Install

```bash
helm install varroa oci://ghcr.io/varroaci/charts/varroa --version 0.1.0 \
  --namespace varroa-system \
  --create-namespace \
  --values values.yaml
```

Read [prerequisites](docs/install/prerequisites.md) and
[installing with Helm](docs/install/helm-install.md). The
[Operator Handbook](docs/README.md) indexes everything else.

## Development

```bash
make localdev   # full stack on a disposable kind cluster
make build      # every binary
make test       # Go tests with the race detector
make lint       # golangci-lint
```

See [AGENTS.md](AGENTS.md) for the codebase map.

## Contributing

Issues and pull requests are welcome. Varroa is AGPL-3.0 with a commercial license
available. See [CLA.md](CLA.md) and [COMMERCIAL-LICENSE.md](COMMERCIAL-LICENSE.md).
