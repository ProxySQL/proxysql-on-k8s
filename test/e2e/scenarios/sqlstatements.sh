#!/usr/bin/env bash
# Scenario: spec.sqlStatements raw admin SQL escape hatch.
#  1. Statements execute on the replica (UPDATE + LOAD visible in runtime).
#  2. Editing a statement re-syncs (lastAppliedHash advances, new value lands).
# Uses mysql-max_connections, which the structured config below does NOT set,
# so any runtime effect is attributable to sqlStatements alone.

scenario_sqlstatements() {
  local ns=e2e-sqlstmt
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
  sqlStatements:
    - "UPDATE global_variables SET variable_value='777' WHERE variable_name='mysql-max_connections'"
    - "LOAD MYSQL VARIABLES TO RUNTIME"
YAML
  kubectl -n "$ns" wait --for=condition=Ready pod/pxc-0 --timeout=120s >/dev/null
  wait_config_synced "$ns" pxcfg 1 120 || { dump_ns "$ns"; return 1; }

  local radmin out hash0
  radmin="$(radmin_pw "$ns" pxc)"
  out="$(admin_query "$ns" pxc "$radmin" \
    "SELECT variable_value FROM runtime_global_variables WHERE variable_name='mysql-max_connections'")"
  [[ "$out" == "777" ]] || { fail "sqlStatements did not apply (max_connections='$out', want 777)"; dump_ns "$ns"; return 1; }
  log "sqlstatements: raw SQL applied (runtime max_connections=777)"

  hash0="$(kubectl -n "$ns" get proxysqlconfig pxcfg -o jsonpath='{.status.lastAppliedHash}')"
  kubectl -n "$ns" patch proxysqlconfig pxcfg --type=json \
    -p='[{"op":"replace","path":"/spec/sqlStatements/0","value":"UPDATE global_variables SET variable_value='\''778'\'' WHERE variable_name='\''mysql-max_connections'\''"}]' >/dev/null
  for _ in $(seq 1 15); do
    [[ "$(kubectl -n "$ns" get proxysqlconfig pxcfg -o jsonpath='{.status.lastAppliedHash}')" != "$hash0" ]] && break
    sleep 4
  done
  out="$(admin_query "$ns" pxc "$radmin" \
    "SELECT variable_value FROM runtime_global_variables WHERE variable_name='mysql-max_connections'")"
  [[ "$out" == "778" ]] || { fail "edited statement did not re-sync (max_connections='$out', want 778)"; dump_ns "$ns"; return 1; }
  log "sqlstatements: statement edit re-synced (runtime max_connections=778)"
}
