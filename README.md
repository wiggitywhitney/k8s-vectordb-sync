# k8s-vectordb-sync

Kubernetes controller that watches cluster resources via dynamic informers and syncs instance metadata to [cluster-whisperer](https://github.com/wiggitywhitney/cluster-whisperer)'s REST API. Built with Go and [Kubebuilder](https://book.kubebuilder.io/).

This controller is one half of a real-time resource sync pipeline. It watches Kubernetes for resource changes (create, update, delete), extracts lightweight metadata, and POSTs batched payloads to cluster-whisperer's REST endpoint. cluster-whisperer handles embedding generation and vector database storage, making resources searchable via natural language. Together, they keep cluster-whisperer's vector search tool up to date with what is currently running in the cluster.

The controller also detects CRD (Custom Resource Definition) changes — when operators are installed or removed, it notifies cluster-whisperer so the capabilities collection stays current. This enables questions like "what certificate management capabilities does this cluster have?" to work immediately after installing cert-manager.

The controller is a pure observer — it has no knowledge of the vector database, embeddings, or search. It watches Kubernetes and forwards metadata via HTTP.

## How It Works

```text
Kubernetes Cluster          k8s-vectordb-sync           cluster-whisperer          Vector DB
┌──────────────────┐        ┌───────────────────┐       ┌─────────────────┐      ┌──────────┐
│ Resource Events  │        │                   │ POST  │ REST Endpoint   │embed │ ChromaDB │
│ (instances)      ├──────> │ Instance Pipeline ├─────> │ /instances/sync ├────> │instances │
│                  │debounce│ (metadata extract) │       │ instanceToDoc() │store │collection│
├──────────────────┤        ├───────────────────┤       ├─────────────────┤      ├──────────┤
│ CRD Events       │        │                   │ POST  │ REST Endpoint   │infer │ ChromaDB │
│ (add/delete)     ├──────> │ CRD Pipeline      ├─────> │ /capabilities/  ├────> │capabilit.│
│                  │debounce│ (CRD names only)  │       │ scan            │store │collection│
└──────────────────┘ batch  └───────────────────┘       └─────────────────┘      └──────────┘
```

The controller runs two parallel pipelines:

### Instance Pipeline

1. **Watch** — Dynamic informers discover and watch configurable resource types across the cluster.
2. **Extract** — Lightweight metadata is pulled from each resource, including namespace, name, kind, labels, and description annotations.
3. **Debounce** — When a resource changes multiple times in quick succession, the controller waits for changes to settle before syncing. Only the latest state is sent (last-state-wins), which avoids flooding the API with intermediate states.
4. **Batch** — Changes are accumulated into payloads with separate upsert and delete lists.
5. **Send** — Batched payloads are POSTed to cluster-whisperer, which handles embedding generation and vector database storage.
6. **Resync** — A periodic full resync ensures eventual consistency.

Delete events bypass the debounce window and are forwarded immediately to minimize stale data in search results.

### CRD Pipeline

When `CAPABILITIES_ENDPOINT` is configured, the controller also watches for CRD changes:

1. **Detect** — CRD add and delete events are identified and routed to a separate pipeline. CRD update events are ignored (CRD spec changes are rare, and the inference pipeline is idempotent).
2. **Debounce/Batch** — CRD add events are debounced and batched, since operator installs land many CRDs at once (e.g., cert-manager adds 6 CRDs). CRD deletes bypass the debounce window and are forwarded immediately.
3. **Send** — Batched CRD names are POSTed to cluster-whisperer's capabilities scan endpoint, which runs schema discovery and LLM inference to populate the capabilities collection.

The CRD pipeline is disabled by default. Set `CAPABILITIES_ENDPOINT` to enable it.

## Quick Start

### Prerequisites

- [Helm](https://helm.sh/) (for deploying to a cluster)
- A kubeconfig pointing at the Kubernetes cluster you want to watch. If you do not have a cluster, consider creating a local one with [Kind](https://kind.sigs.k8s.io/).
- [cluster-whisperer](https://github.com/wiggitywhitney/cluster-whisperer) running with the `serve` command, reachable from the cluster

### Deploy

```bash
helm install k8s-vectordb-sync charts/k8s-vectordb-sync \
  --namespace k8s-vectordb-sync-system \
  --create-namespace \
  --set config.instancesEndpoint=http://cluster-whisperer:3000/api/v1/instances/sync \
  --set config.capabilitiesEndpoint=http://cluster-whisperer:3000/api/v1/capabilities/scan
```

To deploy without CRD detection (instance sync only), omit the `capabilitiesEndpoint` flag.

Verify the controller is running:

```bash
kubectl get pods -n k8s-vectordb-sync-system
```

The controller starts watching resources immediately after the pod becomes ready. Changes are debounced and batched before being sent to cluster-whisperer's REST endpoint.

## Configuration

All configuration is through environment variables. When deploying with Helm, these are set via `values.yaml` under `config.*`.

| Variable | Default | Description |
|----------|---------|-------------|
| `INSTANCES_ENDPOINT` | `http://localhost:3000/api/v1/instances/sync` | cluster-whisperer instance sync URL |
| `CAPABILITIES_ENDPOINT` | *(empty = disabled)* | cluster-whisperer capabilities scan URL |
| `DEBOUNCE_WINDOW_MS` | `10000` | How long to wait (in milliseconds) after a resource changes before syncing it. If the resource changes again during this window, the timer resets. |
| `BATCH_FLUSH_INTERVAL_MS` | `5000` | Maximum time in milliseconds before sending accumulated changes, even if the batch is not full. |
| `BATCH_MAX_SIZE` | `50` | Maximum number of upserts in a single payload. When this limit is reached, the batch is sent immediately. |
| `RESYNC_INTERVAL_MIN` | `1440` | How often (in minutes) the controller performs a full resync of all resources. The default is 24 hours. |
| `WATCH_RESOURCE_TYPES` | *(empty = all)* | Comma-separated list of resource types to watch. When empty, the controller watches all discoverable types. |
| `EXCLUDE_RESOURCE_TYPES` | `events,leases,endpointslices,customresourcedefinitions` | Comma-separated list of resource types to skip. These are excluded because they change frequently and have low value for semantic search. CRDs are excluded from the instance pipeline because they are type definitions, not running workloads — CRD changes are routed to the capabilities pipeline instead. |
| `API_BIND_ADDRESS` | `:8082` | Bind address for the resync trigger API. |
| `LOG_LEVEL` | `info` | Logging verbosity. |

### Resource Filtering

By default, the controller watches all discoverable resource types except high-churn ones (Events, Leases, EndpointSlices). You can narrow the scope to only the types you care about:

```bash
# Watch only specific types
helm install k8s-vectordb-sync charts/k8s-vectordb-sync \
  --namespace k8s-vectordb-sync-system \
  --create-namespace \
  --set config.instancesEndpoint=http://cluster-whisperer:3000/api/v1/instances/sync \
  --set config.watchResourceTypes="deployments,services,statefulsets,configmaps"
```

Or add types to the exclusion list:

```bash
--set config.excludeResourceTypes="events,leases,endpointslices,secrets"
```

## Integration with cluster-whisperer

### Payload Format

The controller POSTs JSON payloads to cluster-whisperer's `/api/v1/instances/sync` endpoint. Each payload contains upserts (resources that were created or updated) and deletes (resource IDs that were removed):

```json
{
  "upserts": [
    {
      "id": "default/apps/v1/Deployment/nginx",
      "namespace": "default",
      "name": "nginx",
      "kind": "Deployment",
      "apiVersion": "apps/v1",
      "apiGroup": "apps",
      "labels": { "app": "nginx" },
      "annotations": { "description": "Web server" },
      "createdAt": "2026-02-20T10:00:00Z"
    }
  ],
  "deletes": [
    "default/apps/v1/Deployment/old-service"
  ]
}
```

cluster-whisperer receives this payload, generates embeddings for each resource, and stores them in ChromaDB. The controller does not need to know anything about how embedding or storage works.

### CRD Payload Format

When the CRD pipeline is enabled, the controller POSTs CRD changes to the `/api/v1/capabilities/scan` endpoint. The payload contains only CRD fully-qualified names — cluster-whisperer handles schema discovery and capability inference.

**Add events** (batched — operator installs land many CRDs at once):

```json
{
  "added": [
    "certificates.cert-manager.io",
    "issuers.cert-manager.io",
    "clusterissuers.cert-manager.io"
  ]
}
```

**Delete events** (immediate — stale capabilities are worse than extra API calls):

```json
{
  "deleted": [
    "certificates.cert-manager.io"
  ]
}
```

### Starting cluster-whisperer

cluster-whisperer must be running in `serve` mode and reachable from the controller pod. In the cluster-whisperer directory:

```bash
# Requires: VOYAGE_API_KEY set, ChromaDB running
npx tsx src/index.ts serve --port 3000 --chroma-url http://localhost:8000
```

See the [cluster-whisperer README](https://github.com/wiggitywhitney/cluster-whisperer) for full setup instructions.

## Observability

### Metrics Endpoint

The controller exposes a Prometheus-compatible metrics endpoint over HTTPS at `/metrics` on port 8443. Metrics are served with TLS using certificates managed by [cert-manager](https://cert-manager.io/) and are protected by Kubernetes RBAC (the `metrics-reader` ClusterRole).

To access metrics manually:

```bash
# Port-forward to the metrics service
kubectl port-forward svc/k8s-vectordb-sync-controller-manager-metrics-service \
  8443:8443 -n k8s-vectordb-sync-system

# Get a service account token
TOKEN=$(kubectl create token k8s-vectordb-sync-controller-manager -n k8s-vectordb-sync-system)

# Fetch metrics (TLS, skip verify for self-signed certs)
curl -k -H "Authorization: Bearer $TOKEN" https://localhost:8443/metrics
```

The controller emits standard controller-runtime metrics including reconciliation counts, queue depth, and Go runtime stats.

### Prometheus / ServiceMonitor

A `ServiceMonitor` is included in the Kustomize manifests at `config/prometheus/`. If you use the Prometheus Operator, it will be deployed automatically and Prometheus will scrape the metrics endpoint.

### Health Probes

The controller exposes health and readiness probes:

| Endpoint | Port | Purpose |
|----------|------|---------|
| `/healthz` | 8081 | Liveness probe |
| `/readyz` | 8081 | Readiness probe |

### Triggering a Resync

The controller performs a full resync on a schedule (default: every 24 hours). You can also trigger one manually by sending a POST request to the controller's API:

```bash
# Port-forward to the controller's API port
kubectl port-forward svc/k8s-vectordb-sync 8082:8082 -n k8s-vectordb-sync-system

# Trigger a full resync
curl -X POST http://localhost:8082/api/v1/resync
```

This re-lists all watched resource types and sends them to cluster-whisperer, which is useful after deploying the controller for the first time or if you suspect the vector database is out of sync.
