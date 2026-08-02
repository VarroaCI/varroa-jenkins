# AGENTS.md — docs/

## Purpose

The user-facing Varroa Operator Handbook. `docs/README.md` is the table of
contents; every top-level page is linked from it under Architecture /
Install / Tutorials / Configuration / Security / Operations / API & CLI.

## Ownership

User-facing guides under the subdirs below, plus top-level pages
(`api-reference.md`, `varroactl.md`, `roadmap.md`). `docs/internal/` is a
separate, explicitly unmaintained archive.

## Local Contracts

**Docs ship with features (root rule):** every feature change updates the
relevant page(s) here in the same change/PR. Not done while a page still
describes the old behavior.

Subdir scope (per `docs/README.md`):

- `architecture/` — components/CRDs/phase lifecycle/terminology
  (`overview.md`); mite agent handshake/stream/auth/drains (`mite.md`); what
  scales how (`scaling.md`).
- `install/` — prerequisites, `make localdev`, Helm chart walkthrough,
  ingress modes, opt-in NetworkPolicy set, air-gapped install runbook
  (`air-gapped.md`, the issue-#194 deny-all-egress procedure).
- `tutorials/` — empty cluster to converged controller; `varroactl` CLI.
- `config/` — bundle sources, CasC catalog, composed bundles (ordering/
  variables/merge), items (jobs/folders/pipelines as YAML), version
  profiles, plugin pinning/drift/rolls, plugin packs
  (`plugin-packs.md`), pod customization + preview.
- `security/` — auth modes (local/OIDC/Dex/LDAP + controller SSO), Varroa
  RBAC, Jenkins RBAC, API key lifecycle.
- `operations/` — lifecycle, brood operations, brood schedules,
  reconciliation modes, rollout waves, multi-tenancy, observability,
  the update center (`update-center.md`), symptom-indexed troubleshooting.
- `internal/` — **historical, not maintained, not operator documentation.**
  Design/review/planning artifacts, several describing superseded
  mechanisms; own `superpowers/plans`+`superpowers/specs` (2026-05–07) and a
  `template-catalog/` plan. Never treat as current behavior. New artifacts
  get an entry in `docs/internal/README.md`.
- `superpowers/` — top-level (not `internal/`) planning artifacts from the
  `superpowers` skill workflow (currently the 2026-07-11 fleet-operations
  plan+spec). Same "not a guide" caveat; not yet folded into `internal/`.

## Work Guidance

- New/changed operator-facing behavior: update the specific page under the
  subdirs above, not `docs/internal/`. New page → link it from
  `docs/README.md`'s matching table.
- Design/planning write-ups made while building a feature go under
  `docs/internal/` and get listed in its `README.md`, not mixed into guides.
- Keep `docs/README.md`'s tables in sync — it's the only index.

## Verification

None (no doc linter/build here; verify by reading the rendered Markdown and
confirming `docs/README.md` links resolve).
