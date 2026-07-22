#!/usr/bin/env bash
# Scenario: password Secret rotation. The operator watches Secrets referenced
# by ProxySQLConfig users; updating the password must trigger an immediate
# re-sync (lastAppliedHash advances) without us touching the CR. Afterwards
# the runtime read-back status (lastRuntimeCheckTime/driftedReplicas) must
# report a clean, drift-free cluster.

scenario_rotate() {
  local ns=e2e-rotate
  kubectl create ns "$ns" >/dev/null
  kubectl -n "$ns" create secret generic app-user-pw --from-literal=password=first-pw >/dev/null
  kubectl -n "$ns" apply -f - >/dev/null <<'YAML'
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLCluster
metadata: {name: pxc}
spec:
  replicas: 1
  persistence: {enabled: false}
  protocols: {mysql: {enabled: true}, pgsql: {enabled: false}}
---
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLConfig
metadata: {name: pxcfg}
spec:
  clusterRef: {name: pxc}
  mysqlServers:
    - {hostgroup: 0, hostname: 10.9.9.9, port: 3306}
  mysqlUsers:
    - {username: app, defaultHostgroup: 0, passwordSecretRef: {name: app-user-pw, key: password}}
  mysqlVariables:
    mysql-monitor_enabled: "false"
YAML
  wait_pod_ready "$ns" pxc-0 || { fail "pxc-0 not Ready"; dump_ns "$ns"; return 1; }
  wait_config_synced "$ns" pxcfg 1 120 || { dump_ns "$ns"; return 1; }

  local radmin out hash0 i
  radmin="$(radmin_pw "$ns" pxc)"
  hash0="$(kubectl -n "$ns" get proxysqlconfig pxcfg -o jsonpath='{.status.lastAppliedHash}')"

  # --- rotate the password Secret (no change to the CR itself) ---
  kubectl -n "$ns" create secret generic app-user-pw \
    --from-literal=password=second-pw --dry-run=client -o yaml \
    | kubectl -n "$ns" apply -f - >/dev/null
  log "rotate: secret app-user-pw updated (first-pw -> second-pw)"

  local rotated=0
  for i in $(seq 1 15); do
    [[ "$(kubectl -n "$ns" get proxysqlconfig pxcfg -o jsonpath='{.status.lastAppliedHash}')" != "$hash0" ]] && { rotated=1; break; }
    sleep 4
  done
  ((rotated)) || { fail "rotate: lastAppliedHash unchanged after secret rotation (~60s) — Secret watch not firing?"; dump_ns "$ns"; return 1; }
  log "rotate: lastAppliedHash advanced after ~$((i*4))s"

  # The user must survive the rotation (re-synced, not dropped).
  out="$(admin_query "$ns" pxc "$radmin" "SELECT COUNT(DISTINCT username) FROM runtime_mysql_users WHERE username='app'")"
  [[ "$out" == "1" ]] || { fail "rotate: user 'app' missing from runtime_mysql_users after rotation (got '$out')"; dump_ns "$ns"; return 1; }
  log "rotate: user 'app' present in runtime after rotation"

  # --- runtime read-back status ---
  local checked=""
  for i in $(seq 1 15); do
    checked="$(kubectl -n "$ns" get proxysqlconfig pxcfg -o jsonpath='{.status.lastRuntimeCheckTime}')"
    [[ -n "$checked" ]] && break
    sleep 4
  done
  [[ -n "$checked" ]] || { fail "rotate: status.lastRuntimeCheckTime never set — runtime read-back not running?"; dump_ns "$ns"; return 1; }
  out="$(kubectl -n "$ns" get proxysqlconfig pxcfg -o jsonpath='{.status.driftedReplicas}')"
  [[ -z "$out" || "$out" == "0" ]] || { fail "rotate: status.driftedReplicas='$out' after rotation (expected 0)"; dump_ns "$ns"; return 1; }
  log "rotate: runtime read-back clean (lastRuntimeCheckTime=$checked, driftedReplicas=${out:-0})"

  kubectl delete ns "$ns" --wait=false >/dev/null
}
