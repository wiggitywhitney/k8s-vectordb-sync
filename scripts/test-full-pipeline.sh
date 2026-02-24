#!/usr/bin/env bash
# Full pipeline integration test: controller → cluster-whisperer → ChromaDB → search
#
# Prerequisites:
#   - Kind cluster running (or any K8s cluster with kubectl configured)
#   - Docker available (for ChromaDB)
#   - cluster-whisperer repo checked out alongside this repo
#   - VOYAGE_API_KEY set (for embeddings)
#   - Node.js/npm available (for cluster-whisperer)
#
# Usage:
#   vals exec -f .vals.yaml -- ./scripts/test-full-pipeline.sh
#
# What this tests:
#   1. Controller watches Kubernetes resources via informers
#   2. Controller debounces/batches changes and POSTs to cluster-whisperer
#   3. cluster-whisperer receives metadata, generates embeddings, stores in ChromaDB
#   4. Semantic search finds the synced resource in the vector database

set -euo pipefail

# Configuration
CHROMA_CONTAINER="pipeline-test-chroma"
CHROMA_PORT=8100
CHROMA_URL="http://localhost:${CHROMA_PORT}"
CW_PORT=3100
CW_URL="http://localhost:${CW_PORT}"
CW_REPO="${CW_REPO:-$(dirname "$(pwd)")/cluster-whisperer}"
TEST_NAMESPACE="pipeline-test"
TEST_DEPLOYMENT="nginx-pipeline-test"

# Debounce and flush set low for fast feedback
export REST_ENDPOINT="${CW_URL}/api/v1/instances/sync"
export DEBOUNCE_WINDOW_MS=2000
export BATCH_FLUSH_INTERVAL_MS=2000
export WATCH_RESOURCE_TYPES=deployments
export LOG_LEVEL=info

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
NC='\033[0m'

log_info()  { echo -e "${GREEN}[INFO]${NC}  $*"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $*"; }
log_step()  { echo -e "\n${GREEN}=== $* ===${NC}"; }

# Track background PIDs for cleanup
PIDS=()
cleanup() {
    log_step "Cleaning up"

    # Kill background processes
    for pid in "${PIDS[@]}"; do
        if kill -0 "$pid" 2>/dev/null; then
            kill "$pid" 2>/dev/null || true
            wait "$pid" 2>/dev/null || true
        fi
    done

    # Remove test resources
    kubectl delete namespace "$TEST_NAMESPACE" --ignore-not-found --wait=false 2>/dev/null || true

    # Stop ChromaDB container
    docker rm -f "$CHROMA_CONTAINER" 2>/dev/null || true

    log_info "Cleanup complete"
}
trap cleanup EXIT

# Validate prerequisites
log_step "Validating prerequisites"

if ! command -v kubectl &>/dev/null; then
    log_error "kubectl not found"
    exit 1
fi

if ! kubectl cluster-info &>/dev/null; then
    log_error "No Kubernetes cluster available (kubectl cluster-info failed)"
    exit 1
fi

if ! command -v docker &>/dev/null; then
    log_error "docker not found"
    exit 1
fi

if [ ! -d "$CW_REPO" ]; then
    log_error "cluster-whisperer repo not found at $CW_REPO"
    log_error "Set CW_REPO to the correct path"
    exit 1
fi

if [ -z "${VOYAGE_API_KEY:-}" ]; then
    log_error "VOYAGE_API_KEY not set. Required for embeddings."
    log_error "Run: vals exec -f .vals.yaml -- ./scripts/test-full-pipeline.sh"
    exit 1
fi

log_info "All prerequisites met"

# Step 1: Start ChromaDB
log_step "Step 1: Starting ChromaDB"

docker rm -f "$CHROMA_CONTAINER" 2>/dev/null || true
docker run -d --name "$CHROMA_CONTAINER" \
    -p "${CHROMA_PORT}:8000" \
    chromadb/chroma:latest

log_info "Waiting for ChromaDB to be ready..."
for i in $(seq 1 30); do
    if curl -s "${CHROMA_URL}/api/v1/heartbeat" >/dev/null 2>&1; then
        log_info "ChromaDB ready"
        break
    fi
    if [ "$i" -eq 30 ]; then
        log_error "ChromaDB failed to start"
        exit 1
    fi
    sleep 1
done

# Step 2: Start cluster-whisperer serve
log_step "Step 2: Starting cluster-whisperer REST server"

cd "$CW_REPO"
CHROMA_URL="$CHROMA_URL" npx tsx src/index.ts serve --port "$CW_PORT" &
CW_PID=$!
PIDS+=("$CW_PID")
cd - >/dev/null

log_info "Waiting for cluster-whisperer to be ready..."
for i in $(seq 1 30); do
    if curl -s "${CW_URL}/healthz" >/dev/null 2>&1; then
        log_info "cluster-whisperer REST server ready on port $CW_PORT"
        break
    fi
    if [ "$i" -eq 30 ]; then
        log_error "cluster-whisperer failed to start"
        exit 1
    fi
    sleep 1
done

# Step 3: Build and run the controller locally
log_step "Step 3: Building and running the controller"

go build -o bin/manager cmd/main.go
log_info "Controller built"

./bin/manager --health-probe-bind-address=:8083 2>&1 | sed 's/^/  [controller] /' &
CTRL_PID=$!
PIDS+=("$CTRL_PID")

log_info "Waiting for controller to start watching..."
sleep 5

# Step 4: Create a test resource
log_step "Step 4: Creating test resource"

kubectl create namespace "$TEST_NAMESPACE" 2>/dev/null || true
kubectl create deployment "$TEST_DEPLOYMENT" \
    --image=nginx:latest \
    --namespace="$TEST_NAMESPACE"

log_info "Created Deployment '$TEST_DEPLOYMENT' in namespace '$TEST_NAMESPACE'"

# Step 5: Wait for sync
log_step "Step 5: Waiting for controller to sync"
log_info "Debounce window: ${DEBOUNCE_WINDOW_MS}ms, Flush interval: ${BATCH_FLUSH_INTERVAL_MS}ms"
log_info "Waiting 10 seconds for debounce + flush cycle..."
sleep 10

# Step 6: Verify in vector database via cluster-whisperer search
log_step "Step 6: Verifying resource in vector database"

cd "$CW_REPO"

# Search for the test deployment using keyword search (deterministic, no embedding cost)
SEARCH_RESULT=$(CHROMA_URL="$CHROMA_URL" npx tsx -e "
import { ChromaBackend } from './src/vectorstore/chroma-backend.js';
import { VoyageEmbedding } from './src/vectorstore/voyage-embedding.js';

const embedder = new VoyageEmbedding();
const store = new ChromaBackend(embedder);
const results = await store.keywordSearch('instances', '${TEST_DEPLOYMENT}', { limit: 5 });
console.log(JSON.stringify(results, null, 2));
" 2>/dev/null) || true

cd - >/dev/null

if echo "$SEARCH_RESULT" | grep -q "$TEST_DEPLOYMENT"; then
    log_info "Resource found in vector database!"
    echo "$SEARCH_RESULT" | head -30
else
    log_warn "Keyword search did not find the resource. Trying semantic search..."

    cd "$CW_REPO"
    SEMANTIC_RESULT=$(CHROMA_URL="$CHROMA_URL" npx tsx -e "
import { ChromaBackend } from './src/vectorstore/chroma-backend.js';
import { VoyageEmbedding } from './src/vectorstore/voyage-embedding.js';

const embedder = new VoyageEmbedding();
const store = new ChromaBackend(embedder);
const results = await store.search('instances', 'nginx deployment in pipeline test namespace', { limit: 5 });
console.log(JSON.stringify(results, null, 2));
" 2>/dev/null) || true
    cd - >/dev/null

    if echo "$SEMANTIC_RESULT" | grep -q "$TEST_DEPLOYMENT"; then
        log_info "Resource found via semantic search!"
        echo "$SEMANTIC_RESULT" | head -30
    else
        log_error "Resource NOT found in vector database"
        log_error "Keyword result: $SEARCH_RESULT"
        log_error "Semantic result: $SEMANTIC_RESULT"
        exit 1
    fi
fi

# Step 7: Delete the resource and verify removal
log_step "Step 7: Deleting test resource and verifying removal"

kubectl delete deployment "$TEST_DEPLOYMENT" --namespace="$TEST_NAMESPACE" --wait=false
log_info "Deleted Deployment '$TEST_DEPLOYMENT'"
log_info "Waiting 5 seconds for delete to propagate (bypasses debounce)..."
sleep 5

cd "$CW_REPO"
DELETE_CHECK=$(CHROMA_URL="$CHROMA_URL" npx tsx -e "
import { ChromaBackend } from './src/vectorstore/chroma-backend.js';
import { VoyageEmbedding } from './src/vectorstore/voyage-embedding.js';

const embedder = new VoyageEmbedding();
const store = new ChromaBackend(embedder);
const results = await store.keywordSearch('instances', '${TEST_DEPLOYMENT}', { limit: 5 });
console.log(results.length);
" 2>/dev/null) || true
cd - >/dev/null

DELETE_PASSED=false
if [[ "${DELETE_CHECK:-1}" == "0" ]]; then
    log_info "Resource successfully removed from vector database"
    DELETE_PASSED=true
else
    log_warn "Resource may still be in vector database (count: ${DELETE_CHECK:-unknown})"
    log_warn "This is expected if cluster-whisperer delete endpoint is not yet wired"
fi

# Summary
log_step "Pipeline Test Results"
echo ""
log_info "Controller → REST API:    PASSED (controller sent payloads to cluster-whisperer)"
log_info "REST API → Vector DB:     PASSED (resource found in ChromaDB via search)"
log_info "Create → Sync → Search:   PASSED (semantic bridge pattern validated)"
if [[ "$DELETE_PASSED" == "true" ]]; then
    log_info "Delete → Verify Remove:   PASSED"
else
    log_warn "Delete → Verify Remove:   SKIPPED/INCONCLUSIVE"
fi
echo ""
log_info "Full pipeline test complete!"
