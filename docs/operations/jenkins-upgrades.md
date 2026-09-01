# Jenkins Upgrades

Varroa tracks upstream Jenkins LTS patches for the version profiles it manages. For each one it resolves the plugin closure for a newer patch before anyone touches it, checks that closure against every controller running that line, and hands an operator a `ProfileCandidate` to review and promote. Nothing rolls automatically: promotion is a deliberate act, and even after promotion a cluster can hold the actual rollout on a dial.

## What the tracker watches

A ticker reconciler checks each `lts`-channel `JenkinsVersionProfile` whose `spec.version` names a two-segment line (for example `2.555`) against the upstream update-center metadata Varroa already resolves plugins from. When it finds a patch newer than the profile's current `resolveVersion`, it builds a `ProfileCandidate` for that profile. Discovering a brand-new LTS line, or an EOL date on a line already in use, produces an `upgrade.upstream.observed` activity event only: informational, no candidate, no CRD write.

The tracker never mutates a profile and never rolls anything by itself. It always checks upstream at updates.jenkins.io directly, with no in-cluster fallback source. When upstream can't be reached, whether from an air-gapped install, restricted egress, or a transient outage, the tick simply produces no new candidates and retries on the next one. Existing candidates and the profiles already in the cluster are unaffected. This is expected steady-state behavior in an air-gapped cluster, not a failure to alert on.

## The `ProfileCandidate` lifecycle

Each candidate is named `<profile-name>-<resolveVersion with dots turned into dashes>` and carries a reference to the target profile, the version it was discovered at, the version it would promote to, and a ConfigMap holding the resolved plugin closure. Its `status.phase` moves through:

- **Pending**: created, still being resolved and checked.
- **Ready**: every gating check passed; open for promotion.
- **Promoted**: an operator promoted it; the target profile now runs this version.
- **Failed**: a gating check failed; see the candidate's conditions for which one.
- **Superseded**: a newer candidate for the same profile was created or promoted while this one was still open.

Six conditions record how a candidate got there:

| Condition | Meaning |
|---|---|
| `Resolved` | The candidate's target version and profile reference are valid. |
| `ClosureClean` | The transitive plugin closure resolved without conflicts. |
| `CoreCompatOK` | The resolved closure's plugin baseline is compatible with the candidate's Jenkins core. |
| `PluginsServable` | Every pinned plugin is servable from the in-cluster update center, where one is enabled. An unreachable or air-gapped update center blocks this condition actionably (the same shape as the existing `WaitingForUpdateCenter` condition on provisioning) rather than failing silently. |
| `PreflightChecked` | The advisory fleet pre-flight has run: every controller currently resolving to the target profile's line was checked against the candidate's plugin set, using the same pin check that runs against the active set. |
| `Promoted` | Set only once an operator has promoted the candidate. |

The pre-flight is **advisory, not blocking**. A candidate can reach `Ready` and be promoted even when the pre-flight reports controllers whose bundles would conflict with the new plugin set. Those findings surface as `pluginPinConflict.detected` activity events and in the candidate's status summary, so an operator can weigh the risk before promoting, but Varroa does not withhold the candidate over them.

## Promoting a candidate

Promotion copies a `Ready` candidate's resolved content into its target `JenkinsVersionProfile` and pluginset ConfigMap, exactly as if an operator had hand-edited the profile. The existing profile-resolution and plugin-rollout path is unchanged by this. Once promoted, any other open candidate for the same profile moves to `Superseded`.

Promote via the API:

```bash
curl -X POST -H "Authorization: Bearer $TOKEN" \
  https://<varroa-host>/api/v1/version-candidates/<name>/promote
```

or via the `promote_version_candidate` MCP tool. Both list and inspect candidates first (`GET /api/v1/version-candidates`, `GET /api/v1/version-candidates/{name}`, and their `list_version_candidates`/`get_version_candidate` MCP equivalents) to review a candidate's phase, conditions, and pre-flight summary before acting.

## Controlling the rollout: `upgradePolicy`

`ProvisioningDefaults.spec.upgradePolicy` is a cluster-wide dial with two values:

- **`auto`** (default): a promoted profile's new version rolls out to Connected controllers on that line exactly as any profile change does today, through the existing version-roll gate.
- **`manual`**: a Connected controller whose effective profile has advanced past the version it's currently running gets an `UpgradePending` condition instead of rolling. The condition clears once the controller actually rolls, or once the policy flips back to `auto`.

`upgradePolicy` only ever holds a controller that's already running; new or re-provisioning controllers always deploy the currently promoted revision regardless of the dial. It also only holds profile-driven advances: a controller with an explicit `resourceOverlay` image override, or one blocked by the core-compatibility gate, is unaffected.

`manual` is a hold, not a release mechanism: nothing on the dial itself performs the held rollout for you. Releasing a held controller means flipping the dial back to `auto`, changing the controller's effective image by some other means, or running the `upgrade` brood verb described below. That verb gives per-controller control over exactly this release step without changing anything documented above.

## Releasing a rollout across a fleet: the `upgrade` brood verb

A brood operation with `spec.action.verb: upgrade` runs the release step across a set of controllers in the same waved fashion as any other brood verb. See [Brood operations](brood-operations.md) for wave sizing and pause/resume mechanics. It applies only to Connected controllers; anything else is skipped, not failed.

`upgrade` supports two granularities, chosen by whether `spec.action.upgrade.targetVersion` is set:

- **Release** (`targetVersion` omitted): each target keeps whatever version and profile it's already resolving to. The operation computes the image that profile currently resolves to and hands it to the target's version-roll gate, the same gate `upgradePolicy: manual` holds behind. This is how a `manual` cluster releases some or all of its held controllers without flipping the cluster-wide dial back to `auto`.
- **Bulk version move** (`targetVersion` set): every target moves to the named version instead of whatever its own profile currently resolves to. This value is validated at admission against the cluster's `JenkinsVersionProfile`s. It must match a profile's version exactly, match a profile's LTS line (for example `2.555.1` against a `2.555` profile), or be `""`/`"lts"` to fall back to the embedded plugin baseline. A `targetVersion` that matches none of these fails the whole operation before any target is touched.

Either granularity runs the same per-target plugin-pin pre-flight before writing anything. The target's own unresolved bundle content is checked against the resolved profile's plugin set, and the profile's `PluginsServable` condition is checked when it has an open `ProfileCandidate`. A target that fails either check is marked Failed with a reason naming which one, and nothing is written for it. The rest of the operation's targets are unaffected, and the failure surfaces as the usual `pluginPinConflict.detected` activity event.

A released target isn't considered done the moment the write lands. When the controller has a version to move to, the operation waits for it to leave `Connected`/`Running` and come back before marking it Succeeded, the same evidence-based completion every other brood verb uses. A target already running the released version has nothing to roll, so it succeeds straight away. That makes the verb safe to re-run over a fleet that is already partly upgraded.

## Reviewing and rolling out a candidate in the dashboard

Everything above is also driven from the dashboard, not just the API. Settings ▸ Versions lists each cluster's `JenkinsVersionProfile`s alongside any open `ProfileCandidate`s for them, split into an active table and a collapsed history of ones already promoted or superseded. Each active row shows the six gating conditions as chips and, once a pre-flight has run, how many controllers it flagged and why.

Promote is only enabled once a candidate reaches `Ready`, and clicking it asks for confirmation first. The confirmation copy tracks the cluster's `upgradePolicy`, so the operator sees the actual consequence, not just a version number, before committing to it. Under `auto` the confirmation reads: "This will roll every controller pinned to line `<line>` through the version-roll gate." Under `manual` it reads: "This will hold every controller pinned to line `<line>` with `UpgradePending` until released."

Once a `manual`-policy promotion lands, a held controller's detail page shows an "Upgrade pending release" chip in place of the usual roll-in-progress indicator, with the candidate's own status message underneath. If the same controller is also blocked for an unrelated reason, such as an incompatible core, the blocking banner takes over and the pending-release chip stays hidden. A controller is never shown as both blocked and merely pending at once.

Releasing held controllers is a brood operation like any other. Pick the `upgrade` verb from the brood operations dashboard, then choose "Release held upgrade" to let each target roll to whatever its own profile already resolves to. Alternatively, choose "Move to version" to send every target to one version or line, regardless of what each is individually pinned to. For "Move to version," pick a "Latest LTS" shortcut, an exact version from the picker, or a line typed by hand; a blank entry falls back to the baseline pin. The run's detail page shows each target's progress and any per-target plugin-pin failure it hit along the way. A whole-operation failure, most commonly a `targetVersion` that couldn't be resolved against any profile, is called out on that same page next to the phase and who started the run. It doesn't only show up as a raw reason code in the API response.
