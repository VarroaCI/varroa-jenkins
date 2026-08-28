# Air-gapped example

A worked minimal setup for a cluster with no outbound internet. The full
guide, including seed-pack workflows and troubleshooting, is
[docs/install/air-gapped.md](../../docs/install/air-gapped.md).

The order matters: plugins are exported on a connected machine first, so the
air-gapped cluster never needs to reach `updates.jenkins.io`.

1. **Export a plugin seed on a machine with internet access.** Pick the
   version profile your controllers will run and export its exact pinned
   plugin set:

   ```bash
   varroactl export plugins \
     --profile jenkins-version-2-555 \
     --to oci://registry.internal.example.com/varroa/plugin-pack:jenkins-version-2-555
   ```

   For sneakernet transfer, export `--to tar:///tmp/seed.tar.gz` instead and
   import it on the other side with `varroactl import`.

2. **Mirror the chart and images.** Pull the chart once on the connected
   machine (`helm pull oci://ghcr.io/varroaci/charts/varroa`) and push it to
   your internal registry, and mirror the component images referenced in
   `values-airgap.yaml` the same way. Jenkins itself needs the same
   treatment: mirror the Jenkins core and inbound-agent images and point
   `ProvisioningDefaults` at your registry — see
   [docs/install/air-gapped.md](../../docs/install/air-gapped.md).

3. **Install the chart** with the update center enabled and pull-through off:

   ```bash
   helm install varroa oci://registry.internal.example.com/varroa/charts/varroa \
     --namespace varroa-system --create-namespace \
     --values values-airgap.yaml
   ```

   Add your auth values (see `../oidc-values.yaml` or
   `../local-auth-values.yaml`) — air-gap and auth choices are independent.

4. **Create a controller** pinned to the seeded version:

   ```bash
   kubectl apply -f controller.yaml
   ```

Controllers stay in `WaitingForUpdateCenter` rather than silently reaching
for the internet if the update center is missing plugins their profile pins —
seed first, then provision.
