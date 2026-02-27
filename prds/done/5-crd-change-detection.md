# PRD #5: CRD Change Detection for Capabilities Sync

**Status**: Complete
**Created**: 2026-02-25
**Completed**: 2026-02-26
**GitHub Issue**: [#5](https://github.com/wiggitywhitney/k8s-vectordb-sync/issues/5)

---

## Problem Statement

The controller currently watches resource instances and syncs metadata to cluster-whisperer's `/api/v1/instances/sync` endpoint. When a new operator is installed (e.g., cert-manager), the controller correctly detects new pods, deployments, and services. However, nobody tells cluster-whisperer about the **new resource types** (CRDs) that the operator installed.

This means the capabilities collection in the vector DB stays stale. The agent can't answer questions like "what certificate management capabilities does this cluster have?" because the capability inference pipeline (`discoverResources → inferCapabilities → storeCapabilities`) only runs when explicitly triggered.

**Concrete example**: Installing cert-manager adds 6 CRDs (Certificate, CertificateRequest, Issuer, ClusterIssuer, Challenge, Order). The controller synced all the new instances, but the agent couldn't answer "what certificate management capabilities does this cluster have?" because the capabilities collection didn't know about the new CRDs.

**Historical context**: The Cluster Sync Watcher feature in cluster-whisperer originally planned CRD change detection but was superseded when this controller was built. The CRD-watching half was dropped — only instance sync was implemented.

## Solution

Detect CRD add/delete events in the controller and POST to a new capabilities scan endpoint on cluster-whisperer. The controller's job is simple: notice CRD changes and tell cluster-whisperer which CRD names changed. cluster-whisperer handles schema discovery, LLM inference, and vector DB storage using its existing capability pipeline.

This follows Viktor Farcic's architecture. His dot-ai-controller uses a `CapabilityScanConfig` CRD to trigger capability scans when new CRDs appear. His controller POSTs to a capability scan endpoint on his MCP server, which runs LLM inference on the CRD schemas. Our version keeps it simpler — environment variable configuration instead of a custom CRD.

### Key Design Decisions

1. **Separate endpoint, separate payload**: CRD events go to `CAPABILITIES_ENDPOINT` (not `INSTANCES_ENDPOINT`). The payload contains CRD names, not instance metadata, because cluster-whisperer already knows how to discover schemas and run inference given a CRD name.

2. **Reuse debounce/batch for adds**: When an operator installs, many CRDs land at once (cert-manager adds 6). CRD add events use the same debounce/batch pattern as instances to coalesce them into a single POST.

3. **Deletes bypass debounce**: Same as instance deletes — stale capabilities in search results are worse than extra API calls. CRD deletions are forwarded immediately.

4. **Filter CRD events from instance pipeline**: CRD objects should only go to the capabilities endpoint, not the instance sync endpoint. A CRD is a type definition, not a running workload.

### Architecture

```text
Kubernetes Cluster          k8s-vectordb-sync           cluster-whisperer          Vector DB
┌──────────────────┐        ┌───────────────────┐       ┌─────────────────┐      ┌──────────┐
│ Resource Events  │        │                   │ POST  │ REST Endpoint   │embed │ ChromaDB │
│ (instances)      ├──────→ │ Instance Pipeline ├─────→ │ /instances/sync ├────→ │instances │
│                  │debounce│ (existing)        │       │ instanceToDoc() │store │collection│
├──────────────────┤        ├───────────────────┤       ├─────────────────┤      ├──────────┤
│ CRD Events       │        │                   │ POST  │ REST Endpoint   │infer │ ChromaDB │
│ (add/delete)     ├──────→ │ CRD Pipeline      ├─────→ │ /capabilities/  ├────→ │capabilit.│
│                  │debounce│ (new)             │       │ scan            │store │collection│
└──────────────────┘ batch  └───────────────────┘       └─────────────────┘      └──────────┘
```

### CRD Event Payload

**Add/Update** (batched, POST to capabilities endpoint):
```json
{
  "upserts": [
    "certificates.cert-manager.io",
    "issuers.cert-manager.io",
    "clusterissuers.cert-manager.io"
  ]
}
```

**Delete** (immediate, POST to capabilities endpoint):
```json
{
  "deletes": [
    "certificates.cert-manager.io"
  ]
}
```

The payload is intentionally minimal — just CRD names. cluster-whisperer already has the capability inference pipeline (`discoverResources → inferCapabilities → storeCapabilities`) which handles schema discovery via `kubectl explain` and LLM inference. The controller doesn't need to send schemas.

---

## Success Criteria

- [x] Controller detects CRD add events and POSTs CRD names to the capabilities endpoint
- [x] Controller detects CRD delete events and POSTs deletions immediately (bypass debounce)
- [x] CRD add events are debounced/batched (operator installs land many CRDs at once)
- [x] CRD events are routed to the capabilities endpoint, not the instance sync endpoint
- [x] `REST_ENDPOINT` renamed to `INSTANCES_ENDPOINT` (breaking change, v0.1.0)
- [x] New `CAPABILITIES_ENDPOINT` config env var (separate from `INSTANCES_ENDPOINT`)
- [x] Helm chart updated with both renamed and new configuration options
- [x] All three test tiers pass (unit, integration, e2e)
- [x] End-to-end: install a CRD → controller detects → capabilities endpoint receives CRD name
- [x] Full-stack validation: install real operators (CloudNativePG, Redis, MongoDB) → CRDs appear in vector DB with inferred descriptions
- [x] README updated with new functionality using `/write-docs` skill

## Milestones

- [x] **M1**: Endpoint Rename & CRD Event Detection
  - Rename `REST_ENDPOINT` → `INSTANCES_ENDPOINT` across codebase (config, Helm chart, README, tests, CI)
  - Clean break — no deprecation fallback (v0.1.0, single user)
  - Identify CRD resources (`customresourcedefinitions.apiextensions.k8s.io`) in the event stream
  - Route CRD events to a separate processing path (not the instance pipeline)
  - Extract CRD fully-qualified names (e.g., `certificates.cert-manager.io`) from the resource object
  - Add CRDs to `EXCLUDE_RESOURCE_TYPES` default list so they don't flow through the instance pipeline
  - Unit tests for CRD identification, name extraction, and routing logic

- [x] **M2**: CRD Debounce, Batching & REST Client
  - CRD-specific debounce buffer that batches add events (reuses same debounce pattern)
  - CRD delete events bypass debounce (immediate forwarding)
  - REST client for the capabilities endpoint with the CRD payload format (`upserts`/`deletes` arrays)
  - New `CAPABILITIES_ENDPOINT` config env var (default: empty string = feature disabled)
  - Retry logic matching existing REST client (exponential backoff, no-retry on 4xx)
  - Unit tests for CRD debounce behavior, payload assembly, and REST client
  - Integration tests for CRD event → debounce → REST POST pipeline

- [x] **M3**: Configuration, Deployment & Documentation
  - Helm chart updated with `CAPABILITIES_ENDPOINT` in values.yaml
  - Feature is disabled by default (empty endpoint = no CRD event forwarding)
  - E2E tests in CI: install CRD in Kind cluster → verify controller POSTs to mock capabilities endpoint
  - All test tiers green
  - README updated using `/write-docs` skill with new architecture diagram, configuration reference, and CRD detection feature documentation

- [x] **M4**: Full-Stack Validation with Real Operators
  - Cross-repo validation script that runs the full pipeline against real cluster-whisperer + ChromaDB
  - Install real operators one at a time into a Kind cluster and verify each operator's CRDs appear in the capabilities collection with inferred descriptions:
    1. **CloudNativePG** — `helm install cnpg cnpg/cloudnative-pg` → verify CRDs (e.g., `clusters.postgresql.cnpg.io`) have capability entries in vector DB
    2. **OpsTree Redis Operator** — `helm install redis-operator ot-helm/redis-operator` → verify CRDs (e.g., `redis.redis.redis.opstreelabs.in`) have capability entries in vector DB
    3. **MongoDB Community Operator** — `helm install community-operator mongodb/community-operator` → verify CRDs (e.g., `mongodbcommunity.mongodbcommunity.mongodb.com`) have capability entries in vector DB
  - Each verification checks: CRD name exists in capabilities collection AND has an inferred description
  - Uninstall an operator and verify its capability entries are removed from the vector DB
  - Validates the full semantic bridge: install operator → controller detects CRDs → cluster-whisperer infers capabilities → agent can answer "what capabilities does this cluster have?"

## Technical Approach

### CRD Identification

CRDs are discovered via the same dynamic informer mechanism as all other resources. The resource type is `customresourcedefinitions` in API group `apiextensions.k8s.io`. The watcher already sees these events — they just need to be identified and routed differently.

The CRD fully-qualified name (e.g., `certificates.cert-manager.io`) can be extracted from the CRD object's `metadata.name` field, which is always in the format `<plural>.<group>`.

### Routing Logic

The watcher's event handler needs a routing decision:
- If the resource is a CRD (`apiGroup == "apiextensions.k8s.io"` and `kind == "CustomResourceDefinition"`) → send to CRD pipeline
- Otherwise → send to instance pipeline (existing behavior)

CRDs should also be added to the default exclusion list for the instance pipeline to prevent them from being sent as instance metadata.

### Debounce/Batch Reuse

The CRD pipeline uses a separate `DebounceBuffer` instance with the same configuration (debounce window, batch size, flush interval). This keeps CRD batches separate from instance batches and routes to a different endpoint. Delete bypass works identically — CRD deletes go immediately.

### Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `INSTANCES_ENDPOINT` | `http://localhost:3000/api/v1/instances/sync` | cluster-whisperer instance sync URL (renamed from `REST_ENDPOINT`) |
| `CAPABILITIES_ENDPOINT` | *(empty = disabled)* | cluster-whisperer capabilities scan URL (e.g., `http://cluster-whisperer:3000/api/v1/capabilities/scan`) |

`REST_ENDPOINT` is renamed to `INSTANCES_ENDPOINT` — clean break, no fallback. When `CAPABILITIES_ENDPOINT` is empty, the CRD pipeline is not created and CRD events are silently dropped (no error, just not processed). This keeps the feature opt-in and backward-compatible.

### What Happens in cluster-whisperer (Out of Scope for This PRD)

The capabilities scan endpoint (`POST /api/v1/capabilities/scan`) will be implemented separately in cluster-whisperer. It receives CRD names and runs the existing capability inference pipeline:
- **For adds**: `discoverResources(crdNames) → inferCapabilities(schemas) → storeCapabilities(docs)`
- **For deletes**: Remove the capability entry from the vector DB by CRD name

This PRD covers only the k8s-vectordb-sync side.

## Dependencies

- **Existing debounce/batch infrastructure** (`internal/controller/debounce.go`) — reused for CRD events
- **Existing REST client** (`internal/client/rest.go`) — pattern reused for capabilities endpoint
- **cluster-whisperer capabilities endpoint** — must exist before e2e validation, but the controller can be developed and tested independently using a mock server

## Out of Scope

- cluster-whisperer capabilities scan endpoint implementation (separate PRD in that repo)
- CRD schema extraction or LLM inference (cluster-whisperer's responsibility)
- CRD update events (CRD spec changes are rare and the capability inference pipeline is idempotent — a periodic resync or manual trigger handles this)
- CapabilityScanConfig CRD for configuration (Viktor's pattern — our env var approach is simpler and sufficient)
- Multi-cluster CRD detection
- CRD version tracking (v1alpha1 → v1beta1 → v1 transitions)

---

## Design Decisions

| Date | Decision | Rationale |
|------|----------|-----------|
| 2026-02-25 | Rename `REST_ENDPOINT` → `INSTANCES_ENDPOINT` (breaking) | With two endpoints, the generic name becomes ambiguous. Clean break at v0.1.0 with a single user. No deprecation fallback — just rename everywhere. |
| 2026-02-25 | Separate `CAPABILITIES_ENDPOINT` over reusing `INSTANCES_ENDPOINT` | Different payload format, different downstream processing. Instance sync and capability scan are distinct pipelines in cluster-whisperer. Keeps routing explicit. |
| 2026-02-25 | CRD names only in payload (not schemas) | cluster-whisperer already has the `discoverResources → inferCapabilities` pipeline that handles schema discovery and LLM inference. Sending schemas from the controller would duplicate logic and create coupling. |
| 2026-02-25 | Feature disabled by default (empty endpoint) | Backward-compatible. Existing deployments continue working unchanged. Users opt in by setting `CAPABILITIES_ENDPOINT`. |
| 2026-02-25 | Reuse debounce/batch pattern over custom CRD batching | Operator installs land many CRDs at once (cert-manager: 6, Prometheus: 8+). Same coalescing logic applies. Separate buffer instance keeps CRD and instance batches independent. |
| 2026-02-25 | Skip CRD update events | CRD spec changes (adding fields, changing versions) are rare. The capability inference pipeline is idempotent — periodic resync or manual trigger handles schema updates. Avoids complexity of diffing CRD specs. |
| 2026-02-25 | Env var config over Viktor's CapabilityScanConfig CRD | Consistent with the instance-sync controller's approach. CRD-based config adds complexity (defining, generating, reconciling) that isn't needed for this feature. It can be added later if needed. |
| 2026-02-25 | CRD GVR always discovered when capabilities pipeline enabled | The resource filter (allowlist/blocklist) controls instance pipeline discovery. CRDs are in the default exclusion list and would also be missed in allowlist mode. The watcher's `watchCRDs` flag ensures the CRD informer is always created when `CAPABILITIES_ENDPOINT` is set, regardless of filter settings. |
| 2026-02-26 | CRD payload uses `upserts`/`deletes` (not `added`/`deleted`) | Consistent API surface — the instance sync endpoint already uses `upserts`/`deletes`. Both endpoints on the same server, consumed by the same client, should use the same field names. |

---

## Progress Log

| Date | Milestone | Notes |
|------|-----------|-------|
| 2026-02-25 | M1 complete | Renamed REST_ENDPOINT → INSTANCES_ENDPOINT across 9 files, added CAPABILITIES_ENDPOINT config, implemented CRD event detection with IsCRD() + CrdEvents channel routing, added customresourcedefinitions to default exclusions, 9 new unit tests |
| 2026-02-25 | M2 complete | CrdDebounceBuffer with add debounce/batch and delete bypass, CrdSyncPayload type (upserts/deletes arrays), Payload interface on REST client for reuse across both pipelines, CRD pipeline wired in main.go gated on CAPABILITIES_ENDPOINT, 12 new unit tests, 4 integration tests |
| 2026-02-25 | M3 complete | Mock server updated with capabilities scan endpoint, E2E tests for CRD add/delete detection against Kind cluster (6/6 tests pass), watcher fix to always discover CRDs when capabilities pipeline enabled (bypasses filter in allowlist mode), README updated with dual-pipeline architecture diagram, CRD payload format docs, and updated Helm deploy example |
| 2026-02-26 | M4 complete | Full-stack validation script (`test/fullstack/validate.sh`) with `make test-fullstack` target. Validated against Spider Rainbows Kind cluster with real cluster-whisperer + ChromaDB: CloudNativePG (10/10 CRDs), Redis Operator (4/4 CRDs), MongoDB Community (1/1 CRD) all produced capabilities with LLM-inferred descriptions. Uninstall verification confirmed capability removal from ChromaDB. Also wired capabilities pipeline into cluster-whisperer serve command (separate repo commit). |
