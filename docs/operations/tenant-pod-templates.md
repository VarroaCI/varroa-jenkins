# Tenant Pod Templates

A pipelines tenant on a cell gets the operator's `<team>-default` pod template. The tenant bundle
can add more pod templates. Read [Cells and pipelines tenants](cells.md) first.

## Where the templates come from

A tenant bundle composes `podtemplate` catalog items, or declares a kubernetes cloud in a `jcasc`
item. The operator reads the `templates` list of every kubernetes cloud in the tenant's
`jenkins.yaml`, in bundle order. It puts each template on the tenant's own cloud, after
`<team>-default`. Every member cell of the tenant, active or standby, renders the same templates.
A draining tenant keeps its templates, so a build that still runs can get its agent.

## What the operator writes

The operator writes four keys on each template:

| Key | Value |
|---|---|
| `name` | `<team>-<name>` |
| `namespace` | the tenant's `spec.pipelines.agentNamespace` |
| `serviceAccount` | `varroa-agent-<team>` |
| `label` | `varroa-tenant-<team>`, then `<team>-<token>` for each token of the tenant's `label` |

The tenant's own values of these four keys are not used. The operator drops a label token that
starts with `varroa-tenant-`. It also drops a `labels` key and records no warning, because the
plugin attribute is the singular `label`. Every other key, for example `containers`, `volumes`,
`yaml`, or `idleMinutes`, passes through unchanged.

A pipeline selects a template with its prefixed label:

```groovy
pipeline {
  agent { label 'payments-golang' }
  stages { stage('build') { steps { sh 'go build ./...' } } }
}
```

Do not use the label token `default`. It renders `<team>-default`, and the operator's own template
then answers that label.

In `inheritFrom`, name a template by the name the bundle gives it. The operator changes that token
to the `<team>-<name>` form. It keeps every other token as written.

## Variables and skipped templates

The operator resolves the tenant bundle's own variables in each template first, in one pass. A
variable value that names another variable is not resolved again. Then the operator skips a
template, and applies the others, when:

1. the entry is not a map;
2. a value or a key still holds `${`, an escaped `^${` included;
3. `name` is missing, is not a string, or is empty;
4. `name` is `default`;
5. `label` is present and is not a string;
6. an earlier template of the tenant has the same `name`.

Each skip adds a `PodTemplateSkipped` entry to the `Team`'s `TenantWarnings` condition. In a pod
`yaml`, write a shell variable as `$VAR`, not `${VAR}`.

The operator ignores all other content of `jenkins.yaml`. It records one `BundleKeysIgnored`
entry that names each ignored key path. A `jenkins.yaml` that does not parse gives one
`BundleKeysIgnored` entry and no template.

## Agent pod lifetime

An agent pod can outlive the cell instance that launched it. The new instance holds no node for
it until a resume adopts it. These mechanisms delete such a pod:

| Mechanism | Covers | Deletes a stale pod within |
| --- | --- | --- |
| The operator, for a pod that is not Running | Every case | About 4 minutes |
| Kubernetes plugin garbage collection on the tenant cloud | A cell roll, a slot move, a standby promotion | About 41 minutes |
| The operator, for a Running pod | An unschedulable slot holder, a removed slot, a plugin fault | About 46 minutes |
| The operator, during teardown | A dropped `spec.pipelines` or a deleted `Team` | At once |

A pod that is not Running never ran a build step, so no resume adopts it. A Running pod can hold
a node that a late resume adopts, so it outlives the longest resume window.
Every live cell instance refreshes the `kubernetes.jenkins.io/last-refresh` annotation on its own
agent pods once a minute. The operator deletes only a pod that runs as `varroa-agent-<team>`,
carries the `kubernetes.jenkins.io/controller` label, and carries a valid refresh annotation.
Teardown keeps the `Team` finalizer until the tenant's agent pods are gone, or for ten minutes at
most. Each check runs once a minute, and no timeout is configurable.

## Build a container image

An agent pod has no Docker daemon. A `docker.build` step or an `agent { docker … }` block does
not run on a cell. Build the image with kaniko in a pod template instead. Kaniko needs no
daemon, no privileged container, and no security context. Varroa tests this recipe on a cell
with kaniko `v1.23.2`.

Kaniko runs as root. The agent namespace must allow the Pod Security `baseline` level, because
the `restricted` level refuses the pod. BuildKit rootless needs an unconfined seccomp profile,
and `baseline` refuses that profile. The operator renders no NetworkPolicy in the agent
namespace, so the cluster's own egress rules decide which registries the pod reaches.

1. Add a `podtemplate` item to the tenant bundle:

   ```yaml
   # clouds.kubernetes.templates[]
   - name: kaniko
     label: kaniko
     idleMinutes: 0
     podRetention: "never"
     containers:
       - name: kaniko
         image: gcr.io/kaniko-project/executor:v1.23.2-debug
         command: /busybox/cat
         ttyEnabled: true
         resourceRequestCpu: "1"
         resourceRequestMemory: "2Gi"
         resourceLimitCpu: "2"
         resourceLimitMemory: "4Gi"
   ```

2. Write a Docker `config.json` for the push registry. The `auth` value is `user:token` in
   base64:

   ```json
   {"auths": {"registry.example.com": {"auth": "<base64 of user:token>"}}}
   ```

3. Store the file as a projected `secretFile` credential in the agent namespace:

   ```bash
   kubectl -n team-payments create secret generic payments-registry --from-file=file=config.json
   kubectl -n team-payments label secret payments-registry varroa.dev/tenant-credential=true
   kubectl -n team-payments annotate secret payments-registry \
     varroa.dev/credential-id=registry-push varroa.dev/credential-kind=secretFile \
     varroa.dev/credential-filename=config.json
   ```

4. Run kaniko in the `kaniko` container. A step outside `container('kaniko')` runs in the
   `jnlp` container, which has no kaniko. The debug image has no `/bin/sh`, so name the busybox
   shell:

   ```groovy
   pipeline {
     agent { label 'payments-kaniko' }
     stages {
       stage('image') {
         steps {
           withCredentials([file(credentialsId: 'registry-push', variable: 'DOCKER_CFG')]) {
             container(name: 'kaniko', shell: '/busybox/sh') {
               sh '''#!/busybox/sh
                 mkdir -p /kaniko/.docker
                 cp "$DOCKER_CFG" /kaniko/.docker/config.json
                 /kaniko/executor --context "$WORKSPACE" --dockerfile Dockerfile \
                   --destination registry.example.com/payments/app:$BUILD_NUMBER
               '''
             }
           }
         }
       }
     }
   }
   ```

`withCredentials` writes the file in the workspace temporary directory. Every container of the
pod mounts the workspace, so the kaniko container can read the file. Add one `--destination` for
each tag. To keep layers in the registry between builds, add `--cache=true` and `--cache-repo`.

## Security

Varroa does not filter pod specs. A tenant's `yaml` or `volumes` can name a `hostPath`. Pod
Security Admission in the agent namespace is the cluster administrator's control. A tenant
variable value that a pod template uses goes into the cell's configuration. Do not put a secret
in a bundle variable. Use a projected credential.
