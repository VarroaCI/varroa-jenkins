# Operate Varroa through MCP

Varroa exposes fleet operations through the Model Context Protocol (MCP).
Clients act as the owner of a Varroa API key and remain subject to that
identity's permissions.

## Choose the operation surface

| Surface | What it can do | Authorization |
|---|---|---|
| Varroa | Manage fleet resources | Varroa RBAC |
| Jenkins | Call controller tools | Varroa and Jenkins RBAC |
| External systems | Only through a Jenkins tool | Jenkins environment controls |

MCP does not provide a shell or unrestricted Kubernetes access.

## Apply guardrails

- Give each client its own API key.
- Grant only the namespaces, controllers, and actions it needs.
- Review tool annotations before approving writes.
- Fetch one resource when a summarized list lacks required detail.
- Treat field-conflict overrides as a deliberate ownership transfer.
- Review activity events for mutations.
- Treat `call_jenkins_tool` as open-ended because the target plugin defines its
  effects.

Credentials and sensitive status fields are removed from tool results. See
[Tool behavior](tools.md) for result shapes and redaction.

## Start

1. [Connect the MCP client](connecting.md).
2. Confirm the identity with `get_me`.
3. Confirm effective access with `get_my_permissions`.
4. Grant Varroa and Jenkins roles as described in
   [Identity and auditing](identity.md).
5. Read [Writing through MCP](writing.md) before changing resources.

## Related pages

- [Connect an MCP client](connecting.md)
- [Tool behavior](tools.md)
- [Writing through MCP](writing.md)
- [Jenkins controller tools](jenkins-tools.md)
