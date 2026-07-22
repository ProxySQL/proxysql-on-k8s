# Tutorial 5 — Query logging

**What you'll learn**

- Enabling ProxySQL's query log (eventslog) with the Fluent Bit sidecar
- Following a query from the client to a structured JSON log line in
  `kubectl logs -c fluent-bit`
- Sizing the log buffer (`bufferSize`)
- The S3 and HTTP sink variants
- The persistence caveat: why toggling the query log off needs care

**Prerequisites**

- [Tutorial 1](01-first-cluster.md) completed, with the `proxysql-tutorial`
  namespace still around (the `mysql` backend and `app-user` Secret are
  reused). Tutorials 2–4 are not required.

## 1. Create a logging-enabled cluster

`spec.logging` adds a second container to each pod: ProxySQL writes its
eventslog (every MySQL-protocol query, as JSON) to a shared volume, and a
`fluent-bit` sidecar tails it and ships it to a sink — `stdout` here, the
simplest one, which lands in `kubectl logs` and therefore in whatever
cluster-level log collector you already run.

This tutorial uses a separate single-replica cluster so the main tutorial
cluster keeps its shape (and because of the persistence caveat in step 5 —
note the explicit `persistence: {enabled: false}`):

```sh
kubectl -n proxysql-tutorial apply -f - <<'EOF'
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLCluster
metadata:
  name: logproxy
spec:
  replicas: 1
  persistence:
    enabled: false
  logging:
    enabled: true
    queryLog: true
    sinkType: stdout
---
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLConfig
metadata:
  name: logproxy
spec:
  clusterRef: {name: logproxy}
  mysqlServers:
    - hostgroup: 0
      hostname: mysql.proxysql-tutorial.svc.cluster.local
      port: 3306
  mysqlUsers:
    - username: app
      defaultHostgroup: 0
      defaultSchema: appdb
      passwordSecretRef: {name: app-user, key: password}
  mysqlVariables:
    mysql-monitor_enabled: "false"
EOF
kubectl -n proxysql-tutorial wait --for=condition=Ready pod/logproxy-0 --timeout=180s
```

(`queryLog: true` is required whenever `enabled: true` — it's currently the
sidecar's only input, and admission rejects the combination
`enabled: true, queryLog: false`.)

The pod now has two containers:

```sh
kubectl -n proxysql-tutorial get pod logproxy-0 -o jsonpath='{.spec.containers[*].name}'
```

```
proxysql fluent-bit
```

The sidecar runs with the same PSA-`restricted` posture as ProxySQL itself
and deliberately has no probes — a logging hiccup must never gate pod
readiness.

## 2. Run a marker query and find it in the logs

Send a query you'll recognize:

```sh
kubectl -n proxysql-tutorial run mysql-client --rm -i --restart=Never --image=mysql:8.0 \
  --env=MYSQL_PWD=app-secret-pw -- \
  mysql -h logproxy -P6033 -uapp -e "SELECT 'find-me-in-the-logs'"
```

Then look for it in the sidecar's stdout (allow up to ~30 seconds for the
tail/flush cycle):

```sh
kubectl -n proxysql-tutorial logs logproxy-0 -c fluent-bit | grep find-me-in-the-logs
```

```
{"date":1781171904.572144,"client":"10.244.0.56:37046","digest":"0x1C46AE529DD5A40E","duration_us":527,"endtime":"2026-06-11 09:58:22.571146","endtime_timestamp_us":1781171902571146,"errno":0,"event":"COM_QUERY","hostgroup_id":0,"query":"SELECT 'find-me-in-the-logs'","rows_affected":0,"rows_sent":1,"schemaname":"appdb","server":"mysql.proxysql-tutorial.svc.cluster.local:3306","starttime":"2026-06-11 09:58:22.570619","starttime_timestamp_us":1781171902570619,"thread_id":6,"username":"app"}
```

One JSON object per query: the exact SQL, its digest, duration in
microseconds, which user and client sent it, which hostgroup and backend
served it, rows sent/affected, errno. This is an audit/analysis stream of
*every* data-plane query — alert pipelines, slow-query hunting, and security
review all hang off this one line format.

## 3. Buffering: `bufferSize`

ProxySQL writes the eventslog to a shared `emptyDir` volume (rotated at
50MB), and Fluent Bit keeps its read position and a filesystem backlog on
the same volume. `spec.logging.bufferSize` (default `1Gi`) bounds that
volume *and* Fluent Bit's buffer — if a sink is down long enough to fill it,
the oldest unshipped chunks are dropped rather than evicting the pod:

```yaml
  logging:
    enabled: true
    queryLog: true
    sinkType: stdout
    bufferSize: 2Gi
```

Resources for the sidecar default to requests `50m/64Mi`, limits
`200m/128Mi`, overridable via `spec.logging.resources`.

## 4. Shipping somewhere real: the S3 and HTTP sinks

`stdout` is the demo sink. For durable shipping there are two more
(*shown as configuration variants — not executed in this tutorial; they
need real external infrastructure*):

**S3 (or any S3-compatible store):**

```yaml
  logging:
    enabled: true
    queryLog: true
    sinkType: s3
    s3:
      bucket: my-query-logs
      region: us-east-1
      # prefix: /proxysql/logproxy        # default: /proxysql/<cluster>
      # endpoint: https://minio.internal  # for S3-compatible stores
      credentialsSecretRef:
        name: s3-creds        # keys: access-key-id, secret-access-key
```

**HTTP collector:**

```yaml
  logging:
    enabled: true
    queryLog: true
    sinkType: http
    http:
      host: collector.observability.svc
      port: 8443
      uri: /ingest/proxysql
      tls: true
      authTokenSecretRef:
        name: collector-token   # key: token → "Authorization: Bearer <token>"
```

Credentials are never written into the CR or the rendered Fluent Bit config —
they reach the sidecar as environment variables from the referenced Secrets.
Full field reference:
[reference/proxysqlcluster.md](../reference/proxysqlcluster.md).

## 5. The persistence caveat

The eventslog switch lives in ProxySQL's bootstrap config file. Toggling
`queryLog` off *removes* the eventslog lines from that file — and on a
cluster with **persistence enabled** (the default), the container's
`--reload` startup merge re-applies config-file lines over ProxySQL's
on-disk database (`proxysql.db`) but never deletes db entries that are
simply absent from the file. Consequence: on a persistence-enabled
cluster, **toggling `queryLog` off in the CR does not stop an
already-running eventslog** — the pods restart, keep the saved eventslog
settings from `proxysql.db`, and keep logging. To actually stop it there,
flip the variable at runtime on the admin port:

```sql
UPDATE global_variables SET variable_value='false'
  WHERE variable_name='mysql-eventslog_default_log';
LOAD MYSQL VARIABLES TO RUNTIME; SAVE MYSQL VARIABLES TO DISK;
```

(or set `mysql-eventslog_default_log: "false"` via the config's
`mysqlVariables`, which the operator pushes the same way). This is why this
tutorial's `logproxy` cluster runs with `persistence: {enabled: false}` —
on an ephemeral data dir, the bootstrap file wins on every restart and the
CR toggle behaves exactly as written. Decide your stance *before* enabling
the query log on a persistent production cluster.

## Clean up

Remove just this tutorial's cluster (keep the namespace if you're continuing):

```sh
kubectl -n proxysql-tutorial delete proxysqlconfig logproxy
kubectl -n proxysql-tutorial delete proxysqlcluster logproxy
```

Or drop everything:

```sh
kubectl delete namespace proxysql-tutorial
```

## Next

[Tutorial 6 — Monitoring](06-monitoring.md): the Prometheus metrics
endpoint, ServiceMonitor wiring, and which conditions to alert on.
