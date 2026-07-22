#!/usr/bin/env bash
# Scenario: ProxySQLConfig update propagation + out-of-band drift self-heal.
#  1. A spec update (add a backend) propagates and advances LastAppliedHash.
#  2. A server moved between the hostgroups of a mysqlReplicationHostgroups
#     pair (simulating the read_only monitor demoting a writer) is NOT drift:
#     the resync must leave the moved placement alone (#34, membership-only
#     drift semantics).
#  3. Runtime mutated out-of-band (DELETE on the admin) is re-asserted within
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
  # (0,1) is a replication-hostgroup pair: placement within it belongs to
  # ProxySQL's monitor, so drift detection must only enforce membership (#34).
  mysqlReplicationHostgroups:
    - {writerHostgroup: 0, readerHostgroup: 1}
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

  # --- monitor-style move within the replication pair is NOT drift (#34) ---
  # Move the hg0 server into hg1, as the read_only monitor would on a writer
  # demotion (the monitor is disabled here, so nothing will move it back).
  # The operator's informed resync must accept it as membership-converged:
  # the moved placement survives the resync and driftedReplicas stays 0.
  log "drift: moving 10.9.9.9 hg0->hg1 (simulated monitor demotion)"
  local t0 t1
  t0="$(kubectl -n "$ns" get proxysqlconfig pxcfg -o jsonpath='{.status.lastRuntimeCheckTime}')"
  kubectl -n "$ns" run e2e-move --rm -i --restart=Never --image="$MYSQL_IMAGE" --command -- \
    mysql -h pxc -P6032 -uradmin -p"$radmin" \
    -e "UPDATE mysql_servers SET hostgroup_id=1 WHERE hostname='10.9.9.9'; LOAD MYSQL SERVERS TO RUNTIME;" >/dev/null 2>&1 || true
  out="$(admin_query "$ns" pxc "$radmin" "SELECT hostgroup_id FROM runtime_mysql_servers WHERE hostname='10.9.9.9'")"
  [[ "$out" == "1" ]] || { fail "simulated monitor move did not apply (hostgroup='$out')"; dump_ns "$ns"; return 1; }
  # Wait for an informed resync to actually run (lastRuntimeCheckTime moves).
  for i in $(seq 1 20); do
    t1="$(kubectl -n "$ns" get proxysqlconfig pxcfg -o jsonpath='{.status.lastRuntimeCheckTime}')"
    [[ -n "$t1" && "$t1" != "$t0" ]] && break
    sleep 4
  done
  [[ -n "$t1" && "$t1" != "$t0" ]] || { fail "no informed resync ran after the simulated monitor move"; dump_ns "$ns"; return 1; }
  out="$(admin_query "$ns" pxc "$radmin" "SELECT hostgroup_id FROM runtime_mysql_servers WHERE hostname='10.9.9.9'")"
  [[ "$out" == "1" ]] || { fail "resync re-pushed spec placement over the monitor move (hostgroup='$out')"; dump_ns "$ns"; return 1; }
  out="$(kubectl -n "$ns" get proxysqlconfig pxcfg -o jsonpath='{.status.driftedReplicas}')"
  [[ "$out" == "0" ]] || { fail "monitor move flagged as drift (driftedReplicas='$out')"; dump_ns "$ns"; return 1; }
  log "drift: monitor-style move within the pair survived the resync (no drift)"

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
