# Move a Jenkinsfile onto a Cell

A cell runs your Jenkinsfile with no Jenkins web UI, on agent pods in your team's namespace. Make the edits below in order, then find the build on the Pipelines page.

## Before you start

An operator makes your `Team` a pipelines tenant and gives it a composed bundle. You own that bundle's Git repository. Keep these rules in mind:

- `items.yaml` declares your jobs.
- `jenkins.yaml` declares your pod templates only.

Read [Make a Team a pipelines tenant](../operations/cells.md#make-a-team-a-pipelines-tenant) and [Create a bundle](../config/composed-bundles.md#create-a-bundle) first.

## What a cell changes

| Classic controller | Cell |
|---|---|
| Jenkins web UI for job and build pages | No web UI. The Pipelines page shows jobs, builds, logs and artifacts. |
| Jobs created in the UI or by a seed job | Jobs come only from your bundle's `items.yaml`. A job from the UI or a seed job is lost when the cell restarts. |
| Agents anywhere the controller reaches | Agent pods in your agent namespace, or your own registered runners |
| Build history on the controller disk | History in the build record store. The cell's disk is temporary. |
| Global credentials, tools and libraries | Folder-scoped credentials and libraries only |

A cell can restart or move your team at any time. A running Pipeline build resumes on the next cell.

For more detail, read [headless controllers](../security/headless-controllers.md), [Build records](../operations/build-records.md) and [Pipeline mesh](../operations/pipeline-mesh.md).

## 1. Declare the job in items.yaml

Add a `pipeline` item with a `cpsScmFlowDefinition` that points at your existing Jenkinsfile. You can also use a `multibranch` item. The operator puts the item under a top-level folder named after your team.

```yaml
items:
  - kind: pipeline
    name: service
    definition:
      cpsScmFlowDefinition:
        scriptPath: Jenkinsfile
        lightweight: true
        scm:
          gitSCM:
            userRemoteConfigs:
              - userRemoteConfig:
                  url: https://github.com/example/service.git
                  credentialsId: github-token
            branches:
              - branchSpec:
                  name: "*/main"
```

See [Jobs and items](../config/items.md#define-a-pipeline) for every field.

## 2. Select an agent

Replace `agent any` and every inline `kubernetes { yaml … }` agent with a label from your own cloud. The operator gives every tenant the `<team>-default` template. That template has one container and no build tools.

An inline pod has no tenant label. The agent fence keeps such a build in the queue.

```groovy
pipeline {
  agent { label 'payments-default' }
  stages { stage('build') { steps { sh 'echo ready' } } }
}
```

To add a template, add an entry to `jenkins.clouds[].kubernetes.templates` in the bundle's `jenkins.yaml`. The operator adds the `<team>-` prefix to the name and the label.

```yaml
jenkins:
  clouds:
    - kubernetes:
        templates:
          - name: maven
            label: maven
            containers:
              - name: maven
                image: maven:3.9-eclipse-temurin-21
                command: sleep
                args: infinity
```

Select the template with `agent { label 'payments-maven' }`, then run the build step in the container:

```groovy
pipeline {
  agent { label 'payments-maven' }
  stages {
    stage('build') {
      steps { container('maven') { sh 'mvn -B verify' } }
    }
  }
}
```

To build on a machine outside the cluster, register a runner. Select `<team>-runner`, or `<team>-<label>` for a label the runner declares.

Read [Tenant pod templates](../operations/tenant-pod-templates.md#what-the-operator-writes), [External runners](../operations/runners.md#labels-and-the-tenant-fence) and [Isolation controls](../operations/cells.md#isolation-controls).

## 3. Move credentials into Secrets

Create a Kubernetes `Secret` in your agent namespace. Add the label `varroa.dev/tenant-credential: "true"`. Add the annotation `varroa.dev/credential-id` with the same id that the classic controller uses. The cell projects the Secret as a folder credential with that id. Your `credentials('…')` and `withCredentials` calls do not change.

Set the annotation `varroa.dev/credential-kind` to one of these values:

| Kind | Data keys |
|---|---|
| `usernamePassword` | `username`, `password` |
| `secretText` | `secret` |
| `sshPrivateKey` | `privateKey`, `username`, and optional `passphrase` |
| `secretFile` | `file`. The optional annotation `varroa.dev/credential-filename` sets the file name. The default is the Secret name. |
| `certificate` | `keystore` (binary PKCS#12), and optional `password` |
| `githubApp` | `appID`, `privateKey` (PKCS#8 PEM), and optional `owner`, `apiUri` |

Do not put a secret value in a bundle variable or in `items.yaml`. See [Projected credentials](../operations/cells.md#what-composes-a-tenant) for the full rules.

## 4. Load shared libraries from the folder

Declare each library as `folderLibraries` on a folder that your `items.yaml` declares. The `@Library('name')` call then resolves as before.

The library must sit on a folder that the tenant declares. It must not sit on the team root folder, which the operator creates. This example uses a folder named `apps`:

```yaml
- kind: folder
  name: apps
  properties:
    - folderLibraries:
        libraries:
          - libraryConfiguration:
              name: payments-lib
              allowVersionOverride: true
              retriever:
                modernSCM:
                  scm:
                    github:
                      repoOwner: example
                      repository: payments-lib
                      credentialsId: github-token
```

Each library names one source. Set `credentialsId` to the id of a projected tenant credential. Each library also needs `defaultVersion`, `allowVersionOverride: true`, or both.

| Source | Field |
|---|---|
| `modernSCM.scm.github` | A GitHub repository, named by `repoOwner` and `repository` |
| `modernSCM.scm.git` | Any Git host over HTTPS or SSH, named by `remote` |
| `legacySCM.scm.gitSCM` | A Git repository, with `branches` set to one fixed name |

Your bundle cannot add a global library. Global libraries belong to the operator.

Read [Tenant shared libraries](../operations/tenant-libraries.md) for the worked example and the held-folder rules.

## 5. Move triggers into items.yaml

Remove the `triggers {}` block from the Jenkinsfile. A cell rewrites each job from `items.yaml` after a restart. A Jenkinsfile trigger has no effect until the job builds again.

Declare `cron` and `githubPush` in the item's `triggers` list. A cell rejects `pollSCM`, so use a webhook instead.

```yaml
triggers:
  - cron:
      spec: "H 2 * * *"
  - githubPush: {}
```

Point the GitHub webhook at the tenant webhook door:

```text
https://<host>/hibernation/<token>/teams/<team>/queue/github-webhook/
```

Set the webhook's signing secret to the `webhookSecret` value from the `Team` status. Your operator gives you both the token and the secret.

GitHub works without extra setup. Generic and GitLab deliveries use the generic path with a `<team>:` token.

Read [Schedule a job](../config/items.md#schedule-a-job), [Tenant webhooks](../operations/webhooks.md) and [Cell restrictions](../config/items.md#cell-restrictions).

## 6. Review notification steps

A cell runs `slackSend`, `emailext`, and `mail` steps only when the Slack and SMTP settings are in place. See [Slack and email](../operations/notifications.md#slack-and-email). Remove a step when the pool does not configure its channel.

To post to a chat webhook, call it with `sh 'curl …'`. Store the webhook URL in a `secretText` credential. The agent namespace needs egress to the chat host.

```groovy
withCredentials([string(credentialsId: 'chat-webhook', variable: 'HOOK')]) {
  sh 'curl -fsS -X POST -d "{\"text\":\"build done\"}" "$HOOK"'
}
```

See [Network policies](../install/network-policies.md).

## 7. Keep artifacts and test reports

The `archiveArtifacts`, `stash` and `junit` steps work without change. The cell stores artifacts in the object store, and the record store keeps the test cases. The agent namespace needs egress to the blob endpoint.

Make two more edits:

- Remove `options { durabilityHint(...) }`. A job with its own durability setting does not resume after a cell loss.
- Replace `lock(...)` with `varroa.lock(name: …)`. The block runs only after the lock is held.

```groovy
varroa.lock(name: 'deploy-prod', timeout: 300) {
    sh 'deploy.sh'
}
```

Read [Build logs and artifacts](../operations/build-logs-and-artifacts.md#agent-namespace-egress), [Test cases](../operations/build-records.md#test-cases), [The durability requirement](../operations/pipeline-mesh.md#the-durability-requirement) and [Acquire a fleet lock](../config/cross-tenant-pipelines.md#acquire-a-fleet-lock). Read [Build state across slots](../operations/tenant-slots.md#build-state-across-slots) before you move a job to another slot.

## 8. Find the build

Open **Pipelines**, select your team, then select the job. The build page shows the stages, the log, the artifacts and the test results. You can start a build with parameters on that page.

The record store keeps the log and the artifacts after the cell restarts.

Read [Read and run pipelines](pipelines.md#open-a-team), [Open a build](pipelines.md#open-a-build) and [Operate a run](run-operations.md).

## Patterns a cell does not run

| Pattern | Replacement |
|---|---|
| Freestyle job | A `pipeline` item. You can declare a freestyle job as a stopgap. Its log stays on the cell and a cell restart loses it. Its build does not resume, and Replay does not run. |
| Matrix job (`matrix-project`) | A Declarative `matrix {}` block inside a pipeline |
| Maven job type | `sh 'mvn …'` in a container with Maven (step 2) |
| `tools {}` with global tool installers | A container image that has the tool, in a pod template |
| `agent { docker … }`, `docker.build`, `docker.image().inside` | A [kaniko pod template](../operations/tenant-pod-templates.md#build-a-container-image) builds and pushes an image. A [runner](../operations/runners.md) on a host with Docker runs the rest. An agent pod has no Docker daemon. |
| Promoted builds, tag-build actions | A later stage with an `input` step. Tag with `git tag` and an `sshPrivateKey` credential in `withCredentials`. |
| Job DSL seed job | Declare every job in `items.yaml`. The cell lint rejects an inline `jobDsl` step in a job or in a pipeline template body, and a cell does not install the `job-dsl` plugin, so the step fails at run time from any source. Jobs from a seed job leave the folder when the cell restarts. |
| Pipeline Syntax snippet generator | The [Pipeline steps reference](https://www.jenkins.io/doc/pipeline/steps/). An admin can reach the cell UI with [break-glass](../security/headless-controllers.md#break-glass-runbook). |
| Inline `kubernetes { yaml … }` agent | A pod template in the bundle (step 2) |
| `pollSCM` | A webhook (step 5) |

## Next steps

- [Tenant slots](../operations/tenant-slots.md) explains which slot runs a job.
- [Cross-tenant pipelines](../config/cross-tenant-pipelines.md) explains how to call another team's job.
