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

## Prune dead branches

A `multibranch` item sets how long Jenkins keeps the job of a deleted branch or a closed
pull request. Use `orphanedItemStrategy`:

```yaml
items:
  - kind: multibranch
    name: payments-web
    orphanedItemStrategy:
      daysToKeep: 7
      numToKeep: 5
    sourcesList: ...
```

- `daysToKeep` keeps a dead branch job for this number of days after its last build.
- `numToKeep` keeps at most this number of dead branch jobs.
- Omit a limit or set it to `0` for no limit on that axis.
- A negative value rejects the item.

When you omit `orphanedItemStrategy`, Jenkins deletes the job of a dead branch at the next
scan. When you set it, Varroa always turns dead-branch pruning on. Only a `multibranch`
item accepts the key.

## Define a pipeline

```yaml
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

A composed bundle keeps only the `items` key of each item manifest.
It drops `removeStrategy`, `retain`, `root`, and every other top-level key.
The `ComposedBundle` `status.warnings` names each dropped key and its source.
The `dropped-item-keys.yaml` key of the bundle content ConfigMap lists the same keys.

A non-cell controller does not remove an undeclared item.
A renamed item creates the new path and keeps the old path.
Delete an old item from the Jenkins UI.

A cell removes undeclared managed items in each tenant folder.
A held tenant's folder stays as it is.
Unsafe removals appear in `status.pendingItemDeletions` and need the `approve-deletion`
capability. A running job or unreadable build state defers deletion.
Deletion also removes build history.

## GitHub sources

A `multibranch` item can scan a GitHub repository. Use the `github` key under `branchSource.source`. Declare the branch source traits under `traits`:

```yaml
items:
  - kind: multibranch
    name: payments-web
    sourcesList:
      - branchSource:
          source:
            github:
              repoOwner: acme
              repository: web
              credentialsId: github-token
              traits:
                - gitHubBranchDiscovery:
                    strategyId: 3
                - gitHubPullRequestDiscovery:
                    strategyId: 1
                - gitHubForkDiscovery:
                    strategyId: 1
                    trust:
                      gitHubTrustEveryone: {}
                - headWildcardFilter:
                    includes: main release/*
                    excludes: wip/*
                - gitHubSshCheckout:
                    credentialsId: github-ssh-key
                - githubSkipNotifications: {}
    projectFactory:
      workflowBranchProjectFactory:
        scriptPath: Jenkinsfile
```

Each trait entry sets exactly one key. Each key appears at most once in the list. The keys are:

- `gitHubBranchDiscovery` and `gitHubPullRequestDiscovery` take a `strategyId` of `1`, `2`, or `3`.
- `gitHubForkDiscovery` takes a `strategyId` of `1`, `2`, or `3`. Set `trust.gitHubTrustEveryone` to trust every fork. Omit it to trust only forks whose owner has write permission.
- `headWildcardFilter` takes `includes` and `excludes`. Separate the patterns with a space. An empty `includes` matches `*`.
- `gitHubSshCheckout` takes a `credentialsId`. The checkout uses SSH with that credential.
- `githubSkipNotifications` takes no settings. Write it as `githubSkipNotifications: {}`. The source then sends no commit status to GitHub. The `skip-notifications-trait` plugin supplies the trait, and the pinned plugin set includes it.

A declared `traits` list replaces the plugin defaults. Name every discovery trait that the item needs. An empty or absent `traits` list renders no `traits` element. The plugin then applies its own defaults.

The same keys apply to the `modernSCM.scm.github` retriever of a folder library. A folder library renders `githubSkipNotifications` in its retriever. A library source sends no build status, so the trait has no visible effect there.

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

When the gitea plugin is missing, the operator refuses the items content. The condition reason is `ItemsRefused`.

## Folder library credentials

A folder library names its credential by ID. A non-cell controller checks each ID before it loads the items. The check passes when the library's folder sees a credential with that ID. The credential must also have an accepted kind. A folder sees two sets of credentials:

- The `GLOBAL`-scope credentials in the `credentials.system` store of `jenkins.yaml`. Every domain counts.
- The folder credentials of the library's folder and of each ancestor folder.

A `SYSTEM`-scope credential is hidden from items. The check refuses it.

| Retriever | Accepted kinds |
|---|---|
| `modernSCM.scm.github` | `usernamePassword`, `gitHubApp` |
| `modernSCM.scm.git` | `usernamePassword`, `sshPrivateKey`, `gitHubApp` |
| `legacySCM.scm.gitSCM` | `usernamePassword`, `sshPrivateKey`, `gitHubApp` |

The check refuses the whole items content when a library names a missing ID. It also refuses a `SYSTEM`-scope ID. It refuses a kind that its source rejects. The condition depends on the phase:

- While the controller provisions, `BundleResolved` is `False` with reason `ItemsRefused`.
- After the controller connects, `BundleFailed` is `True` with reason `ItemsRefused`. The controller keeps its last good desired state.

The operator refuses the items content for a missing plugin in the same way. The gitea and branch-strategy checks use the same reason.

The check does not count credentials that you create in the Jenkins UI. Create each library credential in `jenkins.yaml` or in a folder.

A system credential ID that holds a `${...}` variable is not counted. While such an ID exists, a library that names an unknown ID is not refused as missing. The check still refuses a `SYSTEM`-scope ID and a kind that its source rejects.

A cell checks its tenant libraries differently. See [tenant libraries](../operations/tenant-libraries.md).

## Cell restrictions

A [cell](../operations/cells.md) merges every tenant's `items.yaml` into one manifest and
checks it against four rules, only on a cell.

| Rule | Rejects |
|---|---|
| No poll SCM | A `pollSCM` trigger, on an item or inside `pipelineTriggers`. Use a webhook instead. |
| No global credentials | A credential declared outside a tenant folder's own credentials property, or a folder credential scoped outside `GLOBAL`. |
| No controller-config dependency | Any item property outside a fixed allow-list of folder- and job-scoped properties. |
| No Job DSL | An inline pipeline `script`, or the inline `jenkinsfile` of a pipeline template body, that calls the `jobDsl` step or `ExecuteDslScripts`. Declare each job in `items.yaml`. A cell also fails the step at run time, from any script source. |

A tenant manifest that fails a rule holds only that tenant's folder. The cell keeps the folder as it is and pushes the other tenants. The `ItemsHeld` warning names the rule and the item path.

A cell also refuses a key that the items schema does not define. A non-cell controller ignores such a key. The `ItemsHeld` warning names each unknown key, its item path, and its key path, for example `items[0].builders[0].jobDsl`.

### Top-level keys

See [Control removal](#control-removal) for the dropped keys.

On a cell, the drop has these effects:

- An undefined top-level key, such as a typo for `items`, holds the tenant. The `ItemsHeld` warning names the key and its source.
- A top-level `root` key holds the tenant. A tenant may not set `root`.
- A `removeStrategy` or `retain` key is ignored. The `BundleKeysIgnored` warning names it.

### Job DSL on a cell

Job DSL seed jobs are not supported on a cell. Declare every job in `items.yaml`. The cell home is temporary, so a cell restart deletes the jobs that a seed job made. A seeded job exists only on the member that ran the seed, and a build for it can go to another member. The lint reads an inline `script` and the inline `jenkinsfile` of a pipeline template, and rejects a `jobDsl` step there before the item reaches the cell. It does not read a Jenkinsfile in SCM or a step name built at run time. A cell does not install the `job-dsl` plugin. A `jobDsl` step from any source fails with `No such DSL method 'jobDsl' found among steps`. The step creates no job. A non-cell controller keeps the `job-dsl` plugin. As a second guard, the Varroa plugin stops a `jobDsl` step on a cell with the message `The jobDsl step is not allowed on a cell. Declare each job in items.yaml.` A cell bundle cannot use the JCasC `jobs:` root, because the `job-dsl` plugin supplies that root.

## Troubleshoot

| Symptom | Check |
|---|---|
| Unknown kind | Use one of the five supported values |
| Old item stays after a rename | A non-cell controller keeps undeclared items. Delete it from the Jenkins UI |
| Setting is absent | Field spelling and indentation |
| Deletion is pending | Pending path, build state, and approval |
| Kind is unsupported | Keep it unmanaged |
| Schedule stops after a cell restart | Move the Jenkinsfile trigger to `triggers` |
| Condition reason is `ItemsRefused` | Read the condition message. Add the missing plugin or credential |
