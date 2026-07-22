# ProxySQL operator documentation

Documentation for the ProxySQL Kubernetes operator (`proxysql.com/v1alpha1`:
`ProxySQLCluster` + `ProxySQLConfig`), organized in four layers. Every
command in the quickstart and tutorials was executed live against a kind
cluster — the outputs shown are real.

## Which doc do I want?

| You are saying… | Go to |
| --- | --- |
| "I want to try it, now" | [Quickstart](quickstart.md) — running queries through ProxySQL in ~5 minutes |
| "Teach me, step by step" | [Tutorials](#tutorials) — a six-part learning path, each ~15 minutes |
| "I'm running this in production" | [User guide](#user-guide) — task-oriented pages per concern |
| "What exactly does field X do?" | [Reference](#reference) — exhaustive tables, field by field |
| "Why is it designed this way?" | [architecture.md](architecture.md) — design rationale and reconcile loops |
| "I'm coming from the v1 charts" | [migration-from-v1.md](migration-from-v1.md) |

## The four layers

**[Quickstart](quickstart.md).** Install the operator, apply one manifest,
run a query, tear it down. The shortest path to a working ProxySQL on
Kubernetes.

**Tutorials.** A guided learning path: each builds on the previous one,
shows real command output, and explains what happened. Do them in order
(3 is optional if you don't care about PostgreSQL).

**User guide.** Task-oriented pages for people operating the thing:
installation choices, day-2 procedures, security posture, backend wiring.
Read the page matching your concern; they cross-link where they touch.

**Reference.** The contract: every field, default, condition, annotation,
and emitted SQL statement, in tables. Nothing task-oriented — the user
guide tells you *how*, the reference tells you *exactly what*.

## Tutorials

| # | Page | What you learn |
| --- | --- | --- |
| 1 | [Your first cluster](tutorials/01-first-cluster.md) | Deploy `ProxySQLCluster` + `ProxySQLConfig`, read status, meet the admin port (6032). |
| 2 | [Query routing](tutorials/02-query-routing.md) | Hostgroups, query rules, rule ordering, rewriting, and the query cache. |
| 3 | [PostgreSQL](tutorials/03-postgresql.md) | The pgsql listener (6133), `pgsql*` config sections, psql end-to-end. |
| 4 | [High availability](tutorials/04-high-availability.md) | Scaling to 3 replicas, write-to-all, killing pods, drift detection and self-heal. |
| 5 | [Query logging](tutorials/05-query-logging.md) | The Fluent Bit sidecar, eventslog to stdout/S3/HTTP, the persistence caveat. |
| 6 | [Monitoring](tutorials/06-monitoring.md) | The metrics port (6070), ServiceMonitor wiring, what to alert on. |

## User guide

| Page | Covers |
| --- | --- |
| [Installation](user-guide/installation.md) | Helm install/upgrade/uninstall, CRD handling, HA for the operator itself. |
| [Clusters](user-guide/clusters.md) | Sizing, auth Secrets, persistence trade-offs, protocols, exposure, rolling updates. |
| [Configuration](user-guide/configuration.md) | The write-to-all sync model, servers/users/rules, variables, drift, deletion semantics. |
| [Security](user-guide/security.md) | Credential flow, auth schemas, RBAC, PSA `restricted`, network exposure surface. |
| [Operations](user-guide/operations.md) | Reading status, troubleshooting table, logs, metrics, monitor-credential rotation runbook, manual admin-port access. |
| [Backends](user-guide/backends.md) | Cookbooks per database operator, external backends, the monitor user, failover stance. |

## Reference

| Page | Covers |
| --- | --- |
| [ProxySQLCluster](reference/proxysqlcluster.md) | Every spec/status field of the cluster CR, defaults and validation. |
| [ProxySQLConfig](reference/proxysqlconfig.md) | Every spec/status field of the config CR, list keys, SQL defaults. |
| [Admin tables](reference/admin-tables.md) | Field → admin-table column mapping, the sync SQL pattern, drift-detection coverage. |
| [Status & conditions](reference/status.md) | Every condition type/reason, phase derivation, requeue cadences, informed resync. |
| [Annotations & finalizers](reference/annotations.md) | Control annotations, labels, the cleanup finalizer and its wedge policy, owned-object names. |
| [Helm values](reference/helm-values.md) | Every value of the `proxysql-operator` chart and the manager flags they render. |

## Design documents

- [architecture.md](architecture.md) — components, reconcile-loop
  pseudocode, and the "why" behind every major decision (two CRDs,
  write-to-all, cnf-in-a-Secret, runtime reconfiguration, the
  `sqlStatements` escape hatch). The source of truth for design
  rationale.
- [superpowers/specs/](superpowers/specs/) — decision records for designs
  that shaped or will shape the operator (external failover, backend
  auto-discovery, logging sidecar, roadmap).
- [migration-from-v1.md](migration-from-v1.md) — chart/value rename map
  from the legacy v1 repo.

Backend cookbooks (apply-and-go examples per database operator) live
outside the docs tree in [`examples/`](../examples/README.md).
