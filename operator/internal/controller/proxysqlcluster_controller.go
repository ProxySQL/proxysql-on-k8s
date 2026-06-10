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
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	proxysqlv1alpha1 "github.com/ProxySQL/kubernetes/operator/api/v1alpha1"
	"github.com/ProxySQL/kubernetes/operator/internal/controller/builders"
)

// ProxySQLClusterReconciler reconciles a ProxySQLCluster into the K8s objects
// that make up the control plane: a StatefulSet, headless + regular Services,
// an admin Secret (created if missing), and an optional PodDisruptionBudget.
type ProxySQLClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=proxysql.com,resources=proxysqlclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=proxysql.com,resources=proxysqlclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=proxysql.com,resources=proxysqlclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch
// configmaps: get + delete only — needed to garbage-collect the legacy
// bootstrap-cnf ConfigMap left behind by operator versions < v0.3.0. The
// operator no longer creates or updates any ConfigMap.
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=get;list;watch;create;update;patch;delete

const (
	condTypeAvailable   = "Available"
	condTypeProgressing = "Progressing"
	condTypeDegraded    = "Degraded"
)

// Reconcile drives the ProxySQLCluster toward its desired state.
//
// Order of operations:
//  1. Fetch the CR.
//  2. Resolve the auth Secret (read existing or create with random passwords).
//  3. Build the desired bootstrap-cnf Secret and ensure it (and garbage-collect
//     the legacy cnf ConfigMap left behind by versions < v0.3.0).
//  4. Ensure the headless + regular Services.
//  5. Ensure the StatefulSet (annotated with the cnf checksum so a content
//     change triggers a rolling restart).
//  6. Ensure the PodDisruptionBudget when replicas > 1.
//  7. Update status from the underlying StatefulSet.
func (r *ProxySQLClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var cluster proxysqlv1alpha1.ProxySQLCluster
	if err := r.Get(ctx, req.NamespacedName, &cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to fetch ProxySQLCluster")
		return ctrl.Result{}, err
	}

	// 1) Resolve passwords (from existing Secret or mint + create).
	pw, err := r.resolvePasswords(ctx, &cluster)
	if err != nil {
		cluster.Status.Phase = proxysqlv1alpha1.PhaseDegraded
		r.setCondition(&cluster, condTypeDegraded, metav1.ConditionTrue, "AuthSecretError", err.Error())
		_ = r.Status().Update(ctx, &cluster)
		return ctrl.Result{}, err
	}

	b := builders.New(&cluster, r.Scheme, pw)

	// 2) Owned resources, in dependency order.
	cnfSecret, err := b.CnfSecret()
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("build cnf secret: %w", err)
	}
	if err := r.ensureCnfSecret(ctx, &cluster, cnfSecret); err != nil {
		return ctrl.Result{}, err
	}
	// Migration from < v0.3.0: the bootstrap cnf used to live in a ConfigMap
	// named after the cluster. Remove it if we own it.
	if err := r.cleanupLegacyCnfConfigMap(ctx, &cluster); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.ensureService(ctx, &cluster, b.Service()); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureService(ctx, &cluster, b.HeadlessService()); err != nil {
		return ctrl.Result{}, err
	}

	cnfChecksum := builders.Sha256(string(cnfSecret.Data["proxysql.cnf"]))
	if err := r.ensureStatefulSet(ctx, &cluster, b.StatefulSet(cnfChecksum)); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.ensurePDB(ctx, &cluster, b.PodDisruptionBudget()); err != nil {
		return ctrl.Result{}, err
	}

	// ServiceMonitor is best-effort: missing prometheus-operator CRD must not
	// fail the reconcile. ensureServiceMonitor surfaces the outcome as a
	// condition but never returns an error.
	r.ensureServiceMonitor(ctx, &cluster, b.ServiceMonitor())

	// 3) Status.
	return ctrl.Result{}, r.updateStatus(ctx, &cluster, b)
}

// resolvePasswords reads the admin/radmin/monitor passwords from the auth Secret.
// When ManagesAuthSecret() and the Secret does not exist yet, it mints random
// passwords and creates the Secret. When the Secret exists with missing keys
// (operator-managed), it backfills them. Externally managed Secrets are
// resolved via builders.PasswordsFromSecret, which also accepts the common
// platform username/password schema.
func (r *ProxySQLClusterReconciler) resolvePasswords(ctx context.Context, cluster *proxysqlv1alpha1.ProxySQLCluster) (builders.Passwords, error) {
	b := builders.New(cluster, r.Scheme, builders.Passwords{})
	keys := b.SecretKeys()

	var sec corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Name: b.SecretName(), Namespace: cluster.Namespace}, &sec)
	switch {
	case apierrors.IsNotFound(err):
		if !b.ManagesAuthSecret() {
			return builders.Passwords{}, fmt.Errorf("auth secret %q not found and spec.auth.secretName was set (externally managed)", b.SecretName())
		}
		pw, perr := mintPasswords()
		if perr != nil {
			return builders.Passwords{}, perr
		}
		b.Pw = pw
		desired := b.AuthSecret()
		if err := controllerutil.SetControllerReference(cluster, desired, r.Scheme); err != nil {
			return builders.Passwords{}, err
		}
		if err := r.Create(ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
			return builders.Passwords{}, fmt.Errorf("create auth secret: %w", err)
		}
		return pw, nil
	case err != nil:
		return builders.Passwords{}, fmt.Errorf("get auth secret: %w", err)
	}

	// Externally managed Secret: accept either the operator schema or the
	// common platform username/password schema (see PasswordsFromSecret).
	if !b.ManagesAuthSecret() {
		pw, perr := builders.PasswordsFromSecret(sec.Data, keys)
		// On success, an absent admin key means the username/password schema
		// matched (a partial operator schema errors instead of falling through).
		if perr == nil && len(sec.Data[keys.AdminPassword]) == 0 {
			logf.FromContext(ctx).V(1).Info("auth secret resolved via username/password schema",
				"secret", b.SecretName())
		}
		return pw, perr
	}

	pw := builders.Passwords{
		Admin:   string(sec.Data[keys.AdminPassword]),
		Radmin:  string(sec.Data[keys.RadminPassword]),
		Monitor: string(sec.Data[keys.MonitorPassword]),
	}

	// Backfill any missing fields when the operator owns the Secret.
	if b.ManagesAuthSecret() {
		changed := false
		if pw.Admin == "" {
			v, err := builders.RandomPassword()
			if err != nil {
				return builders.Passwords{}, err
			}
			pw.Admin = v
			changed = true
		}
		if pw.Radmin == "" {
			v, err := builders.RandomPassword()
			if err != nil {
				return builders.Passwords{}, err
			}
			pw.Radmin = v
			changed = true
		}
		if pw.Monitor == "" {
			v, err := builders.RandomPassword()
			if err != nil {
				return builders.Passwords{}, err
			}
			pw.Monitor = v
			changed = true
		}
		if changed {
			if sec.Data == nil {
				sec.Data = map[string][]byte{}
			}
			sec.Data[keys.AdminPassword] = []byte(pw.Admin)
			sec.Data[keys.RadminPassword] = []byte(pw.Radmin)
			sec.Data[keys.MonitorPassword] = []byte(pw.Monitor)
			if err := r.Update(ctx, &sec); err != nil {
				return builders.Passwords{}, fmt.Errorf("backfill auth secret: %w", err)
			}
		}
	}

	return pw, nil
}

func mintPasswords() (builders.Passwords, error) {
	admin, err := builders.RandomPassword()
	if err != nil {
		return builders.Passwords{}, err
	}
	radmin, err := builders.RandomPassword()
	if err != nil {
		return builders.Passwords{}, err
	}
	monitor, err := builders.RandomPassword()
	if err != nil {
		return builders.Passwords{}, err
	}
	return builders.Passwords{Admin: admin, Radmin: radmin, Monitor: monitor}, nil
}

func (r *ProxySQLClusterReconciler) ensureCnfSecret(ctx context.Context, owner *proxysqlv1alpha1.ProxySQLCluster, desired *corev1.Secret) error {
	existing := &corev1.Secret{}
	existing.Name = desired.Name
	existing.Namespace = desired.Namespace
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, existing, func() error {
		existing.Labels = desired.Labels
		existing.Type = desired.Type
		existing.Data = desired.Data
		return controllerutil.SetControllerReference(owner, existing, r.Scheme)
	})
	return err
}

// cleanupLegacyCnfConfigMap deletes the bootstrap-cnf ConfigMap (named after
// the cluster) that operator versions < v0.3.0 created, now replaced by the
// <cluster>-cnf Secret. Only a ConfigMap controller-owned by this cluster is
// touched; a user-managed ConfigMap that merely shares the name survives.
func (r *ProxySQLClusterReconciler) cleanupLegacyCnfConfigMap(ctx context.Context, owner *proxysqlv1alpha1.ProxySQLCluster) error {
	existing := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: owner.Name, Namespace: owner.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !metav1.IsControlledBy(existing, owner) {
		return nil
	}
	logf.FromContext(ctx).Info("deleting legacy bootstrap-cnf ConfigMap (replaced by Secret)",
		"configmap", existing.Name)
	return client.IgnoreNotFound(r.Delete(ctx, existing))
}

func (r *ProxySQLClusterReconciler) ensureService(ctx context.Context, owner *proxysqlv1alpha1.ProxySQLCluster, desired *corev1.Service) error {
	existing := &corev1.Service{}
	existing.Name = desired.Name
	existing.Namespace = desired.Namespace
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, existing, func() error {
		existing.Labels = desired.Labels
		// Annotations are overwritten wholesale, mirroring label handling:
		// annotations added out-of-band directly on the Service are
		// intentionally not preserved — spec.service.annotations is the
		// source of truth.
		existing.Annotations = desired.Annotations
		// Preserve immutable fields: ClusterIP, ClusterIPs.
		clusterIP := existing.Spec.ClusterIP
		clusterIPs := existing.Spec.ClusterIPs
		existing.Spec = desired.Spec
		if clusterIP != "" && existing.Spec.ClusterIP == "" {
			existing.Spec.ClusterIP = clusterIP
		}
		if len(clusterIPs) > 0 && len(existing.Spec.ClusterIPs) == 0 {
			existing.Spec.ClusterIPs = clusterIPs
		}
		return controllerutil.SetControllerReference(owner, existing, r.Scheme)
	})
	return err
}

func (r *ProxySQLClusterReconciler) ensureStatefulSet(ctx context.Context, owner *proxysqlv1alpha1.ProxySQLCluster, desired *appsv1.StatefulSet) error {
	existing := &appsv1.StatefulSet{}
	existing.Name = desired.Name
	existing.Namespace = desired.Namespace
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, existing, func() error {
		existing.Labels = desired.Labels
		// Selector is immutable; only set on create.
		if existing.CreationTimestamp.IsZero() {
			existing.Spec.Selector = desired.Spec.Selector
			existing.Spec.ServiceName = desired.Spec.ServiceName
			existing.Spec.PodManagementPolicy = desired.Spec.PodManagementPolicy
			existing.Spec.VolumeClaimTemplates = desired.Spec.VolumeClaimTemplates
		}
		existing.Spec.Replicas = desired.Spec.Replicas
		existing.Spec.Template = desired.Spec.Template
		return controllerutil.SetControllerReference(owner, existing, r.Scheme)
	})
	return err
}

func (r *ProxySQLClusterReconciler) ensurePDB(ctx context.Context, owner *proxysqlv1alpha1.ProxySQLCluster, desired *policyv1.PodDisruptionBudget) error {
	if desired == nil {
		// Disabled or single-replica: ensure any previously created PDB is removed.
		existing := &policyv1.PodDisruptionBudget{}
		err := r.Get(ctx, types.NamespacedName{Name: owner.Name, Namespace: owner.Namespace}, existing)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		// Only delete if we own it.
		if !metav1.IsControlledBy(existing, owner) {
			return nil
		}
		return client.IgnoreNotFound(r.Delete(ctx, existing))
	}
	existing := &policyv1.PodDisruptionBudget{}
	existing.Name = desired.Name
	existing.Namespace = desired.Namespace
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, existing, func() error {
		existing.Labels = desired.Labels
		// PDB selector is immutable after create.
		if existing.CreationTimestamp.IsZero() {
			existing.Spec.Selector = desired.Spec.Selector
		}
		existing.Spec.MinAvailable = desired.Spec.MinAvailable
		existing.Spec.MaxUnavailable = desired.Spec.MaxUnavailable
		return controllerutil.SetControllerReference(owner, existing, r.Scheme)
	})
	return err
}

// ensureServiceMonitor creates or updates the ServiceMonitor when the user
// asked for it. The prometheus-operator CRD might not be installed; that's
// surfaced as a non-fatal condition (Type=ServiceMonitorReady) rather than a
// reconcile error so a cluster without Prometheus Operator still works.
//
// When desired is nil (metrics or SM sub-spec disabled), any previously
// created SM is removed if it was operator-owned.
func (r *ProxySQLClusterReconciler) ensureServiceMonitor(ctx context.Context, owner *proxysqlv1alpha1.ProxySQLCluster, desired *unstructured.Unstructured) {
	log := logf.FromContext(ctx)
	condType := "ServiceMonitorReady"

	if desired == nil {
		// Best-effort delete of any previously-created SM. Use unstructured Get
		// so we don't depend on the prom-operator scheme being registered.
		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(builders.ServiceMonitorGVK)
		key := types.NamespacedName{Name: owner.Name, Namespace: owner.Namespace}
		if err := r.Get(ctx, key, existing); err == nil {
			if metav1.IsControlledBy(existing, owner) {
				_ = r.Delete(ctx, existing)
			}
		}
		meta.RemoveStatusCondition(&owner.Status.Conditions, condType)
		return
	}

	if err := controllerutil.SetControllerReference(owner, desired, r.Scheme); err != nil {
		log.Error(err, "ServiceMonitor SetControllerReference failed")
		r.setCondition(owner, condType, metav1.ConditionFalse, "OwnerRefError", err.Error())
		return
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(builders.ServiceMonitorGVK)
	existing.SetName(desired.GetName())
	existing.SetNamespace(desired.GetNamespace())

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, existing, func() error {
		existing.SetLabels(desired.GetLabels())
		existing.Object["spec"] = desired.Object["spec"]
		return controllerutil.SetControllerReference(owner, existing, r.Scheme)
	})
	if err != nil {
		// Most common cause: monitoring.coreos.com/v1 CRD not installed.
		// Don't escalate to a reconcile error — surface as a condition and move on.
		log.Info("ServiceMonitor reconcile skipped (likely no prometheus-operator CRD)", "err", err.Error())
		r.setCondition(owner, condType, metav1.ConditionFalse, "CRDNotInstalledOrFailed", err.Error())
		return
	}
	r.setCondition(owner, condType, metav1.ConditionTrue, "Synced", "ServiceMonitor applied")
}

func (r *ProxySQLClusterReconciler) updateStatus(ctx context.Context, cluster *proxysqlv1alpha1.ProxySQLCluster, b *builders.Builder) error {
	var ss appsv1.StatefulSet
	err := r.Get(ctx, types.NamespacedName{Name: b.Name(), Namespace: b.Namespace()}, &ss)
	notFound := apierrors.IsNotFound(err)
	if err != nil && !notFound {
		return err
	}

	desired := int32(0)
	if b.Spec.Replicas != nil {
		desired = *b.Spec.Replicas
	}
	cluster.Status.ObservedGeneration = cluster.Generation
	cluster.Status.Replicas = desired
	cluster.Status.ReadyReplicas = ss.Status.ReadyReplicas
	cluster.Status.UpdatedReplicas = ss.Status.UpdatedReplicas
	cluster.Status.AdminSecretName = b.SecretName()
	cluster.Status.Endpoints = b.Endpoints()
	cluster.Status.Phase = derivePhase(&ss, notFound, desired)

	if ss.Status.ReadyReplicas == desired && desired > 0 {
		r.setCondition(cluster, condTypeAvailable, metav1.ConditionTrue, "AllReplicasReady",
			fmt.Sprintf("%d/%d replicas ready", ss.Status.ReadyReplicas, desired))
		r.setCondition(cluster, condTypeProgressing, metav1.ConditionFalse, "Steady", "no rollout in progress")
	} else {
		r.setCondition(cluster, condTypeAvailable, metav1.ConditionFalse, "ReplicasNotReady",
			fmt.Sprintf("%d/%d replicas ready", ss.Status.ReadyReplicas, desired))
		r.setCondition(cluster, condTypeProgressing, metav1.ConditionTrue, "Rolling", "waiting for replicas")
	}
	meta.RemoveStatusCondition(&cluster.Status.Conditions, condTypeDegraded)

	return r.Status().Update(ctx, cluster)
}

// derivePhase projects StatefulSet state onto a single coarse phase string.
// Conditions remain the source of truth; this exists for dashboards and
// external pollers. Failed is reserved for future terminal states the
// operator can positively identify. Note the deliberate coarseness: "SS
// exists, 0 ready" maps to Creating even during a total outage of a
// previously-running cluster.
func derivePhase(ss *appsv1.StatefulSet, ssMissing bool, desired int32) string {
	switch {
	case ssMissing || ss.CreationTimestamp.IsZero():
		return proxysqlv1alpha1.PhasePending
	case ss.Status.ReadyReplicas == 0:
		return proxysqlv1alpha1.PhaseCreating
	case ss.Status.ReadyReplicas == desired &&
		(ss.Status.UpdateRevision == "" || ss.Status.UpdateRevision == ss.Status.CurrentRevision):
		return proxysqlv1alpha1.PhaseRunning
	default:
		return proxysqlv1alpha1.PhaseUpdating
	}
}

func (r *ProxySQLClusterReconciler) setCondition(cluster *proxysqlv1alpha1.ProxySQLCluster, t string, s metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               t,
		Status:             s,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: cluster.Generation,
	})
}

// SetupWithManager wires the controller into the manager with watches on the
// owned resources.
func (r *ProxySQLClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&proxysqlv1alpha1.ProxySQLCluster{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.Secret{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Named("proxysqlcluster").
		Complete(r)
}
