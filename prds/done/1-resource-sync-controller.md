# PRD #1: Kubernetes Resource Sync Controller

**Status**: Complete
**Created**: 2026-02-20
**GitHub Issue**: [#1](https://github.com/wiggitywhitney/k8s-vectordb-sync/issues/1)

---

## Problem Statement

The cluster-whisperer agent needs real-time awareness of resource instances running in a Kubernetes cluster. The current approach (PRD #26 in cluster-whisperer) uses batch sync — a CLI command that discovers all resources and loads them into the vector database. This works for small POC clusters but has two fundamental problems:

1. **Doesn't scale for large clusters.** Full discovery re-lists every resource type on every run. In clusters with thousands of resources, this is expensive and slow.
2. **No change detection between runs.** If a resource is created or deleted between manual syncs, the vector database is stale with no mechanism to detect or respond to the change.

Kubernetes already solves both problems natively — informers provide event-driven notifications of changes, backed by etcd as the source of truth. A controller that watches for changes and syncs them in real-time is the correct architectural pattern.

## Solution

Build a Kubernetes controller using Go and Kubebuilder that:
1. Watches resource instances across configurable resource types via dynamic informers
2. Debounces and batches change events to avoid overwhelming the downstream API
3. POSTs instance metadata to a REST API endpoint on cluster-whisperer, which handles embedding and vector DB storage
4. Runs periodic full resyncs for eventual consistency

This follows the same architecture as Viktor Farcic's [dot-ai-controller](https://github.com/vfarcic/dot-ai-controller), which is the reference implementation. The controller is a pure HTTP client — it watches Kubernetes and forwards metadata. It has no knowledge of the vector database, embeddings, or Chroma.

### Why Controller → REST API (Not Controller → Chroma Directly)

The controller POSTs raw metadata to cluster-whisperer's REST endpoint. cluster-whisperer handles document formatting, embedding generation, and vector DB storage. This separation means:
- **Controller stays simple** — just watches and forwards, no embedding logic
- **Document schema owned in one place** — cluster-whisperer controls how instances become vector documents
- **DB-agnostic** — switching from Chroma to Qdrant only requires changes in cluster-whisperer, not the controller
- **Matches Viktor's pattern** — his controller POSTs to his MCP server the same way

### Reference Implementation: Viktor's dot-ai-controller

Viktor's controller (Go/Kubebuilder) provides the architectural blueprint:
- Dynamic informers watching nearly all resource types
- 10-second debounce window with last-state-wins deduplication
- Batches changes and POSTs to MCP server via REST
- Full resync every 60 minutes for eventual consistency
- Syncs: namespace, name, kind, apiVersion, labels, description annotations, timestamps
- Does NOT sync: spec, status, managedFields (fetched on-demand by the agent)
- Configuration via Custom Resources (ResourceSyncConfig, CapabilityScanConfig)
- See `docs/viktors-pipeline-assessment.md` in cluster-whisperer for full analysis

Our version will follow this pattern but start simpler — environment variable configuration instead of CRDs, focused on the resource instance use case.

---

## Success Criteria

- [x] Controller watches resource instances via Kubernetes informers (event-driven, not polling)
- [x] Changes (create/update/delete) are debounced and batched before sending
- [x] Controller POSTs instance metadata to cluster-whisperer REST API
- [x] cluster-whisperer REST endpoint receives metadata and stores in vector DB
- [x] Periodic full resync ensures eventual consistency
- [x] Controller deploys in-cluster via Helm chart
- [x] End-to-end: deploy a resource → controller detects → vector DB updated → agent can find it

## Milestones

- [x] **M1**: Project Setup & Basic Watching
  - Initialize Go project with Kubebuilder
  - Implement dynamic informers for configurable resource types
  - Extract metadata per resource instance (namespace, name, kind, apiVersion, labels, description annotations)
  - Filter high-churn / low-value resources (Events, Leases, EndpointSlices)
  - Console output showing add/update/delete events as they happen
  - Unit tests for metadata extraction and resource filtering

- [x] **M2**: Debouncing & Batching
  - Debounce rapid changes with configurable window (default 10 seconds)
  - Last-state-wins deduplication (multiple updates to same resource → single event)
  - Batch multiple changes into a single payload
  - Separate upsert and delete batches
  - Unit tests for debounce timing, dedup logic, batch assembly

- [x] **M3**: REST API Integration (spans both repos)
  - **In k8s-vectordb-sync**: REST client that POSTs batched instance metadata to a configurable endpoint
  - **In cluster-whisperer**: REST endpoint that receives instance metadata and stores via existing `storeInstances()` pipeline
  - **In cluster-whisperer**: REST endpoint for delete requests that removes instances from vector DB
  - Handle connection errors, retries, and timeouts
  - Integration tests for the full POST → store flow

- [x] **M4**: Periodic Resync & Reliability
  - Full resync on configurable interval (default 24 hours), plus ad-hoc HTTP trigger (`POST /api/v1/resync`)
  - Reconnection logic when informer watches drop (built into controller-runtime SharedInformer)
  - Health and readiness endpoints (`/healthz`, `/readyz`)
  - Graceful shutdown (drain in-flight batches before exit with fresh context)

- [x] **M5**: Deployment & Configuration
  - Dockerfile (multi-arch: amd64 + arm64)
  - Helm chart with RBAC (ClusterRole: get/list/watch on configurable resource types)
  - Configuration via environment variables (REST endpoint URL, debounce window, resync interval, resource type filter)
  - GitHub Actions CI (build, test, image push)

- [x] **M6**: End-to-End Validation
  - Test full pipeline: controller → REST → vector DB → agent search
  - Test with cluster-whisperer demo cluster scenario
  - Test the semantic bridge pattern: deploy resource → controller syncs → agent finds it
  - Document controller setup, configuration, and integration with cluster-whisperer

## Technical Approach

### Instance Metadata Payload

The controller POSTs this JSON structure to the cluster-whisperer REST endpoint:

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
      "labels": { "app": "nginx", "tier": "frontend" },
      "annotations": { "description": "Main web server" },
      "createdAt": "2026-02-20T10:00:00Z"
    }
  ],
  "deletes": [
    "default/apps/v1/Deployment/old-service"
  ]
}
```

This matches the `ResourceInstance` type in cluster-whisperer. The REST endpoint in cluster-whisperer converts these to `VectorDocument` objects using the existing `instanceToDocument()` function.

### Resource Filtering

**Default excluded resource types** (high-churn, low-value for semantic search):
- Events, Leases, EndpointSlices, ComponentStatuses
- Anything without a `list` verb

**Configurable via environment variable:**
```bash
# Watch specific types only
WATCH_RESOURCE_TYPES=deployments,services,statefulsets,configmaps

# Or exclude specific types (default includes all except the built-in exclusions)
EXCLUDE_RESOURCE_TYPES=events,leases,endpointslices
```

### Debouncing Strategy

Following Viktor's pattern:
1. Change event arrives from informer
2. Start (or reset) a per-resource timer (default 10 seconds)
3. When timer fires, add the resource's latest state to the current batch
4. When batch reaches size threshold or flush interval, POST to REST endpoint
5. Delete events skip the debounce — forward immediately to avoid stale data in search results

### Configuration

Environment variable configuration for the POC (CRD-based config is a future enhancement):

| Variable | Default | Description |
|----------|---------|-------------|
| `REST_ENDPOINT` | `http://localhost:3000/api/v1/instances/sync` | cluster-whisperer REST URL |
| `DEBOUNCE_WINDOW_MS` | `10000` | Debounce window per resource |
| `BATCH_FLUSH_INTERVAL_MS` | `5000` | Maximum time before flushing a batch |
| `BATCH_MAX_SIZE` | `50` | Maximum batch size before flush |
| `RESYNC_INTERVAL_MIN` | `1440` | Full resync interval in minutes (default: daily) |
| `WATCH_RESOURCE_TYPES` | *(empty = all)* | Comma-separated resource types to watch |
| `EXCLUDE_RESOURCE_TYPES` | `events,leases,endpointslices` | Comma-separated types to exclude |
| `API_BIND_ADDRESS` | `:8082` | Bind address for the API server (resync trigger) |
| `LOG_LEVEL` | `info` | Logging verbosity |

## Dependencies

- **Go 1.22+** and **Kubebuilder** for controller scaffolding
- **Kubernetes cluster** with appropriate RBAC for the controller
- **cluster-whisperer** must implement the REST endpoint (M3 of this PRD)
- **cluster-whisperer PRD #26 M2** (instance storage) — the REST endpoint reuses `instanceToDocument()` and `storeInstances()`

## Out of Scope

- Capability inference sync (PRD #25 pipeline) — this controller only handles resource instances
- CRD-based configuration (future enhancement, environment variables are sufficient for POC)
- Multi-cluster watching
- Webhook-based triggers (admission webhooks)
- Authentication/authorization on the REST endpoint (future enhancement)

---

## Design Decisions

| Date | Decision | Rationale |
|------|----------|-----------|
| 2026-02-20 | Go + Kubebuilder over TypeScript | Standard ecosystem for K8s controllers. Kubebuilder provides scaffolding, code generation, and patterns. Viktor's reference implementation uses this stack. |
| 2026-02-20 | Controller → REST API over Controller → Chroma | Keeps controller simple (just HTTP POSTs), keeps document/embedding logic in cluster-whisperer, matches Viktor's architecture. Switching vector DBs only requires changes in one place. |
| 2026-02-20 | Environment variables over CRDs for configuration | CRDs add complexity (defining, generating, reconciling). Env vars are simpler for a POC and sufficient for the demo. CRD config can be added later. |
| 2026-02-20 | Separate repo over subdirectory in cluster-whisperer | Different language (Go vs TypeScript), different build tooling (Kubebuilder/Make vs tsc/npm), different deployment model (in-cluster pod vs local CLI). Kubebuilder wants to own the repo root. |
| 2026-02-20 | Immediate delete forwarding (no debounce) | Stale data in search results is worse than extra API calls. When a resource is deleted, the vector DB should reflect that as quickly as possible. |
| 2026-02-21 | Daily resync (24h) + ad-hoc HTTP trigger over 60-min periodic | Frequent periodic resyncs are wasteful for clusters that don't change often. A daily resync covers eventual consistency; ad-hoc `POST /api/v1/resync` covers on-demand needs (deploy scripts, CI, manual verification). |

---

## Progress Log

| Date | Milestone | Notes |
|------|-----------|-------|
| 2026-02-21 | M1 Complete | Kubebuilder scaffold (Go 1.26, controller-runtime v0.21.0), dynamic informers via discovery API, metadata extraction (namespace/name/kind/apiVersion/labels/filtered-annotations), resource filtering (allowlist/blocklist with default exclusions), console event logging, 35 unit tests passing |
| 2026-02-21 | M2 Complete | DebounceBuffer with per-resource timers, last-state-wins dedup, configurable flush interval/batch size, deletes bypass debounce (forwarded immediately), SyncPayload with separate upserts/deletes, graceful shutdown flush, 9 debounce tests passing |
| 2026-02-21 | M3 Partial (k8s-vectordb-sync side) | REST client (`internal/client/rest.go`) with JSON POST, exponential backoff retry (configurable max retries, initial/max delay), 4xx no-retry vs 5xx/timeout retry, context cancellation support, empty payload skip. Wired into cmd/main.go replacing logPayloads placeholder. 8 contract tests passing. Cluster-whisperer REST endpoint (other repo) still needed to complete M3. |
| 2026-02-23 | M3 Complete (cluster-whisperer side) | Cluster-whisperer implemented `POST /api/v1/instances/sync` using Hono framework with Zod validation. Endpoint processes deletes before upserts, integrates with existing `storeInstances()` pipeline for embedding and ChromaDB storage. Go nil handling (null → empty defaults). Health probes (`/healthz`, `/readyz`). Runs as separate process via `cluster-whisperer serve --port 3000`. 40+ tests (unit, integration against real ChromaDB, contract tests matching controller payload format). M6 now unblocked. |
| 2026-02-21 | M4 Complete | Resync interval default changed to 24h (1440min). Ad-hoc resync via `POST /api/v1/resync` endpoint (`internal/api/server.go`). Watcher.TriggerResync() re-lists all watched GVRs and emits ADD events. API server wired as manager.Runnable on :8082. Graceful shutdown fixed: sender uses fresh context to drain final payloads. Health probes already present from Kubebuilder scaffold. Informer reconnection handled by controller-runtime. 11 new tests (6 watcher, 5 API), 60 total passing. |
| 2026-02-21 | M5 Complete | Dockerfile already existed from Kubebuilder scaffold (multi-stage: Go 1.26 builder + distroless, non-root 65532). Helm chart created (`charts/k8s-vectordb-sync/`): Deployment, ServiceAccount, ClusterRole (`*/*` get/list/watch for dynamic informers), ClusterRoleBinding, Service (health :8081, resync API :8082). All 9 env vars wired through values.yaml. Kustomize RBAC role.yaml fixed from pods-only to broad read access. GitHub Actions CI workflow (`.github/workflows/build.yml`): tests, multi-arch buildx (amd64+arm64), push to ghcr.io with semver/branch/sha tags, GHA build cache. 60 tests still passing. |
| 2026-02-23 | M6 Complete | E2E pipeline tests: mock HTTP server (`test/e2e/mockserver/`) records controller payloads in Kind cluster, Ginkgo tests verify upsert on Deployment create and delete on Deployment remove. Full pipeline script (`scripts/test-full-pipeline.sh`) orchestrates controller + cluster-whisperer + ChromaDB for cross-repo validation of the semantic bridge pattern. README rewritten with architecture overview, Helm deployment guide, configuration reference (9 env vars), and cluster-whisperer integration docs. 92 unit tests passing. PRD complete — all milestones and success criteria met. |
