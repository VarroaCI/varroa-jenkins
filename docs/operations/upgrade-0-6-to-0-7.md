# Upgrade from 0.6 to 0.7: Team to Tenant

Version 0.7.0 renames the pipelines tenant from Team to Tenant. The
product, the API, the CRDs, and the dashboard use the new word. An
installation that runs pipelines Teams, TeamAgents, or Forgejo
hibernation webhooks must run the cutover in this page.

Read the whole page before you start. The cutover has a maintenance
window. You cannot finish it in one command.

## What changes

- The CRD `Team` becomes `Tenant`. The CRD `TeamAgent` becomes
  `TenantAgent`. The field `spec.teamRef` becomes `spec.tenantRef`.
- The `Runner` field `spec.team` becomes `spec.tenant`.
- The API path `/teams/` becomes `/tenants/`. The wake URL of a
  Forgejo webhook changes with it.
- The owned Group and VarroaRoleBinding of each tenant are named
  `tenant-<name>`. They were named `team-<name>`.
- Grants that name `/teams/` or `teams:` change to `/tenants/` and
  `tenants:`.
- The agent token key ID changes from `team-agent-v1` to
  `tenant-agent-v1`. A Team agent token minted before the cutover
  fails. The agent runner mints a new token by itself.
- The instructions file of a Tenant agent is `.varroa/tenant-agent.md`.
- `kubectl get teams` no longer answers. Use `kubectl get tenants`.

The cutover keeps the data. Placements, run history, schedules, and
branch inventories stay in place. Each tenant keeps its cell.

## Before you start

1. Resolve pending approvals. List the pending approvals of the
   assistant and of each Team agent. Approve or deny each one. A
   pending approval stores a `team` argument. The renamed tool does
   not accept that argument, so an approval after the cutover fails
   and ends the session as `failed`.
2. Decide what happens to pending webhook deliveries. The cutover
   stamps each pending delivery with the new Tenant uid. The
   dispatcher then sends it to the receiver. Before the window, pause
   the pipeline and agent activity that enqueues webhooks. Wait until
   the pending count reaches zero, and keep that activity paused until
   step 5 ends. A delivery leased while step 5 changes the tenant uid
   can end as `dropped`.
3. Run the dry run. It reads the clusters and writes nothing:

   ```bash
   ./migrate-team-to-tenant.sh --core <context> --member <context> --dry-run
   ```

   The output lists each object the cutover rewrites. Check the counts
   against your installation.
4. Collect the inputs. You need the record-store DSN, the image
   reference of the 0.7.0 operator, a file with a Forgejo token, and
   the Forgejo base URL in `VARROA_FORGE_URL`.

## The maintenance window

The window starts with step 1 of the cutover and ends after step 9.

- Authorization is degraded from step 3 until the deploy in step 6.
  Step 3 rewrites the grants to `/tenants/` and `tenants:`. The 0.6
  API checks the old names. Requests that depend on a grant can fail
  with a denied response until the deploy finishes.
- Step 4 stops the 0.6 operator. No placement or reconcile runs during
  the time that the operator is stopped.
- Step 6 deploys the 0.7.0 release. Each cell restarts once. A
  pipeline build that runs on a cell during the restart can end as
  `INTERRUPTED` with the reason `cell-rolled`. Its build number is
  not reused.
- The live event stream reconnects after the dashboard restarts.

## Run the cutover

Run the script from one shell:

```bash
export VARROA_FORGE_URL=<https://forgejo.example.com>
./migrate-team-to-tenant.sh \
  --core <context> --member <context> \
  --dsn "<record-store DSN>" \
  --image <0.7.0 operator image ref> \
  --forge-token-file <path to a Forgejo token>
```

The script runs steps 1 to 5 and then prints `DEPLOY NOW`. It waits
at step 6. In a second shell, deploy the 0.7.0 chart with your normal
path, for example `helm upgrade` to chart 0.7.0. The deploy runs
migration 00041 through the recorder. The script continues when the
operator Deployment reports the image and one ready replica.

Each step checks its end state first. You can run the script again
after a failure. It resumes at the step that did not finish.

The script ends with these steps:

- Step 7 removes each old `Team` and `TeamAgent`. It keeps their
  children. It deletes the old Groups and bindings, the old label keys,
  and the old CRDs.
- Step 8 rewrites each Forgejo hibernation webhook from `/teams/` to
  `/tenants/`. The token in the URL does not change.
- Step 9 checks the end state. It exits with a non-zero status when a
  check fails.

## Verify

1. The script prints `cutover verified` and exits with status 0.
2. `kubectl get tenants -A` lists each tenant. Each pipelines tenant
   reports the `TenantPlaced` reason.
3. `kubectl get teams -A` reports that the resource type does not
   exist.
4. `GET /pipelines/<tenant>` shows the run history of each tenant. The
   run count matches the count before the cutover.
5. Push a commit to a tenant repository. The build starts through the
   renamed wake URL.
6. Each Tenant agent reports no false condition. Send one request to
   the agent, and confirm that the agent accepts it.

## Roll back

The cutover has no automatic rollback. Migration 00041 has a down
step, but the cutover changes the Kubernetes objects and the placement
fences as well. To return to 0.6, restore the record store and the
cluster objects from a backup that you took before the window.
