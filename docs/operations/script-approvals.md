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

A pipeline that uses an unapproved signature fails. The Jenkins log of the build names the
signature, for example `method hudson.Util getHost`. Copy that text into `approvedSignatures`.
Review the signature before you approve it. An approved signature lets the tenant code of every
pool cell call that method.
