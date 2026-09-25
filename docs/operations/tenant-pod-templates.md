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

Each skip adds a `PodTemplateSkipped` entry to the `Team`'s `TenantWarnings` condition. Rule 2
keeps a template from reading a cell value or failing the cell for every tenant. In a pod `yaml`,
write a shell variable as `$VAR`, not `${VAR}`.

The operator ignores all other content of `jenkins.yaml`. It records one `BundleKeysIgnored`
entry that names each ignored key path. A `jenkins.yaml` that does not parse gives one
`BundleKeysIgnored` entry and no template.

## Security

Varroa does not filter pod specs. A tenant's `yaml` or `volumes` can name a `hostPath`. Pod
Security Admission in the agent namespace is the cluster administrator's control. A tenant
variable value that a pod template uses goes into the cell's configuration. Do not put a secret
in a bundle variable. Use a projected credential.
