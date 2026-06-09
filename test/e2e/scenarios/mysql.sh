#!/usr/bin/env bash
# Scenario: MySQL data path — a real query routed through ProxySQL :6033 to a
# live MySQL backend (proves frontend+backend auth pass-through), plus a query
# rule that lands in runtime. (True writer/reader split needs replication; that
# is left to the backend-operator examples — see issue #9.)

scenario_mysql() {
  local ns=e2e-mysql
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
---
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLConfig
metadata: {name: pxcfg}
spec:
  clusterRef: {name: pxc}
  mysqlServers:
    - {hostgroup: 0, hostname: mysql.$ns.svc.cluster.local, port: 3306}
  mysqlUsers:
    - {username: app, defaultHostgroup: 0, passwordSecretRef: {name: appcreds, key: password}}
  mysqlQueryRules:
    - {ruleId: 100, active: true, matchDigest: "^SELECT", destinationHostgroup: 0, apply: true}
  # Disable the monitor module: the backend has no "monitor" user with the
  # operator-minted password, and we don't want health checks shunning the
  # (perfectly reachable) server during the test.
  mysqlVariables:
    mysql-monitor_enabled: "false"
YAML
  kubectl -n "$ns" wait --for=condition=Ready pod/pxc-0 --timeout=120s >/dev/null
  wait_config_synced "$ns" pxcfg 1 120 || { dump_ns "$ns"; return 1; }

  local radmin out
  radmin="$(radmin_pw "$ns" pxc)"

  # Query rule landed in runtime?
  out="$(admin_query "$ns" pxc "$radmin" "SELECT rule_id,destination_hostgroup FROM runtime_mysql_query_rules")"
  echo "$out" | grep -qE "^100[[:space:]]+0$" || { fail "query rule 100 not in runtime_mysql_query_rules: '$out'"; dump_ns "$ns"; return 1; }
  log "mysql: query rule present in runtime"

  # Real query through ProxySQL :6033 to the backend (returns the MySQL version).
  out="$(mysql_query "$ns" pxc app appsecret "SELECT VERSION()")"
  echo "$out" | grep -qiE "mysql|[0-9]+\.[0-9]+\.[0-9]+" || { fail "query through ProxySQL did not return a MySQL version: '$out'"; dump_ns "$ns"; return 1; }
  log "mysql: query through ProxySQL :6033 returned version '$out'"
}
