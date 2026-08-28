# Prerequisites

## Cluster Requirements

| Requirement | Minimum |
|---|---|
| Kubernetes | 1.30 |
| Helm | 3.x with OCI support |
| kubectl | Compatible with the cluster |
| Storage | A default `StorageClass` with dynamic provisioning |
| Permissions | Ability to install CRDs and cluster-scoped RBAC |

The default installation runs three NATS servers with persistent JetStream storage. Each Jenkins controller also receives a persistent `$JENKINS_HOME` volume. Ensure the cluster can schedule the control plane, Jenkins pods, and build agents.

No CPU or memory request is applied to Jenkins unless you set one on the `Controller` or its `ControllerClass`. Each controller's default `$JENKINS_HOME` volume is 20 GiB. Allow additional capacity for the mite sidecar, plugin installation, and agent pods.

Verify the cluster:

```bash
kubectl version
kubectl auth can-i create customresourcedefinitions.apiextensions.k8s.io
kubectl get storageclass
helm version
```

At least one StorageClass must be marked `(default)` unless every persistent component receives an explicit storage class.

## Image and Source Access

Nodes must be able to pull Varroa, NATS, Dex when enabled, Jenkins, and agent images. The operator also needs access to configured Git and OCI bundle sources. Jenkins needs access to plugin artifacts unless the in-cluster update center has complete offline coverage.

Use internal mirrors and pull secrets when public egress is unavailable. See [Air-gapped installation](air-gapped.md).

## Browser Access

For HTTPS browser access, provide:

- an ingress controller;
- DNS for the dashboard and controller routes;
- valid TLS certificates;
- OIDC, LDAP, local authentication, or Dex with a configured connector.

The dashboard uses secure login cookies. Production routes must use HTTPS. Subdomain routing also requires a cookie domain that covers the dashboard and Jenkins hosts.

Ingress and DNS are not required for an initial controller test. You can use `kubectl port-forward` instead.

## Optional Services

| Service | Required when |
|---|---|
| cert-manager | Kubernetes should issue or renew certificates. |
| external-dns | Kubernetes should manage DNS records. |
| metrics-server | Gateway and BFF HPAs should scale on CPU. |
| NetworkPolicy-capable CNI | Chart-managed network policies are enabled. |
| OpenTelemetry collector | Telemetry is exported through OTLP. |

Continue with [Installing with Helm](helm-install.md). EKS Auto Mode users should also read [Amazon EKS](aws-eks.md).
