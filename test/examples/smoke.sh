#!/usr/bin/env bash
# End-to-end smoke test for one backend-operator cookbook under examples/.
#
#   test/examples/smoke.sh <example>
#
# where <example> is one of:
#   cloudnativepg | mariadb-operator | percona-ps | percona-pxc |
#   oracle-mysql-operator | crunchy-pgo
#
# For the chosen example it: installs the backend operator, applies the
# example's backend.yaml and waits for it, ensures the ProxySQL operator is
# installed, applies the example's proxysql.yaml and waits for the config to
# sync, then runs a query THROUGH ProxySQL against the real backend.
#
# Designed for the nightly workflow (one example per matrix leg). Heavy and
# network-dependent (pulls real backend operators) — NOT part of per-PR CI.
#
# Prereqs on PATH: kubectl, helm. A kind cluster + the operator image are
# expected to already exist (the workflow sets them up); pass SKIP_OPERATOR=1
# if the ProxySQL operator is already installed.

set -euo pipefail

EX="${1:?usage: smoke.sh <example>}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
IMG="${IMG:-proxysql-operator:ci}"
OPERATOR_NS="${OPERATOR_NS:-proxysql-system}"

c_cyan=$'\033[36m'; c_red=$'\033[31m'; c_grn=$'\033[32m'; c_rst=$'\033[0m'
log()  { printf '%s[smoke:%s]%s %s\n' "$c_cyan" "$EX" "$c_rst" "$*" >&2; }
ok()   { printf '%s[smoke PASS:%s]%s %s\n' "$c_grn" "$EX" "$c_rst" "$*" >&2; }
die()  { printf '%s[smoke FAIL:%s]%s %s\n' "$c_red" "$EX" "$c_rst" "$*" >&2; exit 1; }

helm_repo() { helm repo add "$1" "$2" >/dev/null 2>&1 || true; helm repo update >/dev/null 2>&1; }

ensure_proxysql_operator() {
  [[ "${SKIP_OPERATOR:-0}" == "1" ]] && { log "SKIP_OPERATOR=1"; return; }
  log "installing ProxySQL operator ($IMG)"
  helm upgrade --install proxysql-operator "$REPO_ROOT/charts/proxysql-operator" \
    -n "$OPERATOR_NS" --create-namespace \
    --set image.repository="${IMG%:*}" --set image.tag="${IMG##*:}" --set image.pullPolicy=IfNotPresent \
    --wait --timeout=3m
}

# wait_synced NS CFG WANT — poll ProxySQLConfig.status.syncedReplicas >= WANT.
wait_synced() {
  local ns="$1" cfg="$2" want="${3:-1}" i s
  for ((i=0;i<60;i++)); do
    s="$(kubectl -n "$ns" get proxysqlconfig "$cfg" -o jsonpath='{.status.syncedReplicas}' 2>/dev/null || echo 0)"
    [[ "${s:-0}" -ge "$want" ]] 2>/dev/null && { log "config $ns/$cfg synced=$s"; return 0; }
    sleep 5
  done
  kubectl -n "$ns" get proxysqlconfig "$cfg" -o yaml >&2 || true
  die "config $ns/$cfg did not reach syncedReplicas>=$want"
}

# query_clean — run a client command via a one-shot pod and strip kubectl noise.
strip_noise() { grep -vE '^pod "[^"]*" deleted( from .+ namespace)?[[:space:]]*$|^[[:space:]]*$' || true; }

dir() { echo "$REPO_ROOT/examples/$1"; }

case "$EX" in
  cloudnativepg)
    d="$(dir postgresql/cloudnativepg)"
    helm_repo cnpg https://cloudnative-pg.github.io/charts
    helm install cnpg cnpg/cloudnative-pg -n cnpg-system --create-namespace --wait --timeout=5m
    ensure_proxysql_operator
    kubectl apply -f "$d/backend.yaml"
    log "waiting for CNPG cluster"
    kubectl -n cnpg-demo wait --for=condition=Ready cluster/pg --timeout=8m
    kubectl apply -f "$d/proxysql.yaml"
    kubectl -n cnpg-demo rollout status statefulset/proxysql --timeout=3m
    wait_synced cnpg-demo pxcfg 1
    pw="$(kubectl -n cnpg-demo get secret pg-app -o jsonpath='{.data.password}' | base64 -d)"
    out="$(kubectl -n cnpg-demo run smoke-$RANDOM --rm -i --restart=Never --image=postgres:16 \
      --env=PGPASSWORD="$pw" --command -- \
      psql -h proxysql -p 6133 -U app -d app -tAc "SELECT current_database()" 2>/dev/null | strip_noise || true)"
    echo "$out" | grep -qx app || die "psql through ProxySQL did not return 'app': '$out'"
    ok "psql through ProxySQL :6133 returned '$out'"
    ;;

  mariadb-operator)
    d="$(dir mysql/mariadb-operator)"
    helm_repo mariadb-operator https://helm.mariadb.com/mariadb-operator
    helm install mariadb-operator-crds mariadb-operator/mariadb-operator-crds -n mariadb-operator --create-namespace >/dev/null 2>&1 || true
    helm install mariadb-operator mariadb-operator/mariadb-operator -n mariadb-operator --wait --timeout=5m
    ensure_proxysql_operator
    kubectl apply -f "$d/backend.yaml"
    log "waiting for MariaDB cluster (replication)"
    kubectl -n mariadb-demo wait --for=condition=Ready mariadb/mariadb --timeout=10m
    kubectl apply -f "$d/proxysql.yaml"
    kubectl -n mariadb-demo rollout status statefulset/proxysql --timeout=3m
    wait_synced mariadb-demo pxcfg 1
    # root password is the example placeholder (consistent within the example).
    pw="$(kubectl -n mariadb-demo get secret mariadb-root -o jsonpath='{.data.password}' | base64 -d)"
    out="$(kubectl -n mariadb-demo run smoke-$RANDOM --rm -i --restart=Never --image=mariadb:11.4 \
      --env=MYSQL_PWD="$pw" --command -- \
      mariadb -h proxysql -P6033 -uroot -N -B -e "SELECT @@version" 2>/dev/null | strip_noise || true)"
    echo "$out" | grep -qiE "maria|[0-9]+\.[0-9]+" || die "query through ProxySQL did not return a version: '$out'"
    ok "mariadb query through ProxySQL :6033 returned '$out'"
    ;;

  percona-ps|percona-pxc|oracle-mysql-operator|crunchy-pgo)
    # These backends are heavier and not yet wired into the smoke harness.
    # The example manifests are schema-validated in per-PR CI; full end-to-end
    # coverage here is tracked as follow-up. Skip cleanly (matrix leg passes as
    # "not implemented" rather than failing the nightly).
    log "smoke for '$EX' not implemented yet — skipping (see issue #9)"
    exit 0
    ;;

  *)
    die "unknown example '$EX'"
    ;;
esac
