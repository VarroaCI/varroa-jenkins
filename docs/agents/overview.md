# Use agents with Varroa

An agent in Varroa acts as a person, with that person's permissions, and never
as an administrator token.

## Choose a way to connect

| Way | Who acts | Writes | Page |
|---|---|---|---|
| Your own MCP client | You, through your API key | The client calls write tools directly. Your grants are the gate. | [Connect a client](connecting.md) |
| The assistant in the dashboard | You, while you are signed in | Every write waits for your approval. | [Assistant](assistant.md) |

## What every agent shares

Both ways use one tool surface, one grant check, and one audit trail. MCP does
not provide a shell or unrestricted Kubernetes access. See
[Identity and auditing](identity.md) and [Tool reference](tools.md).

## Start

1. Connect an MCP client, or turn on the assistant.
2. Confirm the identity with `get_me`.
3. Confirm effective access with `get_my_permissions`. Then review the roles
   for each identity in [Identity and auditing](identity.md).
4. Read [Writing through MCP](writing.md) before you change resources.

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

## Related pages

- [Connect a client](connecting.md)
- [Assistant](assistant.md)
- [Identity and auditing](identity.md)
- [Jenkins controller tools](jenkins-tools.md)
- [Tool reference](tools.md)
- [Writing through MCP](writing.md)
