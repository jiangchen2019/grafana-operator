package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/integr8ly/grafana-operator/pkg/apis"
	"github.com/integr8ly/grafana-operator/pkg/controller"
	"github.com/integr8ly/grafana-operator/pkg/controller/common"
	"github.com/integr8ly/grafana-operator/pkg/controller/grafanadashboard"
	"github.com/integr8ly/grafana-operator/version"
	"github.com/operator-framework/operator-sdk/pkg/k8sutil"
	"github.com/operator-framework/operator-sdk/pkg/leader"
	"github.com/operator-framework/operator-sdk/pkg/ready"
	sdkVersion "github.com/operator-framework/operator-sdk/version"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
	"sigs.k8s.io/controller-runtime/pkg/runtime/signals"
)

var log = logf.Log.WithName("cmd")
var flagImage string
var flagImageTag string
var flagPluginsInitContainerImage string
var flagPluginsInitContainerTag string
var flagPodLabelValue string
var flagNamespaces string
var scanAll bool
var flagImagePullSecret string

func printVersion() {
	log.Info(fmt.Sprintf("Go Version: %s", runtime.Version()))
	log.Info(fmt.Sprintf("Go OS/Arch: %s/%s", runtime.GOOS, runtime.GOARCH))
	log.Info(fmt.Sprintf("operator-sdk Version: %v", sdkVersion.Version))
	log.Info(fmt.Sprintf("operator Version: %v", version.Version))
}

func init() {
	flagset := flag.CommandLine
	flagset.StringVar(&flagImage, "grafana-image", "", "Overrides the default Grafana image")
	flagset.StringVar(&flagImageTag, "grafana-image-tag", "", "Overrides the default Grafana image tag")
	flagset.StringVar(&flagPluginsInitContainerImage, "grafana-plugins-init-container-image", "", "Overrides the default Grafana Plugins Init Container image")
	flagset.StringVar(&flagPluginsInitContainerTag, "grafana-plugins-init-container-tag", "", "Overrides the default Grafana Plugins Init Container tag")
	flagset.StringVar(&flagPodLabelValue, "pod-label-value", common.PodLabelDefaultValue, "Overrides the default value of the app label")
	flagset.StringVar(&flagNamespaces, "namespaces", "", "Namespaces to scope the interaction of the Grafana operator. Mutually exclusive with --scan-all")
	flagset.BoolVar(&scanAll, "scan-all", false, "Scans all namespaces for dashboards")
	flagset.StringVar(&flagImagePullSecret, "image-pull-secret", "", "secret to pull grafana image")
	flagset.Parse(os.Args[1:])
}

// Starts a separate controller for the dashboard reconciliation in the background
func startDashboardController(ns string, cfg *rest.Config, signalHandler <-chan struct{}) {
	// Create a new Cmd to provide shared dependencies and start components
	dashboardMgr, err := manager.New(cfg, manager.Options{Namespace: ns})
	if err != nil {
		log.Error(err, "")
		os.Exit(1)
	}

	// Setup Scheme for the dashboard resource
	if err := apis.AddToScheme(dashboardMgr.GetScheme()); err != nil {
		log.Error(err, "")
		os.Exit(1)
	}

	// Use a separate manager for the dashboard controller
	grafanadashboard.Add(dashboardMgr)

	go func() {
		if err := dashboardMgr.Start(signalHandler); err != nil {
			log.Error(err, "dashboard manager exited non-zero")
			os.Exit(1)
		}
	}()
}

// Get the trimmed and sanitized list of namespaces (if --namespaces was provided)
func getSanitizedNamespaceList() []string {
	provided := strings.Split(flagNamespaces, ",")
	var selected []string

	for _, v := range provided {
		v = strings.TrimSpace(v)

		if v != "" {
			selected = append(selected, v)
		}
	}

	return selected
}

func main() {
	// The logger instantiated here can be changed to any logger
	// implementing the logr.Logger interface. This logger will
	// be propagated through the whole operator, generating
	// uniform and structured logs.
	logf.SetLogger(logf.ZapLogger(false))

	printVersion()

	namespace, err := k8sutil.GetWatchNamespace()
	if err != nil {
		log.Error(err, "failed to get watch namespace")
		os.Exit(1)
	}

	if scanAll && flagNamespaces != "" {
		fmt.Fprint(os.Stderr, "--scan-all and --namespaces both set. Please provide only one")
		os.Exit(1)
	}

	// Controller configuration
	controllerConfig := common.GetControllerConfig()
	controllerConfig.AddConfigItem(common.ConfigGrafanaImage, flagImage)
	controllerConfig.AddConfigItem(common.ConfigGrafanaImageTag, flagImageTag)
	controllerConfig.AddConfigItem(common.ConfigPluginsInitContainerImage, flagPluginsInitContainerImage)
	controllerConfig.AddConfigItem(common.ConfigPluginsInitContainerTag, flagPluginsInitContainerTag)
	controllerConfig.AddConfigItem(common.ConfigPodLabelValue, flagPodLabelValue)
	controllerConfig.AddConfigItem(common.ConfigOperatorNamespace, namespace)
	controllerConfig.AddConfigItem(common.ConfigDashboardLabelSelector, "")
	controllerConfig.AddConfigItem(common.ConfigImagePullSecret, flagImagePullSecret)

	// Get the namespaces to scan for dashboards
	// It's either the same namespace as the controller's or it's all namespaces if the
	// --scan-all flag has been passed
	var dashboardNamespaces = []string{namespace}
	if scanAll {
		dashboardNamespaces = []string{""}
		log.Info("Scanning for dashboards in all namespaces")
	}

	if flagNamespaces != "" {
		dashboardNamespaces = getSanitizedNamespaceList()
		if len(dashboardNamespaces) == 0 {
			fmt.Fprint(os.Stderr, "--namespaces provided but no valid namespaces in list")
			os.Exit(1)
		}
		log.Info(fmt.Sprintf("Scanning for dashboards in the following namespaces: [%s]", strings.Join(dashboardNamespaces, ",")))
	}

	// Get a config to talk to the apiserver
	cfg, err := config.GetConfig()
	if err != nil {
		log.Error(err, "")
		os.Exit(1)
	}

	// Become the leader before proceeding
	leader.Become(context.TODO(), "grafana-operator-lock")

	r := ready.NewFileReady()
	err = r.Set()
	if err != nil {
		log.Error(err, "")
		os.Exit(1)
	}
	defer r.Unset()

	// Create a new Cmd to provide shared dependencies and start components
	mgr, err := manager.New(cfg, manager.Options{Namespace: namespace})
	if err != nil {
		log.Error(err, "")
		os.Exit(1)
	}

	log.Info("Registering Components.")

	// Setup Scheme for all resources
	if err := apis.AddToScheme(mgr.GetScheme()); err != nil {
		log.Error(err, "")
		os.Exit(1)
	}

	// Setup all Controllers
	if err := controller.AddToManager(mgr); err != nil {
		log.Error(err, "")
		os.Exit(1)
	}

	log.Info("Starting the Cmd.")

	// Starting the resource auto-detection for the grafana controller
	autodetect, err := common.NewAutoDetect(mgr)
	if err != nil {
		log.Error(err, "failed to start the background process to auto-detect the operator capabilities")
	} else {
		autodetect.Start()
		defer autodetect.Stop()
	}

	signalHandler := signals.SetupSignalHandler()

	// Start one dashboard controller per watch namespace
	for _, ns := range dashboardNamespaces {
		startDashboardController(ns, cfg, signalHandler)
	}

	if err := mgr.Start(signalHandler); err != nil {
		log.Error(err, "manager exited non-zero")
		os.Exit(1)
	}
}
