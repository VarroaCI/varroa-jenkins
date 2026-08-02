# Fixture: OCI layout for offline localdev seeding

This directory holds a checked-in OCI image-layout (`index.json` +
`blobs/sha256/<digest>`) produced by `varroactl export plugins`. It is used by
`localdev.sh` when `LOCALDEV_OFFLINE=1` to seed the update-center store without
network access.

## Regenerate

Run `./generate.sh` from the repository root. This needs network access to
`updates.jenkins.io` (plugin metadata is resolved at export time).
