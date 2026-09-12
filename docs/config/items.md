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

## Skip the initial branch-indexing build

Every `multibranch` job and every `organizationFolder` item carries the
`SkipInitialBuildOnFirstBranchIndexing` build strategy. A branch-indexing scan therefore creates
or updates a branch's job without starting a build. Start a branch's first build from the Varroa
dashboard, or push a commit to it.

A `JenkinsVersionProfile` that replaces the default plugin set must still pin
`basic-branch-build-strategies`. The operator refuses to assemble items for a manifest that
declares a `multibranch` or `organizationFolder` item when the resolved plugin set omits that
plugin.

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

A `pipeline` item can also name a catalog-delivered template instead of an inline or SCM
definition, with `definition.varroaTemplate`:

```yaml
- kind: pipeline
  name: deploy
  definition:
    varroaTemplate:
      template: go-service
      templateVersion: "2.1"
      values:
        goVersion: "1.24"
```

`cpsScmFlowDefinition`, `script`, and `varroaTemplate` are mutually exclusive: a `pipeline` item
sets exactly one. A `multibranch` item can use `projectFactory.varroaMarkerProjectFactory: {}`
instead of scanning branches for a `Jenkinsfile`; it selects a template through a marker file
committed to each branch instead. See [Pipeline templates](pipeline-templates.md) for the
template document, the marker file, and how a template's values are validated.

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

## Cell restrictions

A [cell](../operations/cells.md) merges every tenant's `items.yaml` into one manifest and checks
it against three rules before it applies. These rules apply only on a cell; an ordinary
controller accepts everything this page already describes.

| Rule | Rejects |
|---|---|
| No poll SCM | A `pollSCM` trigger, on an item or inside `pipelineTriggers`. Use a webhook instead. |
| No global credentials | A credential declared outside a tenant folder's own credentials property, or a folder credential whose scope is not `GLOBAL`. |
| No controller-config dependency | Any item property outside a fixed allow-list of folder- and job-scoped properties. |

A tenant manifest that fails any rule fails the whole cell's item assembly. The operator pushes
nothing and reports the failure.

## Troubleshoot

| Symptom | Check |
|---|---|
| Unknown kind | Use one of the five supported values |
| Setting is absent | Field spelling and indentation |
| Deletion is pending | Pending path, build state, and approval |
| Kind is unsupported | Keep it unmanaged |
