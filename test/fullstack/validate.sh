#!/usr/bin/env bash
# Full-stack validation: install real operators → verify capabilities appear in ChromaDB → uninstall → verify removal.
#
# Prerequisites:
#   1. Kind cluster running with k8s-vectordb-sync deployed (CAPABILITIES_ENDPOINT set)
#   2. ChromaDB running and accessible
#   3. cluster-whisperer running with serve command (connected to same cluster + ChromaDB)
#
# Usage:
#   ./test/fullstack/validate.sh
#   CHROMA_URL=http://localhost:8000 POLL_TIMEOUT=600 ./test/fullstack/validate.sh

set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration (overridable via environment variables)
# ---------------------------------------------------------------------------
CHROMA_URL="${CHROMA_URL:-http://localhost:8000}"
CLUSTER_WHISPERER_URL="${CLUSTER_WHISPERER_URL:-http://localhost:3000}"
CONTROLLER_NS="${CONTROLLER_NS:-k8s-vectordb-sync-system}"
POLL_TIMEOUT="${POLL_TIMEOUT:-300}"   # seconds to wait for capabilities to appear
POLL_INTERVAL="${POLL_INTERVAL:-10}"  # seconds between polls
SKIP_CLEANUP="${SKIP_CLEANUP:-false}" # set to "true" to leave operators installed

# ChromaDB v2 API base
CHROMA_API="${CHROMA_URL}/api/v2/tenants/default_tenant/databases/default_database"

# ---------------------------------------------------------------------------
# Output helpers
# ---------------------------------------------------------------------------
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
BOLD='\033[1m'
NC='\033[0m'

log()  { echo -e "${GREEN}[PASS]${NC} $*"; }
info() { echo -e "${BLUE}[INFO]${NC} $*"; }
warn() { echo -e "${YELLOW}[WARN]${NC} $*"; }
fail() { echo -e "${RED}[FAIL]${NC} $*"; }

TOTAL_PASS=0
TOTAL_FAIL=0
RESULTS=()

record_pass() {
  TOTAL_PASS=$((TOTAL_PASS + 1))
  RESULTS+=("${GREEN}PASS${NC} $1")
  log "$1"
}

record_fail() {
  TOTAL_FAIL=$((TOTAL_FAIL + 1))
  RESULTS+=("${RED}FAIL${NC} $1")
  fail "$1"
}

# ---------------------------------------------------------------------------
# ChromaDB helpers
# ---------------------------------------------------------------------------

# Get the capabilities collection UUID from ChromaDB.
get_collection_id() {
  curl -sf "${CHROMA_API}/collections" | \
    python3 -c "
import json, sys
cols = json.load(sys.stdin)
for c in cols:
    if c['name'] == 'capabilities':
        print(c['id'])
        break
"
}

# Query capabilities by API group. Returns JSON with ids, documents, metadatas.
get_capabilities_by_group() {
  local collection_id="$1"
  local api_group="$2"

  curl -sf -X POST "${CHROMA_API}/collections/${collection_id}/get" \
    -H "Content-Type: application/json" \
    -d "{
      \"where\": {\"apiGroup\": {\"\$eq\": \"${api_group}\"}},
      \"include\": [\"documents\", \"metadatas\"]
    }"
}

# Count capabilities for a given API group.
count_capabilities_by_group() {
  local collection_id="$1"
  local api_group="$2"

  get_capabilities_by_group "$collection_id" "$api_group" | \
    python3 -c "import json, sys; d = json.load(sys.stdin); print(len(d.get('ids', [])))"
}

# Print capability details for a given API group.
print_capabilities() {
  local collection_id="$1"
  local api_group="$2"

  get_capabilities_by_group "$collection_id" "$api_group" | \
    python3 -c "
import json, sys
d = json.load(sys.stdin)
ids = d.get('ids', [])
docs = d.get('documents', [])
metas = d.get('metadatas', [])
for i, rid in enumerate(ids):
    doc = (docs[i] or '')[:120] if i < len(docs) else ''
    meta = metas[i] if i < len(metas) else {}
    kind = meta.get('kind', '?')
    confidence = meta.get('confidence', '?')
    print(f'  {rid} (kind={kind}, confidence={confidence})')
    print(f'    {doc}...')
"
}

# ---------------------------------------------------------------------------
# Kubernetes helpers
# ---------------------------------------------------------------------------

# Get CRD names for a given API group from the cluster.
get_crds_for_group() {
  local api_group="$1"
  kubectl get crds -o jsonpath="{range .items[*]}{.metadata.name}{'\n'}{end}" | \
    grep "\.${api_group}$" || true
}

# Wait for at least one CRD to exist for the given API group.
wait_for_crds() {
  local api_group="$1"
  local timeout="$2"
  local elapsed=0

  info "Waiting for CRDs in group ${api_group}..."
  while [[ $elapsed -lt $timeout ]]; do
    local count
    count=$(get_crds_for_group "$api_group" | wc -l | tr -d ' ')
    if [[ "$count" -gt 0 ]]; then
      info "Found ${count} CRDs in group ${api_group}"
      return 0
    fi
    sleep 5
    elapsed=$((elapsed + 5))
    printf "."
  done
  echo ""
  warn "Timed out waiting for CRDs in group ${api_group}"
  return 1
}

# ---------------------------------------------------------------------------
# Poll ChromaDB until capabilities appear for an API group.
# ---------------------------------------------------------------------------
wait_for_capabilities() {
  local collection_id="$1"
  local api_group="$2"
  local expected_min="${3:-1}"
  local timeout="${POLL_TIMEOUT}"
  local elapsed=0

  info "Polling ChromaDB for capabilities (apiGroup=${api_group}, min=${expected_min}, timeout=${timeout}s)..."
  while [[ $elapsed -lt $timeout ]]; do
    local count
    count=$(count_capabilities_by_group "$collection_id" "$api_group")
    if [[ "$count" -ge "$expected_min" ]]; then
      info "Found ${count} capabilities for ${api_group} after ${elapsed}s"
      return 0
    fi
    sleep "$POLL_INTERVAL"
    elapsed=$((elapsed + POLL_INTERVAL))
    printf "\r  Elapsed: %ds / %ds (found: %s)" "$elapsed" "$timeout" "$count"
  done
  echo ""
  warn "Timed out waiting for capabilities (apiGroup=${api_group})"
  return 1
}

# Poll ChromaDB until capabilities are removed for an API group.
wait_for_capability_removal() {
  local collection_id="$1"
  local api_group="$2"
  local timeout="${POLL_TIMEOUT}"
  local elapsed=0

  info "Polling ChromaDB for capability removal (apiGroup=${api_group}, timeout=${timeout}s)..."
  while [[ $elapsed -lt $timeout ]]; do
    local count
    count=$(count_capabilities_by_group "$collection_id" "$api_group")
    if [[ "$count" -eq 0 ]]; then
      info "All capabilities removed for ${api_group} after ${elapsed}s"
      return 0
    fi
    sleep "$POLL_INTERVAL"
    elapsed=$((elapsed + POLL_INTERVAL))
    printf "\r  Elapsed: %ds / %ds (remaining: %s)" "$elapsed" "$timeout" "$count"
  done
  echo ""
  warn "Timed out waiting for capability removal (apiGroup=${api_group})"
  return 1
}

# ---------------------------------------------------------------------------
# Operator definitions
# ---------------------------------------------------------------------------
# Each operator: name, helm_repo_name, helm_repo_url, chart, namespace, api_group
declare -a OPERATORS=(
  "CloudNativePG|cnpg|https://cloudnative-pg.github.io/charts|cnpg/cloudnative-pg|cnpg-system|postgresql.cnpg.io"
  "Redis Operator|ot-helm|https://ot-container-kit.github.io/helm-charts/|ot-helm/redis-operator|redis-operator|redis.redis.opstreelabs.in"
  "MongoDB Community|mongodb|https://mongodb.github.io/helm-charts|mongodb/community-operator|mongodb|mongodbcommunity.mongodb.com"
)

parse_operator() {
  IFS='|' read -r OP_NAME OP_REPO_NAME OP_REPO_URL OP_CHART OP_NAMESPACE OP_API_GROUP <<< "$1"
}

# ---------------------------------------------------------------------------
# Install an operator via Helm.
# ---------------------------------------------------------------------------
install_operator() {
  local op_def="$1"
  parse_operator "$op_def"

  info "Installing ${OP_NAME}..."
  helm repo add "$OP_REPO_NAME" "$OP_REPO_URL" --force-update 2>/dev/null
  helm repo update "$OP_REPO_NAME" 2>/dev/null

  helm install "$OP_REPO_NAME" "$OP_CHART" \
    --namespace "$OP_NAMESPACE" \
    --create-namespace \
    --wait \
    --timeout 5m 2>&1 | sed 's/^/  /'

  info "${OP_NAME} installed"
}

# ---------------------------------------------------------------------------
# Uninstall an operator via Helm.
# ---------------------------------------------------------------------------
uninstall_operator() {
  local op_def="$1"
  parse_operator "$op_def"

  info "Uninstalling ${OP_NAME}..."
  helm uninstall "$OP_REPO_NAME" --namespace "$OP_NAMESPACE" 2>&1 | sed 's/^/  /'

  # Some operators leave CRDs behind; clean them up
  local crds
  crds=$(get_crds_for_group "$OP_API_GROUP")
  if [[ -n "$crds" ]]; then
    info "Cleaning up leftover CRDs for ${OP_API_GROUP}..."
    echo "$crds" | xargs -I{} kubectl delete crd {} --wait=false 2>/dev/null || true
  fi

  # Wait for CRDs to be fully removed from the cluster
  local elapsed=0
  while [[ $elapsed -lt 60 ]]; do
    local remaining
    remaining=$(get_crds_for_group "$OP_API_GROUP" | wc -l | tr -d ' ')
    if [[ "$remaining" -eq 0 ]]; then
      break
    fi
    sleep 5
    elapsed=$((elapsed + 5))
  done

  kubectl delete namespace "$OP_NAMESPACE" --ignore-not-found --wait=false 2>/dev/null || true
  info "${OP_NAME} uninstalled"
}

# ---------------------------------------------------------------------------
# Prerequisite checks
# ---------------------------------------------------------------------------
check_prerequisites() {
  echo ""
  echo -e "${BOLD}=== Prerequisite Checks ===${NC}"
  echo ""

  # kubectl connectivity
  info "Checking kubectl connectivity..."
  if ! kubectl cluster-info &>/dev/null; then
    fail "kubectl cannot reach the cluster. Is your kubeconfig set?"
    exit 1
  fi
  local ctx
  ctx=$(kubectl config current-context)
  info "Connected to cluster: ${ctx}"

  # ChromaDB
  info "Checking ChromaDB at ${CHROMA_URL}..."
  if ! curl -sf "${CHROMA_URL}/api/v2/heartbeat" &>/dev/null; then
    fail "ChromaDB not reachable at ${CHROMA_URL}"
    exit 1
  fi
  info "ChromaDB is reachable"

  # cluster-whisperer
  info "Checking cluster-whisperer at ${CLUSTER_WHISPERER_URL}..."
  if ! curl -sf "${CLUSTER_WHISPERER_URL}/healthz" &>/dev/null; then
    fail "cluster-whisperer not reachable at ${CLUSTER_WHISPERER_URL}"
    exit 1
  fi
  info "cluster-whisperer is reachable"

  # Controller deployment with CAPABILITIES_ENDPOINT
  info "Checking controller deployment in ${CONTROLLER_NS}..."
  local controller_deploy
  controller_deploy=$(kubectl get deploy -n "$CONTROLLER_NS" -l control-plane=controller-manager -o jsonpath='{.items[*].metadata.name}' 2>/dev/null | awk '{print $1}')
  if [[ -z "$controller_deploy" ]]; then
    fail "No controller deployment found with label control-plane=controller-manager in ${CONTROLLER_NS}"
    exit 1
  fi
  info "Found controller deployment: ${controller_deploy}"
  local cap_endpoint
  cap_endpoint=$(kubectl get deploy -n "$CONTROLLER_NS" "$controller_deploy" -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="CAPABILITIES_ENDPOINT")].value}' 2>/dev/null || echo "")
  if [[ -z "$cap_endpoint" ]]; then
    fail "Controller does not have CAPABILITIES_ENDPOINT configured"
    info "Redeploy the controller with CAPABILITIES_ENDPOINT pointing to cluster-whisperer"
    exit 1
  fi
  info "Controller CAPABILITIES_ENDPOINT: ${cap_endpoint}"

  # Helm
  info "Checking helm..."
  if ! command -v helm &>/dev/null; then
    fail "helm not found in PATH"
    exit 1
  fi
  info "helm is available"

  echo ""
  info "All prerequisites met"
}

# ---------------------------------------------------------------------------
# Validate a single operator: install → verify CRDs in cluster → verify capabilities in ChromaDB
# ---------------------------------------------------------------------------
validate_operator() {
  local op_def="$1"
  local collection_id="$2"
  parse_operator "$op_def"

  echo ""
  echo -e "${BOLD}--- ${OP_NAME} ---${NC}"

  # Check if already installed
  local existing_crds
  existing_crds=$(get_crds_for_group "$OP_API_GROUP" | wc -l | tr -d ' ')
  if [[ "$existing_crds" -gt 0 ]]; then
    warn "${OP_NAME} CRDs already present (${existing_crds} CRDs). Skipping install."
    info "Checking if capabilities already exist..."
    local cap_count
    cap_count=$(count_capabilities_by_group "$collection_id" "$OP_API_GROUP")
    if [[ "$cap_count" -gt 0 ]]; then
      record_pass "${OP_NAME}: ${cap_count} capabilities already present"
      print_capabilities "$collection_id" "$OP_API_GROUP"
    else
      info "CRDs present but no capabilities yet. Waiting for pipeline..."
      if wait_for_capabilities "$collection_id" "$OP_API_GROUP"; then
        local count
        count=$(count_capabilities_by_group "$collection_id" "$OP_API_GROUP")
        record_pass "${OP_NAME}: ${count} capabilities verified"
        print_capabilities "$collection_id" "$OP_API_GROUP"
      else
        record_fail "${OP_NAME}: capabilities not found after timeout"
      fi
    fi
    return
  fi

  # Install
  if ! install_operator "$op_def"; then
    record_fail "${OP_NAME}: helm install failed"
    return
  fi

  # Wait for CRDs in cluster
  if ! wait_for_crds "$OP_API_GROUP" 120; then
    record_fail "${OP_NAME}: CRDs did not appear in cluster"
    return
  fi

  # List discovered CRDs
  info "CRDs in cluster for ${OP_API_GROUP}:"
  get_crds_for_group "$OP_API_GROUP" | sed 's/^/  /'

  # Wait for capabilities in ChromaDB
  local crd_count
  crd_count=$(get_crds_for_group "$OP_API_GROUP" | wc -l | tr -d ' ')

  if wait_for_capabilities "$collection_id" "$OP_API_GROUP" "$crd_count"; then
    local cap_count
    cap_count=$(count_capabilities_by_group "$collection_id" "$OP_API_GROUP")
    record_pass "${OP_NAME}: ${cap_count} capabilities with inferred descriptions"
    print_capabilities "$collection_id" "$OP_API_GROUP"
  else
    local partial
    partial=$(count_capabilities_by_group "$collection_id" "$OP_API_GROUP")
    if [[ "$partial" -gt 0 ]]; then
      record_fail "${OP_NAME}: ${partial}/${crd_count} capabilities (partial — missing some CRD schemas)"
      print_capabilities "$collection_id" "$OP_API_GROUP"
    else
      record_fail "${OP_NAME}: no capabilities found after ${POLL_TIMEOUT}s"
    fi
  fi
}

# ---------------------------------------------------------------------------
# Validate uninstall: remove operator → verify capabilities removed from ChromaDB
# ---------------------------------------------------------------------------
validate_uninstall() {
  local op_def="$1"
  local collection_id="$2"
  parse_operator "$op_def"

  echo ""
  echo -e "${BOLD}--- Uninstall Validation: ${OP_NAME} ---${NC}"

  # Confirm capabilities exist before uninstall
  local before_count
  before_count=$(count_capabilities_by_group "$collection_id" "$OP_API_GROUP")
  if [[ "$before_count" -eq 0 ]]; then
    warn "No capabilities found for ${OP_NAME} before uninstall — skipping uninstall validation"
    return
  fi
  info "${before_count} capabilities exist before uninstall"

  # Uninstall
  uninstall_operator "$op_def"

  # Wait for capabilities to be removed
  if wait_for_capability_removal "$collection_id" "$OP_API_GROUP"; then
    record_pass "${OP_NAME} uninstall: capabilities removed from ChromaDB"
  else
    local remaining
    remaining=$(count_capabilities_by_group "$collection_id" "$OP_API_GROUP")
    record_fail "${OP_NAME} uninstall: ${remaining} capabilities still present after ${POLL_TIMEOUT}s"
  fi
}

# ---------------------------------------------------------------------------
# Cleanup: remove all installed operators
# ---------------------------------------------------------------------------
cleanup() {
  if [[ "$SKIP_CLEANUP" == "true" ]]; then
    info "SKIP_CLEANUP=true — leaving operators installed"
    return
  fi

  echo ""
  echo -e "${BOLD}=== Cleanup ===${NC}"
  for op_def in "${OPERATORS[@]}"; do
    parse_operator "$op_def"
    # Only uninstall if the helm release exists
    if helm list -n "$OP_NAMESPACE" 2>/dev/null | grep -q "$OP_REPO_NAME"; then
      uninstall_operator "$op_def" || true
    fi
  done
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
main() {
  echo ""
  echo -e "${BOLD}================================================================${NC}"
  echo -e "${BOLD}  Full-Stack Validation: CRD Change Detection → Capabilities   ${NC}"
  echo -e "${BOLD}================================================================${NC}"
  echo ""
  echo "  ChromaDB:           ${CHROMA_URL}"
  echo "  cluster-whisperer:  ${CLUSTER_WHISPERER_URL}"
  echo "  Controller NS:      ${CONTROLLER_NS}"
  echo "  Poll timeout:       ${POLL_TIMEOUT}s"
  echo ""

  # Prerequisites
  check_prerequisites

  # Get collection ID
  local collection_id
  collection_id=$(get_collection_id)
  if [[ -z "$collection_id" ]]; then
    fail "Could not find 'capabilities' collection in ChromaDB"
    exit 1
  fi
  info "Capabilities collection ID: ${collection_id}"

  # Record baseline using the documented /get endpoint
  local baseline
  baseline=$(curl -sf -X POST "${CHROMA_API}/collections/${collection_id}/get" \
    -H "Content-Type: application/json" \
    -d '{"include": []}' | \
    python3 -c "import json, sys; d = json.load(sys.stdin); print(len(d.get('ids', [])))" 2>/dev/null) || baseline="0"
  info "Baseline capability count: ${baseline}"

  # Install and verify each operator
  echo ""
  echo -e "${BOLD}=== Operator Installation & Verification ===${NC}"
  for op_def in "${OPERATORS[@]}"; do
    validate_operator "$op_def" "$collection_id"
  done

  # Uninstall validation (use the first operator)
  echo ""
  echo -e "${BOLD}=== Uninstall Verification ===${NC}"
  validate_uninstall "${OPERATORS[0]}" "$collection_id"

  # Cleanup remaining operators
  cleanup

  # Results summary
  echo ""
  echo -e "${BOLD}================================================================${NC}"
  echo -e "${BOLD}  Results Summary                                               ${NC}"
  echo -e "${BOLD}================================================================${NC}"
  echo ""
  for result in "${RESULTS[@]}"; do
    echo -e "  ${result}"
  done
  echo ""
  echo -e "  Total: ${GREEN}${TOTAL_PASS} passed${NC}, ${RED}${TOTAL_FAIL} failed${NC}"
  echo ""

  if [[ "$TOTAL_FAIL" -gt 0 ]]; then
    exit 1
  fi
}

# Trap for cleanup on unexpected exit
handle_interrupt() {
  echo ""
  if [[ "${SKIP_CLEANUP:-}" == "true" ]]; then
    warn "Script interrupted. SKIP_CLEANUP=true — preserving state."
  else
    warn "Script interrupted. Cleaning up..."
    cleanup 2>/dev/null || true
  fi
  exit 1
}
trap 'handle_interrupt' INT TERM

main "$@"
