# Write resources through MCP

MCP write tools change live cluster state. Read the current resource before an
update and review the returned object after it.

## Patch a controller

`update_controller` applies only the fields supplied. Common fields have named
arguments. Other mutable controller fields belong in `spec`:

```json
{
  "namespace": "teams-platform",
  "name": "demo",
  "version": "2.555",
  "hibernation": {
    "enabled": true,
    "gracePeriodMinutes": 60
  },
  "spec": {
    "resources": {
      "limits": {"cpu": "4", "memory": "8Gi"}
    },
    "probes": {
      "startup": {"periodSeconds": 20}
    }
  }
}
```

Nested objects merge by key. Omitted fields remain unchanged. A named argument
wins when `spec` contains the same field. Unrecognized top-level arguments are
rejected.

Named arguments do not clear values. Put an empty value or `null` in `spec` to
clear a field. For example, `{"spec": {"hibernation": null}}` removes the
hibernation configuration. The named form `{"hibernation": null}` does not: a
named argument is read only when it carries an object, so a null is dropped and
the request silently changes nothing.

Create tools and updates for other resources may apply a complete object. Read
the discovered tool description before relying on omission behavior.

## Hibernate or wake a controller

Use `hibernate_controller` and `wake_controller`. Hibernation is reported in
`status.hibernated`; it is not a `spec.powerState` value. Both actions are
rejected while `powerState` is `Stopped` and require `controllers:manage`.

## Manage field ownership

Controller spec writes use Kubernetes server-side apply with field manager
`varroa-ui`. If another manager owns a changed field, Varroa returns `409` and
lists the field path and owning manager. No requested changes are applied.

Resolve the conflict in this order:

1. Read the resource and identify the competing manager.
2. Decide which system should own the field.
3. Stop the other writer or remove the field from this request when it should
   keep ownership.
4. Retry `update_controller` with `force: true` only when Varroa should take
   ownership.

```json
{
  "namespace": "teams-platform",
  "name": "demo",
  "version": "2.555",
  "force": true
}
```

`force` changes ownership handling only. It does not change the patch content.
It is accepted only by `update_controller`. Hibernate and wake actions update
status, so they do not participate in spec ownership.

The REST equivalent is `PATCH .../controllers/<namespace>/<name>?force=true`.
The `varroactl` equivalent is `--force` on supported controller update or edit
commands.

## Related pages

- [Tool behavior](tools.md)
- [Controller lifecycle](../operations/lifecycle.md)
- [Pod customization](../config/pod-customization.md)
