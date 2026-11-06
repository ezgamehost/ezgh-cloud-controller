// Package nodeauth turns a node's user data into kubelet's files and is kubelet's bootstrap exec
// plugin. JoinFacts is the user data kubernetes-controller's launch spec writes (D9).
package nodeauth

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// JoinFactsPath is where ezgh-node-auth keeps the node's join facts.
const JoinFactsPath = "/etc/kubernetes/ezgh/join.json"

// JoinFacts is a node's JSON user data (D9). ClusterDNS is [IPv4, IPv6].
type JoinFacts struct {
	Cluster         string            `json:"cluster"`
	ClusterUUID     string            `json:"clusterUuid"`
	Org             string            `json:"org"`
	Project         string            `json:"project"`
	NodeGroup       string            `json:"nodeGroup"`
	PrivateEndpoint string            `json:"privateEndpoint"`
	CABundle        string            `json:"caBundle"`
	ClusterDNS      []string          `json:"clusterDNS"`
	MaxPods         int               `json:"maxPods"`
	Labels          map[string]string `json:"labels"`
	Taints          []Taint           `json:"taints"`
}

// Taint's Effect is NoSchedule, PreferNoSchedule or NoExecute.
type Taint struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Effect string `json:"effect"`
}

// ErrInvalidJoinFacts is user data that will never join: retrying can't help.
var ErrInvalidJoinFacts = errors.New("nodeauth: the node's join facts are invalid")

var (
	idSuffix = regexp.MustCompile(`^[a-z0-9]+$`)
	uuidRe   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	effects  = map[string]bool{"NoSchedule": true, "PreferNoSchedule": true, "NoExecute": true}
)

// ParseJoinFacts decodes and checks join facts; unknown fields are ignored. Labels and taints
// end up on kubelet's command line, so they must be valid Kubernetes names and values.
func ParseJoinFacts(b []byte) (JoinFacts, error) {
	var f JoinFacts
	if err := json.Unmarshal(b, &f); err != nil {
		return JoinFacts{}, fmt.Errorf("%w: not JSON: %w", ErrInvalidJoinFacts, err)
	}
	if err := f.check(); err != nil {
		return JoinFacts{}, fmt.Errorf("%w: %w", ErrInvalidJoinFacts, err)
	}
	return f, nil
}

func (f JoinFacts) check() error {
	label, rest, ok := strings.Cut(f.PrivateEndpoint, ".private.k8s.")
	switch {
	// kubernetes-api's catalog.PrivateHostname: <label>.private.k8s.<region>.<zone>
	case !ok || !idSuffix.MatchString(label) || !strings.Contains(rest, ".") || len(validation.IsDNS1123Subdomain(f.PrivateEndpoint)) > 0:
		return fmt.Errorf("privateEndpoint %q isn't <label>.private.k8s.<region>.<zone>", f.PrivateEndpoint)
	case !hasID(f.Cluster, "kcl_"):
		return fmt.Errorf("cluster %q isn't kcl_…", f.Cluster)
	case !uuidRe.MatchString(f.ClusterUUID):
		return fmt.Errorf("clusterUuid %q isn't a UUID", f.ClusterUUID)
	case !hasID(f.NodeGroup, "kng_"):
		return fmt.Errorf("nodeGroup %q isn't kng_…", f.NodeGroup)
	case f.MaxPods < 1 || f.MaxPods > 110:
		return fmt.Errorf("maxPods %d isn't 1-110", f.MaxPods)
	}
	if err := certificates(f.CABundle); err != nil {
		return err
	}
	if len(f.ClusterDNS) != 2 {
		return errors.New("clusterDNS isn't [IPv4, IPv6]")
	}
	v4, err4 := netip.ParseAddr(f.ClusterDNS[0])
	v6, err6 := netip.ParseAddr(f.ClusterDNS[1])
	if err4 != nil || err6 != nil || !v4.Is4() || !v6.Is6() || v6.Is4In6() {
		return fmt.Errorf("clusterDNS %q isn't [IPv4, IPv6]", f.ClusterDNS)
	}
	for k, v := range f.Labels {
		if len(validation.IsQualifiedName(k)) > 0 || len(validation.IsValidLabelValue(v)) > 0 {
			return fmt.Errorf("label %q=%q isn't a valid Kubernetes label", k, v)
		}
	}
	for _, t := range f.Taints {
		if len(validation.IsQualifiedName(t.Key)) > 0 || len(validation.IsValidLabelValue(t.Value)) > 0 || !effects[t.Effect] {
			return fmt.Errorf("taint %q=%q:%q isn't a valid Kubernetes taint", t.Key, t.Value, t.Effect)
		}
	}
	return nil
}

func hasID(s, prefix string) bool {
	rest, ok := strings.CutPrefix(s, prefix)
	return ok && idSuffix.MatchString(rest)
}

// certificates refuses anything but one or more PEM CERTIFICATEs that parse (several while the
// cluster CA rotates).
func certificates(s string) error {
	rest := []byte(s)
	for n := 0; ; n++ {
		if len(bytes.TrimSpace(rest)) == 0 {
			if n == 0 {
				return errors.New("caBundle holds no certificate")
			}
			return nil
		}
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil || block.Type != "CERTIFICATE" {
			return errors.New("caBundle holds something other than PEM certificates")
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return fmt.Errorf("caBundle doesn't parse: %w", err)
		}
	}
}

// ReadJoinFacts reads JoinFactsPath.
func ReadJoinFacts() (JoinFacts, error) {
	b, err := os.ReadFile(JoinFactsPath)
	if err != nil {
		return JoinFacts{}, err
	}
	return ParseJoinFacts(b)
}
