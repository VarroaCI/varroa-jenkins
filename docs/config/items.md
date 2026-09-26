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

Unknown kinds reject the bundle. Unknown fields are ignored.

## Skip the initial branch-indexing build

Every `multibranch` job and every `organizationFolder` item carries the
`SkipInitialBuildOnFirstBranchIndexing` build strategy. A branch-indexing scan creates or
updates a branch's job without starting a build. Start it from the dashboard, or push a
commit.

A `JenkinsVersionProfile` that replaces the default plugin set must still pin
`basic-branch-build-strategies`. The operator refuses a manifest declaring either kind
when the plugin set omits it.

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

A `pipeline` item can also name a catalog-delivered template with `definition.varroaTemplate`:

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
to select a template per branch through a marker file. See
[Pipeline templates](pipeline-templates.md) for details.

Varroa creates missing items, updates changed items, and skips unchanged items.

## Set build parameters

A job declares parameters as a flat list, or under `properties` (`kind: pipeline` and
`freeStyle` only):

```yaml
- kind: pipeline
  name: deploy
  parameters:
    - string:
        name: VERSION
        defaultValue: latest
    - choice:
        name: ENVIRONMENT
        choices: [dev, staging, prod]
  properties:
    - parameters:
        parameterDefinitions:
          - booleanParam:
              name: DRY_RUN
              defaultValue: false
  definition:
    script: |
      pipeline {
        agent any
        stages {
          stage('deploy') { steps { echo "deploying ${params.VERSION}" } }
        }
      }
```

Each entry names the parameter and sets exactly one of `string`, `choice`, or
`booleanParam`. An unnamed parameter, a duplicate name, and an empty `choices` list all
fail validation.

## Set flow durability

A cell's default flow durability is `MAX_SURVIVABILITY`. A lower hint (`kind: pipeline`
only) stops that job's run-state replication entirely:

```yaml
- kind: pipeline
  name: fast-build
  properties:
    - durabilityHint:
        hint: PERFORMANCE_OPTIMIZED
  definition:
    script: echo hi
```

`hint` accepts `PERFORMANCE_OPTIMIZED`, `SURVIVABLE_NONATOMIC`, or `MAX_SURVIVABILITY`.

## Schedule a job

```yaml
- kind: pipeline
  name: nightly
  triggers:
    - cron:
        spec: "H 2 * * *"
    - varroaUpstream:
        tenant: platform
        job: release
    - githubPush: {}
  definition:
    script: echo nightly
```

Trigger types are `cron`, `pollSCM`, `githubPush` (a push webhook through the `github`
plugin), and [`varroaUpstream`](cross-tenant-pipelines.md). `triggers` is valid on `pipeline`
and `freeStyle` items. A `pipeline` item also accepts `properties[].pipelineTriggers`, merged
with `triggers`. A job holds one trigger of each type. `cron.spec` must not be empty.

A cell rewrites each job from `items.yaml` after a restart. A Jenkinsfile `triggers {}` block
then has no effect until the job builds again. Declare a schedule in `items.yaml`.

## Control removal

| `removeStrategy.items` | Undeclared items |
|---|---|
| `none` | Keep all; formerly managed items stop converging |
| `sync` | Remove undeclared managed items |
| `remove-supported` | Remove undeclared managed items of supported kinds |
| `remove-all` | Remove undeclared items, including unmanaged items |

Renaming creates the new path and treats the old path as undeclared. Its removal
follows `removeStrategy.items`. Deletion also removes build history.

Unsafe removals appear in `status.pendingItemDeletions` and need the `approve-deletion`
capability. A running job or unreadable build state defers deletion.

## Cell restrictions

A [cell](../operations/cells.md) merges every tenant's `items.yaml` into one manifest and
checks it against three rules, only on a cell.

| Rule | Rejects |
|---|---|
| No poll SCM | A `pollSCM` trigger, on an item or inside `pipelineTriggers`. Use a webhook instead. |
| No global credentials | A credential declared outside a tenant folder's own credentials property, or a folder credential scoped outside `GLOBAL`. |
| No controller-config dependency | Any item property outside a fixed allow-list of folder- and job-scoped properties. |

A tenant manifest that fails any rule fails the whole cell's item assembly, and pushes nothing.

## Troubleshoot

| Symptom | Check |
|---|---|
| Unknown kind | Use one of the five supported values |
| Setting is absent | Field spelling and indentation |
| Deletion is pending | Pending path, build state, and approval |
| Kind is unsupported | Keep it unmanaged |
| Schedule stops after a cell restart | Move the Jenkinsfile trigger to `triggers` |
