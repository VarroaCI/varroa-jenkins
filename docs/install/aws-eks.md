# Install on Amazon EKS Auto Mode

Prepare EKS Auto Mode storage and ingress, then use the standard [Helm installation](helm-install.md).

## Prepare the Cluster

Use Kubernetes 1.30 or later and enable Auto Mode compute, block storage, and load balancing. Place nodes and load balancers across at least two availability zones. Configure `kubectl` with an identity that can install CRDs and cluster-scoped RBAC.

```bash
aws eks update-kubeconfig --name <cluster-name> --region <region>
kubectl get nodes
```

Tag public subnets with `kubernetes.io/role/elb=1` and private subnets with `kubernetes.io/role/internal-elb=1` when those tags are not already present.

## Create a Default StorageClass

EKS Auto Mode does not create a StorageClass. Use its EBS provisioner:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: auto-gp3
  annotations:
    storageclass.kubernetes.io/is-default-class: "true"
provisioner: ebs.csi.eks.amazonaws.com
volumeBindingMode: WaitForFirstConsumer
allowVolumeExpansion: true
allowedTopologies:
  - matchLabelExpressions:
      - key: eks.amazonaws.com/compute-type
        values: [auto]
parameters:
  type: gp3
  encrypted: "true"
```

```bash
kubectl apply -f storageclass.yaml
kubectl get storageclass
```

`WaitForFirstConsumer` places an EBS volume in the selected pod's availability zone.

## Create an ALB IngressClass

Use `IngressClassParams` to share one ALB and attach an ACM certificate:

```yaml
apiVersion: eks.amazonaws.com/v1
kind: IngressClassParams
metadata:
  name: varroa-alb
spec:
  scheme: internet-facing
  group:
    name: varroa
  certificateARNs:
    - arn:aws:acm:<region>:<account-id>:certificate/<certificate-id>
---
apiVersion: networking.k8s.io/v1
kind: IngressClass
metadata:
  name: alb
spec:
  controller: eks.amazonaws.com/alb
  parameters:
    apiGroup: eks.amazonaws.com
    kind: IngressClassParams
    name: varroa-alb
```

```bash
kubectl apply -f ingress-class.yaml
kubectl get ingressclass alb
```

The shared group is a trust boundary. Only namespaces whose administrators may share an ALB should use this class.

Configure Varroa to use it:

```yaml
global:
  domain: example.com
frontend:
  host: app.example.com
ingress:
  enabled: true
  className: alb
```

The ACM certificate must cover the dashboard and controller names. Subdomain routing usually needs `app.example.com` and `*.example.com`. Path routing needs only the dashboard hostname. Create Route 53 records manually or use external-dns with least-privilege permissions for the selected hosted zone.

## Enable HPA Metrics

EKS does not install Metrics Server by default. Install it as an EKS community add-on or from the upstream manifest when gateway and BFF HPAs should scale.

```bash
kubectl get apiservice v1beta1.metrics.k8s.io
```

Wait for the APIService `Available` condition before relying on HPA scaling.

## Install and Verify

Follow [Install with Helm](helm-install.md), then check:

```bash
kubectl get pods -n varroa-system
kubectl get ingress -A
kubectl get pvc -A
```

The dashboard Ingress should receive an ALB hostname. DNS and ACM must be ready before browser login succeeds. If a pod reports `exec format error`, use a multi-architecture image or select nodes matching that image architecture.

Before deleting the cluster, delete controllers and ingress resources. Confirm that ALBs, target groups, security groups, and EBS volumes have been removed to avoid retained AWS charges.
