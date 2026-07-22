#!/usr/bin/env bash
# Scenario: multi-replica write-to-all + pod-recreation reconvergence.
# Verifies syncedReplicas reaches the full replica count, then deletes a pod and
# confirms the recreated pod (new IP, empty runtime) gets config re-pushed
# quickly — exercising the address-set fingerprint in the short-circuit.

scenario_multireplica() {
  local ns=e2e-multi
  kubectl create ns "$ns" >/dev/null
  kubectl -n "$ns" apply -f - >/dev/null <<'YAML'
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLCluster
metadata: {name: pxc}
spec:
  replicas: 3
  persistence: {enabled: false}
  protocols: {mysql: {enabled: true}, pgsql: {enabled: false}}
---
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLConfig
metadata: {name: pxcfg}
spec:
  clusterRef: {name: pxc}
  mysqlServers:
    - {hostgroup: 0, hostname: 10.9.9.9, port: 3306, comment: fake}
  mysqlVariables:
    mysql-monitor_enabled: "false"
YAML
  kubectl -n "$ns" rollout status statefulset/pxc --timeout=180s >/dev/null
  wait_config_synced "$ns" pxcfg 3 120 || { dump_ns "$ns"; return 1; }
  log "multireplica: write-to-all reached syncedReplicas=3"

  local radmin oldip newip out
  radmin="$(radmin_pw "$ns" pxc)"

  # Issue #39: a config whose spec.proxysqlServers is empty must NOT wipe the
  # peer table — the operator auto-populates it from the StatefulSet pods, so
  # runtime_proxysql_servers must still hold all 3 peers after the sync.
  local ip0 peers
  ip0="$(kubectl -n "$ns" get pod pxc-0 -o jsonpath='{.status.podIP}')"
  peers="$(admin_query "$ns" "$ip0" "$radmin" "SELECT COUNT(*) FROM runtime_proxysql_servers")"
  if [[ "$peers" != "3" ]]; then
    fail "expected 3 peers in runtime_proxysql_servers after config sync, got '$peers'"
    dump_ns "$ns"
    return 1
  fi
  log "multireplica: runtime_proxysql_servers still has 3 peers after config sync (#39)"
  oldip="$(kubectl -n "$ns" get pod pxc-1 -o jsonpath='{.status.podIP}')"
  log "multireplica: deleting pxc-1 (old IP $oldip)"
  kubectl -n "$ns" delete pod pxc-1 --wait=true >/dev/null
  wait_pod_ready "$ns" pxc-1 || { fail "pxc-1 not Ready"; dump_ns "$ns"; return 1; }
  newip="$(kubectl -n "$ns" get pod pxc-1 -o jsonpath='{.status.podIP}')"
  log "multireplica: pxc-1 recreated (new IP $newip)"

  # Poll the recreated pod directly: it must receive config well before the
  # safety requeue.
  local i
  for i in $(seq 1 15); do
    out="$(admin_query "$ns" "$newip" "$radmin" "SELECT hostgroup_id,hostname FROM runtime_mysql_servers")"
    echo "$out" | grep -qE "^0[[:space:]]+10\.9\.9\.9$" && { log "multireplica: recreated pod has config after ~$((i*4))s"; return 0; }
    sleep 4
  done
  fail "recreated pod pxc-1 did not receive config: '$out'"; dump_ns "$ns"; return 1
}
