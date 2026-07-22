#!/usr/bin/env bash
# Scenario: Pod Security Standards "restricted" admission. Deploys a cluster
# into a namespace that enforces the restricted profile and confirms the
# operator-produced pods are admitted and reach Ready (no PSA violations).
# Includes the #28 networking knobs: tcpKeepalive sysctls (safe-sysctl set
# since K8s 1.29, so restricted PSA must admit them), Service annotations,
# and ClientIP session affinity.

scenario_psa() {
  local ns=e2e-psa
  kubectl create ns "$ns" >/dev/null
  kubectl label ns "$ns" \
    pod-security.kubernetes.io/enforce=restricted \
    pod-security.kubernetes.io/enforce-version=latest --overwrite >/dev/null
  log "psa: namespace enforces pod-security restricted"

  kubectl -n "$ns" apply -f - >/dev/null <<'YAML'
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLCluster
metadata: {name: pxc}
spec:
  replicas: 1
  persistence: {enabled: false}
  protocols: {mysql: {enabled: true}}
  service:
    annotations: {e2e.proxysql.com/lb: internal}
    sessionAffinityTimeoutSeconds: 300
  networking:
    tcpKeepalive: {time: 120, interval: 30, probes: 5}
YAML

  # If the pod template violated restricted (or the keepalive sysctls were
  # not on the node's safe-sysctl list), the StatefulSet controller would
  # emit FailedCreate "violates PodSecurity" / the kubelet would reject the
  # pod instead of it reaching Ready.
  if ! wait_pod_ready "$ns" pxc-0; then
    fail "pxc-0 not Ready under restricted PSA (tcpKeepalive sysctls set)"
    kubectl -n "$ns" get events --sort-by=.lastTimestamp 2>&1 | grep -iE "violate|forbidden|FailedCreate|sysctl|security" | tail -5 >&2 || true
    dump_ns "$ns"
    return 1
  fi
  # Belt-and-braces: assert no PSA violation events were recorded.
  if kubectl -n "$ns" get events 2>/dev/null | grep -qiE "violates PodSecurity|forbidden: violates"; then
    fail "PSA violation events present despite pod becoming Ready"
    return 1
  fi
  log "psa: pxc-0 admitted + Ready under restricted enforcement (keepalive sysctls applied)"

  # The pod must actually carry the sysctls.
  local sysctls
  sysctls="$(kubectl -n "$ns" get pod pxc-0 -o jsonpath='{.spec.securityContext.sysctls[*].name}')"
  for s in net.ipv4.tcp_keepalive_time net.ipv4.tcp_keepalive_intvl net.ipv4.tcp_keepalive_probes; do
    if [[ "$sysctls" != *"$s"* ]]; then
      fail "pod is missing sysctl $s (got: $sysctls)"
      return 1
    fi
  done
  log "psa: pod securityContext carries all three tcp_keepalive sysctls"

  # Regular Service: annotation + ClientIP session affinity.
  local ann affinity affinity_timeout
  ann="$(kubectl -n "$ns" get svc pxc -o jsonpath='{.metadata.annotations.e2e\.proxysql\.com/lb}')"
  if [[ "$ann" != "internal" ]]; then
    fail "service annotation e2e.proxysql.com/lb: got '$ann', want 'internal'"
    return 1
  fi
  affinity="$(kubectl -n "$ns" get svc pxc -o jsonpath='{.spec.sessionAffinity}')"
  affinity_timeout="$(kubectl -n "$ns" get svc pxc -o jsonpath='{.spec.sessionAffinityConfig.clientIP.timeoutSeconds}')"
  if [[ "$affinity" != "ClientIP" || "$affinity_timeout" != "300" ]]; then
    fail "service affinity: got '$affinity'/'$affinity_timeout', want ClientIP/300"
    return 1
  fi
  # The headless Service must stay untouched by spec.service.
  local headless_affinity
  headless_affinity="$(kubectl -n "$ns" get svc pxc-headless -o jsonpath='{.spec.sessionAffinity}')"
  if [[ "$headless_affinity" == "ClientIP" ]]; then
    fail "headless service must not inherit session affinity"
    return 1
  fi
  log "psa: service has LB annotation + ClientIP affinity (timeout 300); headless untouched"
}
