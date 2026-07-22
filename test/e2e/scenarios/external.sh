#!/usr/bin/env bash
# Scenario: external Service exposure — spec.service.external. Kind has no
# cloud controller to provision a LoadBalancer, so this exercises the
# NodePort variant end to end:
#  1. The curated "<cluster>-external" Service carries exactly the selected
#     port (mysql, pinned nodePort 30333) and never the admin port unless
#     exposeAdmin=true.
#  2. status.endpoints.external reflects the NodePort form (host-less list
#     of allocated node ports).
#  3. A real client, from an in-cluster pod, connects through the NODE's
#     internal IP (not a Service DNS name) and runs a query that actually
#     routes through ProxySQL to a live MySQL backend — proving the DATA
#     path, not just the admin plane.
#  4. exposeAdmin flips the admin (6032) port on/off on the external Service.

scenario_external() {
  local ns=e2e-external
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
  service:
    external:
      enabled: true
      type: NodePort
      exposeAdmin: false
      ports:
        mysql: {nodePort: 30333}
---
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLConfig
metadata: {name: pxcfg}
spec:
  clusterRef: {name: pxc}
  mysqlServers:
    - {hostgroup: 0, hostname: mysql.$ns.svc.cluster.local, port: 3306}
  mysqlUsers:
    # defaultSchema: schema-less client sessions would otherwise inherit
    # ProxySQL's mysql-default_schema (information_schema), which the mysql
    # image's app user cannot open as a handshake database.
    - {username: app, defaultHostgroup: 0, defaultSchema: appdb, passwordSecretRef: {name: appcreds, key: password}}
  # Disable the monitor module: the backend has no "monitor" user with the
  # operator-minted password, and we don't want health checks shunning the
  # (perfectly reachable) server during the test.
  mysqlVariables:
    mysql-monitor_enabled: "false"
YAML
  wait_pod_ready "$ns" pxc-0 || { fail "pxc-0 not Ready"; dump_ns "$ns"; return 1; }
  wait_config_synced "$ns" pxcfg 1 120 || { dump_ns "$ns"; return 1; }

  local out node_ip

  # --- external Service shape: NodePort, exactly one port (mysql/6033),
  # pinned nodePort 30333, no admin (6032) ---
  out="$(kubectl -n "$ns" get svc pxc-external -o jsonpath='{.spec.type}')"
  [[ "$out" == "NodePort" ]] || { fail "external: svc type='$out', want NodePort"; dump_ns "$ns"; return 1; }
  out="$(kubectl -n "$ns" get svc pxc-external -o jsonpath='{.spec.ports[*].name}')"
  [[ "$out" == "mysql" ]] || { fail "external: svc port names='$out', want exactly 'mysql' (no admin)"; dump_ns "$ns"; return 1; }
  out="$(kubectl -n "$ns" get svc pxc-external -o jsonpath='{.spec.ports[*].port}')"
  [[ "$out" == "6033" ]] || { fail "external: svc port='$out', want 6033"; dump_ns "$ns"; return 1; }
  out="$(kubectl -n "$ns" get svc pxc-external -o jsonpath='{.spec.ports[*].nodePort}')"
  [[ "$out" == "30333" ]] || { fail "external: svc nodePort='$out', want 30333"; dump_ns "$ns"; return 1; }
  log "external: pxc-external is NodePort with exactly one port (mysql/6033 -> nodePort 30333), no admin"

  # --- status.endpoints.external reflects the NodePort form: a host-less,
  # comma-separated list of allocated node ports (just "30333" here) ---
  for _ in $(seq 1 15); do
    out="$(kubectl -n "$ns" get proxysqlcluster pxc -o jsonpath='{.status.endpoints.external}')"
    [[ "$out" == "30333" ]] && break
    sleep 4
  done
  [[ "$out" == "30333" ]] || { fail "external: status.endpoints.external='$out', want '30333'"; dump_ns "$ns"; return 1; }
  log "external: status.endpoints.external=30333"

  # --- data path: from an in-cluster pod, through the NODE's internal IP
  # (not a Service DNS name) and the pinned nodePort, through ProxySQL, to
  # the live MySQL backend ---
  node_ip="$(kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')"
  [[ -n "$node_ip" ]] || { fail "external: could not resolve a node internal IP"; dump_ns "$ns"; return 1; }
  log "external: node internal IP=$node_ip"

  for _ in 1 2 3 4 5; do
    out="$(kubectl -n "$ns" run "e2e-extq-$RANDOM" --rm -i --restart=Never --image="$MYSQL_IMAGE" \
      --env=MYSQL_PWD=appsecret --command -- \
      mysql -h "$node_ip" -P30333 -uapp -N -B -e "SELECT 1" 2>/dev/null | _strip_noise || true)"
    [[ -n "$out" ]] && break
    sleep 4
  done
  [[ "$out" == "1" ]] || { fail "external: SELECT 1 through ${node_ip}:30333 failed (got '$out')"; dump_ns "$ns"; return 1; }
  log "external: SELECT 1 routed through ${node_ip}:30333 -> ProxySQL -> backend (data path confirmed)"

  # --- exposeAdmin=true adds the admin (6032) port to the external Service ---
  kubectl -n "$ns" patch proxysqlcluster pxc --type=json \
    -p='[{"op":"replace","path":"/spec/service/external/exposeAdmin","value":true}]' >/dev/null
  for _ in $(seq 1 15); do
    out="$(kubectl -n "$ns" get svc pxc-external -o jsonpath='{.spec.ports[*].name}' 2>/dev/null || true)"
    [[ "$out" == *admin* ]] && break
    sleep 4
  done
  [[ "$out" == *admin* ]] || { fail "external: admin port did not appear after exposeAdmin=true (ports='$out')"; dump_ns "$ns"; return 1; }
  log "external: exposeAdmin=true added the admin port (ports='$out')"

  # --- patch back: exposeAdmin=false removes it again ---
  kubectl -n "$ns" patch proxysqlcluster pxc --type=json \
    -p='[{"op":"replace","path":"/spec/service/external/exposeAdmin","value":false}]' >/dev/null
  for _ in $(seq 1 15); do
    out="$(kubectl -n "$ns" get svc pxc-external -o jsonpath='{.spec.ports[*].name}' 2>/dev/null || true)"
    [[ "$out" != *admin* ]] && break
    sleep 4
  done
  [[ "$out" != *admin* ]] || { fail "external: admin port did not disappear after exposeAdmin=false (ports='$out')"; dump_ns "$ns"; return 1; }
  log "external: exposeAdmin=false removed the admin port (ports='$out')"
}
