#!/usr/bin/env bash
# Scenario: logging sidecar (#29), stdout sink — enable spec.logging with
# queryLog, run a marker query through :6033, and assert the eventslog line
# reaches the fluent-bit container's stdout (kubectl logs -c fluent-bit).
# S3/HTTP sinks stay unit/golden-tested (no external infra in CI).

scenario_logging() {
  local ns=e2e-logging
  kubectl create ns "$ns" >/dev/null
  # Minimal MySQL backend + app user (same shape as scenario_mysql, trimmed).
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
  logging:
    enabled: true
    queryLog: true
    sinkType: stdout
---
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLConfig
metadata: {name: pxcfg}
spec:
  clusterRef: {name: pxc}
  mysqlServers:
    - {hostgroup: 0, hostname: mysql.$ns.svc.cluster.local, port: 3306}
  mysqlUsers:
    # defaultSchema: see scenario_mysql — the app user cannot open
    # information_schema as a handshake database.
    - {username: app, defaultHostgroup: 0, defaultSchema: appdb, passwordSecretRef: {name: appcreds, key: password}}
  mysqlQueryRules:
    - {ruleId: 100, active: true, matchDigest: "^SELECT", destinationHostgroup: 0, apply: true}
  # No "monitor" user exists on the backend; keep health checks from shunning it.
  mysqlVariables:
    mysql-monitor_enabled: "false"
YAML
  wait_pod_ready "$ns" pxc-0 || { fail "pxc-0 not Ready"; dump_ns "$ns"; return 1; }
  wait_config_synced "$ns" pxcfg 1 120 || { dump_ns "$ns"; return 1; }

  # Both containers up?
  local out
  out="$(kubectl -n "$ns" get pod pxc-0 -o jsonpath='{.spec.containers[*].name}')"
  [[ "$out" == *fluent-bit* ]] || { fail "logging: pod pxc-0 has no fluent-bit container ('$out')"; dump_ns "$ns"; return 1; }
  log "logging: fluent-bit sidecar present in pod"

  # Marker query through the data plane: the eventslog (format=2, JSON) line
  # must surface in the sidecar's stdout within 90s.
  local marker="e2e-log-marker-${RANDOM}${RANDOM}"
  out="$(mysql_query "$ns" pxc app appsecret "SELECT '$marker'")"
  [[ "$out" == "$marker" ]] || { fail "logging: marker query through :6033 failed (got '$out')"; dump_ns "$ns"; return 1; }
  log "logging: marker query returned through :6033"

  local found="" _
  for _ in $(seq 1 18); do
    if kubectl -n "$ns" logs pxc-0 -c fluent-bit 2>/dev/null | grep -qF "$marker"; then
      found=1
      break
    fi
    sleep 5
  done
  if [[ -z "$found" ]]; then
    kubectl -n "$ns" logs pxc-0 -c fluent-bit --tail=40 >&2 || true
    fail "logging: marker '$marker' not in fluent-bit stdout within 90s"
    dump_ns "$ns"
    return 1
  fi
  log "logging: query log line with marker shipped to fluent-bit stdout"

  kubectl delete ns "$ns" --wait=false >/dev/null
}
