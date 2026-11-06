package main

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/l7"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/lb"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/run"
)

func init() { subcommands["lb"] = runLB }

// runLB serves LoadBalancer Services, Ingresses (class ezgh) and Gateways (GatewayClass ezgh),
// their targets and the readiness-gate webhook (https://127.0.0.1:9446/mutate-pods, every VM; the
// controllers only on the leader).
func runLB([]string) error {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := gatewayv1.Install(scheme); err != nil {
		return err
	}
	env, err := run.Manager("lb", func(o *ctrl.Options) {
		o.Scheme = scheme
		o.Cache.DefaultTransform = cache.TransformStripManagedFields() // every pod is cached
		// Only TLS Secrets are read (Ingress and Gateway certificates), so only they're cached.
		o.Cache.ByObject = map[client.Object]cache.ByObject{&corev1.Secret{}: {Field: fields.OneTermEqualSelector("type", string(corev1.SecretTypeTLS))}}
		o.WebhookServer = webhook.NewServer(webhook.Options{
			Host: "127.0.0.1", Port: lb.WebhookPort, CertDir: lb.WebhookCertDir, CertName: "tls.crt", KeyName: "tls.key",
		})
	})
	if err != nil {
		return err
	}
	if err := lb.Setup(env, l7.Feeds()...); err != nil {
		return err
	}
	if err := l7.Setup(env); err != nil {
		return err
	}
	return env.Start()
}
