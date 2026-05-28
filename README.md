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

## Access providers

### kubeconfig-secretreader (built-in)

This is a built-in fast path that requires no external binary. The controller reads a **complete kubeconfig** from a pre-existing Kubernetes Secret and copies it into the managed kubeconfig Secret.

The `ClusterProfile` access provider entry must be named `kubeconfig-secretreader` and carry the following JSON payload in the `client.authentication.k8s.io/exec` Cluster extension:

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

> **Naming note**: the upstream [cluster-inventory-api](https://github.com/kubernetes-sigs/cluster-inventory-api) project also ships a binary called `kubeconfig-secretreader` that works as an exec credential plugin (returning an `ExecCredential` with token or cert/key data). The two uses of the name refer to different things: in this controller `kubeconfig-secretreader` is a built-in code path that copies a full kubeconfig, while upstream it is an exec plugin binary. A future improvement could unify them by routing `kubeconfig-secretreader` through the generic exec-plugin path described below.

### Exec-plugin providers (via `--clusterprofile-provider-file`)

Any access provider backed by an exec credential plugin can be enabled by passing a provider configuration file to the controller at startup:

```
--clusterprofile-provider-file=/etc/clusterinventory/providers.json
```

The controller invokes the configured binary directly at reconcile time, embeds the returned credentials into a plain kubeconfig (no exec stanza), and stores that kubeconfig in the managed Secret. This means `sveltoscluster-manager` does not need the exec binary in its own pod.

All three credential shapes returned by `ExecCredentialStatus` are supported:

| Returned by plugin | Written to kubeconfig |
|---|---|
| `status.token` | `user.token` |
| `status.clientCertificateData` + `status.clientKeyData` | `user.client-certificate-data` + `user.client-key-data` |
| both token and cert/key | both fields set |

If the plugin returns an `expirationTimestamp`, the controller automatically requeues at 80 % of the remaining token lifetime (minimum 1 minute) so the Secret is refreshed before the credentials expire. For certificate-only plugins that do not set an expiry, rotation relies on the controller's `--sync-period` (default 10 minutes) or an external re-trigger of the `ClusterProfile`.

#### Provider configuration file format

```json
{
  "providers": [
    {
      "name": "my-provider",
      "execConfig": {
        "apiVersion": "client.authentication.k8s.io/v1",
        "command": "/usr/local/bin/my-credential-plugin",
        "args": ["--cluster-name", "$(CLUSTER_NAME)"],
        "env": [
          {"name": "MY_ENV", "value": "value"}
        ],
        "provideClusterInfo": true,
        "interactiveMode": "Never"
      },
      "profileSourcedCLIArgsPolicy": "Append",
      "profileSourcedEnvVarsPolicy": "AppendIfNotExists"
    }
  ]
}
```

The `name` field must match the provider name in the `ClusterProfile`'s `status.accessProviders`. The `profileSourcedCLIArgsPolicy` and `profileSourcedEnvVarsPolicy` fields control whether cluster-specific arguments and environment variables embedded in the `ClusterProfile` extensions (per [KEP-5339](https://github.com/kubernetes/enhancements/issues/5339)) are merged into the plugin invocation.

> **Operational note**: the exec plugin binary must be present and executable inside the controller pod. The controller invokes it with `KUBERNETES_EXEC_INFO` set so that plugins using `provideClusterInfo: true` receive the correct server and CA data.

## Where it stops

- **Kubeconfig refresh**: the controller reconciles the kubeconfig Secret whenever the `ClusterProfile` is reconciled. It does **not** independently watch the source Secret for changes (kubeconfig-secretreader path) or the token expiry beyond the scheduled requeue (exec-plugin path). An external actor (e.g., the cluster manager that writes the `ClusterProfile` status) is responsible for triggering a re-reconcile when credentials rotate out-of-band.
- **Cluster lifecycle**: the controller does not provision, deprovision, or health-check the remote cluster. It only translates the `ClusterProfile` representation into what Sveltos needs. The `SveltosCluster` readiness check (and everything that happens after it) is entirely Sveltos's responsibility.

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
