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
absolute HTTPS origin except on supported local development hosts. After sign
in, inspect `/api/v1/me` and verify the username and groups used by role
bindings.

## Use Dex as an OIDC broker

Enable Dex when an upstream provider needs a Dex connector. Configure Varroa as
a Dex static client and set the upstream callback to the Dex callback URL.

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

Add the required OIDC, LDAP, SAML, or OAuth connector under `dex.config`.

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

Store bind credentials in a Secret. Prefer LDAPS or verified StartTLS-capable
infrastructure, and confirm group resolution through `/api/v1/me`.

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

Create users through the dashboard or users API. Use an external identity
provider for normal multi-user deployments.

## Configure controller SSO safely

The `varroa_token` browser cookie is shared with controller hosts through
`auth.cookieDomain`. Choose the narrowest parent domain containing only Varroa
and managed Jenkins hosts. Use TLS on every host and do not host untrusted
applications under that domain.

All BFF replicas must share the OIDC state signing secret. Helm creates and
preserves it unless `auth.oidc.stateSecret` is supplied.

Controllers need network access to the configured issuer discovery and JWKS
endpoints for initial and signing-key validation.

## Troubleshoot

| Symptom | Check |
|---|---|
| Login loop | Redirect URL, dashboard URL, TLS, and cookie domain |
| Jenkins is anonymous | Cookie coverage and TLS |
| Group binding fails | Resolved claims or LDAP groups in `/api/v1/me` |
| One replica fails callback | Shared state secret |
