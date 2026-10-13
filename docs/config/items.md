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

## Set the quiet period and the resume block

A `pipeline` item accepts two job fields:

```yaml
- kind: pipeline
  name: release
  quietPeriod: 30
  resumeBlocked: true
  definition:
    script: echo release
```

`quietPeriod` sets the job's quiet period in seconds. Without it, the job uses the global quiet
period. `resumeBlocked: true` stops a run from resuming after a controller restart. It has the
same effect as the `disableResume` property.

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
plugin), [`varroaUpstream`](cross-tenant-pipelines.md), and `genericTrigger` (a webhook call
that carries `?token=<team>:<name>`; see [Tenant webhooks](../operations/webhooks.md)). `triggers` is valid on
`pipeline` and `freeStyle` items. A `pipeline` item also accepts `properties[].pipelineTriggers`,
merged with `triggers`. A job holds one trigger of each type. `cron.spec` and `genericTrigger.token`
must not be empty. A tenant's `genericTrigger.token` must start with `<team>:`.

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

## Gitea and Forgejo sources

A `multibranch` item or an `organizationFolder` item can scan a Gitea or Forgejo server. Use
the `gitea` and `giteaNavigator` keys:

```yaml
items:
  - kind: multibranch
    name: payments-api
    sourcesList:
      - branchSource:
          source:
            gitea:
              serverUrl: https://git.example.org
              repoOwner: acme
              repository: api
              credentialsId: forge-token
              traits:
                - giteaBranchDiscovery:
                    strategyId: 3
                - giteaForkDiscovery:
                    strategyId: 1
                    trust: contributors
    projectFactory:
      workflowBranchProjectFactory:
        scriptPath: Jenkinsfile
  - kind: organizationFolder
    name: acme
    navigators:
      - giteaNavigator:
          serverUrl: https://git.example.org
          repoOwner: acme
          credentialsId: forge-token
```

The discovery traits are `giteaBranchDiscovery`, `giteaPullRequestDiscovery`,
`giteaForkDiscovery`, and `giteaTagDiscovery`. Each `strategyId` is `1`, `2`, or `3`. The
fork trust is `nobody`, `contributors`, or `everyone`. When `traits` is empty, the defaults
apply: branch discovery (`1`), origin pull request discovery (`1`), and fork pull request
discovery (`1`) that trusts `contributors`.

The engine never registers a webhook on the forge. Add the webhook by hand, as described in
[Commit status](../operations/commit-status.md) for the webhook. On a cell, declare the
server entry in `CellPool.spec.giteaServers`.

## Cell restrictions

A [cell](../operations/cells.md) merges every tenant's `items.yaml` into one manifest and
checks it against four rules, only on a cell.

| Rule | Rejects |
|---|---|
| No poll SCM | A `pollSCM` trigger, on an item or inside `pipelineTriggers`. Use a webhook instead. |
| No global credentials | A credential declared outside a tenant folder's own credentials property, or a folder credential scoped outside `GLOBAL`. |
| No controller-config dependency | Any item property outside a fixed allow-list of folder- and job-scoped properties. |
| No Job DSL | An inline pipeline `script` that calls the `jobDsl` step or `ExecuteDslScripts`. Declare each job in `items.yaml`. |

A tenant manifest that fails a rule holds only that tenant's folder. The cell keeps the folder as it is and pushes the other tenants. The `ItemsHeld` warning names the rule and the item path.

### Job DSL on a cell

Job DSL seed jobs are not supported on a cell. Declare every job in `items.yaml`. The cell home is temporary, so a cell restart deletes the jobs that a seed job made. A seeded job exists only on the member that ran the seed, and a build for it can go to another member. The lint reads only an inline `script`. It does not read a Jenkinsfile in SCM, a pipeline template body, or a step name built at run time. The `job-dsl` plugin stays installed because a non-cell controller uses it.

## Troubleshoot

| Symptom | Check |
|---|---|
| Unknown kind | Use one of the five supported values |
| Setting is absent | Field spelling and indentation |
| Deletion is pending | Pending path, build state, and approval |
| Kind is unsupported | Keep it unmanaged |
| Schedule stops after a cell restart | Move the Jenkinsfile trigger to `triggers` |
