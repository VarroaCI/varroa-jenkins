# Create Your First Controller

Install a single-node evaluation control plane, create a local administrator, and start one Jenkins controller with the built-in starter bundle.

## Install Varroa

Create `values-eval.yaml`:

```yaml
auth:
  mode: local
  bffUrl: http://localhost:8080
  dashboardUrl: http://localhost:3000
dex:
  enabled: false
operator:
  replicas: 1
gateway:
  replicas: 1
  hpa:
    minReplicas: 1
bff:
  replicas: 1
  hpa:
    minReplicas: 1
nats:
  config:
    cluster:
      enabled: false
      replicas: 1
jetStreamReplicas: 1
```

```bash
helm install varroa oci://ghcr.io/varroaci/charts/varroa \
  --version <version> \
  -n varroa-system --create-namespace \
  -f values-eval.yaml
kubectl get pods -n varroa-system
```

Wait for every pod to become `Ready`.

## Create a Local Administrator

Use a temporary evaluation password and change it after signing in:

```yaml
apiVersion: varroa.dev/v1alpha1
kind: User
metadata:
  name: admin
  namespace: varroa-system
spec:
  displayName: Evaluation Administrator
  password: "<temporary-password>"
---
apiVersion: varroa.dev/v1alpha1
kind: VarroaRoleBinding
metadata:
  name: evaluation-admin
spec:
  roleRef: admin
  subjects:
    - kind: User
      name: admin
```

```bash
kubectl apply -f admin.yaml
kubectl get user admin -n varroa-system -o yaml
```

The operator hashes the password and clears `spec.password`.

## Create Jenkins

```bash
kubectl create namespace jenkins-demo
kubectl apply -f - <<'EOF'
apiVersion: varroa.dev/v1alpha1
kind: Controller
metadata:
  name: demo
  namespace: jenkins-demo
spec: {}
EOF
kubectl get controller demo -n jenkins-demo -w
```

The normal phase sequence is `Pending`, `Provisioning`, `Running`, then `Connected`. Initial image pulls and plugin installation can take several minutes.

An empty spec selects the built-in `varroa-starter` bundle and the compatible Jenkins and plugin baseline embedded in the release.

## Open the Dashboard and Jenkins

Run these port forwards in separate terminals:

```bash
kubectl port-forward -n varroa-system service/varroa-frontend 3000:8080
```

```bash
kubectl port-forward -n varroa-system service/varroa-varroa-bff 8080:8080
```

```bash
PREFIX=demo-$(kubectl get controller demo -n jenkins-demo -o jsonpath='{.metadata.uid}' | cut -c1-8)
kubectl port-forward -n jenkins-demo "svc/$PREFIX-svc" 8081:8080
```

Open <http://localhost:3000> and sign in as `admin`. Then open <http://localhost:8081>. Run the `hello-varroa` pipeline and confirm it prints `hello from Varroa`.

This pipeline verifies bundle delivery and Jenkins configuration. It runs on the Jenkins executor, so test a Kubernetes-agent workload separately before relying on agent provisioning.

## Verify Health

```bash
kubectl get controller demo -n jenkins-demo \
  -o jsonpath='{.status.phase}{"\n"}'
```

The expected value is `Connected`. If it is not, inspect `status.conditions` and follow [Troubleshooting](../operations/troubleshooting.md).

Continue with [Author a custom bundle](custom-bundle.md), [Configure ingress](../install/ingress.md), or [Configure Jenkins RBAC](../security/jenkins-rbac.md).
