# Brood Operations

Use a `BroodOperation` to run one action against a snapshot of controllers. Preview any disruptive operation first.

```bash
varroactl broodop run restart --selector tier=canary --dry-run
varroactl broodop run restart --selector tier=canary --max-parallel 2 --watch
```

Choose exactly one target form: `--names` or `--selector`. A selector can be limited with `--namespaces` and `--clusters`; use `all` only from an operator namespace. Filters for `phase`, `version`, and `bundle` further narrow selector results.

## Set execution policy

| Setting | Default | Effect |
|---|---:|---|
| `--max-parallel` | `1` | Maximum concurrent targets. |
| `--order` | `rolloutWave` | Complete a rollout wave before the next. `name` treats targets as one wave. |
| `--failure-policy` | `FailTidy` | Stop new dispatch after a failure and wait for active targets. |
| `--ttl` | seven days | Retain the completed operation. Use `0` to retain it. |

`FailFast` stops at the first failure. `FailAtEnd` attempts every target. Supported actions are `restart`, `reprovision`, `reconcile`, `stop`, `start`, and `upgrade`. `reconcile` skips stopped and hibernated controllers. `start` wakes either state.

`executeGroovy` runs arbitrary Jenkins code. Configure and authorize it only through [executeGroovy security](../security/execute-groovy.md).

`upgrade` applies only to `Connected` controllers; anything else is skipped, not failed. With `spec.action.upgrade.targetVersion` set, every target moves to that version; left unset, each target releases whatever version its own profile already resolves to, which is how a `manual` upgrade policy hold gets released without flipping the cluster-wide dial. See [Jenkins upgrades](jenkins-upgrades.md) for both granularities and the plugin-pin pre-flight that runs before either one writes anything.

## Monitor and control a run

```bash
varroactl broodop get
varroactl broodop describe <namespace>/<name>
varroactl broodop watch <namespace>/<name>
varroactl broodop suspend <namespace>/<name>
varroactl broodop suspend <namespace>/<name> --off
varroactl broodop cancel <namespace>/<name>
```

Phases are `Pending`, `Running`, `Suspended`, `Succeeded`, `Failed`, and `Canceled`. Only the final three are terminal. Suspension prevents new dispatch; running targets continue. Targeting and execution settings cannot be changed after creation.

For a recurring operation, use [Brood schedules](brood-schedules.md). For staged bundle changes, use [Rollout waves](rollout-waves.md).
