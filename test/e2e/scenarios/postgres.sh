#!/usr/bin/env bash
# Scenario: PostgreSQL data path — a real psql query routed through ProxySQL's
# pgsql port (:6133) to a live postgres backend, and the runtime pgsql tables
# reflect the ProxySQLConfig.

scenario_postgres() {
  local ns=e2e-postgres
  kubectl create ns "$ns" >/dev/null
  kubectl -n "$ns" apply -f - >/dev/null <<'YAML'
apiVersion: v1
kind: Secret
metadata: {name: appcreds}
stringData: {password: "pgsecret"}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: pg}
spec:
  replicas: 1
  selector: {matchLabels: {app: pg}}
  template:
    metadata: {labels: {app: pg}}
    spec:
      containers:
        - name: pg
          image: postgres:16
          env:
            - {name: POSTGRES_USER, value: app}
            - {name: POSTGRES_PASSWORD, value: pgsecret}
            - {name: POSTGRES_DB, value: appdb}
            - {name: POSTGRES_HOST_AUTH_METHOD, value: md5}
          ports: [{containerPort: 5432}]
          readinessProbe:
            exec: {command: ["pg_isready","-U","app","-d","appdb"]}
            initialDelaySeconds: 6
            periodSeconds: 4
---
apiVersion: v1
kind: Service
metadata: {name: pg}
spec:
  selector: {app: pg}
  ports: [{port: 5432, targetPort: 5432}]
YAML
  kubectl -n "$ns" rollout status deploy/pg --timeout=180s >/dev/null

  kubectl -n "$ns" apply -f - >/dev/null <<YAML
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLCluster
metadata: {name: pxc}
spec:
  replicas: 1
  persistence: {enabled: false}
  protocols: {mysql: {enabled: false}, pgsql: {enabled: true}}
---
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLConfig
metadata: {name: pxcfg}
spec:
  clusterRef: {name: pxc}
  pgsqlServers:
    - {hostgroup: 0, hostname: pg.$ns.svc.cluster.local, port: 5432}
  pgsqlUsers:
    - {username: app, defaultHostgroup: 0, passwordSecretRef: {name: appcreds, key: password}}
YAML
  wait_pod_ready "$ns" pxc-0 || { fail "pxc-0 not Ready"; dump_ns "$ns"; return 1; }
  wait_config_synced "$ns" pxcfg 1 120 || { dump_ns "$ns"; return 1; }

  local radmin out
  radmin="$(radmin_pw "$ns" pxc)"

  # Backend present in runtime_pgsql_servers (admin plane is MySQL-wire).
  out="$(admin_query "$ns" pxc "$radmin" "SELECT hostgroup_id,port FROM runtime_pgsql_servers")"
  echo "$out" | grep -qE "^0[[:space:]]+5432$" || { fail "pg backend not in runtime_pgsql_servers: '$out'"; dump_ns "$ns"; return 1; }
  log "postgres: backend present in runtime_pgsql_servers"

  # Real query through ProxySQL :6133 to the live postgres.
  out="$(psql_query "$ns" pxc app pgsecret appdb "SELECT current_database()")"
  echo "$out" | grep -qx "appdb" || { fail "psql through ProxySQL did not return 'appdb': '$out'"; dump_ns "$ns"; return 1; }
  log "postgres: query through ProxySQL :6133 returned '$out'"
}
