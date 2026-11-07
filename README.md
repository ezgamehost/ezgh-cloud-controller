# ezgh-cloud-controller

The EZGH Cloud integration controllers for [EZGH Kubernetes](https://docs.ezghcloud.com): node
lifecycle, load balancers for Services, Ingress and Gateway API, kubelet certificate approval and
Cluster Autoscaler support.

EZGH Kubernetes runs these controllers on every cluster's control plane. This repository holds
their source code; you don't need to deploy them yourself.

## Controllers

One binary, `ezgh-cloud`, provides each controller as a subcommand. Each runs as its own service on
the control plane and is leader-elected through a Lease named `kube-system/ezgh-cloud-<subcommand>`.

| Subcommand | Purpose |
| --- | --- |
| `ccm` | Cloud controller manager (provider `ezgh`): node addresses, provider IDs, per-node volume limits and removal of deleted instances |
| `lb` | EZGH Load Balancers for Services of type `LoadBalancer`, Ingress (IngressClass `ezgh`) and Gateway API v1.6 (GatewayClass `ezgh`), targeting pods directly with readiness gates |
| `csr-approver` | Approves kubelet client and serving certificate requests after verifying the instance |
| `autoscaler-provider` | Cluster Autoscaler `externalgrpc` cloud provider over EZGH autoscaling groups |

The controllers act as the cluster's Bot with short-lived credentials from the control plane's
local agent. They read the cluster's configuration from `/etc/ezgh/cloud.json` and
`/etc/ezgh/kubeconfig/ezgh-cloud.conf`; set `EZGH_CLOUD_CONFIG`, `EZGH_CLOUD_KUBECONFIG` and
`EZGH_CREDENTIALS_URL` to override them for local development. Logs are structured JSON.

Related components: [ezgh-cni](https://github.com/ezgamehost/ezgh-cni) (pod networking),
[ezgh-csi](https://github.com/ezgamehost/ezgh-csi) (storage),
[ezgh-pod-identity](https://github.com/ezgamehost/ezgh-pod-identity) (workload credentials) and
[ezgh-secrets-store-provider](https://github.com/ezgamehost/ezgh-secrets-store-provider) (secrets).

## Binaries

Binaries for linux/amd64 and linux/arm64, with `SHA256SUMS`, are published on
[GitHub Releases](https://github.com/ezgamehost/ezgh-cloud-controller/releases).

## Building from source

Requires Go 1.27.1 or later.

```sh
CGO_ENABLED=0 GOOS=linux go build ./cmd/ezgh-cloud
```

## Testing

| Check | Command |
| --- | --- |
| Unit tests | `go test -race $(go list ./... \| grep -v /internal/kit/)` |
| Lint | `gofmt -l .`, `go vet` and `staticcheck` |
| Logging | `scripts/check-logging.sh` |
| Gateway API conformance | `go test -tags conformance ./internal/cloud/l7/` against a live cluster (see the test file for setup) |

Continuous integration runs everything except conformance on every pull request and push to `main`.

## Releases

Releases follow semantic versioning. Pushing a `vX.Y.Z` tag on `main` publishes the binaries.
Published versions are immutable; fixes ship as new versions.

## Vendored code

`internal/kit`, the generated API clients in `internal/ezghclient` and `internal/nodeauth` are
vendored from shared EZGH libraries and refreshed with `scripts/sync-kit.sh`,
`internal/ezghclient/sync.sh` and `scripts/sync-copies.sh`. Changes to them are made upstream.

## Security

Please report vulnerabilities privately as described in [SECURITY.md](SECURITY.md).

## License

Licensed under the [Apache License 2.0](LICENSE).
