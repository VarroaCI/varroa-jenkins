# Varroa Operator Handbook

Varroa manages Jenkins controllers on Kubernetes through declarative resources, shared configuration, centralized access control, and fleet operations.

## Start here

- **Evaluate:** [Architecture](architecture/overview.md), [prerequisites](install/prerequisites.md), then [create a controller](tutorials/first-controller.md).
- **Install:** [Helm installation](install/helm-install.md), [ingress](install/ingress.md), and [network policies](install/network-policies.md).
- **Operate:** [Reconciliation](operations/reconciliation.md), [observability](operations/observability.md), and [troubleshooting](operations/troubleshooting.md).
- **Automate:** [API](api-reference.md), [varroactl](varroactl.md), or [MCP clients](agents/connecting.md).

Commands use `kubectl` and `helm`. Replace values in `<angle-brackets>` before running them. Example hosts use `example.com`.

## Architecture

- [Overview](architecture/overview.md): components, resources, and controller lifecycle
- [The mite](architecture/mite.md): controller agent identity, commands, and failure behavior
- [Scaling](architecture/scaling.md): replicas, capacity controls, and sizing

## Install

- [Prerequisites](install/prerequisites.md)
- [Helm installation](install/helm-install.md)
- [Ingress](install/ingress.md)
- [Multi-cluster](install/multi-cluster.md)
- [Amazon EKS](install/aws-eks.md)
- [Air-gapped installation](install/air-gapped.md)
- [Network policies](install/network-policies.md)

## Tutorials

- [Create your first controller](tutorials/first-controller.md)
- [Author a bundle](tutorials/custom-bundle.md)

## Configuration

- [Composed bundles](config/composed-bundles.md)
- [CasC catalog](config/casc-catalog.md)
- [Bundle sources](config/bundle-sources.md)
- [Jobs and items](config/items.md)
- [Controller classes](config/controller-classes.md)
- [Jenkins versions](config/jenkins-versions.md)
- [Plugin pinning](config/plugin-pinning.md)
- [Plugin packs](config/plugin-packs.md)
- [Pod customization](config/pod-customization.md)

## Security

- [Authentication](security/authentication.md)
- [Varroa RBAC](security/varroa-rbac.md)
- [Jenkins RBAC](security/jenkins-rbac.md)
- [API keys](security/api-keys.md)
- [executeGroovy controls](security/execute-groovy.md)

## AI and agents

- [Agentic fleet operations](agents/overview.md)
- [Connect a client](agents/connecting.md)
- [Tool conventions](agents/tools.md)
- [Write through MCP](agents/writing.md)
- [Identity and auditing](agents/identity.md)
- [Jenkins controller tools](agents/jenkins-tools.md)

## Operations

- [Reconciliation](operations/reconciliation.md)
- [Lifecycle](operations/lifecycle.md)
- [Multi-tenancy](operations/multi-tenancy.md)
- [Brood operations](operations/brood-operations.md)
- [Brood schedules](operations/brood-schedules.md)
- [Rollout waves](operations/rollout-waves.md)
- [Update center](operations/update-center.md)
- [Observability](operations/observability.md)
- [Troubleshooting](operations/troubleshooting.md)

## API and CLI

- [API reference](api-reference.md)
- [varroactl](varroactl.md)
