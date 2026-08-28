# Manage Jenkins jobs and folders

Declare jobs and folders in an `items.yaml` file referenced by a
[bundle source](bundle-sources.md).

## Choose a kind

| Kind | Required content |
|---|---|
| `folder` | `name`; may contain nested `items` |
| `freeStyle` | `name` |
| `pipeline` | `name` and inline or SCM `definition` |
| `multibranch` | `name` and at least one source |
| `organizationFolder` | `name` and at least one navigator |

Unknown kinds reject the bundle. Unknown fields are ignored, so validate field
names before rollout.

## Define a pipeline

```yaml
removeStrategy:
  items: none
  rbac: sync
items:
  - kind: folder
    name: platform
    items:
      - kind: pipeline
        name: deploy
        properties:
          - buildDiscarder:
              strategy:
                logRotator:
                  numToKeep: 30
        definition:
          cpsScmFlowDefinition:
            scriptPath: Jenkinsfile
            lightweight: true
            scm:
              gitSCM:
                userRemoteConfigs:
                  - userRemoteConfig:
                      url: https://github.com/example/service.git
                      credentialsId: github-app
                branches:
                  - branchSpec:
                      name: "*/main"
```

An inline definition uses `definition.script`:

```yaml
- kind: pipeline
  name: smoke
  definition:
    script: |
      pipeline {
        agent any
        stages {
          stage('test') { steps { echo 'ok' } }
        }
      }
```

Varroa creates missing items, updates changed items, and skips unchanged items.

## Control removal

| `removeStrategy.items` | Undeclared items |
|---|---|
| `none` | Keep all; formerly managed items stop converging |
| `sync` | Remove undeclared managed items |
| `remove-supported` | Remove undeclared managed items of supported kinds |
| `remove-all` | Remove undeclared items, including unmanaged items |

Renaming creates the new path and treats the old path as a removal. Deletion
also removes build history.

Unsafe removals appear in `status.pendingItemDeletions` and require the
controller `approve-deletion` capability. A running job or unreadable build
state can defer deletion.

## Troubleshoot

| Symptom | Check |
|---|---|
| Unknown kind | Use one of the five supported values |
| Setting is absent | Field spelling and indentation |
| Deletion is pending | Pending path, build state, and approval |
| Kind is unsupported | Keep it unmanaged |
