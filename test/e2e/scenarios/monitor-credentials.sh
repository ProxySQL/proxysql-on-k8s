#!/usr/bin/env bash
# Scenario: external and operator-managed auth Secrets control the monitor
# account on both protocols, and Secret rotation reaches running pods.

scenario_monitor_credentials() {
  local ns=e2e-monitor-credentials
  kubectl create ns "$ns" >/dev/null
  kubectl -n "$ns" create secret generic pxc-auth \
    --from-literal=admin-password=demo-admin \
    --from-literal=radmin-password=demo-radmin \
    --from-literal=monitor-password=demo-monitor-old >/dev/null
  kubectl -n "$ns" apply -f - >/dev/null <<'YAML'
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLCluster
metadata: {name: pxc}
spec:
  replicas: 1
  auth: {secretName: pxc-auth}
  persistence: {enabled: false}
  protocols: {mysql: {enabled: true}, pgsql: {enabled: true}}
YAML
  wait_pod_ready "$ns" pxc-0 || { fail "monitor credentials: pxc-0 not Ready"; dump_ns "$ns"; return 1; }

  local count uid_before uid_after hash_before hash_after reason managed_radmin
  # kubectl run can return the same single-row result twice through attach
  # and logs. The query returns one count, so use its final output line.
  count="$(admin_query "$ns" pxc demo-radmin \
    "SELECT COUNT(*) FROM runtime_global_variables WHERE variable_name IN ('mysql-monitor_username','pgsql-monitor_username') AND variable_value='monitor'" | tail -n 1)"
  [[ "$count" == "2" ]] || { fail "monitor credentials: legacy username default reached $count/2 protocols"; dump_ns "$ns"; return 1; }
  uid_before="$(kubectl -n "$ns" get pod pxc-0 -o jsonpath='{.metadata.uid}')"
  hash_before="$(kubectl -n "$ns" get statefulset pxc -o jsonpath='{.metadata.annotations.proxysql\.com/vars-applied-hash}')"

  # Change both monitor values in the existing auth Secret. No CR edit is
  # made: the Secret watch must enqueue the cluster.
  kubectl -n "$ns" patch secret pxc-auth --type=merge \
    -p "{\"data\":{\"monitor_username\":\"$(printf %s monitor_ro | base64 -w0)\",\"monitor-password\":\"$(printf %s demo-monitor-new | base64 -w0)\"}}" >/dev/null
  for _ in $(seq 1 20); do
    count="$(admin_query "$ns" pxc demo-radmin \
      "SELECT COUNT(*) FROM runtime_global_variables WHERE variable_name IN ('mysql-monitor_username','pgsql-monitor_username') AND variable_value='monitor_ro'" | tail -n 1)"
    [[ "$count" == "2" ]] && break
    sleep 3
  done
  [[ "$count" == "2" ]] || { fail "monitor credentials: custom username reached $count/2 protocols"; dump_ns "$ns"; return 1; }
  count="$(admin_query "$ns" pxc demo-radmin \
    "SELECT COUNT(*) FROM runtime_global_variables WHERE variable_name IN ('mysql-monitor_password','pgsql-monitor_password') AND variable_value='demo-monitor-new'" | tail -n 1)"
  [[ "$count" == "2" ]] || { fail "monitor credentials: rotated password reached $count/2 protocols"; dump_ns "$ns"; return 1; }
  uid_after="$(kubectl -n "$ns" get pod pxc-0 -o jsonpath='{.metadata.uid}')"
  hash_after="$(kubectl -n "$ns" get statefulset pxc -o jsonpath='{.metadata.annotations.proxysql\.com/vars-applied-hash}')"
  [[ "$uid_after" == "$uid_before" && "$hash_after" != "$hash_before" ]] || {
    fail "monitor credentials: expected runtime apply without a pod replacement"; dump_ns "$ns"; return 1;
  }

  # An operator-managed auth Secret uses the owned-Secret watch instead of
  # spec.auth.secretName. Rotate it without changing the cluster resource.
  kubectl -n "$ns" apply -f - >/dev/null <<'YAML'
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLCluster
metadata: {name: managed}
spec:
  replicas: 1
  persistence: {enabled: false}
  protocols: {mysql: {enabled: true}, pgsql: {enabled: true}}
YAML
  wait_pod_ready "$ns" managed-0 || { fail "monitor credentials: managed-0 not Ready"; dump_ns "$ns"; return 1; }
  managed_radmin="$(kubectl -n "$ns" get secret managed -o jsonpath='{.data.radmin-password}' | base64 -d)"
  [[ -n "$managed_radmin" ]] || { fail "monitor credentials: managed radmin password missing"; dump_ns "$ns"; return 1; }
  count="$(admin_query "$ns" managed "$managed_radmin" \
    "SELECT COUNT(*) FROM runtime_global_variables WHERE variable_name IN ('mysql-monitor_username','pgsql-monitor_username') AND variable_value='monitor'" | tail -n 1)"
  [[ "$count" == "2" ]] || { fail "monitor credentials: managed default reached $count/2 protocols"; dump_ns "$ns"; return 1; }

  kubectl -n "$ns" patch secret managed --type=merge \
    -p "{\"data\":{\"monitor_username\":\"$(printf %s managed_ro | base64 -w0)\",\"monitor-password\":\"$(printf %s managed-monitor-new | base64 -w0)\"}}" >/dev/null
  for _ in $(seq 1 20); do
    count="$(admin_query "$ns" managed "$managed_radmin" \
      "SELECT COUNT(*) FROM runtime_global_variables WHERE (variable_name IN ('mysql-monitor_username','pgsql-monitor_username') AND variable_value='managed_ro') OR (variable_name IN ('mysql-monitor_password','pgsql-monitor_password') AND variable_value='managed-monitor-new')" | tail -n 1)"
    [[ "$count" == "4" ]] && break
    sleep 3
  done
  [[ "$count" == "4" ]] || { fail "monitor credentials: managed rotation reached $count/4 values"; dump_ns "$ns"; return 1; }

  kubectl -n "$ns" patch secret pxc-auth --type=merge \
    -p '{"data":{"monitor_username":""}}' >/dev/null
  reason=""
  for _ in $(seq 1 20); do
    reason="$(kubectl -n "$ns" get proxysqlcluster pxc -o jsonpath='{.status.conditions[?(@.type=="Degraded")].reason}')"
    [[ "$reason" == "AuthSecretError" ]] && break
    sleep 3
  done
  [[ "$reason" == "AuthSecretError" ]] || { fail "monitor credentials: empty username did not set AuthSecretError"; dump_ns "$ns"; return 1; }

  kubectl delete ns "$ns" --wait=false >/dev/null
}
