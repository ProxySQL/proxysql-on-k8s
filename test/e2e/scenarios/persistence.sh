#!/usr/bin/env bash
# Scenario: persistence + --reload semantics (issue #50).
# Pins the cnf-over-proxysql.db merge behavior on a PERSISTENCE-ENABLED
# cluster (every other scenario disables persistence). The container runs
# `proxysql -f -c ... --reload`, which merges the bootstrap cnf over the
# persisted proxysql.db on every start:
#  1. First boot seeds from the cnf; a pod restart onto the existing PVC
#     keeps the value (db and cnf agree).
#  2. A runtime-applied value edit (no restart) survives a later pod
#     restart: the runtime pass SAVEd it to disk AND the updated cnf merges
#     the same value over the db (crash consistency).
#  3. A variable key ADDED to spec.variables takes the structural-restart
#     path, and after the rollout --reload merges the new cnf key over the
#     existing proxysql.db — the exact gap #50 fixes (without --reload the
#     db won and the new key silently never took effect).

# _persistence_wait_pod_ready NS POD TIMEOUT_LOOPS — wait for a pod to be
# Ready, tolerating the not-found window right after a pod delete while the
# StatefulSet recreates it.
_persistence_wait_pod_ready() {
  local ns="$1" pod="$2" loops="${3:-30}"
  for _ in $(seq 1 "$loops"); do
    kubectl -n "$ns" wait --for=condition=Ready "pod/$pod" --timeout=10s >/dev/null 2>&1 && return 0
    sleep 2
  done
  fail "pod $ns/$pod did not become Ready"
}

scenario_persistence() {
  local ns=e2e-persistence
  kubectl create ns "$ns" >/dev/null
  kubectl -n "$ns" apply -f - >/dev/null <<'YAML'
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLCluster
metadata: {name: pxc}
spec:
  replicas: 1
  persistence: {enabled: true}
  protocols: {mysql: {enabled: true, port: 6033}, pgsql: {enabled: false}}
  variables:
    mysql:
      mysql-max_connections: "600"
YAML
  # First boot waits on PVC provisioning too — allow longer than usual.
  _persistence_wait_pod_ready "$ns" pxc-0 60 || { dump_ns "$ns"; return 1; }

  local radmin out
  radmin="$(radmin_pw "$ns" pxc)"

  # (a) first boot seeds from the cnf
  out="$(admin_query "$ns" pxc "$radmin" \
    "SELECT variable_value FROM runtime_global_variables WHERE variable_name='mysql-max_connections'")"
  [[ "$out" == "600" ]] || { fail "initial runtime mysql-max_connections='$out', want 600"; dump_ns "$ns"; return 1; }
  log "persistence: initial runtime mysql-max_connections=600"

  # (b) pod restart onto the existing PVC: proxysql.db and cnf agree, the
  # value must survive.
  kubectl -n "$ns" delete pod pxc-0 --wait=true >/dev/null
  _persistence_wait_pod_ready "$ns" pxc-0 || { dump_ns "$ns"; return 1; }
  out="$(admin_query "$ns" pxc "$radmin" \
    "SELECT variable_value FROM runtime_global_variables WHERE variable_name='mysql-max_connections'")"
  [[ "$out" == "600" ]] || { fail "after pod restart mysql-max_connections='$out', want 600"; dump_ns "$ns"; return 1; }
  log "persistence: value survived pod restart onto existing PVC (600)"

  # Record post-restart state: the new pod starts at restartCount=0 with the
  # current cnf-checksum; step (c) must change neither.
  local restarts0 annot0
  restarts0="$(kubectl -n "$ns" get pod pxc-0 -o jsonpath='{.status.containerStatuses[0].restartCount}')"
  annot0="$(kubectl -n "$ns" get pod pxc-0 -o jsonpath='{.metadata.annotations.proxysql\.com/cnf-checksum}')"
  log "persistence: recorded restartCount=$restarts0, cnf-checksum=$annot0"

  # (c) value edit of an existing key: runtime-apply path, no restart.
  kubectl -n "$ns" patch proxysqlcluster pxc --type=json \
    -p='[{"op":"replace","path":"/spec/variables/mysql/mysql-max_connections","value":"601"}]' >/dev/null
  for _ in $(seq 1 15); do
    out="$(admin_query "$ns" pxc "$radmin" \
      "SELECT variable_value FROM runtime_global_variables WHERE variable_name='mysql-max_connections'")"
    [[ "$out" == "601" ]] && break
    sleep 4
  done
  [[ "$out" == "601" ]] || { fail "runtime mysql-max_connections='$out', want 601"; dump_ns "$ns"; return 1; }
  local restarts_now annot_now
  restarts_now="$(kubectl -n "$ns" get pod pxc-0 -o jsonpath='{.status.containerStatuses[0].restartCount}')"
  [[ "$restarts_now" == "$restarts0" ]] || { fail "runtime apply restarted the pod (restartCount $restarts0 -> $restarts_now)"; dump_ns "$ns"; return 1; }
  annot_now="$(kubectl -n "$ns" get pod pxc-0 -o jsonpath='{.metadata.annotations.proxysql\.com/cnf-checksum}')"
  [[ "$annot_now" == "$annot0" ]] || { fail "runtime apply rolled the pod (cnf-checksum $annot0 -> $annot_now)"; dump_ns "$ns"; return 1; }
  log "persistence: runtime-applied 601 without restart"

  # ... and the runtime-applied value survives a pod restart: SAVE ... TO
  # DISK put 601 in proxysql.db, and the updated cnf merges the same 601
  # over it via --reload. db and cnf agree — crash consistency.
  kubectl -n "$ns" delete pod pxc-0 --wait=true >/dev/null
  _persistence_wait_pod_ready "$ns" pxc-0 || { dump_ns "$ns"; return 1; }
  out="$(admin_query "$ns" pxc "$radmin" \
    "SELECT variable_value FROM runtime_global_variables WHERE variable_name='mysql-max_connections'")"
  [[ "$out" == "601" ]] || { fail "after restart runtime mysql-max_connections='$out', want 601"; dump_ns "$ns"; return 1; }
  log "persistence: runtime-applied value survived pod restart (601)"
  annot0="$(kubectl -n "$ns" get pod pxc-0 -o jsonpath='{.metadata.annotations.proxysql\.com/cnf-checksum}')"

  # (d) ADD a new variable key: structural-restart path; after the rollout
  # --reload must merge the new cnf key over the existing proxysql.db. This
  # is the #50 gap: without --reload the db wins and the key never lands.
  kubectl -n "$ns" patch proxysqlcluster pxc --type=json \
    -p='[{"op":"add","path":"/spec/variables/mysql/mysql-max_allowed_packet","value":"33554432"}]' >/dev/null
  for _ in $(seq 1 30); do
    annot_now="$(kubectl -n "$ns" get pod pxc-0 -o jsonpath='{.metadata.annotations.proxysql\.com/cnf-checksum}' 2>/dev/null || true)"
    [[ -n "$annot_now" && "$annot_now" != "$annot0" ]] && break
    sleep 2
  done
  [[ "$annot_now" != "$annot0" ]] || { fail "added key did not roll the pod (cnf-checksum still $annot0)"; dump_ns "$ns"; return 1; }
  log "persistence: added key rolled the pod (cnf-checksum changed)"
  _persistence_wait_pod_ready "$ns" pxc-0 || { dump_ns "$ns"; return 1; }
  out="$(admin_query "$ns" pxc "$radmin" \
    "SELECT variable_value FROM runtime_global_variables WHERE variable_name='mysql-max_allowed_packet'")"
  [[ "$out" == "33554432" ]] || { fail "added key runtime mysql-max_allowed_packet='$out', want 33554432 (cnf did not merge over proxysql.db)"; dump_ns "$ns"; return 1; }
  # The pre-existing value must be intact after the merge.
  out="$(admin_query "$ns" pxc "$radmin" \
    "SELECT variable_value FROM runtime_global_variables WHERE variable_name='mysql-max_connections'")"
  [[ "$out" == "601" ]] || { fail "after added-key rollout mysql-max_connections='$out', want 601"; dump_ns "$ns"; return 1; }
  log "persistence: new cnf key merged over existing proxysql.db (33554432), existing value intact (601)"

  # --- removal caveat: a key removed from the cnf keeps its db value ---
  # --reload never deletes db-only entries (INSERT OR REPLACE, no DELETE),
  # so removing a spec.variables key rolls the pod (structural: line gone)
  # but the runtime value survives from proxysql.db. Pin that documented
  # caveat so an upstream behavior change is caught here.
  annot0="$annot_now"
  kubectl -n "$ns" patch proxysqlcluster pxc --type=json \
    -p='[{"op":"remove","path":"/spec/variables/mysql/mysql-max_allowed_packet"}]' >/dev/null
  for _ in $(seq 1 30); do
    annot_now="$(kubectl -n "$ns" get pod pxc-0 -o jsonpath='{.metadata.annotations.proxysql\.com/cnf-checksum}' 2>/dev/null)"
    [[ -n "$annot_now" && "$annot_now" != "$annot0" ]] && break
    sleep 2
  done
  [[ "$annot_now" != "$annot0" ]] || { fail "removed key did not roll the pod"; dump_ns "$ns"; return 1; }
  _persistence_wait_pod_ready "$ns" pxc-0 || { dump_ns "$ns"; return 1; }
  out="$(admin_query "$ns" pxc "$radmin" \
    "SELECT variable_value FROM runtime_global_variables WHERE variable_name='mysql-max_allowed_packet'")"
  [[ "$out" == "33554432" ]] || { fail "removed key expected to KEEP db value 33554432 (documented caveat), got '$out'"; dump_ns "$ns"; return 1; }
  log "persistence: removed key kept its proxysql.db value (documented removal caveat pinned)"
}
