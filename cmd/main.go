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
	"time"

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

	"github.com/wiggitywhitney/k8s-vectordb-sync/internal/api"
	"github.com/wiggitywhitney/k8s-vectordb-sync/internal/client"
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
	var metricsCertPath string
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "", "The path to the TLS certificates for the metrics server. "+
		"When set, the metrics endpoint will be served over HTTPS.")
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
		"apiBindAddress", cfg.APIBindAddress,
		"logLevel", cfg.LogLevel,
	)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress:   metricsAddr,
			SecureServing: metricsCertPath != "",
			CertDir:       metricsCertPath,
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

	// Create debounce buffer that batches watcher events
	debouncer := controller.NewDebounceBuffer(
		ctrl.Log.WithName("debounce"),
		cfg,
	)

	// Add debounce buffer as a runnable
	if err := mgr.Add(wrapDebouncer(debouncer, watcher)); err != nil {
		setupLog.Error(err, "Failed to add debouncer to manager")
		os.Exit(1)
	}

	// REST client sends batched payloads to cluster-whisperer
	restClient := client.New(
		ctrl.Log.WithName("rest-client"),
		cfg.RESTEndpoint,
	)

	// Add sender as a runnable that consumes payloads and POSTs them
	if err := mgr.Add(wrapSender(restClient, debouncer)); err != nil {
		setupLog.Error(err, "Failed to add REST sender to manager")
		os.Exit(1)
	}

	// API server for ad-hoc operations (resync trigger)
	apiServer := api.NewServer(
		ctrl.Log.WithName("api"),
		cfg.APIBindAddress,
		watcher,
	)
	if err := mgr.Add(apiServer); err != nil {
		setupLog.Error(err, "Failed to add API server to manager")
		os.Exit(1)
	}

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

// debouncerRunnable wraps the DebounceBuffer to implement manager.Runnable.
type debouncerRunnable struct {
	debouncer *controller.DebounceBuffer
	watcher   *controller.Watcher
}

func wrapDebouncer(d *controller.DebounceBuffer, w *controller.Watcher) *debouncerRunnable {
	return &debouncerRunnable{debouncer: d, watcher: w}
}

func (r *debouncerRunnable) Start(ctx context.Context) error {
	r.debouncer.Run(ctx, r.watcher.Events)
	return nil
}

// senderRunnable reads batched payloads from the debouncer and POSTs them
// to cluster-whisperer via the REST client.
type senderRunnable struct {
	client    *client.RESTClient
	debouncer *controller.DebounceBuffer
}

func wrapSender(c *client.RESTClient, d *controller.DebounceBuffer) *senderRunnable {
	return &senderRunnable{client: c, debouncer: d}
}

func (s *senderRunnable) Start(ctx context.Context) error {
	log := ctrl.Log.WithName("sender")
	for payload := range s.debouncer.Payloads {
		// Use the manager context while it's active. When it cancels,
		// the debouncer flushes remaining payloads and closes the channel.
		// For those final payloads, use a short-lived context so they
		// can still be sent during graceful shutdown.
		sendCtx := ctx
		if ctx.Err() != nil {
			drainCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			sendCtx = drainCtx
			log.Info("Draining final payload during shutdown",
				"upserts", len(payload.Upserts),
				"deletes", len(payload.Deletes),
			)
			err := s.client.Send(sendCtx, payload)
			cancel()
			if err != nil {
				log.Error(err, "Failed to send payload during shutdown",
					"upserts", len(payload.Upserts),
					"deletes", len(payload.Deletes),
				)
			} else {
				log.V(1).Info("Payload sent during shutdown",
					"upserts", len(payload.Upserts),
					"deletes", len(payload.Deletes),
				)
			}
			continue
		}

		if err := s.client.Send(sendCtx, payload); err != nil {
			log.Error(err, "Failed to send payload",
				"upserts", len(payload.Upserts),
				"deletes", len(payload.Deletes),
			)
			continue
		}
		log.V(1).Info("Payload sent",
			"upserts", len(payload.Upserts),
			"deletes", len(payload.Deletes),
		)
	}
	return nil
}
