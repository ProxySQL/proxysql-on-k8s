#!/usr/bin/env bash
# Scenario: runtime-applied variables changes (no pod restart).
# Demonstrates the runtime-reconfig feature:
#  1. spec.variables.mysql changes to runtime-settable variables apply via admin
#     SQL without pod restart or config checksum change.
#  2. Structural changes (e.g., spec.protocols.mysql.port) trigger rollout.

scenario_runtimereconfig() {
  local ns=e2e-runtimereconfig
  kubectl create ns "$ns" >/dev/null
  kubectl -n "$ns" apply -f - >/dev/null <<'YAML'
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLCluster
metadata: {name: pxc}
spec:
  replicas: 1
  persistence: {enabled: false}
  protocols: {mysql: {enabled: true, port: 6033}, pgsql: {enabled: false}}
  variables:
    mysql:
      mysql-max_connections: "700"
YAML
  kubectl -n "$ns" wait --for=condition=Ready pod/pxc-0 --timeout=120s >/dev/null

  local radmin out restarts0 annot0 varshash0
  radmin="$(radmin_pw "$ns" pxc)"

  # Assert initial runtime value
  out="$(admin_query "$ns" pxc "$radmin" \
    "SELECT variable_value FROM runtime_global_variables WHERE variable_name='mysql-max_connections'")"
  [[ "$out" == "700" ]] || { fail "initial runtime mysql-max_connections='$out', want 700"; dump_ns "$ns"; return 1; }
  log "runtimereconfig: initial runtime mysql-max_connections=700"

  # Record initial state
  restarts0="$(kubectl -n "$ns" get pod pxc-0 -o jsonpath='{.status.containerStatuses[0].restartCount}')"
  annot0="$(kubectl -n "$ns" get pod pxc-0 -o jsonpath='{.metadata.annotations.proxysql\.com/cnf-checksum}')"
  varshash0="$(kubectl -n "$ns" get statefulset pxc -o jsonpath='{.metadata.annotations.proxysql\.com/vars-applied-hash}')"
  log "runtimereconfig: recorded initial restartCount=$restarts0, annotation=$annot0, vars-applied-hash=$varshash0"

  # Patch to 701 (runtime-settable variable, no restart expected)
  kubectl -n "$ns" patch proxysqlcluster pxc --type=json \
    -p='[{"op":"replace","path":"/spec/variables/mysql/mysql-max_connections","value":"701"}]' >/dev/null

  # Poll until runtime is synced
  for _ in $(seq 1 15); do
    out="$(admin_query "$ns" pxc "$radmin" \
      "SELECT variable_value FROM runtime_global_variables WHERE variable_name='mysql-max_connections'")"
    [[ "$out" == "701" ]] && break
    sleep 4
  done
  [[ "$out" == "701" ]] || { fail "runtime mysql-max_connections='$out', want 701"; dump_ns "$ns"; return 1; }
  log "runtimereconfig: runtime mysql-max_connections updated to 701"

  # Assert no restart occurred
  local restarts_now
  restarts_now="$(kubectl -n "$ns" get pod pxc-0 -o jsonpath='{.status.containerStatuses[0].restartCount}')"
  [[ "$restarts_now" == "$restarts0" ]] || { fail "pod restarted (was $restarts0, now $restarts_now)"; dump_ns "$ns"; return 1; }
  log "runtimereconfig: pod did not restart (restartCount=$restarts0)"

  # Assert annotation unchanged
  local annot_now
  annot_now="$(kubectl -n "$ns" get pod pxc-0 -o jsonpath='{.metadata.annotations.proxysql\.com/cnf-checksum}')"
  [[ "$annot_now" == "$annot0" ]] || { fail "pod annotation changed (was $annot0, now $annot_now)"; dump_ns "$ns"; return 1; }
  log "runtimereconfig: pod annotation unchanged"

  # Assert the runtime-apply path was taken: the STS object-level
  # vars-applied-hash annotation advanced (the operator records each runtime
  # push there), which together with the unchanged restartCount and unchanged
  # pod cnf-checksum proves runtime apply without a restart. (The Progressing
  # condition's "RuntimeApplied: ..." message is transient — overwritten to
  # Steady by the follow-up reconcile — so it is not asserted here.)
  local varshash_now
  varshash_now="$(kubectl -n "$ns" get statefulset pxc -o jsonpath='{.metadata.annotations.proxysql\.com/vars-applied-hash}')"
  [[ "$varshash_now" != "$varshash0" ]] || { fail "vars-applied-hash did not change (still '$varshash0') — runtime apply path not taken"; dump_ns "$ns"; return 1; }
  log "runtimereconfig: STS vars-applied-hash advanced ($varshash0 -> $varshash_now) — runtime apply confirmed"

  # Structural change: patch spec.protocols.mysql.port to 6034 (triggers rollout)
  kubectl -n "$ns" patch proxysqlcluster pxc --type=json \
    -p='[{"op":"replace","path":"/spec/protocols/mysql/port","value":6034}]' >/dev/null

  # Wait for pod to be recreated (annotation should change)
  for _ in $(seq 1 30); do
    annot_now="$(kubectl -n "$ns" get pod pxc-0 -o jsonpath='{.metadata.annotations.proxysql\.com/cnf-checksum}' 2>/dev/null || true)"
    [[ "$annot_now" != "$annot0" && -n "$annot_now" ]] && break
    sleep 2
  done
  [[ "$annot_now" != "$annot0" ]] || { fail "pod was not rolled out (annotation still $annot0)"; dump_ns "$ns"; return 1; }
  log "runtimereconfig: pod rolled out (annotation changed)"

  # Wait for cluster to become Ready again
  kubectl -n "$ns" wait --for=condition=Ready pod/pxc-0 --timeout=120s >/dev/null || { dump_ns "$ns"; return 1; }
  log "runtimereconfig: cluster returned to Ready after structural change"
}
