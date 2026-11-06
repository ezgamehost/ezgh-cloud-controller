// Package csr approves kubelets' certificate requests (spec "Joining"). kube-controller-manager
// runs with --controllers=*,-csrapproving (D8), so this is the only approver for both kubelet
// signers. It approves and never denies: a CSR it won't approve stays pending, with a log line and
// a Warning event naming the rule that failed.
package csr

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strings"

	certv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/ccm"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/config"
	cloudevents "github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/events"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/run"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient"
)

// NotApproved is the Warning event's reason on a CSR left pending by a rule.
const NotApproved = "NotApproved"

var usages = map[string][]certv1.KeyUsage{
	certv1.KubeAPIServerClientKubeletSignerName: {certv1.UsageClientAuth, certv1.UsageDigitalSignature, certv1.UsageKeyEncipherment},
	certv1.KubeletServingSignerName:             {certv1.UsageServerAuth, certv1.UsageDigitalSignature, certv1.UsageKeyEncipherment},
}

// Approver reconciles CertificateSigningRequests.
type Approver struct {
	Client     client.Client
	Clients    *ezghclient.Clients
	ConfigPath string
	Recorder   events.EventRecorder
	Log        *slog.Logger
}

// SetupWithManager registers the approver on CertificateSigningRequests.
func (a *Approver) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&certv1.CertificateSigningRequest{}).
		WithOptions(controller.Options{RateLimiter: run.RateLimiter()}).Complete(a)
}

// Reconcile approves req's CSR when every rule holds.
func (a *Approver) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var csr certv1.CertificateSigningRequest
	if err := a.Client.Get(ctx, req.NamespacedName, &csr); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if usages[csr.Spec.SignerName] == nil || slices.ContainsFunc(csr.Status.Conditions, func(c certv1.CertificateSigningRequestCondition) bool {
		return c.Type == certv1.CertificateApproved || c.Type == certv1.CertificateDenied || c.Type == certv1.CertificateFailed
	}) {
		return reconcile.Result{}, nil
	}
	rule, err := a.check(ctx, &csr)
	if err != nil {
		return run.Fail(a.Recorder, &csr, err)
	}
	if rule != "" {
		a.Log.Warn("CSR left pending", "csr", csr.Name, "signer", csr.Spec.SignerName, "rule", rule)
		cloudevents.Warn(a.Recorder, &csr, NotApproved, "Not approved: %s", rule)
		return reconcile.Result{}, nil
	}
	csr.Status.Conditions = append(csr.Status.Conditions, certv1.CertificateSigningRequestCondition{
		Type: certv1.CertificateApproved, Status: corev1.ConditionTrue, Reason: "EZGHAutoApproved",
		Message: "The requester is the node the certificate names", LastUpdateTime: metav1.Now(),
	})
	if err := a.Client.SubResource("approval").Update(ctx, &csr); err != nil {
		return reconcile.Result{}, err
	}
	a.Log.Info("CSR approved", "csr", csr.Name, "signer", csr.Spec.SignerName)
	return reconcile.Result{}, nil
}

// check is the first rule csr fails, or "". An error is a read that didn't answer.
func (a *Approver) check(ctx context.Context, csr *certv1.CertificateSigningRequest) (string, error) {
	block, _ := pem.Decode(csr.Spec.Request)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return "the request isn't a PEM CERTIFICATE REQUEST", nil
	}
	x, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil || x.CheckSignature() != nil {
		return "the request doesn't parse or its signature doesn't verify", nil
	}
	name, ok := strings.CutPrefix(csr.Spec.Username, "system:node:")
	if !ok || !slices.Contains(csr.Spec.Groups, "system:nodes") {
		return "the requester isn't a node (system:node:<name> in system:nodes)", nil
	}
	id, err := ccm.InstanceID(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}})
	if err != nil {
		return err.Error(), nil
	}
	if x.Subject.CommonName != csr.Spec.Username || !slices.Equal(x.Subject.Organization, []string{"system:nodes"}) || len(x.Subject.Names) != 2 {
		return "the subject must be CN=" + csr.Spec.Username + ", O=system:nodes and nothing else", nil
	}
	for _, u := range csr.Spec.Usages {
		if !slices.Contains(usages[csr.Spec.SignerName], u) {
			return fmt.Sprintf("usage %q isn't allowed for %s", u, csr.Spec.SignerName), nil
		}
	}
	if len(x.URIs) > 0 || len(x.EmailAddresses) > 0 {
		return "URI and email SANs aren't allowed", nil
	}
	client := csr.Spec.SignerName == certv1.KubeAPIServerClientKubeletSignerName
	if client && (len(x.DNSNames) > 0 || len(x.IPAddresses) > 0) {
		return "a client certificate has no SANs", nil
	}
	if !client && slices.ContainsFunc(x.DNSNames, func(d string) bool { return d != name }) {
		return "the only DNS SAN allowed is " + name, nil
	}
	if !client && len(x.IPAddresses) == 0 {
		return "a serving certificate needs at least one IP SAN", nil
	}
	return a.checkInstance(ctx, id, x.IPAddresses)
}

// checkInstance reads instance id live: it must be this cluster's and not terminated, and every ip
// must be one of its addresses.
func (a *Approver) checkInstance(ctx context.Context, id string, ips []net.IP) (string, error) {
	cfg, err := config.Load(a.ConfigPath)
	if err != nil {
		return "", err
	}
	inst, err := ccm.GetInstance(ctx, a.Clients, cfg, id)
	switch {
	case ezghclient.IsNotFound(err):
		return "instance " + id + " doesn't exist", nil
	case errors.Is(err, ccm.ErrNotThisCluster):
		return err.Error(), nil
	case err != nil:
		return "", err
	case inst.Status == "terminated":
		return "instance " + id + " is terminated", nil
	}
	if len(ips) == 0 {
		return "", nil
	}
	addrs, err := ccm.Addresses(ctx, a.Clients, cfg, inst)
	if err != nil {
		return "", err
	}
	for _, ip := range ips {
		if !slices.ContainsFunc(addrs, func(n corev1.NodeAddress) bool { return ip.Equal(net.ParseIP(n.Address)) }) {
			return "IP SAN " + ip.String() + " isn't an address of instance " + id, nil
		}
	}
	return "", nil
}
