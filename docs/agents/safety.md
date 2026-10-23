# Tenant agent safety

Each limit on a Tenant agent is enforced by the system. No limit depends on the
wording of the prompt.

## The boundary

- **Tools.** A Tenant agent sees only the tools that its data level allows. A tool
  that is hidden from the tool list cannot run.
- **Identity.** The agent acts as `agent:<namespace>/<tenant>/<name>`. Its grants
  come from the bindings of its Tenant. A human subject never starts with `agent:`,
  and an agent subject never decides an approval.
- **Budget.** The session limit and the day limit stop a run when the tokens are
  spent. A kill switch stops every running session before its next model call.
- **Egress.** The runner renders no egress rule to a git host. A forge read or
  write goes through the operator.
- **Approval.** A write waits for a person, unless the grant names the tool.

## Never without an approval

Some write tools always need an approval. A grant cannot name them. This list
includes `release_fleet_lock` and `open_pull_request`. The rule is in the policy
package, and the capabilities tab shows the list for each Tenant agent.

Every other write tool needs an approval, unless it is in the eligible list
(`run_pipeline`, `restart_run`, `scan_multibranch`) and the object grants it.

## The credential

Each Tenant agent session holds a short-lived token. The token has a fixed
audience, a fixed issuer, and a lifetime of 15 minutes. The token reaches the
MCP endpoint of the BFF only.

The runner mints the token with the signing key that the chart mounts. The BFF
verifies the token with the same key.

## No forge token leaves the operator

The instructions read and the issue creation both run in the operator of the
core cluster. The operator chooses the credential of the Tenant. The BFF, the
runner, and the model never receive a forge token. The read uses a credential
that cannot push when one exists.

## Shared namespaces

A namespace that two Tenants both list in `spec.namespaces` is a shared
boundary. Treat every Tenant in that namespace as one trust domain.

The acceptance rule admits an object only in a namespace that its Tenant lists in
`spec.namespaces`, or in the release namespace.

## Trigger loss

A trigger is best effort. A producer can fail during an outage, and a dropped
event is never replayed. The runner ignores an event older than one hour, so an
old event cannot start a session after a long outage. An operator can start a
run by hand with the `manual` trigger.

## Data levels

The data level of a session is the lowest of four values: the object's
`dataSharing`, the Tenant's tenant level, the provider's trust, and the chart
ceiling. A read tool above the level is hidden. Result fields above the level are
removed before the model reads them.

## Related pages

- [Tenant agents](tenant-agents.md)
- [Tool reference](tools.md)
- [Identity and auditing](identity.md)
