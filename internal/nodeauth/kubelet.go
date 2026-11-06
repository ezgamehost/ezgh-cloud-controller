package nodeauth

import (
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/yaml"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/nodeauth/imds"
)

// The node's files.
const (
	CAPath                  = "/etc/kubernetes/pki/ca.crt"
	BootstrapKubeconfigPath = "/etc/kubernetes/ezgh/bootstrap.kubeconfig"
	ClusterKubeconfigPath   = "/etc/kubernetes/ezgh/cluster.kubeconfig"
	KubeletEnvPath          = "/etc/kubernetes/ezgh/kubelet.env"
	KubeletDropInPath       = "/etc/kubernetes/kubelet/config.d/10-ezgh.conf"
	NodeGroupLabel          = "kubernetes.ezghcloud.com/node-group"
)

// NodeName is instance's Node name: ins_3k9d0a1m2x7q → ins-3k9d0a1m2x7q (Kubernetes names can't hold _).
func NodeName(instance string) (string, error) {
	if !hasID(instance, "ins_") {
		return "", fmt.Errorf("nodeauth: %q isn't an instance ID (ins_…)", instance)
	}
	return "ins-" + strings.TrimPrefix(instance, "ins_"), nil
}

// ProviderID is the Node's spec.providerID, as the cloud controller writes it.
func ProviderID(region, instance string) string { return "ezgh://" + region + "/" + instance }

// Reserved is kubeReserved by EKS's formula (plan call): CPU 6% of the first core, 1% of the
// second, 0.5% of the third and fourth, 0.25% of each beyond; memory 255 MiB plus 11 MiB a pod.
func Reserved(cpus, maxPods int) (cpuMilli, memoryMi int) {
	for core, milli := range []int{60, 10, 5, 5} {
		if core < cpus {
			cpuMilli += milli
		}
	}
	return cpuMilli + max(cpus-4, 0)*25/10, 255 + 11*maxPods
}

// dropIn holds only what 10-ezgh.conf sets: a zero field in a drop-in would override the image's
// kubelet-config.yaml.
type dropIn struct {
	APIVersion         string            `json:"apiVersion"`
	Kind               string            `json:"kind"`
	ProviderID         string            `json:"providerID"`
	MaxPods            int               `json:"maxPods"`
	ClusterDNS         []string          `json:"clusterDNS"`
	RegisterWithTaints []corev1.Taint    `json:"registerWithTaints,omitempty"`
	KubeReserved       map[string]string `json:"kubeReserved"`
}

// KubeletDropIn is /etc/kubernetes/kubelet/config.d/10-ezgh.conf for a node with cpus CPUs.
func KubeletDropIn(f JoinFacts, in imds.Instance, cpus int) ([]byte, error) {
	if !hasID(in.ID, "ins_") || in.Region == "" {
		return nil, fmt.Errorf("nodeauth: metadata names no instance or region (%q, %q)", in.ID, in.Region)
	}
	cpu, mem := Reserved(cpus, f.MaxPods)
	d := dropIn{APIVersion: "kubelet.config.k8s.io/v1beta1", Kind: "KubeletConfiguration", ProviderID: ProviderID(in.Region, in.ID),
		MaxPods: f.MaxPods, ClusterDNS: f.ClusterDNS,
		KubeReserved: map[string]string{"cpu": fmt.Sprintf("%dm", cpu), "memory": fmt.Sprintf("%dMi", mem)}}
	for _, t := range f.Taints {
		d.RegisterWithTaints = append(d.RegisterWithTaints, corev1.Taint{Key: t.Key, Value: t.Value, Effect: corev1.TaintEffect(t.Effect)})
	}
	return yaml.Marshal(d)
}

// KubeletEnv is /etc/kubernetes/ezgh/kubelet.env: KUBELET_ARGS for kubelet.service. The node's
// addresses are the private interface's, IPv4 first.
func KubeletEnv(f JoinFacts, in imds.Instance) ([]byte, error) {
	name, err := NodeName(in.ID)
	if err != nil {
		return nil, err
	}
	v4, v6, err := privateAddresses(in)
	if err != nil {
		return nil, err
	}
	labels := []string{NodeGroupLabel + "=" + f.NodeGroup}
	for _, k := range slices.Sorted(maps.Keys(f.Labels)) {
		if k != NodeGroupLabel {
			labels = append(labels, k+"="+f.Labels[k])
		}
	}
	args := []string{"--hostname-override=" + name, "--node-ip=" + v4 + "," + v6, "--node-labels=" + strings.Join(labels, ","),
		"--cloud-provider=external", "--config-dir=/etc/kubernetes/kubelet/config.d",
		"--image-credential-provider-config=/etc/kubernetes/ezgh/credential-providers.yaml",
		"--image-credential-provider-bin-dir=/usr/libexec/kubernetes/credential-providers"}
	return []byte("KUBELET_ARGS=" + strings.Join(args, " ") + "\n"), nil
}

// privateAddresses are the single private interface's IPv4 and IPv6 addresses.
func privateAddresses(in imds.Instance) (v4, v6 string, err error) {
	var found []imds.Interface
	for _, i := range in.Interfaces {
		if i.Kind == "private" {
			found = append(found, i)
		}
	}
	if len(found) != 1 {
		return "", "", fmt.Errorf("nodeauth: a node needs exactly one private network interface, and %s has %d", in.ID, len(found))
	}
	i := found[0]
	if i.PrivateIPv4 == nil {
		return "", "", fmt.Errorf("nodeauth: metadata doesn't describe the private interface %s yet", i.ID)
	}
	a4, err4 := netip.ParseAddr(*i.PrivateIPv4)
	a6, err6 := netip.ParseAddr(i.IPv6Address)
	if err4 != nil || err6 != nil || !a4.Is4() || !a6.Is6() {
		return "", "", fmt.Errorf("nodeauth: the private interface %s has no IPv4 and IPv6 address yet", i.ID)
	}
	return a4.String(), a6.String(), nil
}

// BootstrapKubeconfig is kubelet's --bootstrap-kubeconfig: ezgh-node-auth exec-credential hands it
// a fresh identity token on every call, so the file holds no credential.
func BootstrapKubeconfig(f JoinFacts) []byte {
	return kubeconfig(f, "kubelet-bootstrap", &clientcmdapi.AuthInfo{Exec: &clientcmdapi.ExecConfig{
		APIVersion: "client.authentication.k8s.io/v1", Command: "/usr/bin/ezgh-node-auth", Args: []string{"exec-credential"},
		InteractiveMode: clientcmdapi.NeverExecInteractiveMode,
	}})
}

// ClusterKubeconfig is the host-network add-ons' kubeconfig (kube-proxy, vpc-cni): the private
// endpoint and the reading pod's own service-account token, since Services aren't up before them.
func ClusterKubeconfig(f JoinFacts) []byte {
	return kubeconfig(f, "service-account", &clientcmdapi.AuthInfo{TokenFile: "/var/run/secrets/kubernetes.io/serviceaccount/token"})
}

func kubeconfig(f JoinFacts, user string, auth *clientcmdapi.AuthInfo) []byte {
	c := clientcmdapi.NewConfig()
	c.Clusters[f.Cluster] = &clientcmdapi.Cluster{Server: "https://" + f.PrivateEndpoint, CertificateAuthorityData: []byte(f.CABundle)}
	c.AuthInfos[user] = auth
	c.Contexts[user] = &clientcmdapi.Context{Cluster: f.Cluster, AuthInfo: user}
	c.CurrentContext = user
	b, err := clientcmd.Write(*c)
	if err != nil {
		panic(err) // only an unencodable Config fails, and this one is fixed
	}
	return b
}
