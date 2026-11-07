// Package run is what every ezgh-cloud subcommand starts from: the kit's logger (with the
// Kubernetes libraries routed into it), a controller-runtime manager on the ezgh:cloud kubeconfig,
// leader-elected through the Lease kube-system/ezgh-cloud-<subcommand>, the EZGH API clients as
// the cluster's Bot, and the shared backoff.
package run

import (
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/config"
	cloudevents "github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/events"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/apiclient"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/logging"
)

// Kubeconfig is the ezgh:cloud client certificate's kubeconfig, written by k8sagent.
const Kubeconfig = "/etc/ezgh/kubeconfig/ezgh-cloud.conf"

// Env is a subcommand's running context.
type Env struct {
	Manager ctrl.Manager
	Clients *ezghclient.Clients
	Log     *slog.Logger
	// ConfigPath is cloud.json; controllers load it on each use (config.Load), never once.
	ConfigPath string
}

// Config reads cloud.json now.
func (e *Env) Config() (config.Config, error) { return config.Load(e.ConfigPath) }

// Option adjusts the manager's options (a webhook server, a cache filter).
type Option func(*ctrl.Options)

// Logger is the kit's logger for ezgh-cloud-<sub>, also given to klog and controller-runtime.
func Logger(sub string) *slog.Logger {
	log := logging.New(os.Stdout, "ezgh-cloud-"+sub, os.Getenv("APP_ENV") == "local")
	klog.SetSlogLogger(log)
	ctrl.SetLogger(logr.FromSlogHandler(log.Handler()))
	return log
}

// Manager builds sub's Env. EZGH_CLOUD_KUBECONFIG, EZGH_CLOUD_CONFIG and EZGH_CREDENTIALS_URL
// override the control-plane VM's paths (local runs, tests).
func Manager(sub string, opts ...Option) (*Env, error) {
	log := Logger(sub)
	kubeconfig := getenv("EZGH_CLOUD_KUBECONFIG", Kubeconfig)
	rest, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, err
	}
	rest.UserAgent = "ezgh-cloud-" + sub
	o := ctrl.Options{
		Metrics:                       metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress:        "0",
		LeaderElection:                true,
		LeaderElectionID:              "ezgh-cloud-" + sub,
		LeaderElectionNamespace:       "kube-system",
		LeaderElectionReleaseOnCancel: true,
		GracefulShutdownTimeout:       ptr.To(30 * time.Second),
	}
	for _, opt := range opts {
		opt(&o)
	}
	mgr, err := ctrl.NewManager(rest, o)
	if err != nil {
		return nil, err
	}
	src := &ezghclient.LocalSource{URL: getenv("EZGH_CREDENTIALS_URL", ezghclient.DefaultCredentialsURL)}
	return &Env{
		Manager:    mgr,
		Clients:    ezghclient.New(src, "ezgh-cloud-"+sub),
		Log:        log,
		ConfigPath: getenv("EZGH_CLOUD_CONFIG", config.Path),
	}, nil
}

// Start runs the manager until SIGTERM or SIGINT, then drains: leader election is released and
// running reconciles get GracefulShutdownTimeout to finish.
func (e *Env) Start() error { return e.Manager.Start(ctrl.SetupSignalHandler()) }

// Recorder is the events.k8s.io recorder controllers report on.
func (e *Env) Recorder(name string) events.EventRecorder { return e.Manager.GetEventRecorder(name) }

// RateLimiter is every reconciler's backoff: 5 s doubling to 5 min per item, or the wait an
// answer's Retry-After asked for (recorded by Fail) when that is longer.
func RateLimiter() workqueue.TypedRateLimiter[reconcile.Request] {
	return workqueue.NewTypedMaxOfRateLimiter[reconcile.Request](
		workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](5*time.Second, 5*time.Minute),
		retryAfterLimiter{},
	)
}

// retryAfters holds the last Retry-After per object, read once by the limiter. Objects of
// different kinds with one name share an entry, which can only lengthen a wait.
var retryAfters sync.Map // reconcile.Request → time.Duration

type retryAfterLimiter struct{}

func (retryAfterLimiter) When(item reconcile.Request) time.Duration {
	if d, ok := retryAfters.LoadAndDelete(item); ok {
		return d.(time.Duration)
	}
	return 0
}
func (retryAfterLimiter) Forget(item reconcile.Request)     { retryAfters.Delete(item) }
func (retryAfterLimiter) NumRequeues(reconcile.Request) int { return 0 }

// busyPoll is how soon an object is reconciled again while its resource is busy.
const busyPoll = 15 * time.Second

// Busy reports whether err is a 409 that only means "not yet": another operation on the resource
// (often our own create) is still running, or the load balancer is still being made. It returns
// how soon to try again.
func Busy(err error) (time.Duration, bool) {
	e, ok := apiclient.AsError(err)
	if !ok || e.Status != http.StatusConflict || e.Code != "operation_in_progress" && e.Code != "load_balancer_provisioning" {
		return 0, false
	}
	return max(busyPoll, e.RetryAfter), true
}

// Fail reports err on obj and hands it back for the workqueue's backoff, which keeps growing while
// the errors last; a Retry-After longer than the backoff is honoured. A Busy err isn't a failure:
// it's tried again soon, without an event or a growing backoff.
func Fail(rec events.EventRecorder, obj runtime.Object, err error) (reconcile.Result, error) {
	if d, ok := Busy(err); ok {
		return reconcile.Result{RequeueAfter: d}, nil
	}
	if d := cloudevents.Report(rec, obj, err); d > 0 {
		if m, merr := meta.Accessor(obj); merr == nil {
			retryAfters.Store(reconcile.Request{NamespacedName: types.NamespacedName{Namespace: m.GetNamespace(), Name: m.GetName()}}, d)
		}
	}
	return reconcile.Result{}, err
}

func getenv(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
