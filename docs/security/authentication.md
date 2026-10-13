# Configure authentication

Set `auth.mode` in Helm values.

| Mode | Identity source | Groups |
|---|---|---|
| `oidc` | OIDC issuer, directly or through Dex | Configured token claim |
| `ldap` | LDAP directory | LDAP group search |
| `local` | Varroa `User` resources | Varroa groups |

Configure authorization with [Varroa RBAC](varroa-rbac.md).

## Use direct OIDC

```yaml
auth:
  mode: oidc
  cookieDomain: .varroa.example.com
  dashboardUrl: https://app.varroa.example.com
  oidc:
    issuer: https://login.example.com/
    clientId: varroa-dashboard
    clientSecret: <client-secret>
    redirectUrl: https://app.varroa.example.com/api/v1/callback
    scopes: openid,profile,email,groups
    userClaim: preferred_username,sub
    groupClaim: groups
dex:
  enabled: false
```

Register the exact redirect URL with the provider. `dashboardUrl` must be an
absolute HTTPS origin. Check the username and groups in `/api/v1/me`.

## Map token claims to users and groups

`oidc.userClaim` and `oidc.groupClaim` name the ID token claims that Varroa reads.
Each setting takes a comma-separated list of claim names.

- Varroa reads each named claim from the ID token. Names match literally.
- A group claim holds a JSON array of strings or one string. Each value is one group name.
- Varroa does not split a string. A value with a comma or a space stays one group name.
- Other claim shapes are ignored.
- Nested claims are not read. `realm_access.roles` does not work. In Keycloak, add a
  protocol mapper that writes the roles to a top-level claim, and name that claim.
- A user claim can be any top-level string claim.
- Keep `preferred_username` or `sub` in `userClaim`. Other credentials do not carry
  custom claims. Each credential type sets these fields:
  - Local and LDAP session tokens set `sub`, `preferred_username`, `email`, `name`,
    and `groups`.
  - An API key sets the stored `subject`, `preferred_username`, `email`, `name`, and
    the owner's groups.
  - A schedule credential sets all four identity fields to the schedule subject. It
    carries no groups.
- A token whose subject, standard user claim, or configured user claim starts with
  `local:` is refused. The check ignores surrounding space and letter case. The
  prefix is reserved for local accounts.
- Group values from an IdP token are not checked for the prefix. Group subjects use
  a separate key kind, so a group value cannot match a user binding.
- The same claim names apply to the dashboard and to every Jenkins controller.

For AWS Cognito groups, set the group claim to the Cognito claim name:

```yaml
auth:
  oidc:
    userClaim: preferred_username,sub
    groupClaim: cognito:groups
```

For a provider that writes groups to a `roles` claim:

```yaml
auth:
  oidc:
    userClaim: preferred_username,sub
    groupClaim: roles
```

When a user signs in with no groups, the BFF logs
`user has zero groups in ID token`. Check the group claim name when you see this message.

## Use Dex as an OIDC broker

Use Dex when a provider needs a Dex connector.

```yaml
auth:
  mode: oidc
  cookieDomain: .varroa.example.com
  dashboardUrl: https://app.varroa.example.com
  oidc:
    clientId: varroa
    clientSecret: <shared-client-secret>
    redirectUrl: https://app.varroa.example.com/api/v1/callback
dex:
  enabled: true
  config:
    issuer: https://dex.varroa.example.com
    staticClients:
      - id: varroa
        secret: <shared-client-secret>
        redirectURIs:
          - https://app.varroa.example.com/api/v1/callback
```

Add the connector under `dex.config`.

### Connect a Forgejo or Gitea host

Use the Dex `gitea` connector for a Forgejo or Gitea host.
Create an OAuth2 application on the forge.
Set its redirect URI to the Dex callback, `<dex issuer>/callback`.

```yaml
dex:
  config:
    connectors:
      - type: gitea
        id: forgejo
        name: Forgejo
        config:
          baseURL: https://forge.example.com
          clientID: <forge-oauth-client-id>
          clientSecret: <forge-oauth-client-secret>
          redirectURI: https://dex.varroa.example.com/callback
          loadAllGroups: true
          useLoginAsID: true
```

Dex sends one group for each organization, `<org>`, and one for each team, `<org>:<team>`.
A user on no team of an organization gets no group for it.
A `Team` or a `VarroaRoleBinding` can bind either string, for example `acme` or `acme:Owners`.
With `useLoginAsID`, the forge login is the user identity.
If the forge does not listen on port 443, add its port to `networkPolicy.dexConnectorEgress.ports`.

## Use LDAP

```yaml
auth:
  mode: ldap
  bffUrl: "http://<release>-varroa-bff.<namespace>.svc.cluster.local:8080"
  cookieDomain: .varroa.example.com
  dashboardUrl: https://app.varroa.example.com
dex:
  enabled: false
```

The BFF signs LDAP sessions, so the chart requires `auth.bffUrl`.
Controllers verify the sessions with the BFF keys at that URL.

Set these BFF environment variables:

| Setting | Variable |
|---|---|
| Server | `VARROA_LDAP_URL` |
| Direct bind | `VARROA_LDAP_BIND_DN_TEMPLATE` |
| User search base | `VARROA_LDAP_USER_SEARCH_BASE` |
| User search filter | `VARROA_LDAP_USER_SEARCH_FILTER`, default `(uid=%s)` |
| Email attribute | `VARROA_LDAP_USER_EMAIL_ATTR`, default `mail` |
| Display name | `VARROA_LDAP_USER_NAME_ATTR`, default `cn` |
| Service account DN | `VARROA_LDAP_SERVICE_ACCOUNT_DN` |
| Service account password | `VARROA_LDAP_SERVICE_ACCOUNT_PASSWORD` |

| Group setting | Variable |
|---|---|
| Group search base | `VARROA_LDAP_GROUP_SEARCH_BASE` |
| Group search filter | `VARROA_LDAP_GROUP_SEARCH_FILTER`, default `(member=%s)` |
| Group name attribute | `VARROA_LDAP_GROUP_NAME_ATTR`, default `cn` |
| CA bundle path | `VARROA_LDAP_CA_CERT_FILE` |

Store bind credentials in a Secret and prefer LDAPS.

## Use local authentication

```yaml
auth:
  mode: local
  bffUrl: "http://<release>-varroa-bff.<namespace>.svc.cluster.local:8080"
  cookieDomain: .varroa.example.com
  dashboardUrl: https://app.varroa.example.com
dex:
  enabled: false
```

The chart requires `auth.bffUrl` in this mode.
Create users through the dashboard or users API.

## Add local accounts beside OIDC

Keep `auth.mode: oidc` and declare the accounts:

```yaml
auth:
  mode: oidc
  bffUrl: "http://<release>-varroa-bff.<namespace>.svc.cluster.local:8080"
  localAccounts:
    enabled: true
---
apiVersion: varroa.dev/v1alpha1
kind: User
metadata:
  name: rescue
  namespace: varroa-system
  labels:
    varroa.dev/managed-by: local
spec:
  purpose: break-glass   # or service
  password: <initial-password>
```

An account needs `purpose` and signs in as `local:<name>`. The profile page shows such a session as `via local account`.
Each sign-in writes an `auth.local.login` event. A failed attempt on a declared
account writes `auth.local.login_failed`. An unknown name writes no event.

## Configure controller SSO safely

The `varroa_token` browser cookie is shared with controller hosts through
`auth.cookieDomain`. Choose the narrowest parent domain containing only Varroa
and managed Jenkins hosts. Use TLS on every host. Host no untrusted applications
under that domain.

Helm creates and keeps the shared OIDC state secret unless
`auth.oidc.stateSecret` is set.

Controllers must reach the discovery and JWKS endpoints of each issuer.
A member cluster has no BFF, so set its `auth.bffUrl` to the public dashboard origin.

## Troubleshoot

| Symptom | Check |
|---|---|
| Login loop | Redirect URL, dashboard URL, TLS, and cookie domain |
| Jenkins is anonymous | Cookie coverage and TLS |
| Group binding fails | Resolved claims or LDAP groups in `/api/v1/me` |
| One replica fails callback | Shared state secret |
