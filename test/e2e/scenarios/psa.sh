#!/usr/bin/env bash
# Scenario: Pod Security Standards "restricted" admission. Deploys a cluster
# into a namespace that enforces the restricted profile and confirms the
# operator-produced pods are admitted and reach Ready (no PSA violations).

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
YAML

  # If the pod template violated restricted, the StatefulSet controller would
  # emit FailedCreate "violates PodSecurity" instead of creating the pod.
  if ! kubectl -n "$ns" wait --for=condition=Ready pod/pxc-0 --timeout=120s >/dev/null 2>&1; then
    fail "pxc-0 not Ready under restricted PSA"
    kubectl -n "$ns" get events --sort-by=.lastTimestamp 2>&1 | grep -iE "violate|forbidden|FailedCreate|security" | tail -5 >&2 || true
    dump_ns "$ns"
    return 1
  fi
  # Belt-and-braces: assert no PSA violation events were recorded.
  if kubectl -n "$ns" get events 2>/dev/null | grep -qiE "violates PodSecurity|forbidden: violates"; then
    fail "PSA violation events present despite pod becoming Ready"
    return 1
  fi
  log "psa: pxc-0 admitted + Ready under restricted enforcement"
}
