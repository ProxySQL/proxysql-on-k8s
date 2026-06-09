#!/usr/bin/env bash
# Shared helpers for the ProxySQL operator e2e suite (sourced by run.sh).
#
# Conventions:
#   - One kind cluster, one operator install, each scenario in its own namespace.
#   - Backends and clients are plain upstream images (mysql:8.0, postgres:16) so
#     no backend operator is required for the per-PR suite.
#   - The admin plane (6032) is MySQL-wire and only the `radmin` account may
#     connect remotely; `admin` is localhost-only in ProxySQL.

set -euo pipefail

# ---- output ----------------------------------------------------------------
c_cyan=$'\033[36m'; c_red=$'\033[31m'; c_grn=$'\033[32m'; c_rst=$'\033[0m'
log()  { printf '%s[e2e]%s %s\n' "$c_cyan" "$c_rst" "$*" >&2; }
ok()   { printf '%s[e2e PASS]%s %s\n' "$c_grn" "$c_rst" "$*" >&2; }
fail() { printf '%s[e2e FAIL]%s %s\n' "$c_red" "$c_rst" "$*" >&2; return 1; }
need() { command -v "$1" >/dev/null 2>&1 || { echo "missing required tool: $1" >&2; exit 1; }; }

# ---- config (overridable) --------------------------------------------------
KIND_CLUSTER="${KIND_CLUSTER:-proxysql-e2e}"
KIND_NODE_IMAGE="${KIND_NODE_IMAGE:-kindest/node:v1.31.0}"
IMG="${IMG:-proxysql-operator:e2e}"
OPERATOR_NS="${OPERATOR_NS:-proxysql-system}"
MYSQL_IMAGE="${MYSQL_IMAGE:-mysql:8.0}"
PG_IMAGE="${PG_IMAGE:-postgres:16}"

# Repo root (this file lives in test/e2e/).
E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$E2E_DIR/../.." && pwd)"

# ---- cluster / operator ----------------------------------------------------
ensure_cluster() {
  if kind get clusters 2>/dev/null | grep -qx "$KIND_CLUSTER"; then
    log "reusing kind cluster '$KIND_CLUSTER'"
  else
    log "creating kind cluster '$KIND_CLUSTER' ($KIND_NODE_IMAGE)"
    kind create cluster --name "$KIND_CLUSTER" --image "$KIND_NODE_IMAGE"
  fi
  kubectl cluster-info --context "kind-$KIND_CLUSTER" >/dev/null
}

build_and_load_image() {
  if [[ "${SKIP_BUILD:-0}" == "1" ]]; then
    log "SKIP_BUILD=1 — assuming $IMG already loaded into kind"
  else
    log "building operator image $IMG"
    ( cd "$REPO_ROOT/operator" && docker buildx build --load -t "$IMG" . )
    log "kind-loading $IMG"
    kind load docker-image "$IMG" --name "$KIND_CLUSTER"
  fi
  # Pre-load the ProxySQL data-plane image too, to avoid Docker Hub flakiness.
  if docker image inspect proxysql/proxysql:3.0 >/dev/null 2>&1 || docker pull proxysql/proxysql:3.0 >/dev/null 2>&1; then
    kind load docker-image proxysql/proxysql:3.0 --name "$KIND_CLUSTER" >/dev/null 2>&1 || true
  fi
}

install_operator() {
  log "installing proxysql-operator chart into $OPERATOR_NS"
  # Short config-resync interval so the drift scenario heals quickly instead of
  # waiting the 2m production default.
  helm upgrade --install proxysql-operator "$REPO_ROOT/charts/proxysql-operator" \
    --namespace "$OPERATOR_NS" --create-namespace \
    --set image.repository="${IMG%:*}" \
    --set image.tag="${IMG##*:}" \
    --set image.pullPolicy=Never \
    --set configResyncInterval="${CONFIG_RESYNC_INTERVAL:-15s}" \
    --wait --timeout=2m
  kubectl -n "$OPERATOR_NS" rollout status deploy/proxysql-operator --timeout=2m
}

# ---- helpers ---------------------------------------------------------------
# radmin_pw NS SECRET -> prints the minted radmin password.
radmin_pw() {
  kubectl -n "$1" get secret "$2" -o jsonpath='{.data.radmin-password}' | base64 -d
}

# wait_config_synced NS NAME WANT TIMEOUT_S
wait_config_synced() {
  local ns="$1" name="$2" want="$3" timeout="${4:-120}" i
  for ((i=0; i<timeout; i+=5)); do
    local got
    got="$(kubectl -n "$ns" get proxysqlconfig "$name" -o jsonpath='{.status.syncedReplicas}' 2>/dev/null || true)"
    [[ "${got:-0}" == "$want" ]] && { log "$ns/$name syncedReplicas=$want (after ${i}s)"; return 0; }
    sleep 5
  done
  kubectl -n "$ns" get proxysqlconfig "$name" -o yaml >&2 || true
  fail "$ns/$name did not reach syncedReplicas=$want in ${timeout}s"
}

# _strip_noise filters the cruft a one-shot `kubectl run --rm -i` leaves on
# stdout: the `pod "x" deleted` cleanup notice and blank lines. (We pass the DB
# password via an env var, never -p, so there is no client warning to strip.)
_strip_noise() {
  grep -vE '^pod "[^"]*" deleted$|^[[:space:]]*$' || true
}

# admin_query NS HOST RADMIN_PW SQL -> runs SQL against the admin port (6032)
# via a one-shot mysql client. Retries on empty (after stripping noise) to
# absorb transient client-pod flakes.
admin_query() {
  local ns="$1" host="$2" pw="$3" sql="$4" i out
  for i in 1 2 3 4 5; do
    out="$(kubectl -n "$ns" run "e2e-myq-$RANDOM" --rm -i --restart=Never --image="$MYSQL_IMAGE" \
      --env=MYSQL_PWD="$pw" --command -- \
      mysql -h "$host" -P6032 -uradmin -N -B -e "$sql" 2>/dev/null | _strip_noise || true)"
    [[ -n "$out" ]] && { printf '%s' "$out"; return 0; }
    sleep 4
  done
  printf '%s' "$out"
}

# psql_query NS HOST USER PW DB SQL -> runs SQL through the pgsql port (6133).
psql_query() {
  local ns="$1" host="$2" user="$3" pw="$4" db="$5" sql="$6" i out
  for i in 1 2 3 4 5; do
    out="$(kubectl -n "$ns" run "e2e-pgq-$RANDOM" --rm -i --restart=Never --image="$PG_IMAGE" \
      --env=PGPASSWORD="$pw" --command -- \
      psql -h "$host" -p 6133 -U "$user" -d "$db" -tA -c "$sql" 2>/dev/null | _strip_noise || true)"
    [[ -n "$out" ]] && { printf '%s' "$out"; return 0; }
    sleep 4
  done
  printf '%s' "$out"
}

# mysql_query NS HOST USER PW SQL -> runs SQL through the mysql data port (6033).
mysql_query() {
  local ns="$1" host="$2" user="$3" pw="$4" sql="$5" i out
  for i in 1 2 3 4 5; do
    out="$(kubectl -n "$ns" run "e2e-dq-$RANDOM" --rm -i --restart=Never --image="$MYSQL_IMAGE" \
      --env=MYSQL_PWD="$pw" --command -- \
      mysql -h "$host" -P6033 -u"$user" -N -B -e "$sql" 2>/dev/null | _strip_noise || true)"
    [[ -n "$out" ]] && { printf '%s' "$out"; return 0; }
    sleep 4
  done
  printf '%s' "$out"
}

# dump_ns NS -> diagnostics on failure.
dump_ns() {
  local ns="$1"
  { echo "---- namespace $ns ----"
    kubectl -n "$ns" get all 2>&1
    kubectl -n "$OPERATOR_NS" logs deploy/proxysql-operator --tail=120 2>&1 | grep -i "$ns" || true
  } >&2
}
