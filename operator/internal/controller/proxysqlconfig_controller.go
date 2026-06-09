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
	"sort"
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

	// requeueAfterSuccess: after a successful sync, requeue at this cadence as
	// a safety net (catches Pod restarts that wiped runtime tables before
	// ProxySQL Cluster sync caught up).
	requeueAfterSuccess = 30 * time.Second
	// requeueAfterTransient: after a transient failure (cluster not found,
	// admin unreachable on some replicas), retry sooner.
	requeueAfterTransient = 5 * time.Second
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
//     status.LastAppliedHash AND every ready pod was synced.
//  7. For each ready pod, open a SQL connection to its admin port and run
//     the full Sync (DELETE/INSERT/LOAD/SAVE per table + variables).
//  8. Update status (LastAppliedHash, LastSyncTime, SyncedReplicas, Conditions).
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

	// 2) Read cluster admin Secret.
	var adminSec corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: b.SecretName(), Namespace: cluster.Namespace}, &adminSec); err != nil {
		r.setCfgCondition(&cfg, cfgCondReady, metav1.ConditionFalse, "AdminSecretMissing", err.Error())
		_ = r.Status().Update(ctx, &cfg)
		return ctrl.Result{RequeueAfter: requeueAfterTransient}, nil
	}
	adminPassword := string(adminSec.Data[keys.AdminPassword])
	if adminPassword == "" {
		err := fmt.Errorf("admin secret %q is missing key %q", adminSec.Name, keys.AdminPassword)
		r.setCfgCondition(&cfg, cfgCondReady, metav1.ConditionFalse, "AdminSecretIncomplete", err.Error())
		_ = r.Status().Update(ctx, &cfg)
		return ctrl.Result{RequeueAfter: requeueAfterTransient}, nil
	}

	// 3) Resolve user passwords (mysql_users + pgsql_users) from Secrets.
	desired, err := r.buildDesired(ctx, &cfg)
	if err != nil {
		r.setCfgCondition(&cfg, cfgCondReady, metav1.ConditionFalse, "UserSecretError", err.Error())
		_ = r.Status().Update(ctx, &cfg)
		return ctrl.Result{RequeueAfter: requeueAfterTransient}, nil
	}

	// 4) Discover ready ProxySQL pods.
	addrs, err := r.discoverPodAddresses(ctx, &cluster, adminPort)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(addrs) == 0 {
		r.setCfgCondition(&cfg, cfgCondReady, metav1.ConditionFalse, "NoReadyReplicas",
			"no ready ProxySQL pods to push config to")
		_ = r.Status().Update(ctx, &cfg)
		return ctrl.Result{RequeueAfter: requeueAfterTransient}, nil
	}

	// 5) Compute spec hash for short-circuit.
	hash := hashDesired(desired)
	allHealthy := cfg.Status.LastAppliedHash == hash &&
		cfg.Status.SyncedReplicas == int32(len(addrs)) &&
		cfg.Status.ObservedGeneration == cfg.Generation
	if allHealthy {
		log.V(1).Info("ProxySQLConfig unchanged; skipping SQL push", "hash", hash, "replicas", len(addrs))
		return ctrl.Result{RequeueAfter: requeueAfterSuccess}, nil
	}

	// 6) Fan out writes.
	synced, syncErrs := r.applyToReplicas(ctx, addrs, adminPassword, desired)

	// 7) Status.
	cfg.Status.ObservedGeneration = cfg.Generation
	cfg.Status.SyncedReplicas = int32(synced)
	if synced == len(addrs) && len(syncErrs) == 0 {
		cfg.Status.LastAppliedHash = hash
		now := metav1.NewTime(time.Now())
		cfg.Status.LastSyncTime = &now
		r.setCfgCondition(&cfg, cfgCondReady, metav1.ConditionTrue, "Synced",
			fmt.Sprintf("config applied to %d/%d replicas", synced, len(addrs)))
		r.setCfgCondition(&cfg, cfgCondProgressing, metav1.ConditionFalse, "Steady", "")
		meta.RemoveStatusCondition(&cfg.Status.Conditions, cfgCondDegraded)
		if err := r.Status().Update(ctx, &cfg); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueAfterSuccess}, nil
	}

	// Partial or full failure.
	r.setCfgCondition(&cfg, cfgCondReady, metav1.ConditionFalse, "PartialSync",
		fmt.Sprintf("synced %d/%d replicas", synced, len(addrs)))
	r.setCfgCondition(&cfg, cfgCondDegraded, metav1.ConditionTrue, "SyncErrors",
		joinErrs(syncErrs))
	if err := r.Status().Update(ctx, &cfg); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueAfterTransient}, nil
}

// buildDesired translates the K8s spec into the resolved Desired struct
// the proxysqlclient package operates on.
func (r *ProxySQLConfigReconciler) buildDesired(ctx context.Context, cfg *proxysqlv1alpha1.ProxySQLConfig) (*proxysqlclient.Desired, error) {
	d := &proxysqlclient.Desired{
		AdminVariables:      cfg.Spec.AdminVariables,
		MySQLVariables:      cfg.Spec.MySQLVariables,
		PostgreSQLVariables: cfg.Spec.PostgreSQLVariables,
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
	for _, r2 := range cfg.Spec.MySQLQueryRules {
		d.MySQLQueryRules = append(d.MySQLQueryRules, proxysqlclient.MySQLQueryRule{
			RuleID: r2.RuleID, Active: r2.Active, Username: r2.Username,
			SchemaName: r2.SchemaName, MatchPattern: r2.MatchPattern, MatchDigest: r2.MatchDigest,
			DestinationHostgroup: r2.DestinationHostgroup, Apply: r2.Apply, Comment: r2.Comment,
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
			RuleID: r2.RuleID, Active: r2.Active, MatchPattern: r2.MatchPattern,
			DestinationHostgroup: r2.DestinationHostgroup, Apply: r2.Apply, Comment: r2.Comment,
		})
	}
	for _, s := range cfg.Spec.ProxySQLServers {
		d.ProxySQLServers = append(d.ProxySQLServers, proxysqlclient.ProxySQLServer{
			Hostname: s.Hostname, Port: s.Port, Weight: s.Weight, Comment: s.Comment,
		})
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

// discoverPodAddresses lists Pods owned by the ProxySQLCluster and returns
// host:port addresses for any pod that has a Ready status and a non-empty IP.
func (r *ProxySQLConfigReconciler) discoverPodAddresses(ctx context.Context, cluster *proxysqlv1alpha1.ProxySQLCluster, port int32) ([]string, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(cluster.Namespace), client.MatchingLabels{
		"proxysql.com/cluster": cluster.Name,
	}); err != nil {
		return nil, err
	}
	var out []string
	for _, p := range pods.Items {
		if p.Status.PodIP == "" || p.DeletionTimestamp != nil {
			continue
		}
		if !isPodReady(&p) {
			continue
		}
		out = append(out, fmt.Sprintf("%s:%d", p.Status.PodIP, port))
	}
	// Deterministic order for logs/status.
	sort.Strings(out)
	return out, nil
}

func isPodReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// applyToReplicas opens a connection to each addr and runs Sync. Returns the
// count of replicas that synced successfully, plus the per-addr errors.
func (r *ProxySQLConfigReconciler) applyToReplicas(ctx context.Context, addrs []string, password string, d *proxysqlclient.Desired) (int, []error) {
	log := logf.FromContext(ctx)
	var ok int
	var errs []error
	for _, addr := range addrs {
		pxc, err := proxysqlclient.New(addr, "admin", password)
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

func (r *ProxySQLConfigReconciler) setCfgCondition(cfg *proxysqlv1alpha1.ProxySQLConfig, t string, s metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
		Type:               t,
		Status:             s,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: cfg.Generation,
	})
}

// hashDesired returns a stable SHA-256 over the JSON-marshaled Desired.
// Used to short-circuit when nothing has changed since the last successful
// sync. Maps are serialized in sorted order by encoding/json.
func hashDesired(d *proxysqlclient.Desired) string {
	b, err := json.Marshal(d)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
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
func (r *ProxySQLConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&proxysqlv1alpha1.ProxySQLConfig{}).
		Watches(&proxysqlv1alpha1.ProxySQLCluster{}, handler.EnqueueRequestsFromMapFunc(r.configsForCluster)).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.configsForPod)).
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
