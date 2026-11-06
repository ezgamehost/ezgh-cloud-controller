package ezghclient

import (
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/apiclient"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/blockstoreapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/certificatesapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/computeapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/filesapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/lbapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/networkapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/secretsapi"
)

// Clients are the EZGH APIs the controllers call, all as the cluster's Bot.
type Clients struct {
	LB           *lbapi.Client
	Blockstore   *blockstoreapi.Client
	Compute      *computeapi.Client
	Network      *networkapi.Client
	Files        *filesapi.Client
	Secrets      *secretsapi.Client
	Certificates *certificatesapi.Client
}

// ServiceURL is https://<host>.ezghcloud.com, or EZGH_<HOST>_URL (as ezgh-cli reads it).
func ServiceURL(host string) string {
	if u := os.Getenv("EZGH_" + strings.ReplaceAll(strings.ToUpper(host), "-", "_") + "_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "https://" + host + ".ezghcloud.com"
}

// New builds the clients at their public hosts.
func New(src CredentialSource, userAgent string) *Clients { return NewAt(src, userAgent, ServiceURL) }

// NewAt builds the clients with url(host) as each one's base URL (a test server).
func NewAt(src CredentialSource, userAgent string, url func(host string) string) *Clients {
	httpClient := &http.Client{Timeout: 30 * time.Second, Transport: keyTransport{http.DefaultTransport}}
	api := func(service, host string) *apiclient.Client {
		return &apiclient.Client{
			Service: service, BaseURL: url(host), HTTP: httpClient, Auth: authorizer{src}, UserAgent: userAgent,
			// Short blips are retried here; a longer Retry-After goes back to the workqueue, so no
			// reconcile worker sleeps for long.
			MaxAttempts: 3, MaxWait: 5 * time.Second,
		}
	}
	return &Clients{
		LB:           &lbapi.Client{API: api("Load Balancers", "lb-api")},
		Blockstore:   &blockstoreapi.Client{API: api("Block Storage", "blockstore-api")},
		Compute:      &computeapi.Client{API: api("Compute", "compute-api")},
		Network:      &networkapi.Client{API: api("Network", "network-api")},
		Files:        &filesapi.Client{API: api("Files", "files-api")},
		Secrets:      &secretsapi.Client{API: api("Secrets", "secrets")},
		Certificates: &certificatesapi.Client{API: api("Certificates", "certificates")},
	}
}
