# Logging Sidecar (Fluent Bit): Design

**Date:** 2026-06-10
**Status:** Decided
**Issue:** #29 (design-only; implementation lands behind the toggle in a follow-up branch)
**Decision:** Default-off Fluent Bit sidecar on `ProxySQLCluster` pods, shipping
ProxySQL's query log (eventslog) to one of three sinks: `stdout | s3 | http`.
Query log lives on a dedicated `logs` emptyDir shared by both containers; the
Fluent Bit config is a second key in the existing bootstrap cnf object; PSA
`restricted` is preserved end to end.

## Context

ProxySQL's error log already goes to stderr and lands in `kubectl logs` — no
sidecar needed for that. What is *not* reachable today is the **query log**
(eventslog): ProxySQL writes it to a file in its datadir, which under
`readOnlyRootFilesystem` is the PVC, invisible to any log pipeline. The
sidecar's value is (a) making query logs available at all, and (b) shipping
them to non-stdout sinks (S3 for audit retention, HTTP for collectors that
speak it) without users having to bolt their own agent onto our StatefulSet.

## API shape

New `spec.logging` on `ProxySQLClusterSpec`, modeled on `MetricsSpec` as an
optional sub-spec. Per CLAUDE.md, default-**off** booleans stay plain `bool`
(`*bool` is reserved for default-true fields), so `Enabled` and `QueryLog` are
plain `bool`.

```go
// LoggingSpec configures the optional Fluent Bit log-shipping sidecar.
type LoggingSpec struct {
    // Enabled adds the fluent-bit sidecar. Default off.
    Enabled bool `json:"enabled,omitempty"`
    // QueryLog enables ProxySQL's eventslog (all MySQL-protocol queries) and
    // ships it. Currently the sidecar's only input, so admission rejects
    // enabled=true with queryLog=false until more inputs exist.
    QueryLog bool `json:"queryLog,omitempty"`
    // +kubebuilder:validation:Enum=stdout;s3;http
    // +kubebuilder:default=stdout
    SinkType string `json:"sinkType,omitempty"`
    S3   *S3SinkSpec   `json:"s3,omitempty"`   // required iff sinkType=s3
    HTTP *HTTPSinkSpec `json:"http,omitempty"` // required iff sinkType=http
    // Image is the Fluent Bit image. Pinned default; never `latest`.
    // +kubebuilder:default="fluent/fluent-bit:4.0.3"
    Image string `json:"image,omitempty"`
    // Resources for the sidecar. Defaults: req 50m/64Mi, lim 200m/128Mi.
    Resources corev1.ResourceRequirements `json:"resources,omitempty"`
    // BufferSize bounds the logs emptyDir (sizeLimit) and the Fluent Bit
    // filesystem buffer. Default 1Gi.
    BufferSize resource.Quantity `json:"bufferSize,omitempty"`
}

type S3SinkSpec struct {
    Bucket string `json:"bucket"`           // required
    Region string `json:"region"`           // required
    Prefix string `json:"prefix,omitempty"` // default /proxysql/<cluster>
    Endpoint string `json:"endpoint,omitempty"` // S3-compatible stores
    // CredentialsSecretRef names a Secret with keys access-key-id /
    // secret-access-key. Credentials are NEVER inline in the CR.
    CredentialsSecretRef corev1.LocalObjectReference `json:"credentialsSecretRef"`
}

type HTTPSinkSpec struct {
    Host string `json:"host"`               // required
    Port int32  `json:"port,omitempty"`     // default 443 if tls else 80
    URI  string `json:"uri,omitempty"`      // default "/"
    TLS  bool   `json:"tls,omitempty"`
    // AuthTokenSecretRef names a Secret (key: token) sent as
    // `Authorization: Bearer <token>`. Optional; never inline.
    AuthTokenSecretRef *corev1.LocalObjectReference `json:"authTokenSecretRef,omitempty"`
}
```

Admission (CEL + webhook, same pattern as existing validation): `sinkType=s3`
requires `s3`, `sinkType=http` requires `http`, and `enabled && !queryLog` is
rejected with "logging.queryLog is the only input; enable it or disable
logging" — better an explicit error than a silently idle sidecar.

Image pinning: the default is a concrete digest-stable tag
(`fluent/fluent-bit:4.0.3`), bumped deliberately via PRs like the ProxySQL
image default. Users override `image` wholesale for air-gapped registries.

## Query-log plumbing under readOnlyRootFilesystem

**Decision: a dedicated `logs` emptyDir mounted at `/var/log/proxysql` in both
containers.** Not the datadir: the datadir is the PVC when persistence is on,
and coupling log churn to admin-DB storage sizing (and PVC IOPS) is wrong;
when persistence is off it's an unbounded emptyDir we'd then have to bound
anyway. A separate emptyDir with `sizeLimit: <bufferSize>` keeps log retention
node-local, bounded, and identical in both persistence modes.

When `queryLog` is enabled the bootstrap cnf's `mysql_variables` block gains:

```
eventslog_filename="/var/log/proxysql/queries"
eventslog_default_log=1
eventslog_format=2          # JSON lines — Fluent Bit parses natively
eventslog_filesize=52428800 # 50MB; ProxySQL rotates to queries.00000002, ...
```

Rotation is ProxySQL's own (`eventslog_filesize` suffix rollover); Fluent Bit
tails `queries*` so rotated files are picked up, with its position DB on the
same emptyDir. ProxySQL does not delete rotated files; the emptyDir
`sizeLimit` is the backstop (kubelet evicts the pod past it, surfacing
misconfiguration loudly rather than silently). A pruning mechanism (e.g.
Fluent Bit 4.x `tail` `exit_on_eof`-style cleanup or a deliberate small
`eventslog_filesize` + low file count) is a noted follow-up for implementation
to verify under sustained load; the design accepts 50MB-granularity turnover.

Interaction with #19 (richer query rules): `eventslog_default_log=1` logs all
queries; once query rules grow a `log` field, users wanting per-rule logging
can keep `default_log` behavior — making it configurable is a follow-up, not
part of this round.

## Fluent Bit config generation

**Decision: a second key, `fluent-bit.conf`, in the existing bootstrap cnf
object** (the ConfigMap today; it rides along when #21 moves the cnf to a
Secret). Rationale: one fewer object to build/own/garbage-collect, the same
mounted `config` volume serves both containers (the sidecar mounts the
`fluent-bit.conf` item at `/fluent-bit/etc/fluent-bit.conf`), and — key point —
the file contains **no secrets**: S3/HTTP credentials reach the sidecar as env
vars from `secretKeyRef` and are referenced in the config as `${...}`, which
Fluent Bit expands at startup.

The existing `proxysql.com/cnf-checksum` annotation becomes the SHA-256 over
*both* keys, so a sink config change rolls the pods (no reliance on Fluent Bit
hot reload). Sketch:

```
[SERVICE]
    storage.path  /var/log/proxysql/flb-storage
[INPUT]
    name          tail
    path          /var/log/proxysql/queries*
    parser        json
    db            /var/log/proxysql/flb-tail.db
    storage.type  filesystem

# sinkType=stdout                # sinkType=s3                      # sinkType=http
[OUTPUT]                         [OUTPUT]                           [OUTPUT]
    name   stdout                    name        s3                     name    http
    match  *                         match       *                      match   *
    format json_lines                bucket      <bucket>               host    <host>
                                     region      <region>               port    <port>
                                     s3_key_format /<prefix>/...        uri     <uri>
                                     # creds via AWS_ACCESS_KEY_ID /    tls     on|off
                                     # AWS_SECRET_ACCESS_KEY env        header  Authorization Bearer ${FLB_HTTP_TOKEN}
```

Every output gets `storage.total_limit_size` derived from `bufferSize` so a
sink outage buffers to the emptyDir up to a bound, then drops oldest chunks.

`stdout` is the default and the CI-able sink: the query log lands in the
sidecar's own `kubectl logs`, zero external dependencies.

## Container and security posture

The sidecar is a **regular container** (`fluent-bit`) in the StatefulSet pod —
not a native sidecar initContainer; ordering guarantees aren't worth the extra
machinery for a log shipper, and failure isolation is identical: a sidecar
crash restarts only the sidecar, never ProxySQL. No liveness/readiness probes
on it (kubelet restart-on-exit suffices; probes would only add ways to disturb
the pod). It does not gate pod readiness.

PSA `restricted`, same as everything else this operator produces:
`runAsNonRoot: true`, `runAsUser/runAsGroup: 999` (the fluent-bit image runs
fine as an arbitrary non-root uid), `readOnlyRootFilesystem: true`,
`allowPrivilegeEscalation: false`, capabilities drop ALL, seccomp
RuntimeDefault. Its only writable path is the `logs` emptyDir (tail DB +
filesystem buffer). Resource defaults 50m/64Mi requests, 200m/128Mi limits,
overridable via `logging.resources`.

Builder shape follows convention: pure `builders/loggingsidecar.go` returning
the container + volume + the rendered `fluent-bit.conf` string from the
defaulted spec; the StatefulSet builder appends them when
`Spec.Logging.Enabled`.

## Testing

- **Builders (unit):** sidecar container/volume/mounts present only when
  enabled; security context fields exact; `fluent-bit.conf` golden-rendered
  per sink (stdout/s3/http), env wiring from secret refs; cnf gains eventslog
  variables iff `queryLog`; checksum covers both keys.
- **envtest:** enabling/disabling `spec.logging` adds/removes the sidecar and
  rolls the checksum; admission rejections (missing sink block, queryLog off).
- **e2e (kind):** stdout-sink scenario — enable `logging` + `queryLog`, run a
  marker query through 6033, assert it appears in
  `kubectl logs <pod> -c fluent-bit`. S3/HTTP sinks stay unit/golden-tested
  (no external infra in CI).

## Non-goals

- Log aggregation infrastructure (Loki/Elastic/etc.) — bring your own sink.
- Parsing, enrichment, or filtering pipelines beyond JSON passthrough.
- Shipping ProxySQL's error log — it is already on stderr/`kubectl logs`.
- PostgreSQL-protocol query logging: ProxySQL 3.x eventslog coverage for the
  pgsql frontend is unverified; if/when it exists, it is the same plumbing
  (`pgsql_variables` eventslog settings + a second tail path) — tracked as a
  follow-up, not designed here.
