package controller

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	"github.com/wiggitywhitney/k8s-vectordb-sync/internal/config"
	"github.com/wiggitywhitney/k8s-vectordb-sync/internal/filter"
	"github.com/wiggitywhitney/k8s-vectordb-sync/internal/metadata"
)

// EventType represents the kind of change detected by an informer.
type EventType string

const (
	EventAdd    EventType = "ADD"
	EventUpdate EventType = "UPDATE"
	EventDelete EventType = "DELETE"
)

// ResourceEvent is a change event emitted by the watcher for downstream processing.
type ResourceEvent struct {
	Type     EventType
	Instance metadata.ResourceInstance
}

// Watcher discovers API resources, creates dynamic informers, and emits
// ResourceEvents for add/update/delete changes.
type Watcher struct {
	log             logr.Logger
	dynamicClient   dynamic.Interface
	discoveryClient discovery.DiscoveryInterface
	filter          *filter.ResourceFilter
	resyncInterval  time.Duration

	// Events channel for downstream consumers (debouncer/batcher)
	Events chan ResourceEvent

	// watchedGVRs is the set of GVRs discovered and watched during Start().
	// Used by TriggerResync to re-list all resources on demand.
	// Guarded by watchedGVRsMu because Start() writes while TriggerResync/WatchedGVRCount read concurrently.
	watchedGVRsMu sync.RWMutex
	watchedGVRs   []schema.GroupVersionResource

	factory  dynamicinformer.DynamicSharedInformerFactory
	stopCh   chan struct{}
	stopOnce sync.Once
}

// NewWatcher creates a Watcher from the given rest config and controller configuration.
func NewWatcher(log logr.Logger, restConfig *rest.Config, cfg config.Config) (*Watcher, error) {
	dynClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("creating dynamic client: %w", err)
	}

	discoClient, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("creating discovery client: %w", err)
	}

	f := filter.New(cfg.WatchResourceTypes, cfg.ExcludeResourceTypes)

	return &Watcher{
		log:             log,
		dynamicClient:   dynClient,
		discoveryClient: discoClient,
		filter:          f,
		resyncInterval:  cfg.ResyncInterval,
		Events:          make(chan ResourceEvent, 10000),
		stopCh:          make(chan struct{}),
	}, nil
}

// Start discovers watchable resources, sets up informers, and begins watching.
// It blocks until the context is cancelled.
func (w *Watcher) Start(ctx context.Context) error {
	// Propagate context cancellation to stopCh so WaitForCacheSync
	// unblocks if the context is cancelled before caches sync.
	go func() {
		<-ctx.Done()
		w.Stop()
	}()

	discovered := w.discoverResources()
	w.watchedGVRsMu.Lock()
	w.watchedGVRs = discovered
	w.watchedGVRsMu.Unlock()

	w.log.Info("Discovered watchable resources", "count", len(discovered))

	w.factory = dynamicinformer.NewDynamicSharedInformerFactory(w.dynamicClient, w.resyncInterval)

	for _, gvr := range w.watchedGVRs {
		informer := w.factory.ForResource(gvr)
		_, err := informer.Informer().AddEventHandler(w.makeEventHandler())
		if err != nil {
			w.log.Error(err, "Failed to add event handler", "resource", gvr.String())
			continue
		}
		w.log.V(1).Info("Watching resource", "group", gvr.Group, "version", gvr.Version, "resource", gvr.Resource)
	}

	w.factory.Start(w.stopCh)
	w.log.Info("Waiting for informer caches to sync...")
	w.factory.WaitForCacheSync(w.stopCh)
	w.log.Info("Informer caches synced, watching for changes")

	// Block until context is done
	<-ctx.Done()
	return ctx.Err()
}

// Stop shuts down all informers. The Events channel is not closed here
// because informer callbacks may still be in-flight; the debouncer exits
// via its own context cancellation.
func (w *Watcher) Stop() {
	w.stopOnce.Do(func() {
		close(w.stopCh)
	})
}

// discoverResources uses the Kubernetes discovery API to find all resources
// that pass the configured filter. Discovery errors for individual API groups
// are logged but not fatal — partial results are used.
func (w *Watcher) discoverResources() []schema.GroupVersionResource {
	_, resourceLists, err := w.discoveryClient.ServerGroupsAndResources()
	if err != nil {
		// Discovery can return partial results with errors for unavailable groups.
		// Log the error but continue with what we have.
		w.log.V(1).Info("Partial discovery error (continuing with available resources)", "error", err)
	}

	var gvrs []schema.GroupVersionResource
	for _, list := range resourceLists {
		gv, parseErr := schema.ParseGroupVersion(list.GroupVersion)
		if parseErr != nil {
			w.log.V(1).Info("Skipping unparseable group version", "groupVersion", list.GroupVersion, "error", parseErr)
			continue
		}

		for _, resource := range list.APIResources {
			// Skip subresources (e.g., pods/status, deployments/scale)
			if strings.Contains(resource.Name, "/") {
				continue
			}

			if !filter.ShouldWatch(w.filter, resource) {
				continue
			}

			gvrs = append(gvrs, schema.GroupVersionResource{
				Group:    gv.Group,
				Version:  gv.Version,
				Resource: resource.Name,
			})
		}
	}

	return gvrs
}

// makeEventHandler creates cache.ResourceEventHandlerFuncs that emit ResourceEvents.
func (w *Watcher) makeEventHandler() cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				return
			}
			instance := metadata.Extract(u)
			w.emit(ResourceEvent{Type: EventAdd, Instance: instance})
			w.log.V(1).Info("Resource added",
				"kind", instance.Kind, "namespace", instance.Namespace, "name", instance.Name)
		},
		UpdateFunc: func(oldObj, newObj any) {
			oldU, ok1 := oldObj.(*unstructured.Unstructured)
			newU, ok2 := newObj.(*unstructured.Unstructured)
			if !ok1 || !ok2 {
				return
			}
			// Only emit if metadata we care about has changed
			if !metadataChanged(oldU, newU) {
				return
			}
			instance := metadata.Extract(newU)
			w.emit(ResourceEvent{Type: EventUpdate, Instance: instance})
			w.log.V(1).Info("Resource updated",
				"kind", instance.Kind, "namespace", instance.Namespace, "name", instance.Name)
		},
		DeleteFunc: func(obj any) {
			// Handle DeletedFinalStateUnknown wrapper
			if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = tombstone.Obj
			}
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				return
			}
			instance := metadata.Extract(u)
			w.emit(ResourceEvent{Type: EventDelete, Instance: instance})
			w.log.Info("Resource deleted",
				"kind", instance.Kind, "namespace", instance.Namespace, "name", instance.Name)
		},
	}
}

// emit sends a ResourceEvent to the events channel without blocking.
// If the channel is full or the watcher has been stopped, the event is dropped.
func (w *Watcher) emit(event ResourceEvent) {
	select {
	case <-w.stopCh:
		return
	case w.Events <- event:
	default:
		w.log.Info("Event channel full, dropping event",
			"type", event.Type, "id", event.Instance.ID)
	}
}

// metadataChanged checks if the metadata fields we sync have changed between
// old and new versions. Ignores status and spec changes.
func metadataChanged(oldObj, newObj *unstructured.Unstructured) bool {
	if oldObj.GetResourceVersion() == newObj.GetResourceVersion() {
		return false
	}

	// Check labels
	if !mapsEqual(oldObj.GetLabels(), newObj.GetLabels()) {
		return true
	}

	// Check annotations (pre-filter)
	if !mapsEqual(oldObj.GetAnnotations(), newObj.GetAnnotations()) {
		return true
	}

	return false
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TriggerResync performs a full re-list of all watched resource types and
// emits ADD events for every resource found. This pushes complete cluster
// state through the debounce → REST pipeline, ensuring downstream
// consistency. Safe to call while informers are running.
func (w *Watcher) TriggerResync(ctx context.Context) (int, error) {
	w.watchedGVRsMu.RLock()
	gvrs := make([]schema.GroupVersionResource, len(w.watchedGVRs))
	copy(gvrs, w.watchedGVRs)
	w.watchedGVRsMu.RUnlock()

	w.log.Info("Starting ad-hoc resync", "gvrCount", len(gvrs))
	total := 0

	for _, gvr := range gvrs {
		list, err := w.dynamicClient.Resource(gvr).List(ctx, metav1.ListOptions{})
		if err != nil {
			w.log.Error(err, "Failed to list resource during resync", "resource", gvr.String())
			continue
		}

		for i := range list.Items {
			instance := metadata.Extract(&list.Items[i])
			w.emit(ResourceEvent{Type: EventAdd, Instance: instance})
			total++
		}

		w.log.V(1).Info("Resynced resource type",
			"resource", gvr.String(), "count", len(list.Items))
	}

	w.log.Info("Ad-hoc resync complete", "totalResources", total)
	return total, nil
}

// WatchedGVRCount returns the number of GVRs being watched (for health/readiness checks).
func (w *Watcher) WatchedGVRCount() int {
	w.watchedGVRsMu.RLock()
	defer w.watchedGVRsMu.RUnlock()
	return len(w.watchedGVRs)
}

// MetadataChanged exposes the change detection logic for testing.
func MetadataChanged(oldObj, newObj *unstructured.Unstructured) bool {
	return metadataChanged(oldObj, newObj)
}
