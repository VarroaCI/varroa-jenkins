# AGENTS.md — hack/

## Purpose

Dev-tooling scripts outside the normal `go build`/`go test` path:
version-profile plugin-lock generation, the local kind dev stack, a mock OIDC
provider for tests, and the OpenAPI bundler invoked by `make generate-client`.

## Ownership

Standalone scripts and small Go `main` packages, each independent.

## Local Contracts

- `build-plugin-pack.sh` — builds the per-profile seed plugin-pack OCI artifacts
  (via `varroactl export plugins`) published by the release workflow to
  `ghcr.io/varroaci/varroa-jenkins/plugin-pack:<profileName>`. Same lock inputs as
  `gen-plugin-lock.sh`, so packs can never skew from the shipped version profiles.
- `localdev/pluginpack-fixture/` — small checked-in OCI-layout pack (4 plugins)
  used by `LOCALDEV_OFFLINE=1` localdev seeding; structurally validated by a Go
  check in the localdev CI job. `LOCALDEV_OFFLINE=1` implies
  `LOCALDEV_SKIP_CONTROLLER=1` (the fixture can't satisfy a real profile closure);
  `seed_updatecenter` in `localdev.sh` otherwise seeds from the published ghcr
  packs and imports via `varroactl import --to uc://` (`VARROACTL_UC_TOKEN`).
- `gen-plugin-lock.sh` — regenerates `internal/controller/pluginlock/lock.yaml`
  and the Helm `JenkinsVersionProfile` templates under
  `charts/varroa/templates/version-profiles/`. Reads `version-profiles.yaml`
  as source of truth; runs `jenkins-plugin-cli` in
  `jenkins/jenkins:<resolveVersion>` per profile. Requires `yq` v4, `docker`,
  network to `updates.jenkins.io`. Uses `--list`, not `--available-updates`.
  Supports `--validate-only`. **Not run during reconciliation** — lockfile
  changes are reviewed source changes. Lock `baseline` must equal the
  version controllers actually run, or `plugins-init` aborts.
  Also builds `plugin/target/varroa-mite-auth.hpi` when absent
  (`maven:3.9-eclipse-temurin-21`) and runs `go run ./cmd/bootstrapdeps
  --resolve` per profile, writing that set's `bootstrap:` block. A missing
  mandatory dependency aborts the run, so `go` is a prerequisite alongside
  `yq` and `docker`. The whole lock is staged in a temp file and moved into
  place only on success — never append to `$OUTFILE` mid-loop, or a failed
  assertion leaves a truncated lock behind. Pass the core seed set to
  `jenkins-plugin-cli` as `--plugins` **arguments**, never as a bind-mounted
  `--plugin-file`: the jenkins image runs as uid 1000, `mktemp` yields a 0600
  file owned by the invoking user, and plugin-cli treats an unreadable
  `--plugin-file` as an EMPTY plugin list while still exiting 0 — so it passed
  on uid-1000 dev machines and silently resolved zero plugins on every CI
  runner (uid 1001).
- `version-profiles.yaml` — reviewed manifest: one entry per supported
  version/line (`version`, `channel` lts/weekly, optional
  `recommended`/`eol`/`resolveVersion`). 2-segment `version` = LTS line;
  3-segment = exact pin. `resolveVersion` required for 2-segment LTS lines,
  must be earliest patch clearing both OSS plugins' required-core and the
  in-house `varroa-mite-auth` plugin's `pom.xml` floor. `weekly` entry must
  equal `lock.yaml`'s `baseline`.
- `localdev/` — everything `make localdev` needs: `localdev.sh` (phases
  up/images/controller/down; idempotent; `LOCALDEV_SKIP_CONTROLLER=1` skips
  sample workload), `kind-config.yaml`, `git-server/` (in-cluster smart-HTTP
  git server backing the sample `ComposedBundle`; falls back to a public
  CasC bundle repo's `bundle-test` path if it
  misbehaves), `bundle/` (sample bundle files), `manifests/` (admin
  user/rolebinding, git-server, sample `Controller`). Walkthrough:
  `docs/install/local-development.md`.
- `mock-oidc/` — standalone Go module test OIDC provider: ed25519-signed ID
  tokens, in-memory auth-code store, static test users. For CI/local
  auth-flow tests, not a real IdP.
- `openapi-bundle/` — `main.go`, run via `go run ./hack/openapi-bundle` by
  `make generate-client`. Loads `varroa.root.yaml`, internalizes `$ref`s,
  validates, writes canonical `varroa.json`/`varroa.yaml` (via
  `sigs.k8s.io/yaml.JSONToYAML`, deliberately not a raw Go-map marshal). See
  `api/AGENTS.md` for the full contract.
- `update-bundle.sh` — pushes a local bundle edit to the git repo named by
  `BUNDLE_REPO` and patches a live `Controller`'s
  `spec.bundleRef.revision`, polling `status.configHash` for convergence.
  Usage: `BUNDLE_REPO=git@github.com:your-org/your-casc-bundle-repo.git
  ./hack/update-bundle.sh <controller-name> <bundle-dir>`.
- `integration-values.yaml` — Helm values for CI integration-test install:
  Dex with static-password `local` connector (no group claims — group-based
  RBAC can't be exercised via it), everything else disabled.
- `export-public.sh` — assembles a leak-gated public snapshot of this repo
  (tracked files minus `export-public.excludes` prefixes, `public-overlay/`
  applied on top, `ghcr.io/varroaci` rewritten to `ghcr.io/varroaci`) for the
  VarroaCI/varroa-jenkins public fork, then runs a fail-closed leak gate
  against the assembled tree: blocklist terms in file **content** (`-a`, so
  binary files are scanned too, not skipped) and in file/dir **path names**,
  secret-pattern regexes (also `-a`), any **symlink** found anywhere in the
  tree (policy: fail on all of them, unconditionally, with no allowlist path
  at all — a tracked symlink's target is invisible to every other check, so
  a symlink can never be judged safe from its name alone; assembly also uses
  `cp -P` so a symlink is preserved as a symlink rather than silently
  dereferenced into the target's content), and >10MB files.
  `./hack/export-public.sh [DEST]` assembles + gates;
  `./hack/export-public.sh --gate-only DIR` runs only the gate against an
  existing directory (used to test the gate in isolation). Excludes in
  `export-public.excludes` are path **prefixes** (trailing `/` for
  directories), matched against the full tracked path — not substrings.
  `export-public.allowlist` holds category-prefixed (`content:`/`path:`)
  entries, each an **exact whole-line match** (`grep -x -F`, not a
  substring) of one specific hit line the gate actually emits, filtered only
  against hits from that same scan stream — a content-scan entry can never
  suppress a path-name-scan hit or vice versa, and (per above) there is no
  `symlink:` category at all. Content-scan hit lines are normalized to
  `content:<relpath>:<line>:<content>` and path-name-scan hits to
  `path:<stream-line-number>:<relative-path>` before matching; personally
  reviewed and judged safe to ship, keep it minimal and add entries only
  after reading the actual hit from a real gate run. **Fail-closed is
  load-bearing**: every gate check's tool exit status is captured explicitly
  (`set +e` / `set -e`, never `|| true`) and anything other than "ran and
  found nothing" aborts the whole script (missing
  `rg`/`grep`/`find`/`realpath` on `PATH`, a missing — not just empty —
  `export-public.excludes`/`export-public.allowlist`, or a scan command
  erroring out all hard-fail before the gate can report "clean"). Before the
  assembly path's `rm -rf "$DEST"`, `DEST` is canonicalized with
  `realpath -m` (alongside `$HOME` and the repo root) and refused with exit 2
  if the resolved path is `/`, or is equal to or an ancestor of `$HOME`, the
  repo root, or `/` — closes `..`-laden and symlink-laden `DEST` spellings
  that a literal-string comparison would miss — plus a refusal if an
  existing `DEST` contains a `.git` directory. `public-overlay/` holds files
  that fully replace their
  snapshot counterpart in the assembled tree (public-facing
  `AGENTS.md`/`CLAUDE.md`, CI workflows, `SECURITY.md`, `CONTRIBUTING.md`, …).
  `export-public.sh`, `export-public.excludes`, `export-public.allowlist`,
  and `public-overlay/` itself are all listed in `export-public.excludes` —
  this export-assembly tooling is private-repo-only and never ships in the
  public tree it produces.

## Work Guidance

- Every generated artifact here (`lock.yaml`, chart version-profile
  templates, `varroa.json`/`varroa.yaml`) is derived — edit the source
  manifest/spec and re-run the script.
- `version-profiles.yaml` edits land via reviewed PR; re-run
  `gen-plugin-lock.sh` in the same change.

## Verification

- `hack/gen-plugin-lock.sh --validate-only`
- `make localdev`
