#!/usr/bin/env bash
# Scenario: mysqlGaleraHostgroups — ProxySQL's Galera monitor places nodes and
# the operator follows it.
#
#  1. Two Galera-enabled MariaDB backends (each bootstrapped as its own
#     single-node cluster, so each reports wsrep_local_state=4 Synced
#     independently) are declared in the writer hostgroup only. With
#     maxWriters=1 the monitor must elect ONE writer and park the other in
#     the backup-writer hostgroup.
#  2. Failover-follow: read_only=1 on the current writer moves it to the
#     reader hostgroup and promotes the peer into the writer hostgroup.
#  3. Drift regression (the point of the four-hostgroup equivalence class):
#     an operator resync must NOT revert the monitor's placement, and
#     status.driftedReplicas must stay 0. Before the drift change, every
#     resync re-pushed the spec's static placement over the monitor.
#  4. wsrep_reject_queries=ALL parks a node in the offline hostgroup.
#
# Unlike every other scenario this one needs a WORKING monitor, so the
# auth Secret is pre-created (spec.auth.secretName) with a known
# monitor-password and the 'monitor' user is created on both backends before
# ProxySQL starts probing them. ProxySQL's monitor username is fixed to
# "monitor" by the bootstrap cnf.

GALERA_IMAGE="${GALERA_IMAGE:-mariadb:11.4}"

# _galera_hosts NS HOST RADMIN_PW HG -> newline-separated ONLINE hostnames in
# hostgroup HG, sorted so the output is comparable.
_galera_hosts() {
  admin_query "$1" "$2" "$3" \
    "SELECT hostname FROM runtime_mysql_servers WHERE hostgroup_id=$4 AND status='ONLINE' ORDER BY hostname"
}

# _galera_count NS HOST RADMIN_PW HG -> number of ONLINE servers in hostgroup HG.
_galera_count() {
  admin_query "$1" "$2" "$3" \
    "SELECT COUNT(*) FROM runtime_mysql_servers WHERE hostgroup_id=$4 AND status='ONLINE'"
}

# _galera_sql NS DEPLOY SQL -> run SQL on a backend as root.
_galera_sql() {
  kubectl -n "$1" exec "deploy/$2" -- \
    mariadb -uroot -prootsecret -N -B -e "$3" 2>/dev/null
}

scenario_galera() {
  local ns=e2e-galera
  kubectl create ns "$ns" >/dev/null

  # Known passwords up front: the monitor user must exist on the backends with
  # the same password the cnf hands to ProxySQL's monitor module.
  kubectl -n "$ns" create secret generic pxc-auth \
    --from-literal=admin-password=adminsecret \
    --from-literal=radmin-password=radminsecret \
    --from-literal=monitor-password=monitorsecret >/dev/null

  local b
  for b in gal-a gal-b; do
    kubectl -n "$ns" apply -f - >/dev/null <<YAML
apiVersion: apps/v1
kind: Deployment
metadata: {name: $b}
spec:
  replicas: 1
  selector: {matchLabels: {app: $b}}
  template:
    metadata: {labels: {app: $b}}
    spec:
      containers:
        - name: mariadb
          image: $GALERA_IMAGE
          # An empty gcomm:// address bootstraps a new single-node cluster,
          # which reaches wsrep_local_state=4 (Synced) on its own — all
          # ProxySQL's Galera check needs. No inter-node traffic, so no
          # 4567/4568/4444 plumbing is required.
          args:
            - --wsrep-on=ON
            - --wsrep-provider=/usr/lib/galera/libgalera_smm.so
            - --wsrep-cluster-address=gcomm://
            - --wsrep-cluster-name=$ns
            - --binlog-format=ROW
            - --default-storage-engine=InnoDB
            - --innodb-autoinc-lock-mode=2
          env:
            - {name: MARIADB_ROOT_PASSWORD, value: rootsecret}
            - {name: MARIADB_ROOT_HOST, value: "%"}
          ports: [{containerPort: 3306}]
          # Ready only once wsrep reports Synced: this doubles as the
          # assertion that the provider actually loaded.
          readinessProbe:
            exec:
              command:
                - /bin/sh
                - -c
                - mariadb -uroot -prootsecret -N -B -e "SHOW STATUS LIKE 'wsrep_local_state'" | grep -qw 4
            initialDelaySeconds: 20
            periodSeconds: 5
            failureThreshold: 30
---
apiVersion: v1
kind: Service
metadata: {name: $b}
spec:
  selector: {app: $b}
  ports: [{port: 3306, targetPort: 3306}]
YAML
  done
  for b in gal-a gal-b; do
    kubectl -n "$ns" rollout status "deploy/$b" --timeout=300s >/dev/null ||
      { fail "galera: backend $b never became Synced"; dump_ns "$ns"; return 1; }
  done
  log "galera: both backends report wsrep_local_state=4 (Synced)"

  # ProxySQL's Galera check reads wsrep_* status plus read_only; USAGE alone is
  # not enough on MariaDB, so grant the status/process privileges too.
  for b in gal-a gal-b; do
    _galera_sql "$ns" "$b" "
      CREATE USER IF NOT EXISTS 'monitor'@'%' IDENTIFIED BY 'monitorsecret';
      GRANT USAGE, REPLICATION CLIENT, PROCESS ON *.* TO 'monitor'@'%';
      GRANT SELECT ON performance_schema.* TO 'monitor'@'%';
      FLUSH PRIVILEGES;" >/dev/null ||
      { fail "galera: could not create the monitor user on $b"; dump_ns "$ns"; return 1; }
  done
  log "galera: monitor user created on both backends"

  kubectl -n "$ns" apply -f - >/dev/null <<YAML
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLCluster
metadata: {name: pxc}
spec:
  replicas: 1
  persistence: {enabled: false}
  protocols: {mysql: {enabled: true}, pgsql: {enabled: false}}
  auth: {secretName: pxc-auth}
---
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLConfig
metadata: {name: pxcfg}
spec:
  clusterRef: {name: pxc}
  # Both nodes are declared in the WRITER hostgroup only. Everything else is
  # the Galera monitor's job — that is the whole point of the table.
  mysqlServers:
    - {hostgroup: 10, hostname: gal-a.$ns.svc.cluster.local, port: 3306}
    - {hostgroup: 10, hostname: gal-b.$ns.svc.cluster.local, port: 3306}
  mysqlGaleraHostgroups:
    - writerHostgroup: 10
      readerHostgroup: 11
      backupWriterHostgroup: 12
      offlineHostgroup: 13
      maxWriters: 1
  # Tight monitor intervals so placement converges in seconds, not minutes.
  mysqlVariables:
    mysql-monitor_galera_healthcheck_interval: "1000"
    mysql-monitor_connect_interval: "1000"
    mysql-monitor_ping_interval: "1000"
    mysql-monitor_read_only_interval: "1000"
YAML
  wait_pod_ready "$ns" pxc-0 || { fail "pxc-0 not Ready"; dump_ns "$ns"; return 1; }
  wait_config_synced "$ns" pxcfg 1 120 || { dump_ns "$ns"; return 1; }

  local radmin writer peer writer_dep out t0 t1
  radmin="$(radmin_pw "$ns" pxc-auth)"

  # --- 1. maxWriters=1: one writer, one backup writer ---
  for _ in $(seq 1 30); do
    [[ "$(_galera_count "$ns" pxc "$radmin" 10)" == "1" &&
       "$(_galera_count "$ns" pxc "$radmin" 12)" == "1" ]] && break
    sleep 4
  done
  out="$(_galera_count "$ns" pxc "$radmin" 10)"
  [[ "$out" == "1" ]] ||
    { fail "galera: want exactly 1 ONLINE writer in hg10, got '$out'"; dump_ns "$ns"; return 1; }
  out="$(_galera_count "$ns" pxc "$radmin" 12)"
  [[ "$out" == "1" ]] ||
    { fail "galera: want exactly 1 ONLINE backup writer in hg12, got '$out'"; dump_ns "$ns"; return 1; }
  writer="$(_galera_hosts "$ns" pxc "$radmin" 10)"
  peer="$(_galera_hosts "$ns" pxc "$radmin" 12)"
  log "galera: monitor elected writer=$writer, backup writer=$peer (maxWriters=1)"

  # --- 2. failover-follow: demote the writer, the peer is promoted ---
  writer_dep="${writer%%.*}"
  _galera_sql "$ns" "$writer_dep" "SET GLOBAL read_only=1" >/dev/null ||
    { fail "galera: could not set read_only on $writer_dep"; dump_ns "$ns"; return 1; }
  log "galera: read_only=1 on the current writer ($writer_dep)"
  for _ in $(seq 1 30); do
    [[ "$(_galera_hosts "$ns" pxc "$radmin" 10)" == "$peer" &&
       "$(_galera_hosts "$ns" pxc "$radmin" 11)" == "$writer" ]] && break
    sleep 4
  done
  out="$(_galera_hosts "$ns" pxc "$radmin" 10)"
  [[ "$out" == "$peer" ]] ||
    { fail "galera: hg10 should hold the promoted peer '$peer', got '$out'"; dump_ns "$ns"; return 1; }
  out="$(_galera_hosts "$ns" pxc "$radmin" 11)"
  [[ "$out" == "$writer" ]] ||
    { fail "galera: hg11 should hold the demoted writer '$writer', got '$out'"; dump_ns "$ns"; return 1; }
  log "galera: failover followed — $peer promoted to hg10, $writer demoted to hg11"

  # --- 3. drift regression: a resync must not revert the monitor ---
  # Baseline AFTER the new placement is live, then wait for the NEXT informed
  # resync so a resync that snapshotted the old state cannot pass vacuously.
  t0="$(kubectl -n "$ns" get proxysqlconfig pxcfg -o jsonpath='{.status.lastRuntimeCheckTime}')"
  for _ in $(seq 1 25); do
    t1="$(kubectl -n "$ns" get proxysqlconfig pxcfg -o jsonpath='{.status.lastRuntimeCheckTime}')"
    [[ -n "$t1" && "$t1" != "$t0" ]] && break
    sleep 4
  done
  [[ -n "$t1" && "$t1" != "$t0" ]] ||
    { fail "galera: no informed resync ran after the failover"; dump_ns "$ns"; return 1; }
  out="$(_galera_hosts "$ns" pxc "$radmin" 10)"
  [[ "$out" == "$peer" ]] ||
    { fail "galera: resync re-pushed spec placement over the monitor (hg10='$out', want '$peer')"; dump_ns "$ns"; return 1; }
  # driftedReplicas has omitempty and no CRD default: 0 serializes as "".
  out="$(kubectl -n "$ns" get proxysqlconfig pxcfg -o jsonpath='{.status.driftedReplicas}')"
  [[ -z "$out" || "$out" == "0" ]] ||
    { fail "galera: monitor placement flagged as drift (driftedReplicas='$out')"; dump_ns "$ns"; return 1; }
  log "galera: monitor placement survived the resync, driftedReplicas=0"

  # --- 4. offline hostgroup: a node rejecting queries is parked in hg13 ---
  _galera_sql "$ns" "$writer_dep" "SET GLOBAL wsrep_reject_queries=ALL" >/dev/null || true
  log "galera: wsrep_reject_queries=ALL on $writer_dep"
  for _ in $(seq 1 30); do
    [[ "$(_galera_hosts "$ns" pxc "$radmin" 13)" == "$writer" ]] && break
    sleep 4
  done
  out="$(_galera_hosts "$ns" pxc "$radmin" 13)"
  [[ "$out" == "$writer" ]] ||
    { fail "galera: hg13 (offline) should hold '$writer', got '$out'"; dump_ns "$ns"; return 1; }
  log "galera: query-rejecting node parked in the offline hostgroup (hg13)"

  # The offline move is ALSO not drift — hg13 is part of the row's class.
  # driftedReplicas still carries the value from the PREVIOUS runtime check at
  # this point, so wait for a fresh one before reading it, exactly as the
  # failover assertion above does; otherwise this passes vacuously.
  t0="$t1"
  for _ in $(seq 1 25); do
    t1="$(kubectl -n "$ns" get proxysqlconfig pxcfg -o jsonpath='{.status.lastRuntimeCheckTime}')"
    [[ -n "$t1" && "$t1" != "$t0" ]] && break
    sleep 4
  done
  [[ -n "$t1" && "$t1" != "$t0" ]] ||
    { fail "galera: no informed resync ran after the offline move"; dump_ns "$ns"; return 1; }
  out="$(kubectl -n "$ns" get proxysqlconfig pxcfg -o jsonpath='{.status.driftedReplicas}')"
  [[ -z "$out" || "$out" == "0" ]] ||
    { fail "galera: offline placement flagged as drift (driftedReplicas='$out')"; dump_ns "$ns"; return 1; }
  # ...and the node is still parked offline, not dragged back by that resync.
  out="$(_galera_hosts "$ns" pxc "$radmin" 13)"
  [[ "$out" == "$writer" ]] ||
    { fail "galera: resync moved the offline node out of hg13 (hg13='$out')"; dump_ns "$ns"; return 1; }
  log "galera: offline placement survived a resync and is not drift either"
}
