[![CI](https://github.com/projectsveltos/clusterinventory-controller/actions/workflows/main.yaml/badge.svg)](https://github.com/projectsveltos/clusterinventory-controller/actions)
[![Go Report Card](https://goreportcard.com/badge/github.com/projectsveltos/addon-controller)](https://goreportcard.com/report/github.com/projectsveltos/clusterinventory-controller)
[![License](https://img.shields.io/badge/license-Apache-blue.svg)](LICENSE)
[![Slack](https://img.shields.io/badge/join%20slack-%23projectsveltos-brighteen)](https://join.slack.com/t/projectsveltos/shared_invite/zt-1hraownbr-W8NTs6LTimxLPB8Erj8Q6Q)
[![LinkedIn](https://custom-icon-badges.demolab.com/badge/LinkedIn-0A66C2?logo=linkedin-white&logoColor=fff)](https://www.linkedin.com/company/projectsveltos/)
[![X URL](https://img.shields.io/twitter/url/https/twitter.com/projectsveltos.svg?style=social&label=Follow%20%40projectsveltos)](https://x.com/projectsveltos)

👋 Welcome to **Projectsveltos**!

<div align="center">

| 🌐 Website | 📚 Documentation | 📅 Book a Demo | 💼 Enterprise Support | 🏢 Adopters |
|:---:|:---:|:---:|:---:|:---:|
| [Visit](https://website.projectsveltos.io) | [Get Started](https://projectsveltos.github.io/sveltos/) | [Schedule 30 min](https://cal.com/gianluca-mardente-nuclsu/30min) | [Contact Us](mailto:gianluca@projectsveltos.io) | [View List](https://github.com/projectsveltos/adopters/blob/main/ADOPTERS.md) |

</div>

# clusterinventory-controller

Bridges the [Kubernetes Cluster Inventory API](https://github.com/kubernetes-sigs/cluster-inventory-api) (`ClusterProfile`) with [Sveltos](https://github.com/projectsveltos) by creating and managing `SveltosCluster` resources.

## What it does

The controller watches `ClusterProfile` objects (`multicluster.x-k8s.io/v1alpha1`) and for each one:

1. **Reads the kubeconfig** — extracts the cluster credentials from the access provider referenced in `status.accessProviders` (or the deprecated `status.credentialProviders`).
2. **Creates a kubeconfig Secret** — stores the kubeconfig bytes in a namespaced Secret (`<cluster-name>-sveltos-kubeconfig`) in the same namespace as the `ClusterProfile`. This Secret is owned by the controller and kept in sync: if the source changes and the `ClusterProfile` is reconciled, the Secret is updated.
3. **Creates a SveltosCluster** — creates a `lib.projectsveltos.io/v1beta1` `SveltosCluster` in the same namespace, pointing it at the managed kubeconfig Secret. From this point on Sveltos treats the cluster as any other managed cluster and can deploy add-ons to it.

All three objects share the same name and namespace as the originating `ClusterProfile`. When a `ClusterProfile` is deleted the controller removes the `SveltosCluster` and the kubeconfig Secret before releasing its finalizer.

### Flow

```
ClusterProfile (multicluster.x-k8s.io/v1alpha1)
       │
       │  status.accessProviders
       ▼
clusterinventory-controller
       │
       ├──► Secret  <cluster-name>-sveltos-kubeconfig  (kubeconfig bytes)
       │
       └──► SveltosCluster  <cluster-name>             (points to Secret above)
                  │
                  ▼
           Sveltos (addon-controller, sveltoscluster-manager, …)
```

## Where it stops

- **Access provider support**: only the `kubeconfig-secretreader` provider is supported today. This provider reads a full kubeconfig from a pre-existing Kubernetes Secret referenced inside the `client.authentication.k8s.io/exec` extension of the `ClusterProfile` status. Exec-plugin providers (where the `ClusterProfile` vends short-lived credentials via an external binary) are not yet supported; adding them requires implementing a new helper and wiring it into `getKubeconfig()` in `controllers/utils.go`.
- **Kubeconfig refresh**: the controller reconciles the kubeconfig Secret whenever the `ClusterProfile` is reconciled. It does **not** independently watch the source Secret for changes; an external actor (e.g., the cluster manager that writes the `ClusterProfile` status) is responsible for triggering a re-reconcile when credentials rotate.
- **Cluster lifecycle**: the controller does not provision, deprovision, or health-check the remote cluster. It only translates the `ClusterProfile` representation into what Sveltos needs. The `SveltosCluster` readiness check (and everything that happens after it) is entirely Sveltos's responsibility.

## Access provider: kubeconfig-secretreader

The `kubeconfig-secretreader` provider expects the following JSON payload embedded in the `client.authentication.k8s.io/exec` extension of the access provider entry:

```json
{
  "name":      "my-kubeconfig-secret",
  "key":       "kubeconfig",
  "namespace": "optional-override"
}
```

| Field | Required | Description |
|---|---|---|
| `name` | yes | Name of the Secret that holds the kubeconfig |
| `key` | yes | Data key inside the Secret |
| `namespace` | no | Namespace of the Secret; defaults to the `ClusterProfile` namespace |

## Managed resources

All resources created by the controller carry the label:

```
clusterinventory.projectsveltos.io/managed-by: clusterinventory-controller
```

The controller adds the finalizer `clusterinventory.projectsveltos.io/finalizer` to each `ClusterProfile` it reconciles and removes it only after the `SveltosCluster` and kubeconfig Secret have been deleted.

## Requirements

- Kubernetes 1.28+
- [Sveltos](https://github.com/projectsveltos/addon-controller) installed in the cluster (provides the `SveltosCluster` CRD and controllers)
- `ClusterProfile` CRD from [cluster-inventory-api](https://github.com/kubernetes-sigs/cluster-inventory-api) installed in the cluster
