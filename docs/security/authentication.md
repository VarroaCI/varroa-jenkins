# Configure authentication

Set `auth.mode` in Helm values.

| Mode | Identity source | Groups |
|---|---|---|
| `oidc` | OIDC issuer, directly or through Dex | Configured token claim |
| `ldap` | LDAP directory | LDAP group search |
| `local` | Varroa `User` resources | Varroa groups |

Authorization is configured separately with [Varroa RBAC](varroa-rbac.md).

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

## Use Dex as an OIDC broker

Use Dex when an upstream provider needs a Dex connector.

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

## Use LDAP

```yaml
auth:
  mode: ldap
  cookieDomain: .varroa.example.com
  dashboardUrl: https://app.varroa.example.com
dex:
  enabled: false
```

Configure the BFF with these environment variables:

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

An account needs `purpose` and signs in as `local:<name>` from the login
page. Each sign-in
writes an `auth.local.login` activity event. A failed attempt on a declared
account writes `auth.local.login_failed`. An unknown name writes no event.

## Configure controller SSO safely

The `varroa_token` browser cookie is shared with controller hosts through
`auth.cookieDomain`. Choose the narrowest parent domain containing only Varroa
and managed Jenkins hosts. Use TLS on every host and do not host untrusted
applications under that domain.

Helm creates and keeps the shared OIDC state secret unless
`auth.oidc.stateSecret` is set.

Controllers need network access to the discovery and JWKS endpoints of each
issuer.

## Troubleshoot

| Symptom | Check |
|---|---|
| Login loop | Redirect URL, dashboard URL, TLS, and cookie domain |
| Jenkins is anonymous | Cookie coverage and TLS |
| Group binding fails | Resolved claims or LDAP groups in `/api/v1/me` |
| One replica fails callback | Shared state secret |
