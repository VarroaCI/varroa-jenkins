# Using the varroactl CLI

`varroactl` is a command-line client for managing Varroa controllers from the
terminal. It provides a kubectl-style interface for listing, creating, editing,
and deleting controllers without the Varroa dashboard.

## Prerequisites

- A running Varroa deployment ([Helm install](../install/helm-install.md))
- Access to the Varroa API (the `--server` URL)
- Authentication credentials — either an API key
  ([API keys](../security/api-keys.md)) or a local/ldap user account

## Build the CLI

```bash
# From the repository root.
make build-cli
```

Produces `bin/varroactl`. Check the version:

```bash
bin/varroactl version --client
```

## Log in

### Browser flow (recommended)

```bash
bin/varroactl login --server https://varroa.example.com
```

Opens your default browser. Authenticate, approve the key request, and the
token is stored automatically.

### API key

If you already have a `vk_` token:

```bash
bin/varroactl login --server https://varroa.example.com --api-key vk_...
```

## List Controllers

```bash
bin/varroactl get controllers
```

Add `-n <namespace>` to filter, or `-A` for all namespaces.

## Describe a Controller

```bash
bin/varroactl describe controller <namespace>/<name>
```

Shows a human-readable summary: version, power state, endpoint, bundle
reference, mite status, and pending actions.

## Working with multiple clusters

In multi-cluster environments, controller commands accept `--cluster` to target
a specific cluster. By default, lists show controllers from all clusters with a
`CLUSTER` column. Use `--all-clusters` to force an unfiltered aggregate, or
`get clusters` to list available clusters. See
[Multi-cluster](../varroactl.md#multi-cluster) in the reference.

## Where to go next

- **Full CLI reference:** [varroactl.md](../varroactl.md) — login flows,
  contexts, all commands, output formats, exit codes, and key lifecycle.
- **API keys:** Manage keys via the [dashboard](../security/api-keys.md)
  or the `varroactl logout` / `varroactl logout --revoke` commands.
- **Streaming & MCP:** Real-time logs, events, activity, watch, and the MCP
  bridge — see the [CLI reference](../varroactl.md#streaming).
