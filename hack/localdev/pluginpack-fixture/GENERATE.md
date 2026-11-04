# Fixture: OCI layout for offline localdev seeding

This directory holds a checked-in OCI image-layout (`index.json` +
`blobs/sha256/<digest>`). `varroactl export plugins` writes the layout.
`localdev.sh` imports it when `LOCALDEV_OFFLINE=1`. The import seeds the
update-center store without network access.

## Profile name

The pack names `jenkins-version-<baseline>`, with each dot changed to a dash.
For example, the baseline `2.588` gives `jenkins-version-2-588`. The script
reads the baseline from `internal/controller/pluginlock/lock.yaml`.

## Regenerate

Run `hack/localdev/pluginpack-fixture/generate.sh` from the repository root.
The script needs Go and network access to `updates.jenkins.io`. An old pin
can also need `repo.jenkins-ci.org`.

Run the script again after each baseline change.

The PR **Checks** job validates this directory. The job fails on a
non-baseline profile.
