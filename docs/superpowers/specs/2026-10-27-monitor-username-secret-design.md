# Configurable ProxySQL monitor username from the auth Secret

**Status:** Approved by Wazir (2026-10-29)

**Owner:** Wazir Ahmed

**Date:** 2026-10-27

**Branch:** `v0.0.1a_2836`

**Repository:** `proxysql-on-k8s`

## Goal

Let a `ProxySQLCluster` use a monitor account whose username is not `monitor`. Keep the existing Secret-backed auth contract and the `monitor` default for clusters whose Secret has no username override. Apply Secret changes to the running ProxySQL instances as well as new instances.

## Resource contract

`ProxySQLCluster.spec.auth.secretName` continues to identify the auth Secret. The current `admin-password`, `radmin-password`, and `monitor-password` keys retain their meanings. Add one optional literal key, `monitor_username`:

```yaml
stringData:
  admin-password: <admin password>
  radmin-password: <radmin password>
  monitor-password: <monitor password>
  monitor_username: monitor_ro
```

If `monitor_username` is absent, use `monitor`. If it is present but empty or cannot be safely rendered as a ProxySQL configuration username, report a configuration error rather than silently selecting another account. The operator-managed Secret may omit this optional key and use the default. The existing fallback Secret schema (`username` and `password`, with optional `monitor-password`) remains supported; its `username` is an **admin** username and must never be mistaken for `monitor_username`.

No plaintext credential field or new Secret reference is added to the CRD. Do not accept the monitor pair through `ProxySQLCluster.spec.variables` or `ProxySQLConfig.spec.mysqlVariables`; those settings remain operator-owned so the Secret and another variable source cannot fight over them.

## Rendering and reconciliation

Resolve the monitor username and password from one auth Secret read. Carry the username through the existing builder inputs and render it as `mysql_variables.monitor_username` whenever MySQL is enabled and `pgsql_variables.monitor_username` whenever PostgreSQL is enabled. Keep the corresponding password lines sourced from `monitor-password`. Validate before rendering so a username cannot corrupt the generated `proxysql.cnf`.

When the auth Secret changes, reconcile the owning cluster and apply the changed monitor settings to each relevant ProxySQL instance. Use the operator's existing runtime-apply/read-back flow for value changes; preserve its restart behavior if runtime apply cannot make a change effective. Do not log credential values or emit them in status/events.

## Approaches considered

- **Chosen: optional username in the existing auth Secret.** This extends the current password source, needs no CRD field, and keeps old clusters working.
- **Rejected: inline monitor variables in a CR.** It would put the password in readable Kubernetes resource content and introduce a second owner for variables already rendered by the operator.
- **Rejected: a separate monitor Secret.** It adds a second credential reference and update path without changing the outcome.

## Phases

1. **Secret contract and validation:** define the optional `monitor_username` key, default behavior, and safe username syntax in the auth Secret resolver. Cover both supported Secret schemas and the operator-managed Secret.
2. **Builder and apply path:** render the resolved name in MySQL and PostgreSQL bootstrap configuration; make a Secret rotation converge on running instances using the existing apply/read-back mechanism.
3. **Verification and documentation:** test legacy default, custom username, missing/empty/invalid key, both protocols, and rotation. Update the auth Secret reference documentation and examples without publishing actual passwords.

## Acceptance

- A Secret with `monitor_username=monitor_ro` and `monitor-password` causes the configured ProxySQL monitor username and password to match those values on each enabled protocol.
- An older Secret with no `monitor_username` continues to configure username `monitor`.
- An empty or invalid explicit username fails with an actionable condition. A partial required password schema retains its existing failure behavior.
- Changing the Secret updates the applied monitor settings; old and new instances converge without a second configuration owner.
- Secret values remain absent from CR fields, status, events, and logs.
