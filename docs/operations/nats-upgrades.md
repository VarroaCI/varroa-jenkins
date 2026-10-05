# NATS Upgrades

`nats.container.image.tag` pins the NATS server at `2.14.7-alpine`. A change upgrades the whole JetStream store.

## Upgrade

1. Set the tag and run `helm upgrade`. The StatefulSet rolls one pod at a time; each pod logs `Recovering N consumers for stream` as it rejoins.
2. Confirm every stream replica is `current` through `/jsz?streams=true` (see [Troubleshooting](troubleshooting.md)), then publish, consume, and read one KV key.

Never add `prev_encryption_key`. Upgrade to `2.15.x` from `2.14.7` or later only.

## Rollback

Set the tag back and upgrade. A store written by `2.12` or later needs `2.11.9` or later; one written by `2.15` needs `2.14.7` or later. Never return to `2.10.x`. Each pod logs `Stream state magic and version mismatch` once and rebuilds its state files without loss.
