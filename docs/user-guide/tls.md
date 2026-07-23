# TLS

Certificate issuance and TLS wiring for a `ProxySQLCluster`: the frontend
client ports (mysql/pgsql), the admin/cluster-peering interface, and,
separately, ProxySQL's trust toward your backend databases. For the
complete field-by-field contract see the [ProxySQLCluster
reference — TLS](../reference/proxysqlcluster.md#tls); for credentials,
RBAC, and the rest of the security posture see
[Security](./security.md).

TLS is off by default (`spec.tls` absent, or present with
`enabled: false`) and renders byte-identical to a cluster with no `tls`
block at all — turning it on is an explicit, opt-in choice, not something
that changes behavior underneath an existing cluster.

## The three tiers

`spec.tls` resolves the frontend/admin serving certificate through a
strict precedence, evaluated every reconcile:

| Tier | Trigger | Who issues | Who rotates |
|---|---|---|---|
| 1 — your own Secret | `spec.tls.secretName` set | you | you — the operator mounts it as-is and never re-issues it |
| 2 — cert-manager | `secretName` empty, `spec.tls.issuerRef.name` set | cert-manager, via an operator-owned `Certificate` named `<cluster>-tls` | cert-manager |
| 3 — self-signed (default) | `enabled: true`, neither of the above set | the operator (a CA in `<cluster>-tls-ca`, a serving cert in `<cluster>-tls`) | the operator |

Whichever tier resolves, the **same** wiring reaches the pod: a `tls-init`
init container symlinks the resolved Secret's `tls.crt`/`tls.key`/`ca.crt`
onto ProxySQL's fixed datadir cert names, because ProxySQL 3.0 has no
frontend/admin cert-path *variables* at all — it only ever reads
`proxysql-{ca,cert,key}.pem` (or auto-generates them if absent). This
detail matters later: it's exactly what makes rotation restart-free (see
[Rotation](#rotation)).

### Tier 3 — let the operator handle it

```yaml
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLCluster
metadata:
  name: proxysql
spec:
  replicas: 3
  tls:
    enabled: true
```

The operator mints a 5-year self-signed CA into `<cluster>-tls-ca` (once,
preserved across reconciles) and a 90-day serving certificate into
`<cluster>-tls`, reissued automatically inside the 30-day renewal window
(`duration`/`renewBefore`, both overridable). This is the zero-config
path: no cert-manager dependency, works in any cluster.

### Tier 1 — bring your own certificate

```yaml
spec:
  tls:
    enabled: true
    secretName: my-proxysql-cert   # kubernetes.io/tls: tls.crt, tls.key, ca.crt
```

**`ca.crt` is required even here** — not just `tls.crt`/`tls.key`. The
`tls-init` container symlinks all three datadir names unconditionally, and
a missing `ca.crt` fails validation before the Secret is ever mounted (see
[Validate-and-hold](#validate-and-hold-nothing-ships-unverified)). The
operator never rotates or re-issues a tier-1 Secret; renewing it is
entirely your responsibility (and, once you do, the operator picks up the
new content as a restart-free [rotation](#rotation)).

### Tier 2 — cert-manager

```yaml
spec:
  tls:
    enabled: true
    issuerRef:
      name: my-cluster-issuer
      kind: ClusterIssuer       # default: Issuer
      # group: cert-manager.io  # default
    duration: "2160h"           # 90d, cert-manager's default lifetime here
    renewBefore: "720h"         # 30d
```

The operator creates and owns a cert-manager `Certificate` object named
`<cluster>-tls`, targeting a Secret of the same name. **If you already
manage a `Certificate` under that exact name in the cluster's namespace,
it collides with the operator's** — cert-manager only lets one controller
own a given `Certificate`/Secret pair cleanly. Let the operator own it (the
common case), or pick a different tier.

## Backend TLS is a different PKI

`spec.tls.backend` configures a **completely separate** trust
relationship: ProxySQL's connection to your MySQL/PostgreSQL backends, not
the certificate your clients see.

```yaml
spec:
  tls:
    enabled: true
    backend:
      caSecretName: mysql-server-ca        # trusts the BACKEND's cert
      clientCertSecretName: proxysql-mtls  # optional, for backend mTLS
```

**Never point `backend.caSecretName` at `<cluster>-tls-ca`** unless you
have actually issued your backend database's server certificate from that
same CA — which is unusual. `ssl_p2s_ca` (rendered from
`caSecretName`) must trust whatever *your database* presents, and that's
almost always a different issuer than the one signing ProxySQL's own
frontend certificate. Setting `spec.tls.backend` only supplies the trust
material; it doesn't turn TLS *on* for any given backend server — pair it
with `useSSL: true` on the matching `ProxySQLConfig` entry:

```yaml
apiVersion: proxysql.com/v1alpha1
kind: ProxySQLConfig
metadata: {name: proxysqlcfg}
spec:
  clusterRef: {name: proxysql}
  mysqlServers:
    - {hostgroup: 0, hostname: mysql-primary, port: 3306, useSSL: true}
```

The equivalent `pgsqlServers[].useSSL` field works the same way for
PostgreSQL backends: it sets `pgsql_servers.use_ssl` on the matching row.
See the [ProxySQLConfig
reference](../reference/proxysqlconfig.md#pgsqlservers).

## Admin (6032) serves TLS too

Once `spec.tls.enabled` is true, the admin port serves the exact same
datadir certificates as the data ports — probe-verified against
`proxysql/proxysql:3.0`. There's no way to TLS the client-facing ports
while leaving admin plaintext, or vice versa; the operator's own config
pushes and admin dials switch to TLS along with everything else.

## Enable/disable = one structural restart

Flipping `spec.tls.enabled` changes the pod template (the init container
and the `tls` Secret volume appear or disappear), so — like any other
structural change — it rolls every pod exactly once. This is the *only*
time a TLS change restarts pods under normal operation; see
[Rotation](#rotation) for what happens after.

### Disabling on a persistence-enabled cluster

If `persistence.enabled: true` (the default), disabling TLS needs one
extra bit of housekeeping that the operator handles for you automatically:
the `tls-init` symlinks left on the PVC (`proxysql-{ca,cert,key}.pem`
pointing at a Secret mount that no longer exists) would otherwise leave
ProxySQL trying to auto-generate certs *through* a dangling symlink,
failing on `BIO_new_file`, and exiting at boot — a crash-loop. This was
probe-verified against the shipped image while building this feature.
The operator detects the transition (a StatefulSet that was previously
TLS-wired going TLS-off) and renders a `tls-cleanup` init container that
removes exactly those three symlinks — never a real `.pem` file left by a
cluster that auto-generated its own certs before TLS was ever configured
— before the main container starts.

**This container stays in the pod template afterwards, by design.** It's
an idempotent no-op once the symlinks are gone, and there's no benefit to
churning the template again just to remove it. Don't be surprised to see
`tls-cleanup` in `kubectl get sts <cluster> -o yaml` long after TLS was
last enabled on that cluster — it's inert, not a sign something's wrong.

A cluster with `persistence.enabled: false` never needs this: an emptyDir
datadir starts fresh on every boot.

## Rotation

Once TLS is wired, any change to the resolved Secret's *content* — a
manual replacement, cert-manager reissuing ahead of `renewBefore`, the
operator's own tier-3 renewal — is a **rotation**, applied without
restarting pods:

1. The operator issues `PROXYSQL RELOAD TLS` on every ready replica over
   the admin port.
2. It then completes a real TLS handshake against that replica and
   compares the served leaf certificate's fingerprint against the new
   `tls.crt`.
3. Once every ready replica verifies, the object-level
   [`proxysql.com/tls-applied-hash`](../reference/annotations.md#proxysqlcomtls-applied-hash-operator-set-on-the-statefulset-object)
   annotation advances to record it — this never touches the pod template,
   so no rollout happens.

### Verification is always pinned — never skip-verify

The handshake in step 2 doesn't just check "is this cert signed by
something in our trust store" — it accepts a replica's presented
certificate **only if** it is byte-identical to the Secret's new
`tls.crt`, **or** it chains to the Secret's *current* `ca.crt` and
presents that replica's own SAN. A shared CA is not treated as proof of
identity on its own.

### The one case that always falls back to a restart: leaf + CA swapped together

If a rotation replaces **both** the leaf and the CA in the same write —
the shape you get from a manual tier-1 Secret swap done carelessly, or
from a tier-3 self-signed CA renewal (see below) — there is, briefly, no
verifiable handshake path: a replica still running the *old* material
doesn't match the new leaf, and it doesn't chain to the new CA either. The
operator retries for a bounded window (2 minutes by default — comfortably
above worst-case kubelet Secret-mount propagation), surfacing
`Degraded=True`/`TLSRotationError` while it waits. **When the window
expires, this is not a stuck rotation** — it's expected, documented
behavior: the engine falls back to exactly **one** rolling restart, which
delivers the new material through the kubelet instead of the network, and
that restart's own template bump
([`proxysql.com/tls-restart`](../reference/annotations.md#proxysqlcomtls-restart-operator-set-on-the-pod-template))
is content-hashed and idempotent.

If you need a manual tier-1 rotation to stay restart-free, do it in two
writes: publish the new leaf first (still chained to the old CA, so a
verifiable path exists throughout), then publish the new CA into `ca.crt`
in a second update.

### CA renewal is a hard cutover (tier 3)

The tier-3 self-signed CA is long-lived (5 years) specifically so this is
rare, but it's worth understanding: the operator never lets a serving
certificate outlive the CA that signed it. When the CA is due for
renewal — because it's within `duration + renewBefore` of its own expiry,
or was somehow invalidated — the operator mints a new CA **and**
immediately reissues the serving certificate from it in the same pass. A
serving cert chained to a CA the operator no longer trusts is never left
in place. From the rotation engine's point of view this is exactly the
leaf+CA-together case above: verification can't complete over the
network while old pods still serve the old material, so it degrades to
the same one-rolling-restart fallback. You'll see `TLSRotationError`
degrade briefly and then a single rollout — that's the mechanism working
as intended, not a bug.

## Switching tiers leaves old Secrets behind

The operator only cleans up what it's sure is safe to clean up. Switching
from tier 3 to tier 1, for example, leaves `<cluster>-tls` and
`<cluster>-tls-ca` sitting in the namespace, unreferenced — the operator
doesn't know whether you still want that self-signed material around for
some other purpose, so it doesn't delete Secrets on a tier change (it
never deletes a Secret it didn't mint for the currently-active tier's
purpose, and never touches a user-provided one at all). Moving *off* tier
2 does garbage-collect the operator-owned `Certificate` object, so
cert-manager stops fighting over the Secret, but the Secret's last-written
content is left behind too.

These leftovers are inert once the tier has moved on — nothing references
them — but they're your responsibility to remove:

```bash
kubectl delete secret <cluster>-tls-ca <cluster>-tls   # after confirming the new tier works
```

## Validate-and-hold: nothing ships unverified

Before any TLS wiring reaches the StatefulSet, the operator GETs the
resolved Secret and requires `tls.crt`, `tls.key`, **and `ca.crt`** to all
be present and non-empty — the kubelet is never the validator of last
resort. If that fails (missing Secret, missing key, cert-manager not
installed, issuance still pending), the reconcile does not wedge: it holds
the last-good render (a previously-wired StatefulSet keeps its current
mount; a cluster that was never TLS-wired renders with no TLS this pass),
surfaces `Degraded=True`/`TLSSecretError`
(see the [status reference](../reference/status.md)), and keeps retrying.
Backend-TLS Secrets go through the identical check, independently.

## Connecting as a client

The `proxysql/proxysql:3.0` image ships the **MariaDB** flavor of the
`mysql` CLI, not the Oracle one — it has **no `--ssl-mode` flag**. That
flag only exists on the Oracle MySQL client. Both clients can verify a
ProxySQL-fronted connection; the flags differ:

**MariaDB client** (what ships in the operator's own image, and in most
`mariadb`/distro packages) — chain-verified against a CA file, no
plaintext fallback:

```bash
mysql -h proxysql -P 6033 -u app -p --ssl-ca=/path/to/ca.crt -e "SELECT 1"
```

**Oracle MySQL client** (if you're connecting from a client image that
ships it instead) — the flag your muscle memory probably reaches for:

```bash
mysql -h proxysql -P 6033 -u app -p --ssl-mode=VERIFY_CA --ssl-ca=/path/to/ca.crt -e "SELECT 1"
```

Get `ca.crt` from the resolved TLS Secret (`<cluster>-tls` on tiers 2/3,
or your own `secretName` on tier 1) — e.g.
`kubectl get secret <cluster>-tls -o jsonpath='{.data.ca\.crt}' | base64 -d > ca.crt`.
Note TLS here is **available, not mandatory**: `spec.tls` gives ProxySQL a
certificate to present, but a plaintext connection still succeeds unless
you separately require TLS per user (`mysqlUsers[].useSSL: true` on the
`ProxySQLConfig`) — that per-user flag is the only require-TLS enforcement
the operator exposes.

## Non-goals (v1)

**Frontend/client-certificate authentication (mTLS) is not supported.**
`spec.tls` issues and wires a *server* certificate ProxySQL presents to
clients; there is no equivalent of `backend.clientCertSecretName` for
requiring or verifying a *client* certificate on the frontend ports. If
you need mutual TLS on the client-facing side today, put it in front of
ProxySQL (an ingress/mesh layer that terminates or forwards mTLS) rather
than expecting `spec.tls` to enforce it.

## Migrating hand-wired ssl_p2s variables

If you were setting backend TLS through raw `spec.variables` entries
(`mysql-ssl_p2s_ca`, `mysql-ssl_p2s_cert`, `mysql-ssl_p2s_key`, and the
`pgsql-` equivalents) before this feature existed, those six keys are now
**reserved unconditionally** — regardless of whether `spec.tls` is set at
all — because `spec.tls.backend` is now the operator's own path for
rendering them, and a reconcile that still sets them directly is rejected
(`spec.variables: "<key>" is reserved (bootstrap-structural)`). Migrate to
[`spec.tls.backend`](../reference/proxysqlcluster.md#tlsbackendspec):

```diff
 spec:
-  variables:
-    mysql:
-      mysql-ssl_p2s_ca: /some/path/ca.pem
+  tls:
+    enabled: true
+    backend:
+      caSecretName: mysql-server-ca
```

`mysql-have_ssl`/`pgsql-have_ssl` are **not** reserved and remain
user-settable via `spec.variables` either way — they control whether
ProxySQL offers TLS on the frontend at all, independent of who's managing
the certificate behind it. The unrendered `ssl_p2s_capath`/
`ssl_p2s_cipher`/`ssl_p2s_crl`/`ssl_p2s_crlpath` tuning knobs also remain
user-settable; the operator only reserves the three keys it actually
renders per protocol.

## Next

- [ProxySQLCluster reference — TLS](../reference/proxysqlcluster.md#tls) —
  every field, default, and validation rule.
- [Annotations reference](../reference/annotations.md) —
  `tls-applied-hash`, `tls-rotation-state`, `tls-restart`.
- [Status reference](../reference/status.md) — `TLSSecretError` /
  `TLSRotationError` conditions.
- [Security](./security.md) — credentials, RBAC, network exposure.
