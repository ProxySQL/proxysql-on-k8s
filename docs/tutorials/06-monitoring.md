# Tutorial 6 — Monitoring

**What you'll learn**

- Reading `phase` and `endpoints` programmatically (for dashboards/scripts)
- What ProxySQL exposes on the metrics port (6070) and how to scrape it
- Wiring a Prometheus Operator `ServiceMonitor` — and how its absence is
  surfaced
- Which conditions and status fields are worth alerting on

**Prerequisites**

- [Tutorial 1](01-first-cluster.md) completed, with the `proxysql-tutorial`
  namespace and the `proxysql` cluster still around. (Outputs below show the
  3-replica cluster from [tutorial 4](04-high-availability.md); a 1-replica
  cluster behaves identically.)

## 1. Status for machines: phase and endpoints

Everything a dashboard or script needs is in status — no parsing of pod
names or Service ports required:

```sh
kubectl -n proxysql-tutorial get pxc proxysql -o jsonpath='{.status.phase}'; echo
kubectl -n proxysql-tutorial get pxc proxysql -o jsonpath='{.status.endpoints}'; echo
```

```
Running
{"admin":"proxysql.proxysql-tutorial.svc:6032","metrics":"proxysql.proxysql-tutorial.svc:6070","mysql":"proxysql.proxysql-tutorial.svc:6033"}
```

`phase` is a coarse single word — `Pending` | `Creating` | `Running` |
`Updating` | `Degraded` (`Failed` is reserved) — built for external pollers.
`endpoints` lists `host:port` per *enabled* surface, so it changes shape
when you toggle protocols. For anything finer than one word, read the
conditions (step 4). Full semantics:
[reference/status.md](../reference/status.md).

## 2. The metrics port (6070)

Every ProxySQL pod exposes its built-in Prometheus exporter (the REST API)
on port 6070 by default, published on the cluster Service and announced as
`endpoints.metrics`. Plain HTTP, path `/metrics`:

```sh
kubectl -n proxysql-tutorial run curl-client --rm -i --restart=Never --image=curlimages/curl -- \
  sh -c "curl -s http://proxysql:6070/metrics | head -n 6"
```

```
# HELP exposer_transferred_bytes_total Transferred bytes to metrics services
# TYPE exposer_transferred_bytes_total counter
exposer_transferred_bytes_total 0
# HELP exposer_scrapes_total Number of times metrics were scraped
# TYPE exposer_scrapes_total counter
exposer_scrapes_total 0
```

The interesting series are prefixed `proxysql_`:

```sh
kubectl -n proxysql-tutorial run curl-client --rm -i --restart=Never --image=curlimages/curl -- \
  sh -c "curl -s http://proxysql:6070/metrics | grep -E '^proxysql_(uptime|mysql_backend|client_connections|questions)' | head -8"
```

```
proxysql_client_connections_total{protocol="pgsql",status="aborted"} 0
proxysql_client_connections_total{protocol="pgsql",status="created"} 0
proxysql_client_connections_total{protocol="mysql",status="aborted"} 0
proxysql_client_connections_total{protocol="mysql",status="created"} 0
proxysql_client_connections_sha2cached_total 0
proxysql_questions_total{protocol="mysql"} 0
proxysql_uptime_seconds_total 263
proxysql_client_connections_connected{protocol="pgsql"} 0
```

You get connection counts (client and backend, per protocol), query
counters, query-cache hit/miss, per-module memory, uptime, and more —
ProxySQL's `stats_*` tables rendered as Prometheus families. Note that
scraping through the *Service* samples one pod per scrape; Prometheus-style
per-pod scraping is what the ServiceMonitor (next step) is for. Set
`spec.metrics.port` to move it, or `spec.metrics.enabled: false` to remove
the surface entirely.

## 3. ServiceMonitor for Prometheus Operator

If you run the [Prometheus Operator](https://prometheus-operator.dev/) /
kube-prometheus-stack, one toggle makes every pod scraped individually:

```yaml
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLCluster
metadata:
  name: proxysql
spec:
  replicas: 3
  metrics:
    serviceMonitor:
      enabled: true
      interval: 30s        # default
      scrapeTimeout: 10s   # default
      labels:
        release: kube-prometheus-stack   # whatever your Prometheus selects on
```

The operator then owns a `ServiceMonitor` named after the cluster. This is
**best-effort by design**: on a cluster *without* the prometheus-operator
CRDs, enabling it does not break reconciliation — it just surfaces a
condition. Watch (this was executed on a kind cluster with no
prometheus-operator installed):

```sh
kubectl -n proxysql-tutorial patch proxysqlcluster proxysql --type=merge \
  -p '{"spec":{"metrics":{"serviceMonitor":{"enabled":true}}}}'
sleep 5
kubectl -n proxysql-tutorial get pxc proxysql \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}) {.message}{"\n"}{end}'
```

```
Available=True (AllReplicasReady) 3/3 replicas ready
Progressing=False (Steady) no rollout in progress
ServiceMonitorReady=False (CRDNotInstalledOrFailed) no matches for kind "ServiceMonitor" in version "monitoring.coreos.com/v1"
```

The cluster keeps running; the condition tells you the scrape wiring isn't.
Install kube-prometheus-stack and the next reconcile creates the
ServiceMonitor and the condition goes away. Undo the experiment:

```sh
kubectl -n proxysql-tutorial patch proxysqlcluster proxysql --type=merge \
  -p '{"spec":{"metrics":{"serviceMonitor":{"enabled":false}}}}'
```

## 4. What to alert on

ProxySQL-side metrics aside, the operator's own status is the cheapest
health signal you have. Worth wiring up (e.g. via
[kube-state-metrics custom-resource state metrics](https://kubernetes.io/docs/concepts/cluster-administration/kube-state-metrics/)
or a periodic `kubectl`/API poll):

**On `ProxySQLCluster`:**

| Signal | Meaning |
| --- | --- |
| `Available=False` | Not all replicas ready — capacity reduced or gone. Page if it persists. |
| `Degraded=True` | A reconcile error the operator can name (`reason` says which: auth secret problems, etc.). |
| `Progressing=True` for a long time | A rollout that never converges (bad image, unschedulable pods). |
| `phase != Running` (coarse) | One-word equivalent of the above for simple pollers. |

**On `ProxySQLConfig`:**

| Signal | Meaning |
| --- | --- |
| `Ready=False` | Latest config has not reached every replica. |
| `ClusterFound=False` | `clusterRef` points at nothing — config is orphaned. |
| `Degraded=True` | Push/validation errors (`reason` names the failing piece, e.g. `PgsqlDisabled`). |
| `syncedReplicas < cluster replicas` | Some replicas serving stale config. |
| `driftedReplicas > 0` | A replica diverged out-of-band and couldn't be re-pushed yet ([tutorial 4](04-high-availability.md#4-out-of-band-drift-detection-and-self-heal)). |
| `shunnedBackends > 0` | ProxySQL health checks have taken backend rows out of rotation — usually a *database* problem, not a proxy one. |
| `lastRuntimeCheckTime` stale | The operator isn't completing its periodic runtime verification. |

The operator manager itself also exposes controller-runtime metrics
(reconcile rates, errors, workqueue depth) on its own metrics Service in
`proxysql-system` — see the chart's `metrics.*` values in
[reference/helm-values.md](../reference/helm-values.md).

## Clean up

This is the last tutorial using the `proxysql-tutorial` namespace:

```sh
kubectl delete namespace proxysql-tutorial
```

And if you're done with the operator entirely:

```sh
helm uninstall proxysql-operator -n proxysql-system
```

## Next

- [User guide: operations](../user-guide/operations.md) — upgrades, scaling,
  password rotation, troubleshooting.
- [User guide: backends](../user-guide/backends.md) — real replicated
  backends and failover ownership.
- [Reference: status](../reference/status.md) — every condition, reason, and
  field used above.
