# Jobs & Items (declarative)

<!-- sources: internal/jenkins/items/types.go, internal/jenkins/items/parser.go, internal/jenkins/items/engine.go, internal/jenkins/items/configxml.go, internal/bundle/manifest.go, api/v1alpha1/types.go (PendingDeletion), internal/api/handlers.go (approve-deletion) -->

Define Jenkins jobs, folders, and multibranch projects as **plain YAML in your bundle** — versioned in git, reviewed in PRs, applied and converged by Varroa. No Job DSL seed jobs, no Groovy, no script approvals.

## Concepts

- Items live in `items.yaml` files referenced by your bundle's `bundle.yaml` (see [Bundle sources](bundle-sources.md)) or in `item`-type [catalog items](casc-catalog.md).
- The mite's items engine applies them to Jenkins as `config.xml`, tracking every item it created as **managed**. Unmanaged items (created by hand in the UI) are left alone.
- **Diff-before-write**: the engine hashes the desired configuration per item and skips the write when nothing changed — a reconcile tick with no item changes performs zero Jenkins writes, so it never churns config history or touches a job mid-build gratuitously.
- **Guarded deletes**: removing a job destroys its build history, so deletions are gated (see [Removing items](#concepts-removing-items) below).

### Compared with Job DSL

| | Job DSL | Varroa items |
|---|---|---|
| Language | Groovy DSL | YAML |
| Execution | Seed job runs on the controller | Applied by the mite, no seed job |
| Sandbox / script approval | Required, frequent friction | Not applicable |
| Drift | Re-run seed to converge | Converged on the reconciliation interval |
| Removal of undeclared items | `removedJobAction` | `removeStrategy` + build-state guard + approval |
| Review workflow | Groovy diffs | YAML diffs in your bundle PRs |

Job DSL migrants: each `kind` below maps from the DSL job types — `folder` ↔ `folder`, `freeStyle` ↔ `job`, `pipeline` ↔ `pipelineJob`, `multibranch` ↔ `multibranchPipelineJob`, `organizationFolder` ↔ `organizationFolder`.

## Reference: the manifest

```yaml
# items.yaml
removeStrategy:
  items: none          # none (default) | sync | remove-supported | remove-all
  rbac: sync
items:
  - kind: folder
    name: platform
    items: [ ... ]      # folders nest children inline
```

Five item kinds are supported: `folder`, `freeStyle`, `pipeline`, `multibranch`, `organizationFolder`. An unknown `kind` fails validation with `item "<name>": unknown kind` — the bundle is rejected, nothing partial is applied. Unknown **fields** inside a known kind are ignored by the parser, so typos vanish silently: review `kubectl` diffs and the apply results ([Observability](../operations/observability.md)) when a setting doesn't seem to stick.

Fields shared by every kind: `name` (required), `displayName`, `description`, `disabled`, plus per-item RBAC (`groups`, `filteredRoles`) covered in [Jenkins RBAC](../security/jenkins-rbac.md).

## How to define each kind

### Folder

```yaml
items:
  - kind: folder
    name: platform
    displayName: Platform Team
    properties:
      - envVars:
          vars:
            TEAM: platform
      - folderCredentialsProperty:
          folderCredentials:
            - credentials:
                - secretText:
                    id: sonar-token
                    description: SonarQube token
                    secret: "${sonar_token}"        # bundle variable — keep secrets out of git
      - folderLibraries:
          libraries:
            - libraryConfiguration:
                name: platform-shared
                implicit: true
                retriever:
                  modernSCM:
                    scm:
                      github:
                        repoOwner: example
                        repository: jenkins-shared-lib
      - itemRestrictions:
          allowedTypes: [org.jenkinsci.plugins.workflow.job.WorkflowJob]
          filter: true
    items:                                          # children declared inline
      - kind: pipeline
        name: deploy
        definition:
          script: |
            pipeline { agent any; stages { stage('hello') { steps { echo 'hi' } } } }
```

### Pipeline

Inline script or from SCM:

```yaml
  - kind: pipeline
    name: build-api
    concurrentBuild: true
    quietPeriod: 5
    properties:
      - buildDiscarder:
          strategy: { logRotator: { numToKeep: 30, artifactNumToKeep: 5 } }
      - parameters:
          parameterDefinitions:
            - string:  { name: VERSION, defaultValue: latest, trim: true }
            - choice:  { name: ENV, choices: [dev, stage, prod] }
            - booleanParam: { name: DRY_RUN, defaultValue: true }
      - pipelineTriggers:
          triggers:
            - cron: { spec: "H 4 * * 1-5" }
    definition:
      cpsScmFlowDefinition:
        scriptPath: Jenkinsfile
        lightweight: true
        scm:
          gitSCM:
            userRemoteConfigs:
              - userRemoteConfig:
                  url: https://github.com/example/api.git
                  credentialsId: github-app
            branches:
              - branchSpec: { name: "*/main" }
            extensions:
              - cloneOption: { shallow: true, noTags: true, timeout: 10 }
```

### Free-style

```yaml
  - kind: freeStyle
    name: nightly-report
    label: linux
    scm:
      gitSCM:
        userRemoteConfigs:
          - userRemoteConfig: { url: https://github.com/example/reports.git }
        branches:
          - branchSpec: { name: "*/main" }
    triggers:
      - pollSCM: { scmpoll_spec: "H/15 * * * *" }
    builders:
      - shell: { command: ./generate-report.sh }
      - maven: { targets: "clean verify" }
    publishersList:
      - archiveArtifacts: { artifacts: "out/**", onlyIfSuccessful: true }
      - jUnitResultArchiver: { testResults: "**/target/surefire-reports/*.xml", allowEmptyResults: true }
      - mailer: { recipients: platform@example.com, notifyEveryUnstableBuild: true }
    buildDiscarder:
      logRotator: { daysToKeep: 14 }
```

Builders supported: `shell`, `maven`. Publishers: `archiveArtifacts`, `jUnitResultArchiver`, `mailer`. Triggers: `cron`, `pollSCM`. A builder/publisher/trigger the schema doesn't model can't be declared here — keep such jobs unmanaged or extend via JCasC where applicable.

### Multibranch pipeline

```yaml
  - kind: multibranch
    name: api
    sourcesList:
      - branchSource:
          source:
            github:
              repoOwner: example
              repository: api
              credentialsId: github-app
    projectFactory:
      workflowBranchProjectFactory: { scriptPath: Jenkinsfile }
    orphanedItemStrategy: { daysToKeep: 7, numToKeep: 20 }
```

`git` and `github` branch sources are supported.

### Organization folder

```yaml
  - kind: organizationFolder
    name: example-org
    navigators:
      - githubNavigator:
          repoOwner: example
          credentialsId: github-app
    projectFactories:
      - workflowMultiBranchProjectFactory: { scriptPath: Jenkinsfile }
```

Apply any of the above by committing to your bundle repo and letting the [composed bundle](composed-bundles.md) roll out.

**Verify:** after the controller converges (`status.phase: Connected`, apply results clean), the items exist:

```bash
curl -sf -H "Authorization: Bearer $VARROA_API_KEY" \
  https://<controller-host>/api/json?tree=jobs[name] | jq '.jobs[].name'
```

## Concepts: update semantics

On each apply the engine renders every declared item's desired `config.xml`, compares its hash with the last-written hash, and:

- **unchanged + exists** → skipped, no write;
- **changed** → updated in place (Jenkins config history shows one real change);
- **missing** → created (folders first — children require their parents).

Renaming an item in YAML is a **new item plus a removal** of the old name — read the next section before renaming anything with history you care about.

## Concepts: removing items

What happens to a **managed** item you delete from YAML depends on `removeStrategy.items`:

| Strategy | Behavior |
|---|---|
| `none` (default) | Nothing is deleted; the item just stops being managed/converged |
| `sync` | Undeclared managed items are removed |
| `remove-supported` | Removes undeclared items of supported kinds only |
| `remove-all` | Removes all undeclared items, managed or not — the big hammer |

Deletion is guarded twice:

1. **Build-state guard** — an item that is building (or whose build state can't be determined) is not deleted; the engine defers it.
2. **Approval** — deferred/guarded deletions surface on the controller as `status.pendingDeletions` (path, reason, detected time) instead of executing. Approve via the API (requires the `approve-deletion` capability on the controller — see [Varroa RBAC](../security/varroa-rbac.md)):

```bash
kubectl get controller demo -n teams-platform -o jsonpath='{.status.pendingDeletions}' | jq .
curl -sf -X POST -H "Authorization: Bearer $VARROA_API_KEY" \
  https://app.example.com/api/v1/clusters/core/controllers/teams-platform/demo/approve-deletion \
  -d '{"path": "platform/old-job"}'
```

**Verify:** the entry leaves `status.pendingDeletions` and the job is gone from Jenkins on the next apply.

> [!WARNING]
> Deleting a job destroys its build history permanently. Prefer `none` or `sync` over `remove-all`, and treat approval as the moment of no return.

## Troubleshooting

- Item silently missing a setting → unknown field name (parser ignores unknown keys); check spelling against this page's schema.
- Bundle rejected with `unknown kind` → typo in `kind:` or an unsupported item type.
- Deletion never happens → it's waiting in `status.pendingDeletions` for approval, or the job is continuously building.
- More in [Troubleshooting](../operations/troubleshooting.md).

## Related pages

- [Bundle sources](bundle-sources.md) — where items.yaml lives
- [Jenkins RBAC](../security/jenkins-rbac.md) — per-item `groups`/`filteredRoles`
- [Reconciliation](../operations/reconciliation.md) — when applies happen
- [Composed bundles](composed-bundles.md) — rolling item changes out
