# Control MCP identity and auditing

Every MCP request runs as the owner of its API key. Role changes take effect
without replacing the key.

## Grant control-plane access

Use [Varroa RBAC](../security/varroa-rbac.md) to grant access to Varroa
resources. Check the result with `get_my_permissions`, optionally scoped to a
namespace and controller.

## Grant Jenkins access

Calling a managed controller requires both:

| Check | Required grant |
|---|---|
| Varroa can expose the controller | `controllers:read` in the controller scope |
| Jenkins can run the requested tool | A matching `JenkinsRoleBinding` or linked Jenkins role |

The request reaches Jenkins as the caller's subject, username, email, and
groups. Jenkins applies its normal role-strategy authorization. Varroa does not
send the API key to the controller.

This example grants one group global Jenkins read access on controllers in one
namespace:

```yaml
apiVersion: varroa.dev/v1alpha1
kind: JenkinsRole
metadata:
  name: mcp-reader
spec:
  roleType: Global
  permissions:
    - hudson.model.Hudson.Read
---
apiVersion: varroa.dev/v1alpha1
kind: JenkinsRoleBinding
metadata:
  name: platform-mcp-readers
spec:
  roleRef: mcp-reader
  subjects:
    - kind: Group
      name: platform-team
  controllerScope:
    namespaces: [teams-platform]
  jenkinsScope:
    type: Global
```

Use [Jenkins RBAC](../security/jenkins-rbac.md) for folder, pattern, or agent
scope.

## Audit mutations

Successful mutating MCP tools publish activity with `source: mcp` and an actor
resolved from the calling identity. Filter the activity API or feed by that
source when reviewing agent actions.

Some controller lifecycle requests also produce a later operator event. The
MCP event records who requested the change. The operator event records the
result of reconciliation.

## Related pages

- [API keys](../security/api-keys.md)
- [Varroa RBAC](../security/varroa-rbac.md)
- [Jenkins RBAC](../security/jenkins-rbac.md)
- [Jenkins controller tools](jenkins-tools.md)
