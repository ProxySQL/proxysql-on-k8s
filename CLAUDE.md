# CLAUDE.md

Guidance for Claude Code when working in this repository.

## What this repo is

ProxySQL on Kubernetes, v2. Three layers:

1. **Helm charts** (`charts/`):
   - `proxysql-operator/` — operator install (CRDs + manager + RBAC). Most users want this.
   - `proxysql/` — standalone data-plane chart (Deployment). Operator-less.
   - `proxysql-cluster/` — standalone control-plane chart (StatefulSet + PVC). Operator-less.
2. **Kubebuilder operator** (`operator/`, module `github.com/ProxySQL/kubernetes/operator`):
   - API group `proxysql.com/v1alpha1` with two CRDs: `ProxySQLCluster` (the pods) and `ProxySQLConfig` (the SQL config pushed to them).
   - Two reconcilers, a builders package, one MySQL-wire SQL client (`internal/proxysqlclient`).
3. **Examples** (`examples/`) — backend cookbook entries (Oracle MySQL Operator, Percona PS/PXC, MariaDB Operator, CloudNativePG, Crunchy PGO) plus shared sysbench/pgbench Jobs in `examples/loadgen/`.

`docs/architecture.md` is the source of truth for design rationale and reconcile-loop pseudocode. `docs/migration-from-v1.md` maps v1 chart/value names to this repo's layout. User-facing documentation lives under `docs/` in four layers — `docs/quickstart.md`, `docs/tutorials/`, `docs/user-guide/`, `docs/reference/` — indexed by `docs/README.md`; behavior changes in the operator usually need a matching reference/user-guide update.

## Common commands

Top-level `Makefile`:
```
make lint                  # helm lint every chart
make template              # render every chart (sanity)
make kubeconform           # render + kubeconform schema validation
make sync-crds             # regenerate CRDs and copy them into the operator chart
make operator-image        # build the operator container (single-arch, local docker)
make operator-image-multi  # multi-arch buildx push to IMG
make operator-image-kind   # build + kind load docker-image
make kind-up / kind-down   # local kind cluster
make e2e                   # full kind e2e suite (test/e2e/run.sh)
```

Operator's own Makefile (`cd operator`):
```
make build                 # go build
make test                  # go test ./... — auto-downloads setup-envtest binaries
make lint                  # golangci-lint (pinned via operator's Makefile)
make manifests             # regenerate CRDs into operator/config/crd/bases/
make run                   # run the manager locally against the current kubectx
```

Go toolchain: this repo requires Go 1.25+. The system Go on the dev machine may be older, so commands prefix with `GOTOOLCHAIN=go1.25.10` to let Go auto-fetch the right toolchain. CI uses `actions/setup-go@v5` with `go-version-file: operator/go.mod`.

## Critical conventions

### CRDs live in two places — keep them in sync

Generated CRD YAML lives in `operator/config/crd/bases/`. The operator Helm chart bundles a *copy* under `charts/proxysql-operator/crds/`. The `make sync-crds` target regenerates and copies in one step. **Never** edit the chart copy by hand — it'll drift the next time `make manifests` runs.

### `*bool` for `Enabled` fields that default to true

`PersistenceSpec.Enabled`, `MetricsSpec.Enabled`, `PodDisruptionBudget.Enabled` are all `*bool`, not `bool`. With plain `bool + omitempty`, sending `enabled: false` in YAML marshals away and the CRD's `+kubebuilder:default=true` re-defaults it to true — so users literally cannot disable persistence/metrics/PDB. If you add a new boolean field that defaults to true, use `*bool`. `ServiceMonitor.Enabled` and similar "default-off" booleans can stay as plain `bool` because the zero value is the intended default.

### Builders are pure

Everything under `operator/internal/controller/builders/` returns a desired-state object given a defaulted spec — no K8s client calls, no I/O. Reconcilers do the diff/apply. This is intentional and keeps the builders trivially unit-testable.

### `proxysqlclient.Sync` takes an `Executor` interface

So tests can substitute a recording fake and verify exact SQL emitted. **Don't** introduce a concrete `*Client` dependency inside `sync.go` — it'll break the tests in `sync_test.go`.

### Bootstrap cnf contains passwords — it lives in a Secret

The rendered proxysql.cnf embeds the admin/radmin/monitor passwords, so it ships in the `<cluster>-cnf` Secret (builder: `builders/cnf_secret.go`; the `-cnf` suffix avoids colliding with the auth Secret named `<cluster>`). Don't move it back to a ConfigMap. The reconciler still garbage-collects the legacy `<cluster>` cnf ConfigMap from operator versions < v0.3.0 — that's why the RBAC keeps `configmaps: get;list;watch;delete` (and nothing more).

### ProxySQL admin port speaks MySQL wire protocol

`internal/proxysqlclient/client.go` uses `github.com/go-sql-driver/mysql` against ProxySQL's admin port (default 6032). Both MySQL- and PostgreSQL-protocol clients connecting to the *data* plane use 6033/6133 respectively, but the *admin* plane is always MySQL-wire. Don't be fooled by `pgsql_*` table names — those are admin tables, you query them with the MySQL driver.

### Write-to-all, not cluster-sync-only

The operator pushes `ProxySQLConfig` SQL to every ready replica directly. ProxySQL Cluster sync also runs (when replicas > 1) as a belt-and-braces backup, but the operator-driven writes are what `status.syncedReplicas` tracks. See `docs/architecture.md` for why.

### PSA `restricted` everywhere

Every pod the operator/charts produce runs as: `runAsNonRoot=true`, uid/gid 999, `readOnlyRootFilesystem=true`, drop all caps, RuntimeDefault seccomp. If a change requires loosening any of these, find another way.

### Trivy gates CI on new CVEs — bump, don't suppress

The `operator / image build + trivy` job fails on any HIGH/CRITICAL fixable
CVE in the built image, and its vulnerability DB updates continuously — so a
freshly published advisory can turn CI red on a commit that changed nothing
(it has: `golang.org/x/net` 2026-07-21, `grpc-go` 2026-07-22). The response
is always a dependency bump, never a `.trivyignore`:

```
cd operator && GOTOOLCHAIN=go1.25.10 go get <module>@<fixed-version> && go mod tidy
GOTOOLCHAIN=go1.25.10 go build ./... && GOTOOLCHAIN=go1.25.10 go test ./...
```

Land it on `main` via a minimal PR even when feature branches carry the same
bump — identical `go.mod` changes merge cleanly, and an unpatched `main`
fails Trivy on its next push. The runtime risk is usually nil (the operator
runs no gRPC/HTTP servers beyond metrics + the admin-port MySQL client), but
the gate is deliberate: keeping the image scanner-clean is part of the
contract for people deploying it.

### Upgrade stability: an unchanged spec must render byte-identical output

`operator/internal/controller/builders/golden_test.go` (`TestGolden`) pins
the rendered bootstrap cnf and StatefulSet pod template for a fixed
reference `ProxySQLCluster` spec against committed goldens under
`testdata/golden/`. Since builders are pure (see above), a diff here for an
unchanged spec means every existing cluster gets a one-time rolling restart
on the next reconcile after an operator upgrade — that's the #58
upgrade-stability policy this test gates. If a change legitimately alters
the rendered output (new default, security-context tightening, bugfix),
regenerate with `UPDATE_GOLDEN=1 go test ./internal/controller/builders/ -run TestGolden`,
review the diff, commit it, and add a release-note entry describing the
expected restart (the v0.5.0 `--reload` rollout is the template for that
note). Never regenerate goldens to silence a failure without understanding
why the output changed.

## Branch policy

Default branch is `main`. The legacy v1 demo charts live in the separate repo [`ProxySQL/kubernetes`](https://github.com/ProxySQL/kubernetes); fixes back-ported there don't apply here.

## Where to put new things

| Adding… | Goes in… |
| --- | --- |
| A new field on `ProxySQLClusterSpec` / `ProxySQLConfigSpec` | `operator/api/v1alpha1/*_types.go` — then `make manifests && make sync-crds` |
| A new admin SQL table to push | `operator/internal/proxysqlclient/sync.go` (and a `Desired` field in `types.go`) |
| A new K8s object owned by `ProxySQLCluster` | `operator/internal/controller/builders/<thing>.go` + wire `ensure*` in the reconciler |
| A new backend cookbook | `examples/<family>/<flavor>/{README.md,backend.yaml,proxysql.yaml}` |
| A new CI check | `.github/workflows/ci.yaml` — separate job, runs in parallel with the rest |
