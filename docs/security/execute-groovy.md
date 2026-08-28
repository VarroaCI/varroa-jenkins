# Secure executeGroovy

`executeGroovy` runs arbitrary code in the Jenkins script console. A caller who
can create an allowed operation can administer every selected controller.

This page is the canonical security contract for the verb.

## Authorize the operation

Both controls must allow the request:

1. Varroa RBAC permits creation of the `BroodOperation` in its namespace.
2. `ProvisioningDefaults.spec.broodPolicy.executeGroovy` permits that operation
   namespace.

Policy enforcement applies to resources created through the API, `kubectl`, or
GitOps.

## Disable or restrict the verb

Disable it in a cluster:

```yaml
apiVersion: varroa.dev/v1alpha1
kind: ProvisioningDefaults
metadata:
  name: varroa-defaults
spec:
  broodPolicy:
    executeGroovy:
      enabled: false
```

Allow it only from designated operation namespaces:

```yaml
spec:
  broodPolicy:
    executeGroovy:
      enabled: true
      allowedNamespaces:
        - platform-operations
```

| Configuration | Result |
|---|---|
| Policy or `enabled` omitted | Enabled |
| `enabled: false` | Disabled |
| Empty `allowedNamespaces` | Allowed from every operation namespace |
| Non-empty `allowedNamespaces` | Allowed only when the `BroodOperation` itself is in the list |
| Policy read failure | Allowed after RBAC authorization |

Configure every cluster separately. The target controller namespaces do not
control this policy. A denied operation enters `Failed` and records the reason
in `status.reason`.

Policy is checked during reconciliation. Disabling the verb prevents new
dispatches and skips remaining targets. Work already dispatched cannot be
recalled, so up to the operation's `maxParallel` in-flight scripts may finish.

## Choose and retain script source

Set exactly one source:

| Source | Retention behavior |
|---|---|
| `action.groovy.script` | Script is stored in the `BroodOperation` |
| `action.groovy.itemRef` | A `groovy` catalog item is resolved and snapshotted once |

Catalog changes do not alter a running operation. A `groovy` item is
execution-only and cannot be included in a `ComposedBundle`.

The catalog snapshot is owned by the operation and is removed when the
operation is deleted after `ttlSecondsAfterFinished`. Keep an approved source
in the catalog when scripts must remain recoverable.

## Protect the privileged path

Execution uses a short-lived operator identity with Jenkins `Administer`
permission because the script console has no narrower permission. Protect the
operator signing key as a fleet-wide privileged credential.

When chart NetworkPolicies are enabled, the operator-to-Jenkins egress rule
targets the namespaces listed in the top-level `managedNamespaces` Helm value.
Scoping that value to your controller namespaces restricts both this egress and
operator/BFF workload access. Jenkins must remain reachable on TCP port 8080.

## Audit execution

Each completed target emits a `broodop.target.finished` activity event
attributed to the operation's `startedBy` identity. Events record source
metadata and a digest, not the script body. Jenkins error bodies that may echo
the submitted script are not copied into activity events.

See [Brood operations](../operations/brood-operations.md)
for targeting, parallelism, timeouts, and result handling.
