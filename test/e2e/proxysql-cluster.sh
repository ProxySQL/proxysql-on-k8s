#!/usr/bin/env bash
#
# End-to-end test for the ProxySQL operator.
#
# Steps:
#   1. Create (or reuse) a kind cluster.
#   2. Build the operator image and `kind load` it.
#   3. helm install the proxysql-operator chart.
#   4. kubectl apply a ProxySQLCluster (1 replica) and a ProxySQLConfig.
#   5. Wait for the StatefulSet pod to be Ready.
#   6. Wait for ProxySQLConfig status.syncedReplicas to reach 1.
#   7. Run a sanity query against ProxySQL's admin port and assert that
#      runtime_mysql_servers contains the backend declared in the
#      ProxySQLConfig.
#   8. Tear down (unless KEEP_CLUSTER=1).
#
# Prerequisites on PATH:
#   - kind   (https://kind.sigs.k8s.io/)
#   - helm   3.x
#   - kubectl (any 1.29+ should work)
#   - docker buildx
#
# Environment overrides:
#   KIND_CLUSTER     name of the kind cluster (default: proxysql-e2e)
#   KIND_NODE_IMAGE  node image (default: kindest/node:v1.31.0)
#   IMG              operator image tag (default: proxysql-operator:e2e)
#   KEEP_CLUSTER=1   leave the cluster running after the test
#   SKIP_BUILD=1     skip docker build (use an already-loaded image)

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")"/../.. && pwd)"
KIND_CLUSTER="${KIND_CLUSTER:-proxysql-e2e}"
KIND_NODE_IMAGE="${KIND_NODE_IMAGE:-kindest/node:v1.31.0}"
IMG="${IMG:-proxysql-operator:e2e}"
NAMESPACE="${NAMESPACE:-proxysql-e2e}"
CLUSTER_NAME="pxc"
CONFIG_NAME="pxcfg"

log()  { printf '\033[36m[e2e]\033[0m %s\n' "$*" >&2; }
warn() { printf '\033[33m[e2e]\033[0m %s\n' "$*" >&2; }
fail() { printf '\033[31m[e2e FAIL]\033[0m %s\n' "$*" >&2; exit 1; }

need() { command -v "$1" >/dev/null 2>&1 || fail "missing required tool: $1"; }
need kind
need helm
need kubectl
need docker

cleanup() {
  if [[ "${KEEP_CLUSTER:-0}" == "1" ]]; then
    log "KEEP_CLUSTER=1 — leaving kind cluster '$KIND_CLUSTER' running"
    return
  fi
  log "deleting kind cluster '$KIND_CLUSTER'"
  kind delete cluster --name "$KIND_CLUSTER" >/dev/null 2>&1 || true
}
trap cleanup EXIT

#-----------------------------------------------------------------------------
# 1) Cluster
#-----------------------------------------------------------------------------
if kind get clusters | grep -qx "$KIND_CLUSTER"; then
  log "reusing existing kind cluster '$KIND_CLUSTER'"
else
  log "creating kind cluster '$KIND_CLUSTER' ($KIND_NODE_IMAGE)"
  kind create cluster --name "$KIND_CLUSTER" --image "$KIND_NODE_IMAGE"
fi
kubectl cluster-info --context "kind-$KIND_CLUSTER" >/dev/null

#-----------------------------------------------------------------------------
# 2) Image
#-----------------------------------------------------------------------------
if [[ "${SKIP_BUILD:-0}" == "1" ]]; then
  log "SKIP_BUILD=1 — assuming $IMG is already loaded into the kind cluster"
else
  log "building operator image $IMG"
  ( cd "$REPO_ROOT/operator" && docker buildx build --load -t "$IMG" . )
  log "loading $IMG into kind"
  kind load docker-image "$IMG" --name "$KIND_CLUSTER"
fi

#-----------------------------------------------------------------------------
# 3) Helm install operator
#-----------------------------------------------------------------------------
kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

log "installing proxysql-operator chart"
# Override the image to point at the locally-loaded tag; force pullPolicy=Never
# so kind doesn't try to fetch from a registry.
helm upgrade --install proxysql-operator "$REPO_ROOT/charts/proxysql-operator" \
  --namespace "$NAMESPACE" \
  --set image.repository="${IMG%:*}" \
  --set image.tag="${IMG#*:}" \
  --set image.pullPolicy=Never \
  --wait --timeout=2m

log "waiting for operator deployment Ready"
kubectl -n "$NAMESPACE" rollout status deploy/proxysql-operator --timeout=2m

#-----------------------------------------------------------------------------
# 4) Create ProxySQLCluster + ProxySQLConfig
#-----------------------------------------------------------------------------
log "creating ProxySQLCluster $NAMESPACE/$CLUSTER_NAME"
cat <<EOF | kubectl apply -f -
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLCluster
metadata:
  name: ${CLUSTER_NAME}
  namespace: ${NAMESPACE}
spec:
  replicas: 1
  # Keep test fast — disable persistence so we don't need a PV provisioner.
  persistence:
    enabled: false
  protocols:
    mysql:
      enabled: true
    pgsql:
      enabled: false
EOF

log "waiting for StatefulSet to materialise"
kubectl -n "$NAMESPACE" wait --for=jsonpath='{.status.replicas}'=1 \
  statefulset/"$CLUSTER_NAME" --timeout=2m

log "waiting for pod ${CLUSTER_NAME}-0 to be Ready"
kubectl -n "$NAMESPACE" wait --for=condition=Ready \
  pod/"${CLUSTER_NAME}-0" --timeout=3m

log "creating ProxySQLConfig $NAMESPACE/$CONFIG_NAME"
cat <<EOF | kubectl apply -f -
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLConfig
metadata:
  name: ${CONFIG_NAME}
  namespace: ${NAMESPACE}
spec:
  clusterRef:
    name: ${CLUSTER_NAME}
  mysqlServers:
    - hostgroup: 0
      hostname: 127.0.0.1
      port: 13306
      comment: "fake backend - e2e never actually queries it"
EOF

log "waiting for ProxySQLConfig.status.syncedReplicas=1"
for i in $(seq 1 60); do
  synced="$(kubectl -n "$NAMESPACE" get proxysqlconfig "$CONFIG_NAME" \
    -o jsonpath='{.status.syncedReplicas}' 2>/dev/null || true)"
  if [[ "${synced:-0}" == "1" ]]; then
    log "ProxySQLConfig synced after ${i}s"
    break
  fi
  sleep 1
done
if [[ "${synced:-0}" != "1" ]]; then
  kubectl -n "$NAMESPACE" describe proxysqlconfig "$CONFIG_NAME" >&2 || true
  kubectl -n "$NAMESPACE" logs deploy/proxysql-operator --tail=80 >&2 || true
  fail "ProxySQLConfig did not reach syncedReplicas=1"
fi

#-----------------------------------------------------------------------------
# 5) Assert runtime tables match the spec
#-----------------------------------------------------------------------------
log "reading minted admin password from the operator-managed Secret"
ADMIN_PW="$(kubectl -n "$NAMESPACE" get secret "$CLUSTER_NAME" \
  -o jsonpath='{.data.admin-password}' | base64 -d)"
[[ -n "$ADMIN_PW" ]] || fail "admin password is empty"

log "querying ProxySQL admin port via a transient mysql client pod"
# We can't assume the proxysql container has a mysql client (readOnlyRootFilesystem +
# distroless-style upstream image), so spin up a one-shot mysql:8 pod that connects to
# the in-cluster admin service.
QUERY_OUT="$(kubectl -n "$NAMESPACE" run e2e-mysql-client \
  --rm -i --restart=Never \
  --image=mysql:8.0 \
  --command -- \
  mysql -h "${CLUSTER_NAME}" -P 6032 -uadmin -p"$ADMIN_PW" -N -B \
    -e "SELECT hostgroup_id, hostname, port FROM runtime_mysql_servers ORDER BY hostgroup_id;" 2>&1)"

log "admin query result:"
printf '%s\n' "$QUERY_OUT" >&2

if ! grep -E "^0[[:space:]]+127\.0\.0\.1[[:space:]]+13306$" <<<"$QUERY_OUT" >/dev/null; then
  fail "runtime_mysql_servers does not contain the row we declared"
fi

log "PASS — operator end-to-end flow works"
