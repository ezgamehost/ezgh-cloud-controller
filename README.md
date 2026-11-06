# ezgh-cloud-controller

EZGH Cloud's in-cluster controllers for EZGH Kubernetes, the counterpart of
kubernetes/cloud-provider-aws and aws-load-balancer-controller. One binary, `ezgh-cloud`, runs on
every control-plane VM of an EZGH Kubernetes cluster, one subcommand per systemd unit
(`ezgh-cloud@<subcommand>.service`), each leader-elected through its own Lease
`kube-system/ezgh-cloud-<subcommand>`. They act on EZGH's public APIs as the cluster's Bot, with
credentials from the local agent (`http://127.0.0.1:9444/v1/cluster-credentials`).

| Subcommand | Job |
| --- | --- |
| `ccm` | Cloud controller manager (`k8s.io/cloud-provider`, provider `ezgh`, InstancesV2 only): node addresses, provider IDs, the `blockstore.csi.ezghcloud.com/max-volumes` label, removal of deleted instances |
| `lb` | Services of type LoadBalancer on EZGH Load Balancers, pod targets from EndpointSlices with readiness gates, Ingress (IngressClass `ezgh`) and Gateway API v1.6.3 (GatewayClass `ezgh`) |
| `csr-approver` | Approves kubelet client and serving CSRs after checking the instance live |
| `autoscaler-provider` | Cluster Autoscaler's externalgrpc cloud provider on `127.0.0.1:9447`, over EZGH autoscaling groups |

The CSI controllers and the pod identity webhook, once subcommands here, are
[ezgh-csi](https://github.com/ezgamehost/ezgh-csi) and
[ezgh-pod-identity](https://github.com/ezgamehost/ezgh-pod-identity).

Inputs on the VM: `/etc/ezgh/cloud.json` (the cluster's facts), `/etc/ezgh/kubeconfig/ezgh-cloud.conf`
(user `ezgh:cloud`). `EZGH_CLOUD_CONFIG`, `EZGH_CLOUD_KUBECONFIG` and `EZGH_CREDENTIALS_URL` override
them for local runs. Logs are JSON lines through the vendored kit (`internal/kit/logging`).

## Build and test

Go 1.27.1 (`go.mod`).

| Check | Command |
| --- | --- |
| Build | `CGO_ENABLED=0 GOOS=linux go build ./cmd/ezgh-cloud` |
| Tests | `go test -race $(go list ./... \| grep -v /internal/kit/)` |
| Lint | `gofmt -l .`; `go vet` and `go run honnef.co/go/tools/cmd/staticcheck@v0.8.1` over `$(go list ./... \| grep -v /internal/kit/)` (service-template checks the kit) |
| Logging | `scripts/check-logging.sh` |
| Gateway API conformance | `go test -tags conformance ./internal/cloud/l7/` against a real cluster (see the file's header) |

CI (`.github/workflows/ci.yml`) runs all but conformance on every pull request and push to main.

## Releases

Push a tag `vX.Y.Z` on main. `.github/workflows/release.yml` builds `ezgh-cloud-linux-amd64` and
`ezgh-cloud-linux-arm64` and publishes them with `SHA256SUMS` as the GitHub release. A release is
never rebuilt: a fix ships as a new version.

kubernetes-stack's control-plane payload installs it: `deploy/images/releases.txt` there pins the
version and the SHA-256 of `ezgh-cloud-linux-amd64`, which `deploy/images/payload.sh` checks. The
systemd units and the bootstrap manifest (the `ezgh:cloud` RBAC, Gateway API CRDs, webhook
configurations) live in kubernetes-stack (`cmd/ezgh-cloud/units`, `cmd/ezgh-cloud/bootstrap`).

## Copied code

Other repositories' code is copied in, not imported, each copy with its own sync:

| Path | Source | Sync |
| --- | --- | --- |
| `internal/kit` | ezgamehost/service-template `kit/go` (commit in `internal/kit/KIT_VERSION`) | `scripts/sync-kit.sh` |
| `internal/ezghclient/{apiclient,ids,openapitest,*api}`, `internal/ezghclient/openapi` | ezgamehost/ezgh-cli's API clients and OpenAPI documents (commit in `internal/ezghclient/SOURCE`) | `internal/ezghclient/sync.sh` |
| `internal/nodeauth/{join.go,kubelet.go,imds}`, `cmd/ezgh-cloud/bootstrap/manifest.yaml` (test fixture) | ezgamehost/kubernetes-stack (commit in `COPIES`) | `scripts/sync-copies.sh kubernetes-stack` |

The rest of `internal/ezghclient` (credentials, errors, idempotency keys, `kubernetes*.go`) is this
repository's own. ezgh-csi and ezgh-secrets-store-provider copy `internal/ezghclient` from here, and ezgh-csi and
ezgh-pod-identity copy parts of `internal/cloud`.

## License

Apache-2.0 ([LICENSE](LICENSE)). Security reports: [SECURITY.md](SECURITY.md).
