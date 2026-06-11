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

# rollout_sts NS NAME — wait for the ProxySQL operator to create the
# StatefulSet, then for it to become ready. A bare `rollout status` right
# after `kubectl apply` can race the reconciler and die on NotFound.
rollout_sts() {
  local ns="$1" name="$2" i
  for i in $(seq 1 24); do
    kubectl -n "$ns" get statefulset "$name" >/dev/null 2>&1 && break
    sleep 5
  done
  kubectl -n "$ns" rollout status "statefulset/$name" --timeout=3m
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

# strip_noise drops the `pod "x" deleted[ from <ns> namespace]` cleanup notice
# and blank lines that a one-shot `kubectl run --rm -i` leaves on stdout.
strip_noise() { grep -vE '^pod "[^"]*" deleted( from .+ namespace)?[[:space:]]*$|^[[:space:]]*$' || true; }

# client_query NS IMAGE ENVKV -- CMD...
# Runs CMD in a one-shot pod, returns its stripped stdout, and RETRIES on empty.
# The retry matters: a single attempt can come back empty when kubectl loses the
# attach and falls back to log-streaming, or the backend isn't quite reachable
# yet — which would otherwise fail the smoke spuriously.
client_query() {
  local ns="$1" image="$2" envkv="$3"; shift 3  # remaining args after `--` are CMD
  shift # drop the literal "--"
  local i out
  for i in 1 2 3 4 5 6; do
    out="$(kubectl -n "$ns" run "smoke-$RANDOM" --rm -i --restart=Never --image="$image" \
      --env="$envkv" --command -- "$@" 2>/dev/null | strip_noise || true)"
    [[ -n "$out" ]] && { printf '%s' "$out"; return 0; }
    sleep 5
  done
  printf '%s' "$out"
}

dir() { echo "$REPO_ROOT/examples/$1"; }

case "$EX" in
  cloudnativepg)
    d="$(dir postgresql/cloudnativepg)"
    helm_repo cnpg https://cloudnative-pg.github.io/charts
    helm upgrade --install cnpg cnpg/cloudnative-pg -n cnpg-system --create-namespace --wait --timeout=5m
    ensure_proxysql_operator
    kubectl apply -f "$d/backend.yaml"
    log "waiting for CNPG cluster"
    kubectl -n cnpg-demo wait --for=condition=Ready cluster/pg --timeout=8m
    kubectl apply -f "$d/proxysql.yaml"
    kubectl -n cnpg-demo rollout status statefulset/proxysql --timeout=3m
    wait_synced cnpg-demo pxcfg 1
    pw="$(kubectl -n cnpg-demo get secret pg-app -o jsonpath='{.data.password}' | base64 -d)"
    out="$(client_query cnpg-demo postgres:16 "PGPASSWORD=$pw" -- \
      psql -h proxysql -p 6133 -U app -d app -tAc "SELECT current_database()")"
    echo "$out" | grep -qx app || die "psql through ProxySQL did not return 'app': '$out'"
    ok "psql through ProxySQL :6133 returned '$out'"
    ;;

  mariadb-operator)
    d="$(dir mysql/mariadb-operator)"
    helm_repo mariadb-operator https://helm.mariadb.com/mariadb-operator
    helm upgrade --install mariadb-operator-crds mariadb-operator/mariadb-operator-crds -n mariadb-operator --create-namespace >/dev/null 2>&1 || true
    helm upgrade --install mariadb-operator mariadb-operator/mariadb-operator -n mariadb-operator --wait --timeout=5m
    ensure_proxysql_operator
    kubectl apply -f "$d/backend.yaml"
    log "waiting for MariaDB cluster (replication)"
    kubectl -n mariadb-demo wait --for=condition=Ready mariadb/mariadb --timeout=10m
    kubectl apply -f "$d/proxysql.yaml"
    kubectl -n mariadb-demo rollout status statefulset/proxysql --timeout=3m
    wait_synced mariadb-demo pxcfg 1
    # root password is the example placeholder (consistent within the example).
    pw="$(kubectl -n mariadb-demo get secret mariadb-root -o jsonpath='{.data.password}' | base64 -d)"
    out="$(client_query mariadb-demo mariadb:11.4 "MYSQL_PWD=$pw" -- \
      mariadb -h proxysql -P6033 -uroot -N -B -e "SELECT @@version")"
    echo "$out" | grep -qiE "maria|[0-9]+\.[0-9]+" || die "query through ProxySQL did not return a version: '$out'"
    ok "mariadb query through ProxySQL :6033 returned '$out'"
    ;;

  percona-ps)
    d="$(dir mysql/percona-ps)"
    helm_repo percona https://percona.github.io/percona-helm-charts/
    helm upgrade --install ps-operator percona/ps-operator --version 1.1.0 --set watchAllNamespaces=true \
      -n ps-operator --create-namespace --wait --timeout=5m
    ensure_proxysql_operator
    kubectl apply -f "$d/backend.yaml"
    log "waiting for PerconaServerMySQL (async, 1 primary + 1 replica)"
    kubectl -n percona-ps-demo wait perconaservermysqls.ps.percona.com/cluster1 \
      --for=jsonpath='{.status.state}'=ready --timeout=10m
    kubectl apply -f "$d/proxysql.yaml"
    rollout_sts percona-ps-demo proxysql
    wait_synced percona-ps-demo pxcfg 1
    pw="$(kubectl -n percona-ps-demo get secret cluster1-secrets -o jsonpath='{.data.root}' | base64 -d)"
    # SELECTs are routed to hostgroup 1; either node proves the path works.
    out="$(client_query percona-ps-demo mysql:8.4 "MYSQL_PWD=$pw" -- \
      mysql -h proxysql -P6033 -uroot -N -B -e "SELECT @@hostname")"
    echo "$out" | grep -q "cluster1-mysql" || die "query through ProxySQL did not reach a PS node: '$out'"
    ok "mysql query through ProxySQL :6033 answered by '$out'"
    ;;

  percona-pxc)
    d="$(dir mysql/percona-pxc)"
    helm_repo percona https://percona.github.io/percona-helm-charts/
    helm upgrade --install pxc-operator percona/pxc-operator --version 1.20.0 --set watchAllNamespaces=true \
      -n pxc-operator --create-namespace --wait --timeout=5m
    ensure_proxysql_operator
    kubectl apply -f "$d/backend.yaml"
    log "waiting for PerconaXtraDBCluster (3 nodes; first boot does SSTs)"
    kubectl -n percona-pxc-demo wait perconaxtradbclusters.pxc.percona.com/cluster1 \
      --for=jsonpath='{.status.state}'=ready --timeout=15m
    kubectl apply -f "$d/proxysql.yaml"
    rollout_sts percona-pxc-demo proxysql
    wait_synced percona-pxc-demo pxcfg 1
    pw="$(kubectl -n percona-pxc-demo get secret cluster1-secrets -o jsonpath='{.data.root}' | base64 -d)"
    out="$(client_query percona-pxc-demo mysql:8.4 "MYSQL_PWD=$pw" -- \
      mysql -h proxysql -P6033 -uroot -N -B -e "SELECT @@wsrep_node_name")"
    echo "$out" | grep -q "cluster1-pxc" || die "query through ProxySQL did not reach a Galera node: '$out'"
    ok "mysql query through ProxySQL :6033 answered by Galera node '$out'"
    ;;

  oracle-mysql-operator)
    d="$(dir mysql/oracle-mysql-operator)"
    helm_repo mysql-operator https://mysql.github.io/mysql-operator/
    helm upgrade --install mysql-operator mysql-operator/mysql-operator --version 2.2.8 \
      -n mysql-operator --create-namespace --wait --timeout=5m
    ensure_proxysql_operator
    kubectl apply -f "$d/backend.yaml"
    log "waiting for InnoDBCluster to reach ONLINE"
    kubectl -n oracle-mysql-demo wait innodbcluster/mycluster \
      --for=jsonpath='{.status.cluster.status}'=ONLINE --timeout=10m
    kubectl apply -f "$d/proxysql.yaml"
    rollout_sts oracle-mysql-demo proxysql
    wait_synced oracle-mysql-demo pxcfg 1
    pw="$(kubectl -n oracle-mysql-demo get secret mycluster-secret -o jsonpath='{.data.rootPassword}' | base64 -d)"
    out="$(client_query oracle-mysql-demo mysql:8.4 "MYSQL_PWD=$pw" -- \
      mysql -h proxysql -P6033 -uroot -N -B -e "SELECT @@hostname")"
    echo "$out" | grep -q "mycluster-0" || die "query through ProxySQL did not reach the InnoDB Cluster instance: '$out'"
    ok "mysql query through ProxySQL :6033 answered by '$out'"
    ;;

  crunchy-pgo)
    d="$(dir postgresql/crunchy-pgo)"
    helm upgrade --install pgo oci://registry.developers.crunchydata.com/crunchydata/pgo \
      --version 5.8.3 -n postgres-operator --create-namespace --wait --timeout=5m
    ensure_proxysql_operator
    kubectl apply -f "$d/backend.yaml"
    log "waiting for the Patroni leader pod"
    # The leader pod doesn't exist immediately — poll until the role label shows up.
    sel="postgres-operator.crunchydata.com/cluster=hippo,postgres-operator.crunchydata.com/role=master"
    for i in $(seq 1 60); do
      [[ -n "$(kubectl -n crunchy-demo get pod -l "$sel" -o name 2>/dev/null)" ]] && break
      sleep 5
    done
    kubectl -n crunchy-demo wait pod -l "$sel" --for=condition=Ready --timeout=10m
    kubectl apply -f "$d/proxysql.yaml"
    rollout_sts crunchy-demo proxysql
    wait_synced crunchy-demo pxcfg 1
    pw="$(kubectl -n crunchy-demo get secret hippo-pguser-app -o jsonpath='{.data.password}' | base64 -d)"
    out="$(client_query crunchy-demo postgres:16 "PGPASSWORD=$pw" -- \
      psql -h proxysql -p 6133 -U app -d app -tAc "SELECT current_database()")"
    echo "$out" | grep -qx app || die "psql through ProxySQL did not return 'app': '$out'"
    ok "psql through ProxySQL :6133 returned '$out'"
    ;;

  *)
    die "unknown example '$EX'"
    ;;
esac
