# Tenant Shared Libraries

A tenant folder can load shared Pipeline libraries. Read [Cells and pipelines tenants](cells.md) first.

## Trust

A folder library runs in the Groovy sandbox. The sandbox limits the methods that the library code can call. A tenant declares a folder library. The operator never declares it.

A trusted library runs outside the sandbox. The operator declares it on the cell pool. See [Pool shared libraries](cell-libraries.md).

## Worked example

This example loads a private GitLab library into the tenant `payments`. The library tag is `v1.0.0`.

1. Create a `usernamePassword` Secret in the tenant namespace `payments-ns`. The annotation `varroa.dev/credential-id` names the credential that the library uses.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: gitlab-lib
  namespace: payments-ns
  labels:
    varroa.dev/tenant-credential: "true"
  annotations:
    varroa.dev/credential-id: gitlab-lib
    varroa.dev/credential-kind: usernamePassword
type: Opaque
stringData:
  username: ci-bot
  password: replace-me
```

2. Declare the library on the folder `apps` in the tenant `items.yaml`. The library must sit on a folder that the tenant declares. It must not sit on the tenant root folder. Put the pipeline job inside `apps`.

```yaml
items:
  - kind: folder
    name: apps
    properties:
      - folderLibraries:
          libraries:
            - libraryConfiguration:
                name: platform-lib
                defaultVersion: v1.0.0
                retriever:
                  modernSCM:
                    scm:
                      git:
                        remote: https://gitlab.example.com/platform/jenkins-lib.git
                        credentialsId: gitlab-lib
    items:
      - kind: pipeline
        name: build
        # the job definition goes here
```

3. The Jenkinsfile of the job loads the library at the tag:

```groovy
@Library('platform-lib@v1.0.0') _
```

## Retriever sources

Each library names one retriever. Set exactly one of `modernSCM` and `legacySCM`.

| Retriever | Credential kinds | Notes |
| --- | --- | --- |
| `modernSCM.scm.github` | `usernamePassword` or `gitHubApp` | Needs `repoOwner` and `repository`, or `repositoryUrl` with `configuredByUrl`. |
| `modernSCM.scm.git` | `usernamePassword`, `sshPrivateKey`, or `gitHubApp` | Needs `remote`. The library loads every branch and tag. |
| `legacySCM.scm.gitSCM` | `usernamePassword`, `sshPrivateKey`, or `gitHubApp` | Set `branches` to a fixed name. A `${...}` variable reference fails the check. |

A library needs `defaultVersion`, `allowVersionOverride: true`, or both. Without one of them, no `@Library` line can load it.

## Held folders

A tenant with one of these faults is held. Its folder is not loaded. The `Team` reports `TenantWarnings=True` with reason `ItemsHeld`, and the message gives the reason:

- The retriever sets no source, sets two sources, or misses a required field.
- A credential ID is not provided by a tenant Secret or by a folder credential that the library can see.
- A credential has a kind that the library source does not accept.
- A plugin is missing from the resolved plugin set: `pipeline-groovy-lib`, `git`, or `github-branch-source`.
