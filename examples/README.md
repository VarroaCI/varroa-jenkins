# Examples

Small, self-contained starting points. Each file says what it needs; the full
documentation lives under [docs/](../docs/).

| Example | What it shows |
|---|---|
| [bare-minimum.yaml](bare-minimum.yaml) | The smallest `Controller` — starter bundle, no ingress, port-forward access |
| [controller.yaml](controller.yaml) | A production-shaped `Controller` — version pin, bundle ref, ingress, sizing |
| [composed-bundle.yaml](composed-bundle.yaml) | A `ComposedBundle` built from ordered git/catalog inputs |
| [controller-class.yaml](controller-class.yaml) | A `ControllerClass` shared by many controllers |
| [provisioning-defaults.yaml](provisioning-defaults.yaml) | Cluster-wide `ProvisioningDefaults` |
| [git-ssh-secret.yaml](git-ssh-secret.yaml) | Deploy-key Secret for private bundle repos |
| [catalog-source.yaml](catalog-source.yaml) | A `CatalogSource` syncing reusable catalog items from git |
| [oidc-values.yaml](oidc-values.yaml) | Helm values: direct OIDC identity provider |
| [local-auth-values.yaml](local-auth-values.yaml) | Helm values: built-in local auth, no external IdP |
| [air-gapped/](air-gapped/) | Full air-gapped install: in-cluster update center, seeded plugins, egress lockdown |
