# AGENTS.md — bundles

## Purpose

JCasC content Varroa ships inside its own binary. Today that is exactly one bundle:
`starter/`, the configuration a `Controller` receives when it names no
`composedBundleRef`.

Embedded rather than fetched, so a first run needs no git remote, no OCI registry, and
no network — an air-gapped install provisions the same Jenkins as a connected one.

## Ownership

- `embed.go` — the `go:embed` accessors (`StarterJCasC`, `StarterItems`). Files must live
  under this directory; `go:embed` cannot reach outside its own package.
- `starter/*.yaml` — the content itself.
- The seeding reconciler that turns this content into `CatalogItem` + `ComposedBundle`
  objects lives in [internal/controller](../internal/controller/AGENTS.md)
  (`starter.go`), not here.

## Local Contracts

Content added here must satisfy all of the following, because it is applied to every
zero-config controller with no author present to fix it:

- **No `${...}` placeholders except those the operator injects unconditionally**
  (`varroa_controller_name`, `varroa_controller_namespace`, `varroa_controller_endpoint`,
  `varroa_frontend_url`, `varroa_controller_external_url`, `varroa_controller_path_prefix`).
  The OIDC and login-URL variables are conditional on a configured Resolver and must not
  be used. **The unresolved-variable scan does not skip comments** — a dollar-brace
  placeholder written in a comment fails provisioning exactly as a real one would.
- **No `plugins:` block.** The plugin set comes from the resolved `JenkinsVersionProfile`
  lock. A pinned version here drifts against that lock and wedges controllers in
  Provisioning.
- **No `securityRealm`** (needs a client-secret variable Varroa does not inject) and no
  **`authorizationStrategy`** (Varroa owns it and strips the key during composition).
- **One file per catalog item type.** The composer routes an item's content by
  `spec.type`, so JCasC and items cannot share a file. Adding a third type means a third
  `CatalogItem` in `starter.go` and a third input on the seeded `ComposedBundle`.
- **Every image must be listed in `approvedStarterImages`** (`internal/controller/starter_test.go`).
  A moving tag lets the image change independently of the pinned core and plugin lock,
  and an air-gapped cluster can only mirror an image it can name — but immutability
  cannot be inferred from a tag string, so the list is explicit rather than a shape
  check. Changing an image means editing that list, and whoever edits it is asserting
  the ref is immutable and mirrorable. Prefer an `@sha256` digest where upstream
  publishes one. Refresh alongside the plugin lock, and keep
  `docs/install/air-gapped.md` in step — it names the image operators must mirror.
- **Sample items must succeed on the built-in executor.** `agent any`, not
  `agent { kubernetes { ... } }`. A first-run sample that fails when the cloud is
  misconfigured turns one problem into two. It follows that the sample proves the
  configuration pipeline, not agent provisioning — do not document it as the latter.

## Verification

Seeded-object metadata must satisfy Kubernetes' own limits, which the in-memory
`crdstore.Fake` does not check. A sha256 content hash is 64 characters and label
VALUES cap at 63, so it lives in an **annotation**; a label there silently killed
the whole feature once. `TestStarterObjectLabelsAreValidForKubernetes` guards it.


`go test ./internal/controller/ -run TestStarter` covers all of the above:
`TestStarterContentHasNoVariablesOrPinnedPlugins` asserts the content constraints, and
`TestStarterSeededObjectsCompose` composes what the seeder writes and asserts it produces
non-empty JCasC and items.

## Child DOX Index

None.
