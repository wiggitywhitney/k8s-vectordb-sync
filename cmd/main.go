/*
Copyright 2026.

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
	"context"
	"flag"
	"os"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/wiggitywhitney/k8s-vectordb-sync/internal/config"
	"github.com/wiggitywhitney/k8s-vectordb-sync/internal/controller"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	cfg := config.Load()
	setupLog.Info("Configuration loaded",
		"restEndpoint", cfg.RESTEndpoint,
		"debounceWindow", cfg.DebounceWindow,
		"batchFlushInterval", cfg.BatchFlushInterval,
		"batchMaxSize", cfg.BatchMaxSize,
		"resyncInterval", cfg.ResyncInterval,
		"watchResourceTypes", cfg.WatchResourceTypes,
		"excludeResourceTypes", cfg.ExcludeResourceTypes,
		"logLevel", cfg.LogLevel,
	)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "57d01317.vectordbsync.io",
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	// Create the resource watcher
	watcher, err := controller.NewWatcher(
		ctrl.Log.WithName("watcher"),
		mgr.GetConfig(),
		cfg,
	)
	if err != nil {
		setupLog.Error(err, "Failed to create resource watcher")
		os.Exit(1)
	}

	// Add watcher as a runnable so it starts with the manager
	if err := mgr.Add(wrapWatcher(watcher)); err != nil {
		setupLog.Error(err, "Failed to add watcher to manager")
		os.Exit(1)
	}

	// Start a goroutine to log events from the watcher (M1 console output)
	go logEvents(watcher)

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}

// watcherRunnable wraps the Watcher to implement the manager.Runnable interface.
type watcherRunnable struct {
	watcher *controller.Watcher
}

func wrapWatcher(w *controller.Watcher) *watcherRunnable {
	return &watcherRunnable{watcher: w}
}

func (r *watcherRunnable) Start(ctx context.Context) error {
	return r.watcher.Start(ctx)
}

// logEvents reads from the watcher's event channel and logs each event.
// This is the M1 console output — later milestones will replace this with
// the debouncer/batcher that forwards events to the REST API.
func logEvents(watcher *controller.Watcher) {
	log := ctrl.Log.WithName("events")
	for event := range watcher.Events {
		log.Info("Resource change detected",
			"type", event.Type,
			"id", event.Instance.ID,
			"kind", event.Instance.Kind,
			"namespace", event.Instance.Namespace,
			"name", event.Instance.Name,
		)
	}
}
