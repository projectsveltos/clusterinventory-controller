/*
Copyright 2026. projectsveltos.io. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"flag"
	"fmt"
	"net/http"
	"net/http/pprof"
	"os"
	"time"

	"github.com/spf13/pflag"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cluster-inventory-api/pkg/access"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	controller "github.com/projectsveltos/clusterinventory-controller/controllers"
	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
	logs "github.com/projectsveltos/libsveltos/lib/logsettings"
	//+kubebuilder:scaffold:imports
)

var (
	setupLog             = ctrl.Log.WithName("setup")
	diagnosticsAddress   string
	insecureDiagnostics  bool
	restConfigQPS        float32
	restConfigBurst      int
	webhookPort          int
	syncPeriod           time.Duration
	healthAddr           string
	profilerAddress      string
	concurrentReconciles int
	providerFile         string
)

// Add RBAC for the authorized diagnostics endpoint.
//+kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create
//+kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create

func main() {
	scheme, err := controller.InitScheme()
	if err != nil {
		os.Exit(1)
	}

	klog.InitFlags(nil)
	initFlags(pflag.CommandLine)
	pflag.CommandLine.SetNormalizeFunc(cliflag.WordSepNormalizeFunc)
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	pflag.Parse()

	ctrl.SetLogger(klog.Background())

	ctrlOptions := ctrl.Options{
		Scheme:                 scheme,
		Metrics:                getDiagnosticsOptions(),
		HealthProbeBindAddress: healthAddr,
		WebhookServer: webhook.NewServer(webhook.Options{
			Port: webhookPort,
		}),
		Cache: cache.Options{
			SyncPeriod: &syncPeriod,
		},
		PprofBindAddress: profilerAddress,
	}

	restConfig := ctrl.GetConfigOrDie()
	restConfig.QPS = restConfigQPS
	restConfig.Burst = restConfigBurst

	mgr, err := ctrl.NewManager(restConfig, ctrlOptions)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()

	logs.RegisterForLogSettings(ctx,
		libsveltosv1beta1.ComponentClusterInventory, ctrl.Log.WithName("log-setter"),
		ctrl.GetConfigOrDie())

	var accessCfg *access.Config
	if providerFile != "" {
		accessCfg, err = access.NewFromFile(providerFile)
		if err != nil {
			setupLog.Error(err, "unable to load provider file", "path", providerFile)
			os.Exit(1)
		}
	}

	if err = (&controller.ClusterProfileReconciler{
		Client:               mgr.GetClient(),
		Scheme:               mgr.GetScheme(),
		ConcurrentReconciles: concurrentReconciles,
		AccessConfig:         accessCfg,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ClusterProfile")
		os.Exit(1)
	}

	//+kubebuilder:scaffold:builder

	setupChecks(mgr)

	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func initFlags(fs *pflag.FlagSet) {
	fs.StringVar(&diagnosticsAddress, "diagnostics-address", ":8443",
		"The address the diagnostics endpoint binds to. Served via https with auth by default. "+
			"Use --insecure-diagnostics to serve via http without auth.")

	fs.BoolVar(&insecureDiagnostics, "insecure-diagnostics", false,
		"Enable insecure diagnostics serving. See --diagnostics-address.")

	fs.StringVar(&healthAddr, "health-addr", ":9440",
		"The address the health endpoint binds to.")

	fs.StringVar(&profilerAddress, "profiler-address", "",
		"Bind address to expose the pprof profiler (e.g. localhost:6060)")

	const defaultReconcilers = 10
	fs.IntVar(&concurrentReconciles, "concurrent-reconciles", defaultReconcilers,
		fmt.Sprintf("Maximum number of concurrent reconciles. Defaults to %d", defaultReconcilers))

	const defaultRestConfigQPS = 20
	fs.Float32Var(&restConfigQPS, "kube-api-qps", defaultRestConfigQPS,
		fmt.Sprintf("Maximum queries per second to the Kubernetes API server. Defaults to %d",
			defaultRestConfigQPS))

	const defaultRestConfigBurst = 30
	fs.IntVar(&restConfigBurst, "kube-api-burst", defaultRestConfigBurst,
		fmt.Sprintf("Maximum burst for throttle. Default %d", defaultRestConfigBurst))

	const defaultWebhookPort = 9443
	fs.IntVar(&webhookPort, "webhook-port", defaultWebhookPort, "Webhook Server port")

	const defaultSyncPeriod = 10
	fs.DurationVar(&syncPeriod, "sync-period", defaultSyncPeriod*time.Minute,
		fmt.Sprintf("Minimum interval at which watched resources are reconciled. Default: %d minutes",
			defaultSyncPeriod))

	fs.StringVar(&providerFile, "clusterprofile-provider-file", "",
		"Path to the JSON provider configuration file enabling exec-plugin access providers. "+
			"When empty only the kubeconfig-secretreader provider is supported.")
}

func setupChecks(mgr ctrl.Manager) {
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}
}

func getDiagnosticsOptions() metricsserver.Options {
	if insecureDiagnostics {
		return metricsserver.Options{
			BindAddress:   diagnosticsAddress,
			SecureServing: false,
		}
	}
	return metricsserver.Options{
		BindAddress:    diagnosticsAddress,
		SecureServing:  true,
		FilterProvider: filters.WithAuthenticationAndAuthorization,
		ExtraHandlers: map[string]http.Handler{
			"/debug/pprof/":        http.HandlerFunc(pprof.Index),
			"/debug/pprof/cmdline": http.HandlerFunc(pprof.Cmdline),
			"/debug/pprof/profile": http.HandlerFunc(pprof.Profile),
			"/debug/pprof/symbol":  http.HandlerFunc(pprof.Symbol),
			"/debug/pprof/trace":   http.HandlerFunc(pprof.Trace),
			"/debug/pprof/heap":    pprof.Handler("heap"),
		},
	}
}
