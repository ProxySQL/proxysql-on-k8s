#!/usr/bin/env bash
# ProxySQL operator end-to-end suite.
#
# Brings up ONE kind cluster, installs the operator once, then runs each
# scenario in its own namespace and aggregates pass/fail. This is the entry
# point used by CI (.github/workflows/ci.yaml, the `e2e` job).
#
# Prerequisites on PATH: kind, kubectl, helm, docker (buildx).
# Env overrides: see lib.sh (KIND_CLUSTER, IMG, SKIP_BUILD, KEEP_CLUSTER, ...).

set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e/lib.sh
source "$HERE/lib.sh"
for s in "$HERE"/scenarios/*.sh; do
  # shellcheck disable=SC1090
  source "$s"
done

need kind; need kubectl; need helm; need docker

cleanup() {
  if [[ "${KEEP_CLUSTER:-0}" == "1" ]]; then
    log "KEEP_CLUSTER=1 — leaving kind cluster '$KIND_CLUSTER' running"
    return
  fi
  log "deleting kind cluster '$KIND_CLUSTER'"
  kind delete cluster --name "$KIND_CLUSTER" >/dev/null 2>&1 || true
}
trap cleanup EXIT

ensure_cluster
build_and_load_image
install_operator

# Scenario functions are named scenario_<name>; list in intended run order.
SCENARIOS=(scenario_mysql scenario_postgres scenario_multireplica scenario_drift scenario_sqlstatements scenario_runtimereconfig scenario_psa scenario_delete scenario_rotate scenario_platform scenario_logging)

declare -a PASSED=() FAILED=()
for fn in "${SCENARIOS[@]}"; do
  name="${fn#scenario_}"
  log "════════ scenario: $name ════════"
  if "$fn"; then
    ok "scenario '$name' passed"
    PASSED+=("$name")
  else
    fail "scenario '$name' FAILED" || true
    FAILED+=("$name")
  fi
done

echo "" >&2
log "════════ summary ════════"
log "passed: ${PASSED[*]:-none}"
if ((${#FAILED[@]})); then
  fail "failed: ${FAILED[*]}" || true
  exit 1
fi
ok "all ${#PASSED[@]} scenarios passed"
