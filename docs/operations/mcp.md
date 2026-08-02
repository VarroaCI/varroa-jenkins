# MCP and Jenkins controller tools

Varroa exposes a Model Context Protocol (MCP) endpoint at
`https://<dashboard-host>/api/v1/mcp`. Use a Varroa API key (`vk_…`) in an
`Authorization: Bearer` header for non-browser clients. The
[`varroactl mcp`](../varroactl.md#mcp) bridge reads the configured context's
credential and forwards it to this endpoint for you.

MCP tools include fleet operations for controllers, configuration, users,
groups, activity, and other Varroa resources. Two tools bridge to Jenkins
controllers:

- `list_jenkins_controllers` lists the controllers the caller may view and
  their reachability.
- `call_jenkins_tool` forwards an MCP JSON-RPC request to the target
  controller's `mcp-server` plugin.

## Caller identity and permissions

`call_jenkins_tool` requires Varroa `controllers:read` for the requested
controller. That is a visibility check only. The BFF then mints a short-lived
Jenkins token for the calling identity and sends it to the controller; it does
not forward the API key itself.

The token carries the caller's subject, preferred username, name, email, and
groups. It is valid for five minutes and is audience-scoped to exactly one
controller (`<namespace>/<name>`), so it cannot be used for another controller.
Jenkins receives the caller as that user and applies its normal role-strategy
authorization. A caller who can reach a tool may still receive a Jenkins
authorization error for an operation they are not allowed to perform.

Every supported controller version pins the `mcp-server` plugin in Varroa's core
plugin set. The plugin is therefore delivered and reconciled with the rest of
the controller's managed plugins.

## Granting Jenkins access

Varroa control-plane access and Jenkins permissions are related but separate.
Give the caller `controllers:read` through [Varroa RBAC](../security/varroa-rbac.md),
then grant the caller or one of their groups a Jenkins role for the target
controllers. For example, this gives `acme:platform-team` Jenkins read access on
controllers in `teams-platform`:

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
  name: acme-platform-team-mcp-readers
spec:
  roleRef: mcp-reader
  subjects:
    - { kind: Group, name: "acme:platform-team" }
  controllerScope:
    namespaces: [teams-platform]
  jenkinsScope:
    type: Global
```

With this binding, a caller whose identity includes `acme:platform-team` can use
`call_jenkins_tool` to ask Jenkins `whoAmI` and receives the Jenkins identity
and permissions assigned to that group. See [Jenkins RBAC](../security/jenkins-rbac.md)
for item- and folder-scoped roles.

## Limits and upgrade behavior

`call_jenkins_tool` reaches Jenkins through in-cluster service DNS. It works
only for controllers in the BFF's cluster; hive-cluster controllers are not
reachable through this tool.

The caller needs both Varroa `controllers:read` and an effective federated
Jenkins assignment. Without the Jenkins assignment, the controller rejects the
request even though Varroa can see the controller.

The BFF egress policy selects controller pods by Varroa's managed-by label.
During the first upgrade that adds this label, a controller can be temporarily
unreachable until its StatefulSet rolls; subsequent reconciliation keeps the
label in place.
