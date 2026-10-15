# Publish and select pipeline templates

A `Jenkinsfile` in source control stays the default way to define a pipeline. Use a template when
many repositories need the same tested pipeline shape and only a few values should vary between
them, such as a Go module version or a deploy flag. A template is fleet-wide, versioned by the
catalog, browsable in the Varroa UI, and its parameter form comes from a schema.

A repository selects a template with a small marker file instead of a `Jenkinsfile`. A repository
may carry both files at once while it migrates from one to the other.

## The template document

A `pipeline-template` catalog item holds exactly one `PipelineTemplate` YAML document.

```yaml
apiVersion: varroa.dev/v1
kind: PipelineTemplate
metadata:
  name: go-service
  displayName: Go service
  description: Build, test, and publish a Go service
spec:
  version: 2.1.3
  parameters: {}
  uiSchema: {}
  jenkinsfile: |
    pipeline { ... }
```

| Field | Rule |
|---|---|
| `apiVersion` | Must be `varroa.dev/v1` |
| `kind` | Must be `PipelineTemplate` |
| `metadata.name` | A DNS-1123 label of at most 63 characters. A marker names a template by this value, not by the catalog item's own resource name. |
| `metadata.displayName`, `metadata.description` | Optional, shown in the catalog UI |
| `spec.version` | A strict `MAJOR.MINOR.PATCH` version. No prerelease text and no build metadata. |
| `spec.parameters` | A JSON Schema in the fixed subset below |
| `spec.uiSchema` | Optional, in the fixed uiSchema subset below |
| `spec.jenkinsfile` | The pipeline body, at most 64 KiB. Exactly one of `jenkinsfile` and `scmDefinition`. |
| `spec.scmDefinition` | `remote`, `scriptPath`, an optional `credentialsId`, and an optional `revision`, naming a script in the **template** repository, never in a consumer repository |

The whole document is at most 256 KiB. An unrecognized key at any level is a rejection.

## The parameter schema

`spec.parameters` uses a fixed subset of JSON Schema, chosen so a Go validator, the Jenkins
plugin's validator, and the browser's Ajv validator all agree on every accepted schema.

| Position | Allowed keywords |
|---|---|
| Every object, including the root | `type: object`, `title`, `description`, `properties`, `required`, `additionalProperties` |
| `type: string` | `title`, `description`, `default`, `enum`, `minLength`, `maxLength`, `pattern` |
| `type: number`, `type: integer` | `title`, `description`, `default`, `enum`, `minimum`, `maximum`, `exclusiveMinimum`, `exclusiveMaximum` |
| `type: boolean` | `title`, `description`, `default` |
| `type: array` | `title`, `description`, `default`, `items` (one schema, not a tuple), `minItems`, `maxItems`, `uniqueItems` |
| Root only | `$schema`, and only the value `https://json-schema.org/draft/2020-12/schema` |

The root schema must set `additionalProperties: false`. Any nested `additionalProperties` must
also be `false`. Every `default` must itself satisfy its own schema.

Every other keyword is rejected. The rejected set includes `$ref`, `$defs`, `const`, `oneOf`,
`anyOf`, `allOf`, `if`/`then`/`else`, `contains`, `patternProperties`, `format`, `multipleOf`,
`type: "null"`, and a `type` written as an array. `format` is rejected because the browser's
validator ignores an unregistered format while the other two would reject it. `multipleOf` is
rejected because the browser's validator rounds it to a precision the other two do not apply.

## Pattern constructs

`pattern` accepts only constructs that mean the same thing in Go's engine, Java's engine, and the
browser's engine.

Allowed constructs: literal characters; the class escapes `\d`, `\D`, `\w`, `\W`; an escaped
punctuation character; a character class `[...]` with ranges, an optional leading `^`, and `\-`
inside the class only; the anchors `^` and `$`; the quantifiers `*`, `+`, `?`, `{m}`, `{m,}`, and
`{m,n}` with `m` and `n` at most 1000; alternation `|`; a capturing group `(...)`; and a
non-capturing group `(?:...)`.

`\s` and `\S` are rejected: the three engines disagree on which characters they match. `\-` is
rejected outside a character class.

A quantifier is rejected when it follows a closing parenthesis. `(a+)+` and `(a|a)+` are both
rejected forms. Each form lets one engine backtrack for a very long time on an unlucky value, and
the template plugin runs inside the Jenkins controller against a marker any consumer repository
supplies. A `pattern` is at most 200 characters.

`$` always means the end of the input. The validators rewrite every unescaped `$` to the
end-of-input form before they compile the pattern, so an author never needs an engine-specific
spelling.

## The uiSchema

`spec.uiSchema` accepts only `ui:order`, `ui:title`, `ui:description`, `ui:help`,
`ui:placeholder`, `ui:widget`, and `ui:options`, at the root and in a structure mirroring
`properties`. `ui:widget` is one of `textarea`, `select`, `radio`, `checkbox`, and `updown`.
`ui:options` holds only `rows`. Every other `ui:` key is rejected, because it names a form
component the Varroa theme does not register.

`ui:widget: password` is rejected. A marker value is committed to a consumer repository, so it
must never hold a secret. Get a secret from a folder credential instead.

`ui:order`, when present, must name every declared property or end with `"*"`.

## Size and naming limits

A property name must match `^[a-zA-Z][a-zA-Z0-9_]{0,62}$`, so every value is reachable as
`varroaParams.<name>` in the pipeline body. Nesting is at most 3 levels deep. A schema declares at
most 100 properties in total. An `enum` holds at most 100 entries. The schema text is at most
64 KiB.

## Versioning: the bundle selects, the marker only constrains

A `ComposedBundle` delivers a set of templates through its `pipeline-template` inputs. Two
versions of one template are two separate catalog items. A marker's `templateVersion` picks a
version from that delivered set. It never fetches a version the bundle did not deliver.

| `templateVersion` | Matches |
|---|---|
| Omitted, or `*` | Any delivered version of that name |
| `M` | Any `M.*.*` |
| `M.N` | Any `M.N.*` |
| `M.N.P` | That version only |

Resolution picks the highest matching version. When nothing matches, the failure names the
constraint and lists every delivered version of that name.

## The marker file

A repository selects a template by committing `.varroa/pipeline.yaml`. The path is fixed: no
factory, item, or controller setting changes it.

```yaml
apiVersion: varroa.dev/v1
kind: Pipeline
template: go-service
templateVersion: "2.1"
values:
  goVersion: "1.24"
  publishImage: true
```

| Key | Rule |
|---|---|
| `apiVersion` | Required, must be `varroa.dev/v1` |
| `kind` | Required, must be `Pipeline` |
| `template` | Required, the template's `metadata.name` |
| `templateVersion` | Optional version constraint |
| `values` | Optional mapping, validated against the resolved template's `spec.parameters` |

Any other top-level key is a rejection. The file is at most 64 KiB and must hold no secret.

A branch that carries a valid or an invalid marker still gets a job at branch-indexing time. Only
a missing marker skips the job, the same way a missing `Jenkinsfile` does today. An invalid marker
fails that job's first build.

## Values in the pipeline body

A validated value never becomes Groovy source text. The pipeline body reads
`varroaParams.<name>`, where `varroaParams` is a read-only map built from the resolved marker or
item values. A value holding a quote, a `${`, or Groovy syntax stays a plain string; it never
changes what the pipeline runs.

## Declare a template from an item instead of a marker

An operator-managed [item](items.md) can name a template directly, with no marker file:

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

`definition.varroaTemplate` is a third, mutually exclusive form alongside
`definition.cpsScmFlowDefinition` and `definition.script`. A `multibranch` item can use the marker
factory instead of scanning for a `Jenkinsfile`:

```yaml
- kind: multibranch
  name: svc
  projectFactory:
    varroaMarkerProjectFactory: {}
```

Because an item-declared template is fully visible to the operator, its `values` are checked
against the resolved template's schema when the bundle composes, not only at the first build.

## Pull requests and fork trust

A marker selects code that runs on the fleet, so it comes from the branch's trusted revision, not
from the branch tip. Varroa reuses the trust decision a multibranch project already makes for
`Jenkinsfile`; it adds no second trust setting.

| Fork trust policy | Effect on template selection |
|---|---|
| Trust nobody | A fork pull request always uses the target branch's marker |
| Trust contributors | A repository contributor's pull request uses its own marker; anyone else's uses the target branch's marker |
| Trust users with write permission | A writer's or an admin's pull request uses its own marker; anyone else's uses the target branch's marker |
| Trust everyone | Every pull request uses its own marker |

A fork pull request that adds the very first marker still gets a job at indexing time, because
indexing only tests that the tip carries a marker. Its first build fails, because the trusted
revision, which is the target branch, carries no marker yet. The console names the marker path and
the trust rule.

A fleet that delivers templates naming credentials does not use trust everyone: that policy lets
an untrusted fork author choose which credential a build resolves.

## Where a template can fail, and where you see it

| Layer | Where it runs | What you see |
|---|---|---|
| Catalog sync | When the catalog item is read | `CatalogItem.status.message`, and the composed bundle reaches `phase: Invalid` |
| Bundle composition | When a `ComposedBundle` resolves | `ComposedBundle.status.errors`, and the same message in a preview or a validate call |
| The first build | When a branch's build starts | The build fails, its console names the exact problem, and the activity feed records `pipeline.marker.rejected` |
| A pre-commit check | Before a marker is committed | `varroactl lint marker`, described below |

A catalog variable never reaches a template body or a template schema: `pipeline-templates.yaml`
composes without the variable substitution that other content types use. A template takes its
input from `spec.parameters` and `varroaParams` only.

## Check a marker before you commit it

```bash
varroactl lint marker --team payments --marker .varroa/pipeline.yaml
```

The command posts the marker to the team's own template set and prints each finding's severity,
path, and message. It exits zero when the marker is valid, and non-zero otherwise, so a pre-commit
hook or a pipeline step can gate on it. It contacts no Jenkins controller, so it works even while
one is down. See [varroactl](../varroactl.md) for the full command reference.
