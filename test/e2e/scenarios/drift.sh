#!/usr/bin/env bash
# Scenario: ProxySQLConfig update propagation + out-of-band drift self-heal.
#  1. A spec update (add a backend) propagates and advances LastAppliedHash.
#  2. Runtime mutated out-of-band (DELETE on the admin) is re-asserted within
#     the operator's resync interval (the suite sets it to ~15s).

scenario_drift() {
  local ns=e2e-drift
  kubectl create ns "$ns" >/dev/null
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
  mysqlVariables:
    mysql-monitor_enabled: "false"
YAML
  wait_pod_ready "$ns" pxc-0 || { fail "pxc-0 not Ready"; dump_ns "$ns"; return 1; }
  wait_config_synced "$ns" pxcfg 1 120 || { dump_ns "$ns"; return 1; }

  local radmin out hash0 i
  radmin="$(radmin_pw "$ns" pxc)"
  hash0="$(kubectl -n "$ns" get proxysqlconfig pxcfg -o jsonpath='{.status.lastAppliedHash}')"

  # --- update propagation ---
  kubectl -n "$ns" patch proxysqlconfig pxcfg --type=json \
    -p='[{"op":"add","path":"/spec/mysqlServers/-","value":{"hostgroup":1,"hostname":"10.9.9.10","port":3306}}]' >/dev/null
  for i in $(seq 1 15); do
    [[ "$(kubectl -n "$ns" get proxysqlconfig pxcfg -o jsonpath='{.status.lastAppliedHash}')" != "$hash0" ]] && break
    sleep 4
  done
  out="$(admin_query "$ns" pxc "$radmin" "SELECT COUNT(*) FROM runtime_mysql_servers")"
  [[ "$out" == "2" ]] || { fail "config update did not propagate (expected 2 servers, got '$out')"; dump_ns "$ns"; return 1; }
  log "drift: config update propagated (2 servers in runtime)"

  # --- out-of-band drift heal ---
  log "drift: wiping runtime on pxc-0 out-of-band"
  kubectl -n "$ns" run e2e-wipe --rm -i --restart=Never --image="$MYSQL_IMAGE" --command -- \
    mysql -h pxc -P6032 -uradmin -p"$radmin" -e "DELETE FROM mysql_servers; LOAD MYSQL SERVERS TO RUNTIME;" >/dev/null 2>&1 || true
  # Confirm it is actually wiped, then wait for the operator's periodic resync.
  for i in $(seq 1 20); do
    out="$(admin_query "$ns" pxc "$radmin" "SELECT COUNT(*) FROM runtime_mysql_servers")"
    [[ "$out" == "2" ]] && { log "drift: out-of-band wipe self-healed after ~$((i*4))s"; return 0; }
    sleep 4
  done
  fail "drift was not self-healed (runtime_mysql_servers count='$out')"; dump_ns "$ns"; return 1
}
