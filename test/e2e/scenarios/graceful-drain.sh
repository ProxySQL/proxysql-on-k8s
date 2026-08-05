#!/usr/bin/env bash
# Scenario: graceful client-connection drain on scale-down (ProxySQL SaaS
# #196, AS-7). Pins the opt-in spec.gracefulShutdown behavior end-to-end on
# kind:
#  1. A 2-replica cluster with gracefulShutdown enabled renders the preStop
#     drain hook AND the raised terminationGracePeriodSeconds (drainTimeout
#     + 10s buffer) on the pod spec — the same contract TestGolden pins at
#     the unit level, checked here against a real API-server round-trip.
#  2. Scaling down to 1 replica must still converge cleanly within the grace
#     window: the drain hook (best-effort PROXYSQL PAUSE + bounded wait) must
#     not wedge termination of the removed pod.
# A live client-connection-not-reset assertion would need an in-cluster SQL
# client held open mid-scale-down; the unit tests (TestGolden +
# TestDrainPreStopCommand) already pin the rendered hook's exact PAUSE/wait
# script, so this scenario limits itself to the pod-spec contract + clean
# StatefulSet convergence per the task brief.

scenario_graceful_drain() {
  local ns=e2e-drain
  kubectl create ns "$ns" >/dev/null
  kubectl -n "$ns" apply -f - >/dev/null <<'YAML'
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLCluster
metadata: {name: pxc}
spec:
  replicas: 2
  persistence: {enabled: false}
  protocols: {mysql: {enabled: true}, pgsql: {enabled: false}}
  gracefulShutdown: {enabled: true, drainTimeoutSeconds: 15}
YAML
  kubectl -n "$ns" rollout status statefulset/pxc --timeout=180s >/dev/null
  wait_pod_ready "$ns" pxc-0 || { fail "pxc-0 not Ready"; dump_ns "$ns"; return 1; }
  wait_pod_ready "$ns" pxc-1 || { fail "pxc-1 not Ready"; dump_ns "$ns"; return 1; }
  log "graceful-drain: both pxc-0 and pxc-1 Ready"

  # --- pod-spec assertions on the highest-ordinal pod ---
  local hook tgps
  hook="$(kubectl -n "$ns" get pod pxc-1 -o jsonpath='{.spec.containers[0].lifecycle.preStop.exec.command}')"
  [[ -n "$hook" ]] || { fail "pxc-1 has no preStop hook on the proxysql container"; dump_ns "$ns"; return 1; }
  log "graceful-drain: pxc-1 preStop hook present ($hook)"

  tgps="$(kubectl -n "$ns" get pod pxc-1 -o jsonpath='{.spec.terminationGracePeriodSeconds}')"
  [[ "$tgps" == "25" ]] || { fail "pxc-1 terminationGracePeriodSeconds='$tgps', want 25 (drainTimeoutSeconds=15 + 10s buffer)"; dump_ns "$ns"; return 1; }
  log "graceful-drain: pxc-1 terminationGracePeriodSeconds=25 (15s drain + 10s buffer)"

  # --- scale down: the removed pod's drain hook must not wedge termination ---
  log "graceful-drain: scaling pxc from 2 to 1 replicas"
  kubectl -n "$ns" patch proxysqlcluster pxc --type=merge -p='{"spec":{"replicas":1}}' >/dev/null

  # The removed pod (highest ordinal) must actually terminate within the
  # grace window (terminationGracePeriodSeconds=25s, asserted above): if the
  # best-effort drain hook wedged, this wait times out instead of the pod
  # disappearing. --for=delete also succeeds immediately if it's already gone.
  kubectl -n "$ns" wait --for=delete pod/pxc-1 --timeout=40s >/dev/null 2>&1 \
    || { fail "pxc-1 was not deleted within the grace window (drain hook may have wedged termination)"; dump_ns "$ns"; return 1; }
  log "graceful-drain: pxc-1 terminated cleanly within the grace window"

  kubectl -n "$ns" rollout status statefulset/pxc --timeout=60s >/dev/null \
    || { fail "statefulset did not converge after scale-down"; dump_ns "$ns"; return 1; }
  local ready
  ready="$(kubectl -n "$ns" get statefulset pxc -o jsonpath='{.status.readyReplicas}')"
  [[ "$ready" == "1" ]] || { fail "statefulset readyReplicas='$ready', want 1 after scale-down"; dump_ns "$ns"; return 1; }
  log "graceful-drain: statefulset converged to 1 ready replica after scale-down"
}
