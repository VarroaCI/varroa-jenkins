# Script approvals in a cell

A cell is one Jenkins controller that hosts several tenants. The script-security plugin keeps one
approval list for the whole controller. An approval therefore applies to every tenant on the cell.
This page states the policy for those approvals.

## Policy

- Tenant pipeline code runs in the Groovy sandbox. The sandbox is always on for tenant code.
- Tenant folder libraries run in the sandbox.
- Trusted code comes only from pool libraries. See [Pool shared libraries](cell-libraries.md).
- An operator approves a signature per pool. A tenant cannot approve a signature for itself.

## Declare approved signatures

Set `approvedSignatures` on the `CellPool`. Each entry is one script-security signature:

```yaml
apiVersion: varroa.dev/v1alpha1
kind: CellPool
metadata:
  name: lts-2-555
spec:
  profileRef: lts-2-555
  warm: 2
  approvedSignatures:
    - method hudson.Util getHost
    - staticMethod org.example.Util run java.lang.String
```

Each entry starts with `method`, `staticMethod`, `new`, `field`, or `staticField`. An entry must
not contain a `${` token.

## How a cell renders the list

A cell renders the union of two lists:

- the pool `approvedSignatures` list, and
- the approved signatures in the cell bundle.

The operator removes duplicates and sorts the result. The sorted list keeps the drift check stable.

The operator always writes the list on a cell. When no entry remains, the cell gets an empty list.
An apply therefore revokes any approval that the pool or the cell bundle no longer declares. This
includes an approval that an operator added in the Jenkins user interface.

## Find a signature to approve

A pipeline that uses an unapproved signature fails. The build page shows the refused signature as
the failure reason. The cell Cell tab lists the pending signature with the tenant jobs that
requested it. Copy the signature text into `approvedSignatures`. Review the signature before you
approve it. An approved signature lets the tenant code of every pool cell call that method.

## Pending signatures in the Cell tab

The Cell tab of a cell has a Script approvals panel. The panel lists each signature that the
sandbox refused in a completed build, and each signature that script-security reports as pending.
Each entry names up to three tenant jobs that requested it, newest first. A signature with no
known requester lists with none.

The cell drops every signature that Jenkins already approves. That set holds the signatures that
the cell pool approves and the signatures that the cell bundle approves.

The panel is read-only. It has no approve action. An operator approves a signature only in the
pool declaration.

The cell reports the list on each state snapshot. The operator writes it to
`status.cell.pendingSignatures`. The list holds at most 25 signatures. A signature that is longer
than 1024 characters is not listed.

## The build page reason

When a build fails because the sandbox refused a signature, the build page shows a banner. The
banner names the signature. The record of the run keeps the signature for the same reason.
The completion webhook carries the signature in `rejected_signature`. See
[Notifications](notifications.md).

## Limits

- A rejection that the script catches does not fail the build. The signature still appears in the
  pending list, with no requester, because no build records it.
- The refusals live in memory in the cell JVM. A JVM restart or a pod roll clears them, and a
  signature reappears when a build refuses it again.
- A signature that an operator approves in the Jenkins user interface leaves the list. The
  operator still removes only the signatures that the pool declares.
- The operator filter reads only the pool list. A bundle approval is removed from the panel by the
  plugin, so it leaves the panel as soon as Jenkins approves it.
