# Varroa Operator Handbook

Varroa is a Kubernetes-native operator for managing broods of Jenkins controllers: declarative provisioning, Configuration-as-Code bundles, federated authentication and RBAC, version and plugin governance, and progressive rollouts — from a single control plane.

The dashboard global search opens from the topbar or with `Ctrl+K`/`Cmd+K` and
navigates to authorized controllers, namespaces, groups, and catalog items.

The sidebar organises navigation into three groups: **Operate** (Dashboard, Controllers, Clusters, Activity), **Brood** (Operations, Schedules), and **Manage**. The Manage group's two doors — **Catalog** (`/catalog`) and **Admin & access** (`/settings`) — and their contents are driven by the signed-in user's permissions: the Catalog door appears when the user holds any catalog read permission, and the Admin & access door appears when they hold any global admin-level permission. Below 640 px the sidebar is replaced by a bottom tab bar; a **More** sheet exposes the Brood, Manage, and Account links. The sidebar can also be collapsed to an icon rail via the foot button or the `[` key; the choice persists per browser and nothing else triggers the rail.

**Conventions used throughout:** commands are `kubectl`/`helm`-first with dashboard notes where the UI is the primary flow; YAML examples are complete and appliable as-is; `<angle-brackets>` mark values you must supply; example hosts use `example.com`.

## Where to start

| You are… | Start here |
|---|---|
| **Evaluating** Varroa | [Architecture overview](architecture/overview.md), then the [tutorial](tutorials/first-controller.md) and [roadmap](roadmap.md) |
| **Installing** it | [Prerequisites](install/prerequisites.md) → [Helm install](install/helm-install.md) → [tutorial](tutorials/first-controller.md) |
| **Operating** a running brood | [Reconciliation](operations/reconciliation.md), [Observability](operations/observability.md), [Troubleshooting](operations/troubleshooting.md) |

## Architecture

| Page | What it covers |
|---|---|
| [Overview](architecture/overview.md) | Components, all CRDs, the controller phase lifecycle, terminology |
| [The mite](architecture/mite.md) | The per-controller agent: identity handshake, command stream, Jenkins auth, drains |
| [Scaling](architecture/scaling.md) | What scales how; replicas, activity retention, presets, and the levers |

## Install

| Page | What it covers |
|---|---|
| [Prerequisites](install/prerequisites.md) | Cluster requirements, identity provider, DNS/TLS, tooling |
| [Local development](install/local-development.md) | `make localdev`: the full stack on a disposable kind cluster |
| [Helm install](install/helm-install.md) | Chart walkthrough, values by area, upgrade and uninstall |
| [Ingress](install/ingress.md) | Dashboard + per-controller exposure, TLS, subdomain vs path mode |
| [Network policies](install/network-policies.md) | Opt-in default-deny policy set and tenant scoping |
| [Air-gapped installation](install/air-gapped.md) | Offline / restricted-egress install runbook with private registry mirroring |

## Tutorials

| Page | What it covers |
|---|---|
| [Your first controller](tutorials/first-controller.md) | Empty cluster → running Jenkins in three steps, no git repo or domain required |
| [Authoring a bundle](tutorials/custom-bundle.md) | Replace the built-in starter bundle with configuration you own |
| [Using the varroactl CLI](tutorials/varroactl-cli.md) | Build the CLI, log in, and manage controllers from the terminal |

## Configuration

| Page | What it covers |
|---|---|
| [Bundle sources](config/bundle-sources.md) | CloudBees-style git bundle repos: layout, manifest, private repos, caching |
| [CasC catalog](config/casc-catalog.md) | Publishing and consuming reusable, parameterized config snippets |
| [Composed bundles](config/composed-bundles.md) | Ordered input composition, variables, merge semantics, pausing |
| [Jobs & items](config/items.md) | Declaring jobs/folders/pipelines as YAML — the Job DSL alternative |
| [Jenkins versions](config/jenkins-versions.md) | Version profiles, plugin locks, channels, safe upgrades |
| [Plugin packs](config/plugin-packs.md) | OCI artifact format for distributing resolved plugin sets |
| [Plugin pinning](config/plugin-pinning.md) | Where plugin lists come from, drift, rolls, and approvals |
| [Pod customization](config/pod-customization.md) | Typed pod overrides, health probes, and raw resource overlays, with preview |

## Security

| Page | What it covers |
|---|---|
| [Authentication](security/authentication.md) | Local, direct OIDC, Dex-brokered, native LDAP — and controller SSO |
| [Varroa RBAC](security/varroa-rbac.md) | Control-plane roles, bindings, scopes, custom roles |
| [Jenkins RBAC](security/jenkins-rbac.md) | In-Jenkins permission sets, folder/pattern scopes, generation |
| [API keys](security/api-keys.md) | Programmatic access: create, rotate, revoke |
| [executeGroovy](security/execute-groovy.md) | Fleet-wide script execution: identity, who can invoke it, audit trail, how to disable it |

## Operations

| Page | What it covers |
|---|---|
| [Lifecycle](operations/lifecycle.md) | Stop/start, restarts, reprovision, deletion, backups |
| [Brood operations](operations/brood-operations.md) | Bulk controller operations: run, preview, cancel, suspend |
| [Brood schedules](operations/brood-schedules.md) | Scheduled, recurring brood operations with cron expressions |
| [Reconciliation](operations/reconciliation.md) | automatic/idle/manual modes, intervals, drains, approvals |
| [Rollout waves](operations/rollout-waves.md) | Canary-then-brood bundle delivery and emergency pause |
| [Multi-tenancy](operations/multi-tenancy.md) | Teams, tenant namespaces, isolation, deployable namespaces |
| [MCP and Jenkins controller tools](operations/mcp.md) | MCP endpoint, per-caller Jenkins identity, and RBAC requirements |
| [Observability](operations/observability.md) | Metrics, health endpoints, the activity feed, what to alert on |
| [Troubleshooting](operations/troubleshooting.md) | Symptom-indexed runbook |
| [Update center](operations/update-center.md) | In-cluster plugin metadata and HPI serving, storage backends, pull-through caching, seed import, gap reporting, cross-cluster (hive) access |

## API & CLI

| Page | What it covers |
|---|---|
| [API Reference](api-reference.md) | REST API overview, authentication, conventions, interactive docs, generated Go client |
| [varroactl CLI](varroactl.md) | Command-line client: login flows, contexts, full command surface, streaming, MCP bridge, plugin-pack export/import |

## What's next

The [Roadmap](roadmap.md) covers direction — multi-cluster, air-gapped operation, OCI bundle transport, active/active scaling, hibernation — sourced from the public backlog.

---

Historical design artifacts live under [internal/](internal/README.md) and are not maintained.
