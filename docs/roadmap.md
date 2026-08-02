# Roadmap

<!-- sources: GitHub backlog issues #65 #194 #72 #129 #110 #137 #131 (verified open 2026-07-04), CHANGELOG.md -->

> [!NOTE]
> This page is sourced from the project's public backlog. It is a statement of direction, **not a commitment**, and the ordering is not a schedule. Feedback and use-case reports on the linked issues actively shape priorities.

## What's next

### Multi-cluster controller management — [#65](https://github.com/varroaci/varroa-jenkins/issues/65)

**Problem:** one Varroa installation manages controllers in its own cluster; organizations with regional or per-BU clusters run one control plane each, with no brood-wide view.
**Direction:** extend the control plane to manage controllers across clusters — the mite's [gateway/NATS transport](architecture/mite.md) already decouples controllers from the operator's cluster; the work is credential/network reach, per-cluster provisioning, and dashboard federation.
**Status:** Idea.

### Air-gapped operation (operator-served plugin CAS) — [#194](https://github.com/varroaci/varroa-jenkins/issues/194)

**Problem:** plugin installation reaches the public Jenkins update centers; fully disconnected environments must mirror them by hand today (`ProvisioningDefaults` UC-mirror URLs cover part of this).
**Direction:** end-to-end air-gap support — an operator/BFF-served content-addressed store for plugin binaries, so controllers install from the control plane with no external egress.
**Status:** Accepted, help wanted.

### OCI artifact bundle transport — [#72](https://github.com/varroaci/varroa-jenkins/issues/72)

**Problem:** [bundle sources](config/bundle-sources.md) are git-only; some organizations want configuration promoted and signed like images.
**Direction:** OCI registries as a bundle transport — push a bundle as an artifact, reference it by digest, reuse registry auth/signing/promotion machinery.
**Status:** Idea.

### Scaling primitives — [#129](https://github.com/varroaci/varroa-jenkins/issues/129)

**Problem:** the operator is active/passive today ([Scaling](architecture/scaling.md)); very large broods serialize reconciliation through one leader.
**Resolution:** lease-based shard ownership over the existing consistent-hash ring (active/active reconciliation), bounded worker pool, and a shared git cache.
**Status:** Done (sharding + worker pool in tree; git cache shipped separately).

### Hibernation — [#110](https://github.com/varroaci/varroa-jenkins/issues/110)

**Problem:** idle controllers burn resources; stopping them is manual [`powerState`](operations/lifecycle.md) today.
**Resolution:** automatic scale-to-zero on inactivity (build/queue/HTTP signals) with `spec.powerState: Hibernated`, plus wake-on-webhook (durably queued and replayed) and wake-on-click. See [Lifecycle → hibernate](operations/lifecycle.md#how-to-hibernate-idle-controllers).
**Status:** Done.

### Job cloning between controllers — [#137](https://github.com/varroaci/varroa-jenkins/issues/137)

**Problem:** moving a job between controllers means bundle surgery; teams migrating from shared monolithic controllers want one-click copies.
**Direction:** clone/copy of [declarative items](config/items.md) across controllers via the dashboard — re-declaring the item in the target's bundle rather than copying live Jenkins state.
**Status:** Idea, help wanted.

### Operational hardening remainder — [#131](https://github.com/varroaci/varroa-jenkins/issues/131)

**Problem:** remaining gaps from the hardening review: broader `ControllerSpec` validation, mite certificate revocation, and test-tooling separation (fakemite).
**Direction:** incremental — CRD CEL validation and short-TTL certs already shipped; revocation and the rest follow.
**Status:** Accepted, in progress.

## Recently shipped

For evaluators gauging velocity — landed in the last few release cycles:

- **[Multi-tenancy](operations/multi-tenancy.md)** — Teams, tenant namespaces, isolation.
- **[Jenkins version profiles](config/jenkins-versions.md)** — per-version plugin locks, channels, version-driven upgrades with a compatibility gate.
- **[Rollout waves](operations/rollout-waves.md)** — progressive bundle delivery with a bundle-level pause.
- **[Native LDAP authentication](security/authentication.md)** — direct BFF↔directory binding, no broker.
- **[Deterministic plugin install + roll approvals](config/plugin-pinning.md)** — checksummed plugin sync with `manual`-mode pending-roll approval.
- **[Activity persistence](operations/observability.md)** — JetStream-backed brood activity with a retention dial.

Full detail in [CHANGELOG.md](../CHANGELOG.md).
