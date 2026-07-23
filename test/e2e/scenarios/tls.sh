#!/usr/bin/env bash
# Scenario: TLS management — tier-3 self-signed issuance, a real client
# connect through the DATA plane (6033) with the served leaf verified
# against the operator-minted CA, and restart-free rotation.
#
#  1. spec.tls: {enabled: true} with no secretName/issuerRef selects tier 3:
#     the operator mints a CA into Secret <cluster>-tls-ca and a serving
#     leaf into <cluster>-tls, signed by that CA.
#  2. A client, from an in-cluster debug pod, completes a TLS handshake on
#     6033 (the data plane, not just the 6032 admin port) trusting ONLY the
#     operator-minted CA — VERIFY_CA semantics (chain trust, no hostname
#     check), proven with `openssl s_client -CAfile` reporting
#     "Verify return code: 0 (ok)". A real authenticated SELECT 1 is then
#     run over TLS with the mysql client to prove the DATA path actually
#     works end to end, not just the handshake.
#  3. Deleting the <cluster>-tls Secret forces re-issuance of a new leaf
#     from the SAME CA (the CA Secret is untouched). The rotation engine
#     applies it via `PROXYSQL RELOAD TLS` + handshake verification —
#     restart-free. This is proven by polling the served leaf's SHA-256
#     fingerprint (captured on 6033) until it changes, then asserting the
#     StatefulSet's proxysql.com/tls-applied-hash marker advanced AND every
#     pod's restartCount is unchanged.
#
# Client note: the proxysql/proxysql:3.0 image ships the MariaDB client,
# not the Oracle mysql client — there is no --ssl-mode=VERIFY_CA flag.
# MariaDB's --ssl-verify-server-cert conflates chain AND hostname checks
# (disabling it disables BOTH, verified empirically — it is not a VERIFY_CA
# equivalent). So the literal VERIFY_CA proof is done with
# `openssl s_client -CAfile` (which never checks hostname unless
# -verify_hostname is passed) on 6033; the mysql client separately proves a
# real authenticated query over TLS, connecting via the short Service name
# "pxc" which is itself a SAN, so its default (chain+hostname) verification
# also passes — a strictly stronger check, never a weaker one.

# _tls_probe NS -> two lines: "OK"/"FAIL" (openssl s_client VERIFY_CA
# handshake result) then the served leaf's SHA-256 fingerprint, captured on
# the DATA plane port 6033 via the tlsdebug pod's mounted CA (/tls/ca.crt).
_tls_probe() {
  local ns="$1"
  kubectl -n "$ns" exec tlsdebug -- sh -c '
    set -e
    openssl s_client -starttls mysql -connect pxc:6033 -CAfile /tls/ca.crt </dev/null >/tmp/sess.txt 2>/dev/null || true
    if grep -q "Verify return code: 0 (ok)" /tmp/sess.txt; then echo OK; else echo FAIL; fi
    openssl x509 -noout -fingerprint -sha256 -in /tmp/sess.txt 2>/dev/null || echo ""
  ' 2>/dev/null
}

scenario_tls() {
  local ns=e2e-tls
  kubectl create ns "$ns" >/dev/null
  kubectl -n "$ns" apply -f - >/dev/null <<'YAML'
apiVersion: v1
kind: Secret
metadata: {name: appcreds}
stringData: {password: "appsecret"}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: mysql}
spec:
  replicas: 1
  selector: {matchLabels: {app: mysql}}
  template:
    metadata: {labels: {app: mysql}}
    spec:
      containers:
        - name: mysql
          image: mysql:8.0
          env:
            - {name: MYSQL_ROOT_PASSWORD, value: rootsecret}
            - {name: MYSQL_DATABASE, value: appdb}
            - {name: MYSQL_USER, value: app}
            - {name: MYSQL_PASSWORD, value: appsecret}
          ports: [{containerPort: 3306}]
          readinessProbe:
            exec: {command: ["mysqladmin","ping","-h","127.0.0.1","-uroot","-prootsecret"]}
            initialDelaySeconds: 8
            periodSeconds: 4
---
apiVersion: v1
kind: Service
metadata: {name: mysql}
spec:
  selector: {app: mysql}
  ports: [{port: 3306, targetPort: 3306}]
YAML
  kubectl -n "$ns" rollout status deploy/mysql --timeout=180s >/dev/null

  kubectl -n "$ns" apply -f - >/dev/null <<YAML
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLCluster
metadata: {name: pxc}
spec:
  replicas: 1
  persistence: {enabled: false}
  protocols: {mysql: {enabled: true}, pgsql: {enabled: false}}
  tls:
    enabled: true
---
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLConfig
metadata: {name: pxcfg}
spec:
  clusterRef: {name: pxc}
  mysqlServers:
    - {hostgroup: 0, hostname: mysql.$ns.svc.cluster.local, port: 3306}
  mysqlUsers:
    - {username: app, defaultHostgroup: 0, defaultSchema: appdb, passwordSecretRef: {name: appcreds, key: password}}
  # Disable the monitor module: the backend has no "monitor" user with the
  # operator-minted password, and we don't want health checks shunning the
  # (perfectly reachable) server during the test.
  mysqlVariables:
    mysql-monitor_enabled: "false"
YAML
  wait_pod_ready "$ns" pxc-0 || { fail "pxc-0 not Ready"; dump_ns "$ns"; return 1; }
  wait_config_synced "$ns" pxcfg 1 120 || { dump_ns "$ns"; return 1; }

  # --- tier-3 self-signed issuance: CA + serving Secret exist with all
  # three required keys populated ---
  local key out
  for key in tls.crt tls.key ca.crt; do
    out="$(kubectl -n "$ns" get secret pxc-tls-ca -o jsonpath="{.data.$key}" 2>/dev/null || true)"
    [[ -n "$out" ]] || { fail "tls: Secret pxc-tls-ca missing/empty key $key"; dump_ns "$ns"; return 1; }
    out="$(kubectl -n "$ns" get secret pxc-tls -o jsonpath="{.data.$key}" 2>/dev/null || true)"
    [[ -n "$out" ]] || { fail "tls: Secret pxc-tls missing/empty key $key"; dump_ns "$ns"; return 1; }
  done
  log "tls: tier-3 self-signed CA (pxc-tls-ca) and serving cert (pxc-tls) both populated"

  # --- debug pod: proxysql/proxysql:3.0 (ships both openssl and the mysql
  # client) with the CA mounted from pxc-tls-ca, PSA-restricted securityContext
  # matching the operator's own pod defaults ---
  kubectl -n "$ns" apply -f - >/dev/null <<'YAML'
apiVersion: v1
kind: Pod
metadata: {name: tlsdebug, labels: {app: tlsdebug}}
spec:
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    runAsUser: 999
    runAsGroup: 999
    fsGroup: 999
    seccompProfile: {type: RuntimeDefault}
  containers:
    - name: debug
      image: proxysql/proxysql:3.0
      command: ["sleep", "3600"]
      env: [{name: HOME, value: /tmp}]
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities: {drop: ["ALL"]}
      volumeMounts:
        - {name: ca, mountPath: /tls, readOnly: true}
        - {name: tmp, mountPath: /tmp}
  volumes:
    - name: ca
      secret: {secretName: pxc-tls-ca, items: [{key: ca.crt, path: ca.crt}]}
    - name: tmp
      emptyDir: {}
YAML
  wait_pod_ready "$ns" tlsdebug || { fail "tlsdebug pod not Ready"; dump_ns "$ns"; return 1; }
  log "tls: tlsdebug client pod Ready (CA from pxc-tls-ca mounted at /tls/ca.crt)"

  # --- verified client connect through the DATA plane (6033), VERIFY_CA
  # semantics: chain trust against the operator CA only, no hostname check ---
  local probe0 verify0 fp0
  for _ in 1 2 3 4 5; do
    probe0="$(_tls_probe "$ns")"
    verify0="$(printf '%s\n' "$probe0" | sed -n '1p')"
    fp0="$(printf '%s\n' "$probe0" | sed -n '2p')"
    [[ "$verify0" == "OK" && -n "$fp0" ]] && break
    sleep 4
  done
  [[ "$verify0" == "OK" ]] || { fail "tls: VERIFY_CA handshake on :6033 failed against pxc-tls-ca (got '$verify0')"; dump_ns "$ns"; return 1; }
  [[ -n "$fp0" ]] || { fail "tls: could not capture served leaf fingerprint on :6033"; dump_ns "$ns"; return 1; }
  log "tls: VERIFY_CA handshake on :6033 verified against pxc-tls-ca ($fp0)"

  # --- real authenticated query over TLS, proving the DATA path (not just
  # the handshake). Host "pxc" is itself a SAN, so the mysql client's
  # default (chain+hostname) verification also passes. ---
  for _ in 1 2 3 4 5; do
    out="$(kubectl -n "$ns" exec tlsdebug -- sh -c \
      'MYSQL_PWD=appsecret mysql -h pxc -P6033 -uapp --ssl-ca=/tls/ca.crt -N -B -e "SELECT 1"' 2>/dev/null || true)"
    [[ -n "$out" ]] && break
    sleep 4
  done
  [[ "$out" == "1" ]] || { fail "tls: SELECT 1 over verified TLS through :6033 failed (got '$out')"; dump_ns "$ns"; return 1; }
  log "tls: SELECT 1 over verified TLS through :6033 succeeded (data path confirmed)"

  # --- restart-free rotation: delete the serving Secret so the operator
  # re-issues a new leaf from the SAME (untouched) CA ---
  local hash0 restarts0
  hash0="$(kubectl -n "$ns" get statefulset pxc -o jsonpath='{.metadata.annotations.proxysql\.com/tls-applied-hash}')"
  restarts0="$(kubectl -n "$ns" get pod pxc-0 -o jsonpath='{.status.containerStatuses[0].restartCount}')"
  restarts0="${restarts0:-0}"
  log "tls: recorded pre-rotation tls-applied-hash=$hash0, pxc-0 restartCount=$restarts0"

  kubectl -n "$ns" delete secret pxc-tls >/dev/null
  log "tls: deleted Secret pxc-tls — expecting re-issuance from pxc-tls-ca"

  # Poll for the served fingerprint to change. The rotation engine's
  # restart-fallback window is 2 minutes, so give it comfortably longer
  # than that before failing (54 x 5s = 270s).
  local probe_now verify_now fp_now rotated=0 i
  for i in $(seq 1 54); do
    probe_now="$(_tls_probe "$ns")"
    verify_now="$(printf '%s\n' "$probe_now" | sed -n '1p')"
    fp_now="$(printf '%s\n' "$probe_now" | sed -n '2p')"
    if [[ "$verify_now" == "OK" && -n "$fp_now" && "$fp_now" != "$fp0" ]]; then
      rotated=1
      break
    fi
    sleep 5
  done
  ((rotated)) || { fail "tls: served fingerprint on :6033 unchanged after ~270s (still '$fp0') — rotation did not land"; dump_ns "$ns"; return 1; }
  log "tls: served leaf fingerprint on :6033 changed after ~$((i*5))s ($fp0 -> $fp_now), still VERIFY_CA-valid against the SAME pxc-tls-ca"

  # --- the headline claim: no pod restart across the rotation ---
  local restarts_now
  restarts_now="$(kubectl -n "$ns" get pod pxc-0 -o jsonpath='{.status.containerStatuses[0].restartCount}')"
  restarts_now="${restarts_now:-0}"
  [[ "$restarts_now" == "$restarts0" ]] || { fail "tls: pxc-0 restarted across rotation (was $restarts0, now $restarts_now) — rotation should be restart-free"; dump_ns "$ns"; return 1; }
  log "tls: pxc-0 restartCount unchanged ($restarts0) — rotation was restart-free"

  # --- the StatefulSet's tls-applied-hash marker must have advanced ---
  local hash_now
  hash_now="$(kubectl -n "$ns" get statefulset pxc -o jsonpath='{.metadata.annotations.proxysql\.com/tls-applied-hash}')"
  [[ -n "$hash_now" && "$hash_now" != "$hash0" ]] || { fail "tls: STS tls-applied-hash did not advance (was '$hash0', now '$hash_now')"; dump_ns "$ns"; return 1; }
  log "tls: STS proxysql.com/tls-applied-hash advanced ($hash0 -> $hash_now)"

  # --- data path still works after rotation, with the rotated leaf ---
  for _ in 1 2 3 4 5; do
    out="$(kubectl -n "$ns" exec tlsdebug -- sh -c \
      'MYSQL_PWD=appsecret mysql -h pxc -P6033 -uapp --ssl-ca=/tls/ca.crt -N -B -e "SELECT 1"' 2>/dev/null || true)"
    [[ -n "$out" ]] && break
    sleep 4
  done
  [[ "$out" == "1" ]] || { fail "tls: SELECT 1 over TLS through :6033 failed after rotation (got '$out')"; dump_ns "$ns"; return 1; }
  log "tls: SELECT 1 over TLS through :6033 still succeeds after rotation"
}
