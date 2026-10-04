# varroactl CLI

`varroactl` manages Varroa from a terminal. Command help is exhaustive:

```bash
varroactl --help
varroactl <command> --help
```

## Install and connect

Every release publishes `varroactl` binaries for Linux and macOS on amd64 and
arm64. Download the archive for your platform from the
[latest release](https://github.com/VarroaCI/varroa-jenkins/releases/latest),
verify it, and put it on your path:

```bash
# Resolve the newest release, or set VERSION explicitly to pin one.
VERSION=$(curl -fsSL https://api.github.com/repos/VarroaCI/varroa-jenkins/releases/latest \
  | grep -m1 '"tag_name"' | cut -d'"' -f4)
VERSION=${VERSION#v}
OS=linux   # or darwin
ARCH=amd64 # or arm64

curl -fsSLO "https://github.com/VarroaCI/varroa-jenkins/releases/download/v${VERSION}/varroactl_${VERSION}_${OS}_${ARCH}.tar.gz"
curl -fsSLO "https://github.com/VarroaCI/varroa-jenkins/releases/download/v${VERSION}/SHA256SUMS"

# macOS ships shasum rather than GNU sha256sum.
if command -v sha256sum >/dev/null; then
  sha256sum --ignore-missing -c SHA256SUMS
else
  grep " ./varroactl_${VERSION}_${OS}_${ARCH}.tar.gz$" SHA256SUMS | shasum -a 256 -c -
fi

tar -xzf "varroactl_${VERSION}_${OS}_${ARCH}.tar.gz"
sudo install -m 0755 varroactl /usr/local/bin/varroactl
varroactl version --client
```

Or build it from a source checkout:

```bash
make build-cli
./bin/varroactl version --client
```

Sign in with the browser flow:

```bash
varroactl login --server https://varroa.example.com
```

For automation, pass an API key through standard input:

```bash
printf '%s\n' "$VARROA_API_KEY" | varroactl login \
  --server https://varroa.example.com --api-key -
```

Password accounts use `--username` and `--password-stdin`. OIDC users use the browser flow. See [API keys](security/api-keys.md) for key lifecycle and handling.

## Use contexts

A context stores the server, API key, default namespace, and default cluster. It lets one workstation safely target multiple installations.

```bash
varroactl config set-context production \
  --server https://varroa.example.com --namespace team-a --cluster core
varroactl config use-context production
varroactl config get-contexts
varroactl config current-context
```

Use `--context`, `--server`, `--namespace`, or `--cluster` for one command without changing the saved context. Flag values override environment variables, which override the config file. `VARROACTL_SERVER`, `VARROACTL_API_KEY`, `VARROACTL_CONTEXT`, and `VARROACTL_CONFIG` provide environment-based configuration.

## Common workflows

```bash
# Inspect controllers.
varroactl get controllers -n team-a
varroactl describe controller team-a/controller-a

# Apply a controller definition and observe it.
varroactl create controller -f controller.yaml
varroactl watch -n team-a

# Perform lifecycle work.
varroactl restart controller team-a/controller-a
varroactl power controller team-a/controller-a stopped

# Preview a fleet operation before creating it.
varroactl broodop run restart --selector tier=canary --dry-run

# Trigger a pipeline run and list its builds.
varroactl run team-a/checkout-api --param ENVIRONMENT=staging
varroactl builds team-a/checkout-api

# Check a pipeline template marker before it is committed.
varroactl lint marker --team team-a --marker .varroa/pipeline.yaml

# List the fleet locks, then force-release a releasing lock that no live pod holds.
varroactl locks list --team team-a -o wide
varroactl locks release team-a/deploy-prod --run <run id> --reason "<why>"
```

Use [Controller lifecycle operations](operations/lifecycle.md) for state changes, [Brood operations](operations/brood-operations.md) for fleet actions, [Brood schedules](operations/brood-schedules.md) for recurring work, and the [pipelines tutorial](tutorials/pipelines.md) to trigger a run, read a log, and download an artifact.

`run` splits `<team>/<job>` on the first slash. `<job>` may omit or repeat the team's folder segment; both forms reach the same job. It percent-encodes the job path, so slashes reach the server intact. `--param` is repeatable and takes `key=value`. A repeated key is a usage error. `run` mints one client token per invocation, so a retried request runs one build. It prints the run identifier on success. Add `--follow` to wait for the run to appear and stream its log until the run ends. The command exits non-zero when the run's result is not a success.

`lint marker` reads a [pipeline template](config/pipeline-templates.md) marker file, resolves it against the named team's delivered templates, and prints each finding's severity, path, and message. `--marker` defaults to `.varroa/pipeline.yaml`. The command exits zero when the marker is valid and non-zero otherwise, so a pre-commit hook or a pipeline step can gate on it.

## Work across clusters

Set a context default with `varroactl config set-cluster <name>`, or pass `--cluster <name>` to commands that target a cluster. List membership with:

```bash
varroactl get clusters
```

Selector-based brood operations can include `--clusters <names>` or `--clusters all`. Explicit multi-cluster names use `cluster/namespace/name`. See [Adding a member cluster](install/multi-cluster.md).

## Output, streams, and safety

Use `-o table`, `wide`, `json`, `yaml`, or `name`. Follow controller state with `watch`, `logs`, `events controller`, `mite`, or `activity`.

`logs` also reads a pipeline run's log. Pass a controller argument (`NS/NAME`) or a run identifier (a UUIDv7). A run identifier holds no `/`, so the two forms never collide. Without `-f` the command prints the log once and exits. With `-f` it streams the log until the run ends. After a reconnect, it continues after the last line it printed:

```bash
varroactl logs 0192f0a1-1c2d-7abc-9def-0123456789ab -f
```

`edit`, `patch`, and `power` can encounter field-ownership conflicts. Use `--force` only after reviewing [field conflict recovery](operations/lifecycle.md#resolve-field-conflicts). For plugin-pack and air-gap workflows, use [Plugin packs](config/plugin-packs.md) and [Air-gapped installation](install/air-gapped.md). Use `varroactl logout --revoke` when a stored key must be revoked.

## Scale a cell pool

`cellpool` is a cluster-scoped noun (alias `cellpools`) serving `get` and `describe`, plus the
top-level verb `scale`:

```bash
varroactl get cellpools
varroactl describe cellpool jenkins-lts
varroactl scale cellpool jenkins-lts --warm 3 --max-cells 8
varroactl scale cellpool lts-2-570 --cluster east --warm 2
```

`get cellpools` prints `CLUSTER`, `NAME`, `PROFILE`, `WARM`, `PLACED`, `FREE`, and `READY` for
every cluster. `--cluster` narrows the list. A warning names a cluster that does not answer.
`describe` adds the cluster, the tenant list, and the `PoolReady` condition. A command on one pool
uses the core cluster when you give no `--cluster`. Every cell-pool command refuses `--namespace` and
`--all-namespaces`, because `CellPool` is cluster-scoped.

`scale` sends only the flags you pass, so an unnamed dial keeps its stored value. A call with
neither `--warm` nor `--max-cells` is a usage error, and so is a negative value; both are reported
before any request.

There is no `create cellpool` and no `delete cellpool`. Create and delete a pool with `kubectl` or
through GitOps. See [Cells and pipelines tenants](operations/cells.md).

## Register an external runner

`runners` is an ordinary cluster-scoped noun (alias `rnr`): `get`, `describe`, `create`,
`delete`, `edit`, and `patch` all work. `edit` and `patch` send a JSON merge patch, not a
full-replace `PUT`.

```bash
varroactl get runners
varroactl get runners win-1 -o yaml
varroactl patch runners win-1 -p '{"spec":{"numExecutors":2}}'
```

The `runner` command group adds the registration workflow: `runner register` creates a
`Runner`, waits for the operator to fill its status, and prints the next step; `runner list`
and `runner delete` are thin aliases over the noun verbs.

```bash
varroactl runner register win-1 --tenant team-a --remote-fs /var/j
varroactl runner list --tenant team-a
varroactl runner delete win-1
```

See [External runners](operations/runners.md) for the full field reference, both launcher
classes, and platform walkthroughs.

## Reach a headless controller

A [headless controller](security/headless-controllers.md) has no Ingress and no external URL.
`break-glass` grants a short-lived admin session through a local proxy.

```bash
varroactl break-glass <namespace>/<name> --reason "<why>"
```

| Flag | Default | Meaning |
|---|---|---|
| `--reason` | none, required | Why you need the session. Recorded on the audit event. At most 256 characters. |
| `--ttl` | `15m` | How long the token stays valid. |
| `--target` | `http://127.0.0.1:8080` | The local end of your `kubectl port-forward`. |
| `--listen` | `127.0.0.1:0` | Where the proxy listens. Must be a loopback address. |
| `--cluster` | the context default | The cluster the controller runs on. Break-glass reaches only the core cluster. |
| `-n`, `--namespace` | the context default | The controller's namespace. |

Run the command. In another terminal, run the `kubectl port-forward` command it prints. Open
the proxy URL it prints in a browser. The session ends when the token expires or you press
Ctrl-C.
