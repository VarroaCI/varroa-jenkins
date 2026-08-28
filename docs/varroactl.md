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

For local or LDAP authentication, use `--username` and `--password-stdin`. OIDC installations require the browser flow. See [API keys](security/api-keys.md) for key lifecycle and handling.

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
```

Use [Controller lifecycle operations](operations/lifecycle.md) for state changes, [Brood operations](operations/brood-operations.md) for fleet actions, and [Brood schedules](operations/brood-schedules.md) for recurring work.

## Work across clusters

Set a context default with `varroactl config set-cluster <name>`, or pass `--cluster <name>` to commands that target a cluster. List membership with:

```bash
varroactl get clusters
```

Selector-based brood operations can include `--clusters <names>` or `--clusters all`. Explicit multi-cluster names use `cluster/namespace/name`. See [Adding a member cluster](install/multi-cluster.md).

## Output, streams, and safety

Use `-o table`, `wide`, `json`, `yaml`, or `name`. Follow controller state with `watch`, `logs`, `events controller`, `mite`, or `activity`.

`edit`, `patch`, and `power` can encounter field-ownership conflicts. Use `--force` only after reviewing [field conflict recovery](operations/lifecycle.md#resolve-field-conflicts). For plugin-pack and air-gap workflows, use [Plugin packs](config/plugin-packs.md) and [Air-gapped installation](install/air-gapped.md). Use `varroactl logout --revoke` when a stored key must be revoked.
