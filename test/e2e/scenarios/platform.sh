#!/usr/bin/env bash
# Scenario: the "platform integration" surface — a control plane pre-creates a
# username/password admin Secret, enables the web UI, then polls phase and
# endpoints instead of inspecting Services/StatefulSets.

scenario_platform() {
  local ns=e2e-platform
  kubectl create ns "$ns" >/dev/null
  kubectl -n "$ns" create secret generic platform-admin \
    --from-literal=username=platform --from-literal=password=plat-secret >/dev/null
  kubectl -n "$ns" apply -f - >/dev/null <<'YAML'
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLCluster
metadata: {name: pxc}
spec:
  replicas: 1
  persistence: {enabled: false}
  auth: {secretName: platform-admin}
  protocols: {mysql: {enabled: true}, pgsql: {enabled: false}, web: {enabled: true}}
YAML
  wait_pod_ready "$ns" pxc-0 || { fail "pxc-0 not Ready"; dump_ns "$ns"; return 1; }

  local out _
  # Phase converges to Running once the pod is ready.
  for _ in $(seq 1 15); do
    out="$(kubectl -n "$ns" get proxysqlcluster pxc -o jsonpath='{.status.phase}')"
    [[ "$out" == "Running" ]] && break
    sleep 2
  done
  [[ "$out" == "Running" ]] || { fail "platform: phase='$out', want Running"; dump_ns "$ns"; return 1; }
  log "platform: phase=Running"

  out="$(kubectl -n "$ns" get proxysqlcluster pxc -o jsonpath='{.status.endpoints.mysql}')"
  [[ "$out" == "pxc.${ns}.svc:6033" ]] || { fail "platform: endpoints.mysql='$out'"; dump_ns "$ns"; return 1; }
  out="$(kubectl -n "$ns" get proxysqlcluster pxc -o jsonpath='{.status.endpoints.web}')"
  [[ "$out" == "pxc.${ns}.svc:6080" ]] || { fail "platform: endpoints.web='$out'"; dump_ns "$ns"; return 1; }
  log "platform: endpoints published (mysql, web)"

  # The platform's own username works on the admin port (remote). Retry a few
  # times to absorb transient one-shot client-pod flakes, like admin_query.
  for _ in 1 2 3 4 5; do
    out="$(kubectl -n "$ns" run "e2e-admincheck-$RANDOM" --rm -i --restart=Never --image="$MYSQL_IMAGE" \
      --env=MYSQL_PWD=plat-secret --command -- \
      mysql -h pxc -P6032 -uplatform -N -B -e "SELECT 1" 2>/dev/null | _strip_noise || true)"
    [[ -n "$out" ]] && break
    sleep 4
  done
  [[ "$out" == "1" ]] || { fail "platform: admin login with username/password secret failed (got '$out')"; dump_ns "$ns"; return 1; }
  log "platform: username/password admin credential works remotely"

  # Web UI answers on 6080 (HTTPS, self-signed -> -k). Any real HTTP status
  # counts; curl reports 000 when the connection itself failed. The '\n' after
  # %{http_code} keeps kubectl's pod-cleanup notice off the same line.
  for _ in 1 2 3 4 5; do
    out="$(kubectl -n "$ns" run "e2e-webcheck-$RANDOM" --rm -i --restart=Never --image=curlimages/curl:8.7.1 --command -- \
      curl -ksS -o /dev/null -w '%{http_code}\n' "https://pxc.${ns}.svc:6080/" 2>/dev/null | _strip_noise || true)"
    [[ "$out" =~ ^[1-5][0-9]{2}$ ]] && break
    sleep 4
  done
  [[ "$out" =~ ^[1-5][0-9]{2}$ ]] || { fail "platform: web UI not answering on 6080 (got '$out')"; dump_ns "$ns"; return 1; }
  log "platform: web UI answering on 6080 (HTTP $out)"

  kubectl delete ns "$ns" --wait=false >/dev/null
}
