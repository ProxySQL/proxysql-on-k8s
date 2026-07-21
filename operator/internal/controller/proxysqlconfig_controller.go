/*
Copyright 2026 ProxySQL.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	proxysqlv1alpha1 "github.com/ProxySQL/kubernetes/operator/api/v1alpha1"
	"github.com/ProxySQL/kubernetes/operator/internal/controller/builders"
	"github.com/ProxySQL/kubernetes/operator/internal/proxysqlclient"
)

// ProxySQLConfigReconciler translates a declarative ProxySQLConfig into SQL
// writes on the target ProxySQLCluster's admin port.
type ProxySQLConfigReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// ResyncInterval bounds how long out-of-band runtime drift can persist
	// before the reconciler reads runtime state back from each replica and
	// re-pushes only the ones that drifted. Zero means use the default
	// (defaultDriftResyncInterval).
	ResyncInterval time.Duration
}

// resyncInterval returns the configured interval, or the default when unset.
func (r *ProxySQLConfigReconciler) resyncInterval() time.Duration {
	if r.ResyncInterval > 0 {
		return r.ResyncInterval
	}
	return defaultDriftResyncInterval
}

// +kubebuilder:rbac:groups=proxysql.com,resources=proxysqlconfigs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=proxysql.com,resources=proxysqlconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=proxysql.com,resources=proxysqlconfigs/finalizers,verbs=update
// +kubebuilder:rbac:groups=proxysql.com,resources=proxysqlclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

const (
	cfgCondReady        = "Ready"
	cfgCondProgressing  = "Progressing"
	cfgCondDegraded     = "Degraded"
	cfgCondClusterFound = "ClusterFound"

	// cfgFinalizer guards ProxySQLConfig deletion: the operator clears the
	// managed admin tables on every ready replica before letting the CR go.
	cfgFinalizer = "proxysql.com/config-cleanup"
	// skipCleanupAnnotation ("true") skips the SQL cleanup on deletion. The
	// escape hatch when the cluster is wedged or unreachable forever.
	skipCleanupAnnotation = "proxysql.com/skip-cleanup"

	// requeueAfterSuccess: after a successful sync, requeue at this cadence as
	// a safety net (catches Pod restarts that wiped runtime tables before
	// ProxySQL Cluster sync caught up).
	requeueAfterSuccess = 30 * time.Second
	// requeueAfterTransient: after a transient failure (cluster not found,
	// admin unreachable on some replicas), retry sooner.
	requeueAfterTransient = 5 * time.Second
	// driftResyncInterval bounds how long out-of-band runtime drift can persist.
	// The hash short-circuit skips the SQL push when desired config, replica
	// set, and generation are unchanged — which is cheap but means a pod whose
	// runtime tables were mutated externally (or never converged) would never
	// be corrected. To stay level-based, once this much wall-clock has elapsed
	// since the last sync we read runtime state back from every replica and
	// re-push only the ones that drifted (bypassing the short-circuit).
	defaultDriftResyncInterval = 2 * time.Minute
)

// Reconcile pushes ProxySQLConfig to the target cluster.
//
// Order of operations:
//  1. Fetch the CR.
//  2. Resolve .spec.clusterRef → ProxySQLCluster in the same namespace.
//  3. Read the cluster's admin Secret to obtain the admin password.
//  4. Resolve each MySQL/PostgreSQL user's password from its referenced Secret.
//  5. Discover ready ProxySQL pods via label selector.
//  6. Compute a SHA-256 over the resolved Desired; short-circuit if it matches
//     status.LastAppliedHash AND every ready pod was synced AND the drift
//     resync interval hasn't elapsed. When only the interval elapsed, read
//     runtime state back from each replica and narrow the push to the
//     replicas that actually drifted (read-back failure counts as drift).
//  7. For each target pod, open a SQL connection to its admin port and run
//     the full Sync (DELETE/INSERT/LOAD/SAVE per table + variables).
//  8. Update status (LastAppliedHash, LastSyncTime, SyncedReplicas,
//     DriftedReplicas, ShunnedBackends, LastRuntimeCheckTime, Conditions).
//
// Write strategy: write-to-all. ProxySQL Cluster sync would also propagate
// changes, but explicitly writing to every replica makes SyncedReplicas
// accurate and surfaces unreachable pods immediately.
func (r *ProxySQLConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var cfg proxysqlv1alpha1.ProxySQLConfig
	if err := r.Get(ctx, req.NamespacedName, &cfg); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !cfg.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &cfg)
	}
	if controllerutil.AddFinalizer(&cfg, cfgFinalizer) {
		if err := r.Update(ctx, &cfg); err != nil {
			return ctrl.Result{}, err
		}
	}

	// 1) Resolve target cluster.
	var cluster proxysqlv1alpha1.ProxySQLCluster
	clusterKey := types.NamespacedName{Name: cfg.Spec.ClusterRef.Name, Namespace: cfg.Namespace}
	if err := r.Get(ctx, clusterKey, &cluster); err != nil {
		if apierrors.IsNotFound(err) {
			r.setCfgCondition(&cfg, cfgCondClusterFound, metav1.ConditionFalse, "NotFound",
				fmt.Sprintf("ProxySQLCluster %q not found in namespace %q", cfg.Spec.ClusterRef.Name, cfg.Namespace))
			r.setCfgCondition(&cfg, cfgCondReady, metav1.ConditionFalse, "ClusterMissing", "target cluster is missing")
			_ = r.Status().Update(ctx, &cfg)
			return ctrl.Result{RequeueAfter: requeueAfterTransient}, nil
		}
		return ctrl.Result{}, err
	}
	r.setCfgCondition(&cfg, cfgCondClusterFound, metav1.ConditionTrue, "Found", "ProxySQLCluster resolved")

	b := builders.New(&cluster, r.Scheme, builders.Passwords{})
	keys := b.SecretKeys()
	adminPort := b.Spec.Protocols.Admin.Port

	// pgsql tables on a cluster that isn't listening on pgsql is almost
	// certainly a user error; we still push (the admin tables exist either
	// way) but surface it loudly.
	pgsqlMismatch := pgsqlConfigured(&cfg) && !b.Spec.Protocols.PostgreSQL.IsEnabled()
	if pgsqlMismatch {
		r.setCfgCondition(&cfg, cfgCondDegraded, metav1.ConditionTrue, "PgsqlDisabled",
			"spec declares pgsql servers/users/rules but the referenced cluster has protocols.pgsql.enabled=false")
	}

	// 2) Read cluster admin Secret. We connect with the "radmin" account, not
	// "admin": ProxySQL hardcodes the "admin" user to localhost-only, so a
	// remote (pod-network) admin connection as "admin" is rejected with
	// "User 'admin' can only connect locally". radmin is the remote-capable
	// admin user the operator mints (and also uses for cluster sync).
	var adminSec corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: b.SecretName(), Namespace: cluster.Namespace}, &adminSec); err != nil {
		r.setCfgCondition(&cfg, cfgCondReady, metav1.ConditionFalse, "AdminSecretMissing", err.Error())
		_ = r.Status().Update(ctx, &cfg)
		return ctrl.Result{RequeueAfter: requeueAfterTransient}, nil
	}
	adminPw, pwErr := builders.PasswordsFromSecret(adminSec.Data, keys)
	if pwErr != nil {
		r.setCfgCondition(&cfg, cfgCondReady, metav1.ConditionFalse, "AdminSecretIncomplete", pwErr.Error())
		_ = r.Status().Update(ctx, &cfg)
		return ctrl.Result{RequeueAfter: requeueAfterTransient}, nil
	}
	radminPassword := adminPw.Radmin

	// 3) Resolve user passwords (mysql_users + pgsql_users) from Secrets.
	desired, err := r.buildDesired(ctx, &cfg, b)
	if err != nil {
		r.setCfgCondition(&cfg, cfgCondReady, metav1.ConditionFalse, "UserSecretError", err.Error())
		_ = r.Status().Update(ctx, &cfg)
		return ctrl.Result{RequeueAfter: requeueAfterTransient}, nil
	}

	// 4) Discover ready ProxySQL pods.
	addrs, err := discoverPodAddresses(ctx, r.Client, &cluster, adminPort)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(addrs) == 0 {
		r.setCfgCondition(&cfg, cfgCondReady, metav1.ConditionFalse, "NoReadyReplicas",
			"no ready ProxySQL pods to push config to")
		_ = r.Status().Update(ctx, &cfg)
		return ctrl.Result{RequeueAfter: requeueAfterTransient}, nil
	}

	// 5) Compute a fingerprint over (desired config + the exact pod set) for
	// the short-circuit. Including the address set means a recreated pod (new
	// IP, empty runtime tables) changes the fingerprint and forces a re-push.
	//
	// We skip the SQL push only when nothing observable changed AND we asserted
	// desired state recently. The driftResyncInterval clause keeps the operator
	// level-based: without it, runtime drift that doesn't change the spec/
	// replica-set/generation (e.g. an externally-mutated admin table) would be
	// skipped forever, since the hash and replica count still match.
	hash := syncFingerprint(desired, addrs)
	unchanged := cfg.Status.LastAppliedHash == hash &&
		cfg.Status.SyncedReplicas == int32(len(addrs)) &&
		cfg.Status.ObservedGeneration == cfg.Generation
	dueForResync := resyncDue(cfg.Status.LastSyncTime, time.Now(), r.resyncInterval())

	if unchanged && !dueForResync {
		log.V(1).Info("ProxySQLConfig unchanged; skipping SQL push", "hash", hash, "replicas", len(addrs))
		return ctrl.Result{RequeueAfter: requeueAfterSuccess}, nil
	}

	pushAddrs := addrs
	if unchanged && dueForResync {
		// Informed resync: nothing about the spec or replica set changed, so
		// instead of blind-pushing everything, read runtime state back and
		// re-push only the replicas that actually drifted.
		drifted, shunned := r.verifyReplicas(ctx, addrs, radminPassword, desired)
		now := metav1.NewTime(time.Now())
		cfg.Status.LastRuntimeCheckTime = &now
		cfg.Status.ShunnedBackends = shunned
		cfg.Status.DriftedReplicas = int32(len(drifted))
		if len(drifted) == 0 {
			// Converged everywhere: verification counts as asserting desired
			// state, so advance the resync clock.
			cfg.Status.LastSyncTime = &now
			if err := r.Status().Update(ctx, &cfg); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: requeueAfterSuccess}, nil
		}
		pushAddrs = drifted
	}

	// 6) Fan out writes.
	synced, syncErrs := r.applyToReplicas(ctx, pushAddrs, radminPassword, desired)

	// 7) Status.
	cfg.Status.ObservedGeneration = cfg.Generation
	if synced == len(pushAddrs) && len(syncErrs) == 0 {
		cfg.Status.SyncedReplicas = int32(len(addrs))
		cfg.Status.DriftedReplicas = 0
		cfg.Status.LastAppliedHash = hash
		now := metav1.NewTime(time.Now())
		cfg.Status.LastSyncTime = &now
		r.setCfgCondition(&cfg, cfgCondReady, metav1.ConditionTrue, "Synced",
			fmt.Sprintf("config applied to %d/%d replicas", len(addrs), len(addrs)))
		r.setCfgCondition(&cfg, cfgCondProgressing, metav1.ConditionFalse, "Steady", "")
		// The PgsqlDisabled Degraded condition was already set above when it
		// applies; only clear Degraded when it doesn't.
		if !pgsqlMismatch {
			meta.RemoveStatusCondition(&cfg.Status.Conditions, cfgCondDegraded)
		}
		if err := r.Status().Update(ctx, &cfg); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueAfterSuccess}, nil
	}

	// Partial or full failure.
	cfg.Status.SyncedReplicas = int32(len(addrs) - len(pushAddrs) + synced)
	r.setCfgCondition(&cfg, cfgCondReady, metav1.ConditionFalse, "PartialSync",
		fmt.Sprintf("synced %d/%d replicas (%d re-push targets, %d succeeded)",
			len(addrs)-len(pushAddrs)+synced, len(addrs), len(pushAddrs), synced))
	// Degraded is a single condition: when a pgsql mismatch coexists with sync
	// errors, fold the warning into the message so it isn't silently dropped.
	degradedMsg := joinErrs(syncErrs)
	if pgsqlMismatch {
		degradedMsg = "pgsql declared but protocols.pgsql.enabled=false on cluster; " + degradedMsg
	}
	r.setCfgCondition(&cfg, cfgCondDegraded, metav1.ConditionTrue, "SyncErrors", degradedMsg)
	if err := r.Status().Update(ctx, &cfg); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueAfterTransient}, nil
}

// autoPopulatedPeerComment marks proxysql_servers rows the operator derived
// from the target cluster's StatefulSet pods (spec.proxysqlServers was empty).
const autoPopulatedPeerComment = "operator-populated from ProxySQLCluster pods"

// buildDesired translates the K8s spec into the resolved Desired struct
// the proxysqlclient package operates on. b is the target cluster's builder
// (defaulted spec): when spec.proxysqlServers is empty and the cluster runs
// more than one replica, the peer list is auto-populated from the cluster's
// stable per-pod DNS names so the sync doesn't DELETE the cnf-seeded
// proxysql_servers table and silently disable ProxySQL Cluster sync (#39).
func (r *ProxySQLConfigReconciler) buildDesired(ctx context.Context, cfg *proxysqlv1alpha1.ProxySQLConfig, b *builders.Builder) (*proxysqlclient.Desired, error) {
	d := &proxysqlclient.Desired{
		AdminVariables:      cfg.Spec.AdminVariables,
		MySQLVariables:      cfg.Spec.MySQLVariables,
		PostgreSQLVariables: cfg.Spec.PostgreSQLVariables,
		SQLStatements:       append([]string(nil), cfg.Spec.SQLStatements...),
	}

	for _, s := range cfg.Spec.MySQLServers {
		d.MySQLServers = append(d.MySQLServers, proxysqlclient.MySQLServer{
			Hostgroup: s.Hostgroup, Hostname: s.Hostname, Port: s.Port,
			Weight: s.Weight, MaxConnections: s.MaxConnections,
			MaxReplicationLag: s.MaxReplicationLag, UseSSL: s.UseSSL, Comment: s.Comment,
		})
	}
	for _, h := range cfg.Spec.MySQLReplicationHostgroups {
		d.MySQLReplicationHostgroups = append(d.MySQLReplicationHostgroups, proxysqlclient.MySQLReplicationHostgroup{
			WriterHostgroup: h.WriterHostgroup, ReaderHostgroup: h.ReaderHostgroup,
			CheckType: h.CheckType, Comment: h.Comment,
		})
	}
	for _, a := range cfg.Spec.MySQLHostgroupAttributes {
		d.MySQLHostgroupAttributes = append(d.MySQLHostgroupAttributes, proxysqlclient.MySQLHostgroupAttributes{
			Hostgroup:           a.Hostgroup,
			MaxNumOnlineServers: a.MaxNumOnlineServers, Autocommit: a.Autocommit,
			FreeConnectionsPct: a.FreeConnectionsPct, InitConnect: a.InitConnect,
			Multiplex: a.Multiplex, ConnectionWarming: a.ConnectionWarming,
			ThrottleConnectionsPerSec: a.ThrottleConnectionsPerSec,
			IgnoreSessionVariables:    a.IgnoreSessionVariables,
			Comment:                   a.Comment,
		})
	}
	for _, r2 := range cfg.Spec.MySQLQueryRules {
		d.MySQLQueryRules = append(d.MySQLQueryRules, proxysqlclient.MySQLQueryRule{
			RuleID: r2.RuleID, Active: r2.Active, Username: r2.Username,
			SchemaName: r2.SchemaName, FlagIn: r2.FlagIn,
			MatchPattern: r2.MatchPattern, MatchDigest: r2.MatchDigest,
			FlagOut: r2.FlagOut, ReplacePattern: r2.ReplacePattern,
			DestinationHostgroup: r2.DestinationHostgroup,
			CacheTTL:             r2.CacheTTL, CacheEmptyResult: r2.CacheEmptyResult,
			Timeout: r2.Timeout, Delay: r2.Delay, MirrorHostgroup: r2.MirrorHostgroup,
			ErrorMessage: r2.ErrorMessage, Log: r2.Log,
			Apply: r2.Apply, Comment: r2.Comment,
		})
	}
	for _, u := range cfg.Spec.MySQLUsers {
		pw, err := r.resolveSecretKey(ctx, cfg.Namespace, u.PasswordSecretRef)
		if err != nil {
			return nil, fmt.Errorf("mysql user %q: %w", u.Username, err)
		}
		d.MySQLUsers = append(d.MySQLUsers, proxysqlclient.MySQLUser{
			Username: u.Username, Password: pw,
			DefaultHostgroup: u.DefaultHostgroup, Active: u.Active,
			MaxConnections: u.MaxConnections, UseSSL: u.UseSSL,
			DefaultSchema: u.DefaultSchema, TransactionPersistent: u.TransactionPersistent,
			Comment: u.Comment,
		})
	}
	for _, s := range cfg.Spec.PostgreSQLServers {
		d.PostgreSQLServers = append(d.PostgreSQLServers, proxysqlclient.PostgreSQLServer{
			Hostgroup: s.Hostgroup, Hostname: s.Hostname, Port: s.Port,
			Weight: s.Weight, MaxConnections: s.MaxConnections, Comment: s.Comment,
		})
	}
	for _, u := range cfg.Spec.PostgreSQLUsers {
		pw, err := r.resolveSecretKey(ctx, cfg.Namespace, u.PasswordSecretRef)
		if err != nil {
			return nil, fmt.Errorf("pgsql user %q: %w", u.Username, err)
		}
		d.PostgreSQLUsers = append(d.PostgreSQLUsers, proxysqlclient.PostgreSQLUser{
			Username: u.Username, Password: pw,
			DefaultHostgroup: u.DefaultHostgroup, Active: u.Active, Comment: u.Comment,
		})
	}
	for _, r2 := range cfg.Spec.PostgreSQLQueryRules {
		d.PostgreSQLQueryRules = append(d.PostgreSQLQueryRules, proxysqlclient.PostgreSQLQueryRule{
			RuleID: r2.RuleID, Active: r2.Active, FlagIn: r2.FlagIn,
			MatchPattern: r2.MatchPattern,
			FlagOut:      r2.FlagOut, ReplacePattern: r2.ReplacePattern,
			DestinationHostgroup: r2.DestinationHostgroup,
			CacheTTL:             r2.CacheTTL, CacheEmptyResult: r2.CacheEmptyResult,
			Timeout: r2.Timeout, Delay: r2.Delay, MirrorHostgroup: r2.MirrorHostgroup,
			ErrorMessage: r2.ErrorMessage, Log: r2.Log,
			Apply: r2.Apply, Comment: r2.Comment,
		})
	}
	for _, s := range cfg.Spec.ProxySQLServers {
		d.ProxySQLServers = append(d.ProxySQLServers, proxysqlclient.ProxySQLServer{
			Hostname: s.Hostname, Port: s.Port, Weight: s.Weight, Comment: s.Comment,
		})
	}
	if len(cfg.Spec.ProxySQLServers) == 0 {
		// Auto-populate the peer table (documented CRD behavior, #39).
		// ProxySQLServerDNS returns the same stable per-pod DNS names the
		// bootstrap cnf seeds, and returns nil when the defaulted replica
		// count is <= 1 — there an empty peer table (DELETE) is correct.
		for _, host := range b.ProxySQLServerDNS() {
			d.ProxySQLServers = append(d.ProxySQLServers, proxysqlclient.ProxySQLServer{
				Hostname: host,
				Port:     b.Spec.Protocols.Admin.Port,
				Weight:   0,
				Comment:  autoPopulatedPeerComment,
			})
		}
	}
	return d, nil
}

// resolveSecretKey reads ns/sel.Name and returns the value at sel.Key, or
// "" with an error if missing.
func (r *ProxySQLConfigReconciler) resolveSecretKey(ctx context.Context, ns string, sel corev1.SecretKeySelector) (string, error) {
	var sec corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: sel.Name, Namespace: ns}, &sec); err != nil {
		return "", fmt.Errorf("get secret %q: %w", sel.Name, err)
	}
	v, ok := sec.Data[sel.Key]
	if !ok {
		return "", fmt.Errorf("secret %q has no key %q", sel.Name, sel.Key)
	}
	return string(v), nil
}

// pgsqlConfigured reports whether the spec declares any pgsql-side state.
func pgsqlConfigured(cfg *proxysqlv1alpha1.ProxySQLConfig) bool {
	return len(cfg.Spec.PostgreSQLServers)+len(cfg.Spec.PostgreSQLUsers)+len(cfg.Spec.PostgreSQLQueryRules) > 0
}

// applyToReplicas opens a connection to each addr and runs Sync. Returns the
// count of replicas that synced successfully, plus the per-addr errors.
// Connections use the "radmin" account — ProxySQL restricts "admin" to
// localhost, so remote (pod-network) admin connections must use radmin.
func (r *ProxySQLConfigReconciler) applyToReplicas(ctx context.Context, addrs []string, password string, d *proxysqlclient.Desired) (int, []error) {
	log := logf.FromContext(ctx)
	var ok int
	var errs []error
	for _, addr := range addrs {
		pxc, err := proxysqlclient.New(addr, "radmin", password)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", addr, err))
			continue
		}
		if err := proxysqlclient.Sync(ctx, pxc, d); err != nil {
			log.Error(err, "sync failed", "addr", addr)
			errs = append(errs, err)
			_ = pxc.Close()
			continue
		}
		_ = pxc.Close()
		ok++
	}
	return ok, errs
}

// verifyReplicas reads runtime state back from each replica and returns the
// addresses whose state drifted from desired, plus the total SHUNNED backend
// count. A replica whose read-back fails is treated as drifted: we cannot
// prove it converged, so it goes back through the push path.
func (r *ProxySQLConfigReconciler) verifyReplicas(ctx context.Context, addrs []string, password string, d *proxysqlclient.Desired) (drifted []string, shunned int32) {
	log := logf.FromContext(ctx)
	for _, addr := range addrs {
		pxc, err := proxysqlclient.New(addr, "radmin", password)
		if err != nil {
			drifted = append(drifted, addr)
			continue
		}
		rs, err := proxysqlclient.ReadRuntime(ctx, pxc)
		_ = pxc.Close()
		if err != nil {
			log.V(1).Info("runtime read-back failed; treating replica as drifted", "addr", addr, "error", err.Error())
			drifted = append(drifted, addr)
			continue
		}
		shunned += rs.ShunnedCount()
		if diffs := d.Drift(rs); len(diffs) > 0 {
			log.Info("runtime drift detected", "addr", addr, "diffs", joinTrunc(diffs, 256))
			drifted = append(drifted, addr)
		}
	}
	return drifted, shunned
}

// finalize clears the managed admin tables on every ready replica, then
// releases the finalizer. Policy: never wedge deletion when the operator
// cannot possibly clean up — absent cluster, absent admin Secret, or a Secret
// matching no accepted credential schema all mean we can't authenticate, and
// holding the finalizer forever is worse than leaking config. DO hold the
// finalizer while the cluster exists with no ready pods (releasing would leak
// config onto pods that come back); the skip-cleanup annotation is the escape
// hatch for every stuck-deletion case.
func (r *ProxySQLConfigReconciler) finalize(ctx context.Context, cfg *proxysqlv1alpha1.ProxySQLConfig) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(cfg, cfgFinalizer) {
		return ctrl.Result{}, nil
	}
	if cfg.Annotations[skipCleanupAnnotation] == "true" {
		log.Info("skip-cleanup annotation set; releasing finalizer without cleanup")
		return r.releaseFinalizer(ctx, cfg)
	}

	var cluster proxysqlv1alpha1.ProxySQLCluster
	clusterKey := types.NamespacedName{Name: cfg.Spec.ClusterRef.Name, Namespace: cfg.Namespace}
	if err := r.Get(ctx, clusterKey, &cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return r.releaseFinalizer(ctx, cfg)
		}
		return ctrl.Result{}, err
	}

	b := builders.New(&cluster, r.Scheme, builders.Passwords{})
	var adminSec corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: b.SecretName(), Namespace: cluster.Namespace}, &adminSec); err != nil {
		if apierrors.IsNotFound(err) {
			return r.releaseFinalizer(ctx, cfg)
		}
		return ctrl.Result{}, err
	}
	adminPw, pwErr := builders.PasswordsFromSecret(adminSec.Data, b.SecretKeys())
	if pwErr != nil {
		// Cannot authenticate ⇒ never wedge deletion: release without cleanup.
		log.Info("admin secret matches no accepted schema; releasing finalizer without cleanup",
			"secret", adminSec.Name, "error", pwErr.Error())
		return r.releaseFinalizer(ctx, cfg)
	}
	radminPassword := adminPw.Radmin

	addrs, err := discoverPodAddresses(ctx, r.Client, &cluster, b.Spec.Protocols.Admin.Port)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(addrs) == 0 {
		log.Info("cleanup pending: cluster exists but has no ready pods; retrying",
			"cluster", cluster.Name, "escapeHatch", skipCleanupAnnotation)
		return ctrl.Result{RequeueAfter: requeueAfterTransient}, nil
	}

	// An empty Desired DELETEs every managed table and LOAD/SAVEs each
	// section. Variables are left as-is: ProxySQL has no "unset", and
	// resetting values blind would be worse than leaving them.
	cleaned, errs := r.applyToReplicas(ctx, addrs, radminPassword, &proxysqlclient.Desired{})
	if cleaned != len(addrs) {
		log.Info("cleanup incomplete; retrying", "cleaned", cleaned, "total", len(addrs), "errors", joinErrs(errs))
		return ctrl.Result{RequeueAfter: requeueAfterTransient}, nil
	}
	return r.releaseFinalizer(ctx, cfg)
}

func (r *ProxySQLConfigReconciler) releaseFinalizer(ctx context.Context, cfg *proxysqlv1alpha1.ProxySQLConfig) (ctrl.Result, error) {
	if controllerutil.RemoveFinalizer(cfg, cfgFinalizer) {
		if err := r.Update(ctx, cfg); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *ProxySQLConfigReconciler) setCfgCondition(cfg *proxysqlv1alpha1.ProxySQLConfig, t string, s metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
		Type:               t,
		Status:             s,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: cfg.Generation,
	})
}

// syncFingerprint returns a stable SHA-256 over the JSON-marshaled Desired
// AND the set of pod addresses it was applied to. Used to short-circuit when
// nothing has changed since the last successful sync.
//
// The address set is part of the fingerprint on purpose: SyncedReplicas is a
// count, so it can't distinguish "same 3 pods" from "one pod was recreated
// with a new IP and empty runtime tables." Folding the (sorted, deterministic)
// addresses in means a membership change busts the short-circuit and forces a
// re-push to the fresh pod, rather than waiting for the safety requeue.
//
// On marshal failure it returns a sentinel that cannot equal a real hex SHA,
// so the short-circuit can never spuriously match an unset ("") status.
func syncFingerprint(d *proxysqlclient.Desired, addrs []string) string {
	b, err := json.Marshal(d)
	if err != nil {
		return "marshal-error"
	}
	h := sha256.New()
	h.Write(b)
	// addrs is already sorted by discoverPodAddresses; a NUL separator keeps
	// the boundaries unambiguous.
	for _, a := range addrs {
		h.Write([]byte{0})
		h.Write([]byte(a))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// resyncDue reports whether a drift check is overdue: true if we've never
// synced (last == nil) or at least interval has elapsed since the last time
// desired state was asserted (written or verified). This forces the reconciler
// past the hash short-circuit periodically so externally-drifted runtime state
// is detected and re-pushed (level-based reconciliation) rather than skipped
// forever.
func resyncDue(last *metav1.Time, now time.Time, interval time.Duration) bool {
	return last == nil || now.Sub(last.Time) >= interval
}

func joinErrs(errs []error) string {
	if len(errs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(errs))
	for _, e := range errs {
		parts = append(parts, e.Error())
	}
	return joinTrunc(parts, 512)
}

// joinTrunc joins parts with "; " and truncates to max characters so the
// Conditions message doesn't blow past the K8s 1MiB resource size limit
// when every replica is failing.
func joinTrunc(parts []string, max int) string {
	var b strings.Builder
	for i, p := range parts {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(p)
		if b.Len() > max {
			s := b.String()
			return s[:max] + "..."
		}
	}
	return b.String()
}

// SetupWithManager wires the controller into the manager.
// Watches:
//   - ProxySQLConfig (primary).
//   - ProxySQLCluster: re-reconcile any configs targeting a cluster when it
//     changes (status flip, admin secret rotation, replica count change).
//   - Pods: re-reconcile when a ProxySQL pod becomes Ready or restarts, so
//     fresh pods get config pushed without waiting for the 30s safety requeue.
//   - Secrets: re-reconcile configs when a referenced password Secret or the
//     target cluster's admin Secret changes, so rotation converges immediately.
func (r *ProxySQLConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&proxysqlv1alpha1.ProxySQLConfig{}).
		Watches(&proxysqlv1alpha1.ProxySQLCluster{}, handler.EnqueueRequestsFromMapFunc(r.configsForCluster)).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.configsForPod)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.configsForSecret)).
		Named("proxysqlconfig").
		Complete(r)
}

func (r *ProxySQLConfigReconciler) configsForCluster(ctx context.Context, obj client.Object) []reconcile.Request {
	cluster, ok := obj.(*proxysqlv1alpha1.ProxySQLCluster)
	if !ok {
		return nil
	}
	var configs proxysqlv1alpha1.ProxySQLConfigList
	if err := r.List(ctx, &configs, client.InNamespace(cluster.Namespace)); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, c := range configs.Items {
		if c.Spec.ClusterRef.Name == cluster.Name {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Name: c.Name, Namespace: c.Namespace}})
		}
	}
	return out
}

func (r *ProxySQLConfigReconciler) configsForPod(ctx context.Context, obj client.Object) []reconcile.Request {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil
	}
	clusterName := pod.Labels["proxysql.com/cluster"]
	if clusterName == "" {
		return nil
	}
	var configs proxysqlv1alpha1.ProxySQLConfigList
	if err := r.List(ctx, &configs, client.InNamespace(pod.Namespace)); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, c := range configs.Items {
		if c.Spec.ClusterRef.Name == clusterName {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Name: c.Name, Namespace: c.Namespace}})
		}
	}
	return out
}

// configsForSecret maps a Secret event to every ProxySQLConfig that consumes
// it — either as a user passwordSecretRef or as the admin Secret of the
// cluster the config targets. This makes password rotation converge on the
// next reconcile instead of waiting for the drift resync interval.
func (r *ProxySQLConfigReconciler) configsForSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	sec, ok := obj.(*corev1.Secret)
	if !ok {
		return nil
	}
	var configs proxysqlv1alpha1.ProxySQLConfigList
	if err := r.List(ctx, &configs, client.InNamespace(sec.Namespace)); err != nil {
		return nil
	}
	if len(configs.Items) == 0 {
		return nil
	}
	// Clusters whose derived admin-secret name matches this Secret. On list
	// failure, degrade gracefully: log and still return user-ref matches.
	adminOf := map[string]bool{}
	var clusters proxysqlv1alpha1.ProxySQLClusterList
	if err := r.List(ctx, &clusters, client.InNamespace(sec.Namespace)); err != nil {
		logf.FromContext(ctx).Error(err, "failed to list ProxySQLClusters while mapping Secret event; admin-secret matches will be skipped",
			"secret", sec.Name, "namespace", sec.Namespace)
	} else {
		for i := range clusters.Items {
			if builders.New(&clusters.Items[i], r.Scheme, builders.Passwords{}).SecretName() == sec.Name {
				adminOf[clusters.Items[i].Name] = true
			}
		}
	}
	var out []reconcile.Request
	for _, c := range configs.Items {
		if adminOf[c.Spec.ClusterRef.Name] || configReferencesSecret(&c, sec.Name) {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Name: c.Name, Namespace: c.Namespace}})
		}
	}
	return out
}

func configReferencesSecret(c *proxysqlv1alpha1.ProxySQLConfig, name string) bool {
	for _, u := range c.Spec.MySQLUsers {
		if u.PasswordSecretRef.Name == name {
			return true
		}
	}
	for _, u := range c.Spec.PostgreSQLUsers {
		if u.PasswordSecretRef.Name == name {
			return true
		}
	}
	return false
}
