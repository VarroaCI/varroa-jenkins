# Artifact Credentials

A controller writes archived artifacts to the object store under its own prefix,
`artifacts/<cluster>/<namespace>/<controller>/`. See
[The per-cell artifact prefix](build-logs-and-artifacts.md#the-per-cell-artifact-prefix). Varroa
supplies the credential in one of two modes. Choose one mode per install. The chart refuses a
render that sets both.

| Mode | Values | Scope of one credential | Rotation |
|---|---|---|---|
| Static | `recordStore.blobs.s3.artifactSecret` | one cell, or every cell of a pool | the pod restarts |
| STS | `recordStore.blobs.s3.artifactSTS` | one cell | in place, no restart |

Use STS mode on a store with STS: AWS S3, MinIO, or Ceph RGW. Neither credential is ever `recordStore.blobs.s3.existingSecret`.
That control-plane credential can read the `logs/` prefix, and a cell must not.

## The bucket policy

Write `<p>` for the prefix that one credential may reach:

| Resource | Actions | Condition |
|---|---|---|
| the bucket | `s3:ListBucket` | `s3:prefix` starts with `<p>` |
| the bucket | `s3:GetBucketLocation` | none |
| `<p>*` | `s3:PutObject`, `s3:GetObject`, `s3:DeleteObject` | none |

The policy grants no `sts:*` action and no `s3:CreateBucket`.

## Static mode

`artifactSecret` names a base Secret name in the release namespace. The operator uses the first
candidate that exists with non-empty data:

| Controller | Candidates, in order |
|---|---|
| a cell with the label `varroa.dev/cell-pool=<pool>` | `<artifactSecret>-<controller>`, then `<artifactSecret>-<pool>` |
| any other controller | `<artifactSecret>-<controller>` |

Each Secret holds `access-key-id` and `secret-access-key`. Jenkins ignores a
`session-token` key here. The operator projects the winning Secret into the controller's namespace
as `<controller>-artifacts`. When the source changes, the operator restarts the pod.

A pool Secret is the normal path. Scope it with `<p>` set to
`artifacts/<cluster>/<namespace>/cell-<pool>-`. A pool name that is a prefix of another pool name
(`a` and `a-b`) shares that match. A per-cell Secret overrides the pool Secret for one cell, with
`<p>` set to the cell's own prefix.

## STS mode

The operator calls STS `AssumeRole` for each cell. The request carries an inline session policy:
the table above with `<p>` set to the cell's own prefix. A pool scale-up therefore needs no
credential work.

```yaml
recordStore:
  blobs:
    s3:
      artifactSTS:
        enabled: true
        issuerSecret: varroa-artifact-issuer
        roleArn: arn:aws:iam::123456789012:role/varroa-artifacts
        endpoint: ""      # empty: the endpoint of blobs.s3.url, else AWS STS
        duration: 1h      # 15m to 12h
```

`issuerSecret` holds `access-key-id` and `secret-access-key` of the issuing identity. The operator
reads it on each issue, so you can rotate it without a restart.

| Store | Issuing identity |
|---|---|
| AWS | An IAM user allowed `sts:AssumeRole` on the role. The role trusts that user and grants the bucket policy with `<p>` set to `artifacts/`. |
| MinIO | A non-root user whose policy grants that bucket policy. `roleArn` takes any ARN-shaped value, for example `arn:minio:iam:::role/varroa-artifacts`. |
| Ceph RGW | A user allowed to assume a real role whose policy grants that bucket policy. |

The operator can issue a credential for every cell's prefix. Treat the issuer Secret like the
operator's own credentials. A per-pool issuer is not available yet.

The operator writes the credential into `<cell>-artifacts` and renews it when half of its lifetime
has passed. The kubelet updates the mounted file, and Jenkins reads the new file on its next
request. The pod does not restart. A presigned upload or download URL expires with the session
that signed it, so it can expire up to half a duration after it was issued. For long uploads on
AWS, raise the role's `MaxSessionDuration` and `duration` together.

STS mode serves cells only. Another controller reports `LocalOnly`. A failed first issue blocks provisioning with `ReconcileBlocked`
reason `ArtifactSecretFailed`. A failed renewal keeps the current credential and reports
`ArtifactStorage` `False` with reason `IssueFailed`. The cell then stops taking new tenants.
Varroa never falls back to a static Secret.
