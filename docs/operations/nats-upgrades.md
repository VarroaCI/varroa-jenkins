# NATS Upgrades

`nats.container.image.tag` pins the NATS server at `2.14.7-alpine`. A change upgrades the whole JetStream store.

## Upgrade

1. Set the tag and run `helm upgrade`. The StatefulSet rolls one pod at a time; each pod logs `Recovering N consumers for stream` as it rejoins.
2. Confirm every stream replica is `current` through `/jsz?streams=true` (see [Troubleshooting](troubleshooting.md)), then publish, consume, and read one KV key.

Never add `prev_encryption_key`. Upgrade to `2.15.x` from `2.14.7` or later only.

## Rollback

Set the tag back and upgrade. A store written by `2.12` or later needs `2.11.9` or later; one written by `2.15` needs `2.14.7` or later. Never return to `2.10.x`. Each pod logs `Stream state magic and version mismatch` once and rebuilds its state files without loss.

## Certificates

The `varroa-nats-creds` Secret holds the NATS CA certificate and the NATS server certificate. `helm install` and `helm upgrade` renew them. Nothing renews them between two upgrades.

| Certificate | Life | `helm upgrade` renews it when |
|---|---|---|
| Server certificate | 365 days | Fewer than 180 days remain, or `nats.external.host`, `extraSANs`, or `ipSANs` changes. |
| CA certificate | 3650 days | Fewer than 365 days remain. A CA renewal also signs a new server certificate. |

Run `helm upgrade` at least once every 180 days. An upgrade with no value change also renews a certificate that is due. The NATS pods load a new server certificate without a restart. The `helm upgrade` output shows both end dates. To show them later, run:

```bash
kubectl -n varroa-system get secret varroa-nats-creds -o jsonpath='{.metadata.annotations.varroa\.dev/nats-ca-not-after}{"\n"}{.metadata.annotations.varroa\.dev/nats-tls-not-after}{"\n"}'
```

A CA renewal keeps the CA key and the CA name. The earlier and the renewed CA certificate both verify the server certificate. Each bus client on the core reads the new `ca.crt` on its next connection, without a restart.

Each member cluster holds its own copy of `ca.crt`. That copy stays valid until its own end date. When the `helm upgrade` output reports a CA renewal, copy the new `ca.crt` to each member before that date:

```bash
CA=$(kubectl -n varroa-system get secret varroa-nats-creds -o jsonpath='{.data.ca\.crt}')
kubectl --context <member-context> -n varroa-system patch secret varroa-nats-creds \
  --type merge -p "{\"data\":{\"ca.crt\":\"$CA\"}}"
```

To show the end date of a member copy, run:

```bash
kubectl --context <member-context> -n varroa-system get secret varroa-nats-creds \
  -o jsonpath='{.data.ca\.crt}' | base64 -d | openssl x509 -noout -enddate
```

A renewal never replaces the CA key. To replace the key, for example after a leak, delete the Secret and run `helm upgrade`. This also replaces the bus passwords. Run `helm get notes varroa` for the steps.
