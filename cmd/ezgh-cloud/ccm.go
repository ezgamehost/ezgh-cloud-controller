package main

import (
	"cmp"
	"os"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/util/wait"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/cloud-provider/app"
	"k8s.io/cloud-provider/app/config"
	"k8s.io/cloud-provider/names"
	"k8s.io/cloud-provider/options"
	cliflag "k8s.io/component-base/cli/flag"
	logsapi "k8s.io/component-base/logs/api/v1"
	"k8s.io/klog/v2"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/ccm"
	cloudconfig "github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/config"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/run"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient"
)

func init() { subcommands["ccm"] = runCCM }

// kitLogs is a component-base log format that writes through the kit's logger, so the CCM's own
// logging setup doesn't replace it. The kit logger has no klog verbosity, so -v and --vmodule are
// ignored.
type kitLogs struct{ log logr.Logger }

func (k kitLogs) Create(logsapi.LoggingConfiguration, logsapi.LoggingOptions) (logr.Logger, logsapi.RuntimeControl) {
	return k.log, logsapi.RuntimeControl{}
}

// runCCM runs k8s.io/cloud-provider's cloud controller manager with the ezgh provider: only the
// node controllers, leader-elected through kube-system/ezgh-cloud-ccm, serving on loopback only.
// EZGH_CLOUD_KUBECONFIG, EZGH_CLOUD_CONFIG and EZGH_CREDENTIALS_URL override the VM's paths, as
// for the other subcommands; args are appended to the flags below.
func runCCM(args []string) error {
	log := run.Logger("ccm")
	if err := logsapi.RegisterLogFormat("ezgh", kitLogs{logr.FromSlogHandler(log.Handler())}, logsapi.LoggingStableOptions); err != nil {
		return err
	}
	ccm.Register(&ccm.Cloud{
		Clients:    ezghclient.New(&ezghclient.LocalSource{URL: os.Getenv("EZGH_CREDENTIALS_URL")}, "ezgh-cloud-ccm"),
		ConfigPath: cmp.Or(os.Getenv("EZGH_CLOUD_CONFIG"), cloudconfig.Path),
	})
	opts, err := options.NewCloudControllerManagerOptions()
	if err != nil {
		return err
	}
	cmd := app.NewCloudControllerManagerCommand(opts, initCloud, app.DefaultInitFuncConstructors, names.CCMControllerAliases(), cliflag.NamedFlagSets{}, wait.NeverStop)
	cmd.SetArgs(append([]string{
		"--cloud-provider=" + ccm.ProviderName,
		"--controllers=" + names.CloudNodeController + "," + names.CloudNodeLifecycleController,
		"--kubeconfig=" + cmp.Or(os.Getenv("EZGH_CLOUD_KUBECONFIG"), run.Kubeconfig),
		"--leader-elect=true",
		"--leader-elect-resource-name=ezgh-cloud-ccm",
		"--leader-elect-resource-namespace=kube-system",
		"--bind-address=127.0.0.1",
		"--logging-format=ezgh",
	}, args...))
	cmd.SilenceUsage, cmd.SilenceErrors = true, true // main prints the error once
	return cmd.Execute()
}

func initCloud(c *config.CompletedConfig) cloudprovider.Interface {
	cloud, err := cloudprovider.InitCloudProvider(c.ComponentConfig.KubeCloudShared.CloudProvider.Name, "")
	if err != nil || cloud == nil {
		// The command has no error path here; the unit restarts us.
		klog.Fatalf("ezgh cloud provider: %v", err)
	}
	return cloud
}
