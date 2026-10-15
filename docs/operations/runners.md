# External Runners

A `Runner` binds one external machine, a workstation, a build server, or a virtual machine, to
one pipelines tenant. It attaches a Jenkins agent that the tenant's builds can select with a
label.

A `Runner` is not a Kubernetes agent. Varroa schedules no pod for it and gives it no cloud
credential. It belongs to exactly one tenant. Varroa's agent fence blocks every other tenant's
build from it.

## Launcher classes

`spec.launcher` selects the connection direction:

| Launcher | Direction | Preferred |
|---|---|---|
| `ssh` | The cell dials the runner. | Yes |
| `inbound` | The runner dials the [door](../security/runner-tokens.md). | No |

Prefer `ssh`. The cell connects to the runner directly, so its control traffic never crosses the
door proxy. An `ssh` runner also holds no Varroa token: the operator mints one for `inbound`
runners only.

Use `inbound` when the cell cannot reach the runner host, such as a machine behind a firewall
with no listening SSH server.

An `ssh` runner's host key is trusted on first use. A cell keeps no `known_hosts` file across a
roll, so the cell records the key it sees at the first connection of each pod. The operator's
per-runner egress rule pins the cell to the runner's address on its port.

## The Runner fields

| Field | Rule |
|---|---|
| `team` | Required. Immutable. Names the owning `Team`. |
| `launcher` | Required. Immutable. `ssh` or `inbound`. |
| `labels` | Optional. Suffixes only. The operator prefixes each with `<team>-`. |
| `remoteFS` | Required. The agent's work directory on the machine. |
| `numExecutors` | Optional. Default `1`. |
| `host` | Required for `ssh`. Forbidden for `inbound`. The SSH target. |
| `port` | Optional for `ssh`. Default `22`. Forbidden for `inbound`. |
| `credentialRef` | Required for `ssh`. Forbidden for `inbound`. Names a Secret in the team's agent namespace. |
| `javaPath` | Optional. `ssh` only. An explicit java binary path. |

None of `remoteFS`, `host`, or `javaPath` may contain `${`. Varroa carries these values into
Jenkins configuration as literal text and refuses one that looks like an unresolved
substitution.

`status` reports `nodeName` (`<team>-<runner name>`), `cell`, `cellNamespace`, `token`
(`inbound` only), `connected`, `lastSeen`, and a `Ready` condition. `connected` reflects the
most recent liveness probe; Varroa keeps no runner history log.

## Register a runner

```bash
varroactl runner register <name> --tenant <team> --remote-fs <path> [flags]
```

creates the object, then waits up to 60 seconds for the operator to fill its status.

| Flag | Required | Meaning |
|---|---|---|
| `--tenant` | Yes | The owning team. |
| `--remote-fs` | Yes | The work directory on the machine. |
| `--launcher` | No | `ssh` or `inbound`. Default `inbound`. |
| `--labels` | No | Comma-separated label suffixes. |
| `--host` | `ssh` only | The SSH target. |
| `--port` | No | The SSH port. Default `22`. |
| `--credential` | `ssh` only | The Secret name. |
| `--executors` | No | Executor count. Default `1`. |
| `--java-path` | No | An explicit java binary path. |

`<name>` becomes the runner's object name and half of its rendered node name,
`<team>-<name>`. It must be a DNS-1123 label: lowercase letters, digits, and hyphens only. The
command checks the name before it contacts the server.

For an `inbound` runner, the command prints the exact launch command once the token is minted.
For an `ssh` runner, it prints the Secret the tenant must create instead: its name, its required
label, and its three data keys.

## Register a Windows SSH runner

1. Enable the OpenSSH Server feature on the Windows machine and start the `sshd` service.
   Confirm the host accepts a connection on its SSH port.
2. Create the SSH credential Secret in the tenant's agent namespace:

   ```yaml
   apiVersion: v1
   kind: Secret
   metadata:
     name: win-1-ssh-key
     namespace: team-payments
     labels:
       varroa.dev/runner-credential: "true"
   stringData:
     username: jenkins
     privateKey: |
       <the private key in OpenSSH PEM form>
   ```

3. Register the runner. An OpenSSH session on Windows Server often omits the JDK from its
   default path, so `spec.javaPath` is usually required:

   ```bash
   varroactl runner register win-1 --tenant payments --launcher ssh \
     --host 10.20.0.5 --credential win-1-ssh-key \
     --remote-fs 'C:\J' \
     --java-path 'C:\Program Files\Eclipse Adoptium\jdk-21\bin\java.exe' \
     --labels windows,msbuild
   ```

4. Confirm the connection:

   ```bash
   varroactl get runners win-1 -o yaml
   ```

   Expect `status.connected: true`.

## Register a macOS SSH runner

1. Enable Remote Login under System Settings, Sharing. Confirm the host accepts a connection on
   its SSH port.
2. Log the build user in through Screen Sharing, or set it to auto-login, before registering.
   The SSH login user needs a real login session: Xcode tooling needs a running user session, and
   a bare SSH shell alone does not create one.
3. Create the SSH credential Secret, the same shape as the Windows example above.
4. Register the runner with a macOS-form remote directory:

   ```bash
   varroactl runner register mac-1 --tenant payments --launcher ssh \
     --host 10.20.0.9 --credential mac-1-ssh-key \
     --remote-fs /Users/jenkins/agent --labels macos,xcode
   ```

5. Confirm the connection:

   ```bash
   varroactl get runners mac-1 -o yaml
   ```

   Expect `status.connected: true`.

## Register an inbound runner

1. Register the runner. `--launcher` defaults to `inbound`:

   ```bash
   varroactl runner register lin-1 --tenant payments --remote-fs /home/jenkins/agent
   ```

2. The command waits for the operator to mint a token, then prints the launch command:

   ```
   java -jar agent.jar -url https://<door host>/runners/<token>/ \
     -name payments-lin-1 -secret <token> -webSocket -workDir /home/jenkins/agent
   ```

3. On the machine, fetch the agent jar through the door and run the printed command:

   ```bash
   curl -fsSL -o agent.jar "https://<door host>/runners/<token>/jnlpJars/agent.jar"
   java -jar agent.jar -url "https://<door host>/runners/<token>/" \
     -name payments-lin-1 -secret <token> -webSocket -workDir /home/jenkins/agent
   ```

4. Confirm the connection:

   ```bash
   varroactl get runners lin-1 -o yaml
   ```

   Expect `status.connected: true`.

The `-secret` value is the Varroa runner token, not a Jenkins agent secret. The door ignores it
and injects the real Jenkins secret itself. See [Runner tokens](../security/runner-tokens.md).

## Elastic clouds differ from runners

An EC2, ec2-fleet, or Azure VM cloud is a Jenkins cloud, not a `Runner`. A tenant bundle cannot
declare one: tenant isolation strips Jenkins configuration from every tenant bundle. Only the
cell's own composed bundle, authored by a platform operator, can add one.

Such a cloud must follow four rules:

1. Its credential uses `scope: SYSTEM`.
2. Every instance or template carries the reserved label `varroa-tenant-<team>`, written by the
   platform operator by hand.
3. Its launcher is the plugin's SSH launcher, so the cell dials out.
4. Its name is not a Team name.

An instance missing rule 2 is blocked for every tenant: the agent fence fails closed on an
unlabelled node.

## Labels and the tenant fence

The fence reads one thing on a node: the reserved label `varroa-tenant-<team>`. A node carrying
no reserved label is blocked for every tenant.

Varroa renders that label on every runner node before any other label, so a tenant cannot forge
it. A declared label named `varroa-tenant-x` is itself prefixed to `<team>-varroa-tenant-x` and
carries no reserved token.

Every runner node also carries `<team>-runner`, so a build can select "any runner of my team"
with no declared label. Each entry in `spec.labels` becomes `<team>-<label>`. The `<team>-`
prefix is a naming convention only. It grants no access.

## Cell egress for an SSH host

The operator adds one `NetworkPolicy` egress rule per `ssh` runner of a placed tenant, to
`spec.host` on `spec.port`. An IP host needs no resolution. A hostname is resolved on every
reconcile, and every resolved address gets its own rule. A hostname that resolves to nothing
gets no rule for that one runner; every other runner's rule is unaffected.

Prefer an IP address, or a small and stable set of addresses, for `spec.host`. A hostname whose
answer changes often churns the cell's `NetworkPolicy` on every reconcile. See
[Isolate cells](../install/network-policies.md#isolate-cells).

## Cell rolls and tenant moves

A cell pod carries no durable state. When it rolls, an `ssh` runner relaunches on its own: the
cell dials out again, and its retention strategy is `always`. An `inbound` runner reconnects on
its own, and the door fetches a fresh Jenkins agent secret. Neither case needs re-registration
or any operator action.

When a tenant moves to another cell, its runners move with it. The next reconcile removes their
nodes from the old cell and adds them to the new one. A build already running on a moved
runner's node fails, and the ledger marks that run interrupted.

Deleting a `Runner` needs no cleanup step. The next reconcile regenerates the cell's whole node
and credential list without it.

## Troubleshoot a runner

Every `Ready` reason a `Runner` can report:

| Reason | Status | Meaning | Action |
|---|---|---|---|
| `Connected` | True | The last liveness probe found the node online. | None. |
| `TeamNotFound` | False | `spec.team` names no `Team`. | Fix the name, or create the `Team`. |
| `TeamNotPipelines` | False | The named `Team` sets no `spec.pipelines`. | Add `spec.pipelines`. See [Multi-tenancy](multi-tenancy.md#onboard-a-pipelines-tenant). |
| `LauncherUnsupported` | False | `spec.launcher` is neither `ssh` nor `inbound`. | Fix `spec.launcher`. |
| `TeamNameUnsupported` | False | The `Team` name or the `Runner` name is not a DNS-1123 label. | Rename the object. A dotted name is the usual cause. |
| `AwaitingPlacement` | False | The `Team` is not yet placed on a cell. | Wait, or diagnose placement. See [Cells](cells.md#troubleshoot-placement). |
| `CredentialMissing` | False | The Secret named by `credentialRef` is absent, or lacks the `varroa.dev/runner-credential: "true"` label. | Create or label the Secret. |
| `CredentialMalformed` | False | The Secret is missing `username` or `privateKey`, or a value contains `${`. | Fix the Secret's data. |
| `Offline` | False | The last liveness probe found the node offline, or none has succeeded yet. | Check the machine and its network path to the cell. |
