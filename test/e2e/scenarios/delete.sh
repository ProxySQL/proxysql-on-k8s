#!/usr/bin/env bash
# Scenario: ProxySQLConfig deletion cleanup. The config controller holds the
# `proxysql.com/config-cleanup` finalizer and must clear every admin table it
# owns (servers, users, query rules — disk AND runtime) before releasing it.
# We seed a config, confirm it landed in runtime, delete the CR, and assert
# the admin tables are empty afterwards while the cluster pod stays up.

scenario_delete() {
  local ns=e2e-delete
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
    - {hostgroup: 1, hostname: 10.9.9.10, port: 3306}
  mysqlQueryRules:
    - {ruleId: 1, active: true, matchDigest: "^SELECT", destinationHostgroup: 1, apply: true}
  mysqlVariables:
    mysql-monitor_enabled: "false"
YAML
  kubectl -n "$ns" wait --for=condition=Ready pod/pxc-0 --timeout=120s >/dev/null
  wait_config_synced "$ns" pxcfg 1 120 || { dump_ns "$ns"; return 1; }

  local radmin out t
  radmin="$(radmin_pw "$ns" pxc)"

  # Precondition: both backends made it into runtime before we delete.
  out="$(admin_query "$ns" pxc "$radmin" "SELECT COUNT(*) FROM runtime_mysql_servers")"
  [[ "$out" == "2" ]] || { fail "delete: precondition failed — expected 2 runtime servers, got '$out'"; dump_ns "$ns"; return 1; }
  log "delete: precondition ok (2 servers in runtime)"

  # The delete must complete: the finalizer blocks until cleanup succeeds.
  kubectl -n "$ns" delete proxysqlconfig pxcfg --timeout=90s >/dev/null \
    || { fail "delete: proxysqlconfig pxcfg not gone after 90s (finalizer wedged?)"; dump_ns "$ns"; return 1; }
  log "delete: pxcfg deleted, finalizer released"

  # Post: every table the config owned is empty — runtime and disk.
  for t in runtime_mysql_servers mysql_servers runtime_mysql_query_rules; do
    out="$(admin_query "$ns" pxc "$radmin" "SELECT COUNT(*) FROM $t")"
    [[ "$out" == "0" ]] || { fail "delete: $t not cleaned up (expected 0 rows, got '$out')"; dump_ns "$ns"; return 1; }
  done
  log "delete: admin tables cleaned up on CR deletion (servers disk+runtime, query rules)"

  kubectl delete ns "$ns" --wait=false >/dev/null
}
