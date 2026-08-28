# plugin AGENTS.md

## Purpose

`VarroaSecurityRealm` — a Jenkins plugin (Maven/HPI, artifact `varroa-mite-auth`)
providing CJOC-style federated authentication for every Jenkins controller Varroa
provisions. Built into the Varroa images and delivered to each Jenkins pod via init
container (not installed from the update center). Also carries the activity-feed
emitters and the dashboard-identity banner.

## Ownership

Owns everything under `plugin/`: the `org.jenkinsci.plugins.varroamiteauth` package,
its Jelly/webapp resources, and its unit tests. Contract changes (env vars, JCasC symbol
shape, HTTP endpoint paths) must be coordinated with external consumers — the
`internal/mite/jenkinstoken.go` signer and `internal/bundle/` — in the same change, per
the root AGENTS.md greenfield rule.

## Local Contracts

- **Auth — two credential types**, both handled in `VarroaSecurityRealm.java`:
  1. `varroa_token` cookie (OIDC ID token from the BFF login) — `JWTValidator.java`
     validates it **online** against the issuer's JWKS.
  2. `Authorization: Bearer` header: `vk_*` API keys go to `ApiKeyValidator.java`, which
     calls the gateway verify endpoint (`VARROA_APIKEY_VERIFY_URL`/`VARROA_CA_PEM`,
     60s/10s pos/neg cache TTL, errors never cached). Non-`vk_` Bearer is the mite
     operator JWT — `JWTValidator.validateOperatorToken()` verifies RS256 **fully
     offline** via `VARROA_MITE_PUBKEY_PEM`, no Dex/network call — the counterpart to
     `MiteTokenSigner` in `internal/mite/jenkinstoken.go`. Operator JWTs without
     `varroa_typ` retain the system identities; `varroa_typ="user"` maps to the caller
     using its literal `groups` claim and never grants `ROLE:varroa:system-*`.
  The direct BOM-managed `mailer` plugin dependency supplies the reflective
  `Mailer.UserProperty` used when persisting non-empty user email claims.
  `VarroaCrumbExclusion.java` exempts both Bearer types from CSRF crumb checks.
  JCasC symbol `varroaMiteAuth` (fields `oidcIssuer`/`userClaimNames`/`groupClaimName`,
  falling back to `VARROA_OIDC_ISSUER`/`_USER_CLAIM`/`_GROUP_CLAIM`); round-trip fixtures
  `src/test/resources/.../realm-{minimal,explicit}.yml`.
- **Activity emitters** (feed the push-based Activity page): `ActivitySink.java`
  (bounded, non-blocking, in-memory), `ActivityEvent.java` (normalized shape — actor,
  itemPath, buildNumber, result, url, message; `controller`/`namespace`/`phase`/`reason`
  always `""`, stamped downstream), `ActivityItemListener.java`
  (`item.created/updated/deleted/moved`), `ActivityRunListener.java` (`build.*`),
  `ActivitySaveableListener.java` (`config.changed`, **opt-in**,
  `VARROA_ACTIVITY_SAVEABLE=1`), `HttpActivityFilter.java` (last-HTTP-activity timestamp
  for hibernation idle gauges). `ActivityEndpoint.java` — `RootAction` at
  `/varroa-activity/events` (GET, requires `ROLE:varroa:system-mite`), drains
  `ActivitySink`, returns events + `idle` gauges.
- **Banner/identity** — `VarroaBannerDecorator.java` (`PageDecorator`; user id/name/email,
  `VARROA_BANNER_URL`), rendered by
  `resources/.../VarroaBannerDecorator/header.jelly` + `webapp/banner.js` (feature-detects
  the sticky redesigned Jenkins header, installs at z-index 1000). `resources/index.jelly`
  is the Manage Jenkins plugin description.
- **Build** — `pom.xml`: parent `plugin:5.31`, `jenkins.version` `2.555.3` (Java-21-only
  LTS), packaging `hpi`. `hpi-plugin.version` pinned to `3.1814.v77d15159f9b_d` (parent's
  default can't classify Java-21 class-file version 65 — see `pom.xml:20-30`). **Build
  JDK must be 21.** `dependencyManagement` imports `bom-2.555.x` to align JCasC + its
  test harness with the core baseline.

## Work Guidance

- Changes to `JWTValidator.validateOperatorToken`/`VARROA_MITE_PUBKEY_PEM` must stay in
  lockstep with `internal/mite/jenkinstoken.go` (`MiteTokenSigner`) — same algorithm,
  same claims.
- Changes to `ActivityEvent`'s JSON shape or the `/varroa-activity/events` response need
  a matching update to whatever parses it downstream, in the same change.
- Keep `ActivitySaveableListener` opt-in — it fires on every item config save.

## Verification

```bash
mvn -f plugin/pom.xml -B package   # requires JDK 21; runs unit + JCasC round-trip tests
```
