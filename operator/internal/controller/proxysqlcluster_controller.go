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
	"crypto/tls"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	proxysqlv1alpha1 "github.com/ProxySQL/kubernetes/operator/api/v1alpha1"
	"github.com/ProxySQL/kubernetes/operator/internal/controller/builders"
)

// ProxySQLClusterReconciler reconciles a ProxySQLCluster into the K8s objects
// that make up the control plane: a StatefulSet, headless + regular Services,
// an admin Secret (created if missing), and an optional PodDisruptionBudget.
type ProxySQLClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// TLSRotationWindow bounds how long the TLS rotation engine retries
	// RELOAD-and-verify across reconciles (absorbing kubelet Secret-mount
	// propagation lag) before falling back to a rolling restart. Zero
	// means the default (defaultTLSRotationWindow).
	TLSRotationWindow time.Duration

	// tlsVerifyRetryDelay spaces the quick in-pass verification retries
	// after a RELOAD TLS (zero = defaultTLSVerifyRetryDelay); tlsProbe and
	// tlsReload override the real handshake probe / RELOAD dial (nil = the
	// real implementations). Test seams for the rotation engine.
	tlsVerifyRetryDelay time.Duration
	tlsProbe            func(ctx context.Context, addr, user, pass string, cfg *tls.Config) (string, error)
	tlsReload           func(ctx context.Context, addr, user, pass string, cfg *tls.Config) error
}

// +kubebuilder:rbac:groups=proxysql.com,resources=proxysqlclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=proxysql.com,resources=proxysqlclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=proxysql.com,resources=proxysqlclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch
// pods: get;list;watch only — needed by resolveRestartChecksum's and
// resolveTLSRotation's discoverPodEndpoints calls to find ready replicas to
// push runtime variable changes / PROXYSQL RELOAD TLS to.
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// configmaps: get + delete only — needed to garbage-collect the legacy
// bootstrap-cnf ConfigMap left behind by operator versions < v0.3.0. The
// operator no longer creates or updates any ConfigMap.
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=get;list;watch;create;update;patch;delete
// certificates: the cert-manager tier of spec.tls (issuerRef) creates and
// maintains one cert-manager.io Certificate per cluster; delete covers the
// garbage-collection when the tier is switched away.
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch;create;update;patch;delete

const (
	condTypeAvailable   = "Available"
	condTypeProgressing = "Progressing"
	condTypeDegraded    = "Degraded"
	// condTypePaused reports spec.pause's effect on the StatefulSet: always
	// present (unlike Degraded, which is removed when clean) so pollers can
	// rely on it being set. True only once the StatefulSet has actually
	// reached 0 ready replicas; False while unpaused OR while still
	// stopping (ready > 0) — see updateStatus/updatePausedStatus.
	condTypePaused = "Paused"
)

// Reconcile drives the ProxySQLCluster toward its desired state.
//
// Order of operations:
//  1. Fetch the CR.
//  2. Resolve the auth Secret (read existing or create with random passwords).
//  3. Read the cnf Secret's current data map, then build the desired
//     bootstrap-cnf Secret and ensure it (and garbage-collect the legacy
//     cnf ConfigMap left behind by versions < v0.3.0).
//  4. Ensure the headless + regular Services.
//  5. Read the StatefulSet's current annotations, then resolve whether this
//     cnf change can be applied at runtime without a restart
//     (resolveRestartChecksum), and ensure the StatefulSet with the
//     resulting pod-template checksum and object-level vars-applied-hash.
//  6. Ensure the PodDisruptionBudget when replicas > 1.
//  7. Update status from the underlying StatefulSet, surfacing
//     resolveRestartChecksum's summary through the Progressing condition.
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

	// TLS: resolve the serving-cert Secret (three-tier precedence) and flip
	// the Builder's TLS-render inputs ONLY when validation passes — the cnf
	// and StatefulSet rendered below key on the resolved state, and a
	// template referencing an unsatisfiable Secret must never reach the
	// kubelet (validate-and-hold; see ensureTLSSecrets). A resolution
	// failure is non-wedging: holdTLSLastGood picks the render fallback,
	// the reconcile continues, updateStatus surfaces
	// Degraded=TLSSecretError, and tlsErr requeues at the end (the
	// ExternalServiceError contract).
	tlsReady, tlsErr := r.ensureTLSSecrets(ctx, &cluster, b)
	if b.Spec.TLSEnabled() && !tlsReady {
		if err := r.holdTLSLastGood(ctx, b); err != nil {
			return ctrl.Result{}, err
		}
	}
	// Backend (proxy-to-server) TLS Secrets get the same validate-and-hold
	// treatment: the backend-tls projected volume and ssl_p2s_* cnf
	// variables render only against Secrets proven to carry their
	// load-bearing keys (runs after the serving hold — if that dropped the
	// TLS spec entirely, there is no backend rendering left to validate).
	if backendErr := r.validateBackendTLSSecrets(ctx, b); backendErr != nil {
		tlsErr = errors.Join(tlsErr, backendErr)
		if err := r.holdBackendTLSLastGood(ctx, b); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Capture the cnf Secret's FULL data map BEFORE ensureCnfSecret
	// overwrites it: resolveRestartChecksum diffs every key — proxysql.cnf
	// at value level, every other key (fluent-bit.conf) at byte level.
	oldCnfData, err := r.currentCnfData(ctx, b)
	if err != nil {
		return ctrl.Result{}, err
	}

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
	// The curated external Service (nil when spec.service.external is absent
	// or disabled — then any previously created one is deleted). Deliberately
	// NOT gated on spec.pause: pause semantics retain Services. A persistent
	// apiserver rejection (colliding pinned nodePort, ipFamilies mutation, …)
	// must NOT wedge the rest of the reconcile: carry the error to the end —
	// StatefulSet/PDB/ServiceMonitor still apply, updateStatus surfaces a
	// Degraded=ExternalServiceError condition, and the error is returned for
	// requeue (mirrors handleRuntimeApplyError's non-wedging contract).
	extSvcErr := r.ensureExternalService(ctx, &cluster, b.ExternalService())

	// Capture the StatefulSet's current annotations BEFORE ensureStatefulSet
	// overwrites them: resolveRestartChecksum and resolveTLSRotation need
	// every marker as it stood before this reconcile.
	cur, err := r.currentStatefulSetAnnotations(ctx, b)
	if err != nil {
		return ctrl.Result{}, err
	}

	// TLS disable transition (probe-4): a persistent datadir keeps the cert
	// symlinks after the Secret mount is gone, and proxysql exits at boot on
	// the dangling names — keep rendering the cleanup init container for
	// previously-wired clusters.
	if err := r.resolveTLSCleanup(ctx, b); err != nil {
		return ctrl.Result{}, err
	}

	// TLS rotation BEFORE the vars runtime push: rotation changes Secret
	// content, not cnf text, so it is invisible to the checksum machinery by
	// design — and until RELOAD TLS lands, pods may serve a certificate the
	// routine dials below can't verify (tier-1 full-bundle swaps). Running
	// the rotation engine first heals that before anything else dials. Its
	// error is deferred (non-wedging, like extSvcErr/tlsErr): the outcome's
	// marker values are committed either way, so an open rotation window
	// survives operator restarts.
	rot, rotErr := r.resolveTLSRotation(ctx, &cluster, b, tlsReady, cur, pw.Radmin)
	b.TLSRestartValue = rot.restart

	// Checksum over every cnf Secret key (proxysql.cnf + fluent-bit.conf when
	// logging is enabled) so any config change rolls the pods.
	newHash := builders.CnfChecksum(cnfSecret.Data)
	annotation, appliedVarsHash, structuralAppliedHash, summary, err := r.resolveRestartChecksum(
		ctx, &cluster, oldCnfData, cnfSecret.Data, cur.cnfChecksum, newHash, cur.varsApplied, cur.structuralApplied, pw.Radmin)
	if err != nil {
		// Runtime SQL push failed partway through. Requeue without advancing
		// the vars/structural markers (so the retry re-pushes the same
		// variables), but do NOT skip the StatefulSet: it is re-ensured with
		// the PRE-reconcile checksum annotations — plus THIS reconcile's TLS
		// rotation outcome, which already committed its own dials — and a
		// Degraded condition surfaces the failure. A concurrent rotation
		// error rides along in the returned (requeueing) error.
		markers := stsMarkers{
			varsApplied:       cur.varsApplied,
			structuralApplied: cur.structuralApplied,
			tlsApplied:        rot.applied,
			tlsRotationState:  rot.state,
		}
		return ctrl.Result{}, errors.Join(r.handleRuntimeApplyError(ctx, &cluster, b, cur.cnfChecksum, markers, err), rotErr)
	}
	markers := stsMarkers{
		varsApplied:       appliedVarsHash,
		structuralApplied: structuralAppliedHash,
		tlsApplied:        rot.applied,
		tlsRotationState:  rot.state,
	}
	if err := r.ensureStatefulSet(ctx, &cluster, b.StatefulSet(annotation), markers); err != nil {
		return ctrl.Result{}, err
	}
	summary = mergeSummaries(summary, rot.summary)

	if err := r.ensurePDB(ctx, &cluster, b.PodDisruptionBudget()); err != nil {
		return ctrl.Result{}, err
	}

	// ServiceMonitor is best-effort: missing prometheus-operator CRD must not
	// fail the reconcile. ensureServiceMonitor surfaces the outcome as a
	// condition but never returns an error.
	r.ensureServiceMonitor(ctx, &cluster, b.ServiceMonitor())

	// 3) Status.
	if err := r.updateStatus(ctx, &cluster, b, summary, extSvcErr, tlsErr, rotErr); err != nil {
		return ctrl.Result{}, err
	}
	// Deferred external-Service/TLS failures requeue only after everything
	// else applied and the Degraded condition landed in status. rotErr's
	// backoff paces the RELOAD-and-verify retries inside the rotation window.
	return ctrl.Result{}, errors.Join(extSvcErr, tlsErr, rotErr)
}

// currentCnfData reads the cnf Secret's full data map as it stood before
// this reconcile. Returns nil if the Secret doesn't exist yet (fresh
// cluster) — resolveRestartChecksum treats that as "no prior Secret to diff
// against".
func (r *ProxySQLClusterReconciler) currentCnfData(ctx context.Context, b *builders.Builder) (map[string][]byte, error) {
	var sec corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Name: b.CnfSecretName(), Namespace: b.Namespace()}, &sec)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get cnf secret: %w", err)
	}
	return sec.Data, nil
}

// stsAnnotations is the StatefulSet's marker-annotation snapshot as it
// stood BEFORE this reconcile: the pod-template cnf checksum and TLS
// restart bump, plus the object-level applied-hash markers. Everything is
// "" if the StatefulSet doesn't exist yet.
type stsAnnotations struct {
	cnfChecksum       string // pod-template proxysql.com/cnf-checksum
	varsApplied       string // object proxysql.com/vars-applied-hash
	structuralApplied string // object proxysql.com/structural-applied-hash
	tlsApplied        string // object proxysql.com/tls-applied-hash
	tlsRotationState  string // object proxysql.com/tls-rotation-state
	tlsRestart        string // pod-template builders.TLSRestartAnnotation
}

// stsMarkers is what this reconcile decided the OBJECT-level marker
// annotations should be; ensureStatefulSet commits them in the same write
// as the template (the crash-safety commit point for every engine).
type stsMarkers struct {
	varsApplied       string
	structuralApplied string
	tlsApplied        string // "" removes the annotation (TLS off)
	tlsRotationState  string // "" removes the annotation (no open window)
}

// currentStatefulSetAnnotations reads the marker annotations as they stood
// before this reconcile.
func (r *ProxySQLClusterReconciler) currentStatefulSetAnnotations(ctx context.Context, b *builders.Builder) (stsAnnotations, error) {
	var ss appsv1.StatefulSet
	getErr := r.Get(ctx, types.NamespacedName{Name: b.Name(), Namespace: b.Namespace()}, &ss)
	if apierrors.IsNotFound(getErr) {
		return stsAnnotations{}, nil
	}
	if getErr != nil {
		return stsAnnotations{}, fmt.Errorf("get statefulset: %w", getErr)
	}
	return stsAnnotations{
		cnfChecksum:       ss.Spec.Template.Annotations[annotationCnfChecksum],
		varsApplied:       ss.Annotations[annotationVarsAppliedHash],
		structuralApplied: ss.Annotations[annotationStructuralAppliedHash],
		tlsApplied:        ss.Annotations[annotationTLSAppliedHash],
		tlsRotationState:  ss.Annotations[annotationTLSRotationState],
		tlsRestart:        ss.Spec.Template.Annotations[builders.TLSRestartAnnotation],
	}, nil
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
		// Annotations MERGE (unlike labels): cloud controllers write their
		// own annotations onto LB Services and wiping them every reconcile
		// would fight those controllers. Spec keys win; foreign keys are
		// preserved. Consequence: a key removed from spec.service.annotations
		// lingers on the Service until removed by hand — documented in the
		// API field comment.
		if len(desired.Annotations) > 0 && existing.Annotations == nil {
			existing.Annotations = map[string]string{}
		}
		maps.Copy(existing.Annotations, desired.Annotations)
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

// ensureExternalService creates or updates the curated "<cluster>-external"
// Service, or — when desired is nil (spec.service.external absent or
// disabled) — deletes a previously created one (NotFound is success; only an
// operator-owned Service is touched). The apply path mirrors ensureService,
// including its annotation preserve-foreign-keys merge, and additionally
// preserves apiserver-allocated values the builder leaves unset (node ports,
// healthCheckNodePort) so reconciles don't churn allocations.
func (r *ProxySQLClusterReconciler) ensureExternalService(ctx context.Context, owner *proxysqlv1alpha1.ProxySQLCluster, desired *corev1.Service) error {
	if desired == nil {
		name := builders.New(owner, r.Scheme, builders.Passwords{}).ExternalName()
		existing := &corev1.Service{}
		err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: owner.Namespace}, existing)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !metav1.IsControlledBy(existing, owner) {
			return nil
		}
		return client.IgnoreNotFound(r.Delete(ctx, existing))
	}

	existing := &corev1.Service{}
	existing.Name = desired.Name
	existing.Namespace = desired.Namespace
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, existing, func() error {
		existing.Labels = desired.Labels
		// Annotations MERGE, same semantics as ensureService: spec keys win,
		// annotations written by cloud LB controllers are preserved, and a
		// key removed from the spec lingers until removed by hand.
		if len(desired.Annotations) > 0 && existing.Annotations == nil {
			existing.Annotations = map[string]string{}
		}
		maps.Copy(existing.Annotations, desired.Annotations)
		// Preserve immutable/allocated fields: ClusterIP(s) (immutable), the
		// node port per port name and the healthCheckNodePort (allocated by
		// the apiserver; re-sending 0 every reconcile would churn them).
		clusterIP := existing.Spec.ClusterIP
		clusterIPs := existing.Spec.ClusterIPs
		allocatedNodePort := make(map[string]int32, len(existing.Spec.Ports))
		for _, p := range existing.Spec.Ports {
			allocatedNodePort[p.Name] = p.NodePort
		}
		hcNodePort := existing.Spec.HealthCheckNodePort
		existing.Spec = desired.Spec
		if clusterIP != "" && existing.Spec.ClusterIP == "" {
			existing.Spec.ClusterIP = clusterIP
		}
		if len(clusterIPs) > 0 && len(existing.Spec.ClusterIPs) == 0 {
			existing.Spec.ClusterIPs = clusterIPs
		}
		for i := range existing.Spec.Ports {
			if existing.Spec.Ports[i].NodePort == 0 {
				existing.Spec.Ports[i].NodePort = allocatedNodePort[existing.Spec.Ports[i].Name]
			}
		}
		// Only meaningful (and only allocated) for LoadBalancer +
		// externalTrafficPolicy Local; anywhere else the builder's 0 stands.
		if existing.Spec.Type == corev1.ServiceTypeLoadBalancer &&
			existing.Spec.ExternalTrafficPolicy == corev1.ServiceExternalTrafficPolicyLocal &&
			existing.Spec.HealthCheckNodePort == 0 {
			existing.Spec.HealthCheckNodePort = hcNodePort
		}
		return controllerutil.SetControllerReference(owner, existing, r.Scheme)
	})
	return err
}

// handleRuntimeApplyError recovers from a resolveRestartChecksum failure
// (runtime SQL push to a replica failed) without wedging StatefulSet
// updates: the StatefulSet is still ensured, carrying the PRE-reconcile
// pod-template checksum and vars/structural marker values — identical
// annotations trigger no rollout, but every other pending template or
// replica change still applies — and a Degraded condition (reason
// RuntimeApplyError, message naming the failing replica) surfaces the
// failure. The push error is returned so the caller requeues; with both
// markers unchanged the retry re-pushes the same variables
// (resolveRestartChecksum's crash-safety contract). markers carries the
// pre-reconcile vars/structural values alongside this reconcile's TLS
// rotation outcome (whose dials already happened and must commit).
func (r *ProxySQLClusterReconciler) handleRuntimeApplyError(
	ctx context.Context,
	cluster *proxysqlv1alpha1.ProxySQLCluster,
	b *builders.Builder,
	prev string,
	markers stsMarkers,
	pushErr error,
) error {
	// prev can only be empty when no StatefulSet exists yet, and fresh
	// clusters classify as bootHash before any push runs — but guard anyway
	// so no path can ever create a StatefulSet with an empty checksum
	// annotation.
	if prev != "" {
		if err := r.ensureStatefulSet(ctx, cluster, b.StatefulSet(prev), markers); err != nil {
			return fmt.Errorf("ensure statefulset after runtime-apply failure: %w (runtime apply: %v)", err, pushErr)
		}
	}
	r.setCondition(cluster, condTypeDegraded, metav1.ConditionTrue, "RuntimeApplyError", pushErr.Error())
	if serr := r.Status().Update(ctx, cluster); serr != nil {
		// Best-effort: the requeue driven by pushErr will retry the status
		// write on the next pass.
		logf.FromContext(ctx).Error(serr, "status update after runtime-apply failure")
	}
	return pushErr
}

// ensureStatefulSet creates or updates the StatefulSet. The markers are
// written as OBJECT-level annotations — never on the pod template — so
// recording them never triggers a rollout; see resolveRestartChecksum and
// resolveTLSRotation for what they track. The TLS markers are removed when
// empty (TLS off / no open rotation window).
func (r *ProxySQLClusterReconciler) ensureStatefulSet(ctx context.Context, owner *proxysqlv1alpha1.ProxySQLCluster, desired *appsv1.StatefulSet, m stsMarkers) error {
	existing := &appsv1.StatefulSet{}
	existing.Name = desired.Name
	existing.Namespace = desired.Namespace
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, existing, func() error {
		existing.Labels = desired.Labels
		if existing.Annotations == nil {
			existing.Annotations = map[string]string{}
		}
		existing.Annotations[annotationVarsAppliedHash] = m.varsApplied
		existing.Annotations[annotationStructuralAppliedHash] = m.structuralApplied
		for k, v := range map[string]string{
			annotationTLSAppliedHash:   m.tlsApplied,
			annotationTLSRotationState: m.tlsRotationState,
		} {
			if v == "" {
				delete(existing.Annotations, k)
			} else {
				existing.Annotations[k] = v
			}
		}
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

// updateStatus refreshes cluster.Status from the underlying StatefulSet.
// summary is the human-readable outcome of this reconcile's
// resolveRestartChecksum call ("" when there was nothing to report): a
// "RuntimeApplied: ..." summary always wins the Progressing condition
// (reason RuntimeApplied, ConditionFalse — nothing is rolling); a
// "RestartRequired: ..." summary (or any other non-empty summary) becomes
// the message of the existing Rolling condition, which the StatefulSet
// template diff drives as before.
//
// extSvcErr is this reconcile's ensureExternalService outcome, tlsErr its
// ensureTLSSecrets outcome and rotErr its resolveTLSRotation outcome: any
// being non-nil sets Degraded=True (reason ExternalServiceError /
// TLSSecretError / TLSRotationError, message = the error); all nil clears
// Degraded — the same end-of-updateStatus clearing that removes a stale
// RuntimeApplyError once a reconcile completes cleanly.
func (r *ProxySQLClusterReconciler) updateStatus(ctx context.Context, cluster *proxysqlv1alpha1.ProxySQLCluster, b *builders.Builder, summary string, extSvcErr, tlsErr, rotErr error) error {
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
	ext, err := r.externalEndpoint(ctx, b)
	if err != nil {
		return err
	}
	cluster.Status.Endpoints.External = ext
	cluster.Status.Phase = derivePhase(&ss, notFound, desired, b.Spec.Pause)

	if b.Spec.Pause {
		return r.updatePausedStatus(ctx, cluster, &ss, extSvcErr, tlsErr, rotErr)
	}
	r.setCondition(cluster, condTypePaused, metav1.ConditionFalse, "NotPaused", "cluster is not paused")

	replicasReady := ss.Status.ReadyReplicas == desired && desired > 0
	if replicasReady {
		r.setCondition(cluster, condTypeAvailable, metav1.ConditionTrue, "AllReplicasReady",
			fmt.Sprintf("%d/%d replicas ready", ss.Status.ReadyReplicas, desired))
	} else {
		r.setCondition(cluster, condTypeAvailable, metav1.ConditionFalse, "ReplicasNotReady",
			fmt.Sprintf("%d/%d replicas ready", ss.Status.ReadyReplicas, desired))
	}

	switch {
	case strings.HasPrefix(summary, "RuntimeApplied"):
		// A restart-free variables push always wins the Progressing
		// condition: nothing is rolling out, but it's worth surfacing what
		// just changed.
		r.setCondition(cluster, condTypeProgressing, metav1.ConditionFalse, "RuntimeApplied", summary)
	case strings.HasPrefix(summary, "RestartRequired"):
		// A restart-required change was detected THIS reconcile. The old
		// pods are typically all still Ready at this instant, so this case
		// must come before the Steady branch or the explanation would be
		// swallowed until a pod actually goes NotReady.
		r.setCondition(cluster, condTypeProgressing, metav1.ConditionTrue, "Rolling", summary)
	case replicasReady:
		r.setCondition(cluster, condTypeProgressing, metav1.ConditionFalse, "Steady", "no rollout in progress")
	default:
		msg := "waiting for replicas"
		if summary != "" {
			// Carries a "RestartRequired: ..." explanation when the pod
			// template annotation just changed for that reason; the
			// StatefulSet template diff is what actually drives the
			// rollout, this only improves the message.
			msg = summary
		}
		r.setCondition(cluster, condTypeProgressing, metav1.ConditionTrue, "Rolling", msg)
	}
	r.setDegradedFromDeferredErrors(cluster, extSvcErr, tlsErr, rotErr)

	return r.Status().Update(ctx, cluster)
}

// setDegradedFromDeferredErrors projects this reconcile's deferred,
// non-wedging failures onto the Degraded condition: a persistent apiserver
// rejection of the external Service (colliding pinned nodePort, ipFamilies
// mutation, …) surfaces as reason ExternalServiceError; a TLS Secret
// resolution failure (missing Secret/key, cert-manager CRD absent) as
// reason TLSSecretError; an open TLS rotation window (some replica not yet
// verified serving the rotated certificate) as reason TLSRotationError. A
// clean pass removes Degraded — which is also what clears a stale
// RuntimeApplyError, preserving that contract.
func (r *ProxySQLClusterReconciler) setDegradedFromDeferredErrors(cluster *proxysqlv1alpha1.ProxySQLCluster, extSvcErr, tlsErr, rotErr error) {
	switch {
	case extSvcErr != nil:
		r.setCondition(cluster, condTypeDegraded, metav1.ConditionTrue, "ExternalServiceError", extSvcErr.Error())
	case tlsErr != nil:
		r.setCondition(cluster, condTypeDegraded, metav1.ConditionTrue, reasonTLSSecretError, tlsErr.Error())
	case rotErr != nil:
		r.setCondition(cluster, condTypeDegraded, metav1.ConditionTrue, reasonTLSRotationError, rotErr.Error())
	default:
		meta.RemoveStatusCondition(&cluster.Status.Conditions, condTypeDegraded)
	}
}

// externalEndpoint projects the LIVE "<cluster>-external" Service onto the
// status.endpoints.external string (unlike the rest of ClusterEndpoints,
// which is a pure spec projection, the external entry depends on
// apiserver/cloud-provider allocations). Formats — documented on the API
// field:
//   - LoadBalancer: "host:port" from the first ingress IP (or hostname) and
//     the Service's first port; "" until the LB is provisioned.
//   - NodePort: comma-separated allocated node ports in port order.
//
// Empty when the external Service is disabled or not created yet.
func (r *ProxySQLClusterReconciler) externalEndpoint(ctx context.Context, b *builders.Builder) (string, error) {
	ext := b.Spec.Service.External
	if ext == nil || !ext.Enabled {
		return "", nil
	}
	var svc corev1.Service
	err := r.Get(ctx, types.NamespacedName{Name: b.ExternalName(), Namespace: b.Namespace()}, &svc)
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get external service: %w", err)
	}

	switch svc.Spec.Type {
	case corev1.ServiceTypeLoadBalancer:
		if len(svc.Status.LoadBalancer.Ingress) == 0 || len(svc.Spec.Ports) == 0 {
			return "", nil // not provisioned yet
		}
		host := svc.Status.LoadBalancer.Ingress[0].IP
		if host == "" {
			host = svc.Status.LoadBalancer.Ingress[0].Hostname
		}
		if host == "" {
			return "", nil
		}
		return fmt.Sprintf("%s:%d", host, svc.Spec.Ports[0].Port), nil
	case corev1.ServiceTypeNodePort:
		parts := make([]string, 0, len(svc.Spec.Ports))
		for _, p := range svc.Spec.Ports {
			if p.NodePort != 0 {
				parts = append(parts, fmt.Sprintf("%d", p.NodePort))
			}
		}
		return strings.Join(parts, ","), nil
	}
	return "", nil
}

// updatePausedStatus is updateStatus's branch for spec.pause=true: it skips
// the normal Available/Progressing semantics (which would otherwise read as
// "0/N replicas ready" — technically true but misleading for an
// intentional, operator-driven scale-down) and instead distinguishes
// Stopping (ready > 0: the StatefulSet is still draining down to 0) from
// Paused (ready == 0: fully scaled down) — the Percona pattern referenced
// in #56. condTypePaused is only ConditionTrue once fully paused; Degraded
// is cleared since a paused cluster isn't in an error state — unless the
// external Service (retained during pause) or the TLS Secret resolution
// failed this reconcile (extSvcErr/tlsErr), which degrade exactly as they
// do unpaused.
func (r *ProxySQLClusterReconciler) updatePausedStatus(ctx context.Context, cluster *proxysqlv1alpha1.ProxySQLCluster, ss *appsv1.StatefulSet, extSvcErr, tlsErr, rotErr error) error {
	if ss.Status.ReadyReplicas > 0 {
		msg := fmt.Sprintf("scaling down to 0 replicas (%d still ready)", ss.Status.ReadyReplicas)
		r.setCondition(cluster, condTypePaused, metav1.ConditionFalse, "Stopping", msg)
		r.setCondition(cluster, condTypeAvailable, metav1.ConditionFalse, "Stopping", msg)
		r.setCondition(cluster, condTypeProgressing, metav1.ConditionTrue, "Stopping", msg)
	} else {
		const msg = "all replicas scaled to 0; Services/Secrets/PVCs retained"
		r.setCondition(cluster, condTypePaused, metav1.ConditionTrue, "Paused", msg)
		r.setCondition(cluster, condTypeAvailable, metav1.ConditionFalse, "Paused", msg)
		r.setCondition(cluster, condTypeProgressing, metav1.ConditionFalse, "Paused", "no rollout in progress; cluster is paused")
	}
	r.setDegradedFromDeferredErrors(cluster, extSvcErr, tlsErr, rotErr)
	return r.Status().Update(ctx, cluster)
}

// derivePhase projects StatefulSet state onto a single coarse phase string.
// Conditions remain the source of truth; this exists for dashboards and
// external pollers. Failed is reserved for future terminal states the
// operator can positively identify. Note the deliberate coarseness: "SS
// exists, 0 ready" maps to Creating even during a total outage of a
// previously-running cluster.
//
// paused is spec.pause: when true it wins over everything else, projecting
// to Stopping (still-ready replicas draining down) or Paused (fully scaled
// to 0) instead of the usual Pending/Creating/Running/Updating ladder.
func derivePhase(ss *appsv1.StatefulSet, ssMissing bool, desired int32, paused bool) string {
	if paused {
		if !ssMissing && ss.Status.ReadyReplicas > 0 {
			return proxysqlv1alpha1.PhaseStopping
		}
		return proxysqlv1alpha1.PhasePaused
	}
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

// Field-index keys mapping a ProxySQLCluster to the TLS Secrets it
// REFERENCES (as opposed to owns): the tier-1 serving Secret and the two
// backend Secrets. Registered in SetupWithManager, consumed by
// clustersForTLSSecret.
const (
	tlsSecretNameIndex        = ".spec.tls.secretName"
	tlsBackendCASecretIndex   = ".spec.tls.backend.caSecretName"
	tlsBackendCertSecretIndex = ".spec.tls.backend.clientCertSecretName"
)

// SetupWithManager wires the controller into the manager with watches on
// the owned resources, plus a name-based watch on ALL Secrets: tier-1 user
// Secrets and the tier-2 cert-manager-issued Secret carry no owner
// reference to the cluster, so Owns() alone would leave their content
// changes (rotation!) invisible until the multi-hour informer resync.
func (r *ProxySQLClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	indexer := mgr.GetFieldIndexer()
	type indexed struct {
		key     string
		extract func(*proxysqlv1alpha1.ProxySQLCluster) string
	}
	for _, idx := range []indexed{
		{tlsSecretNameIndex, func(c *proxysqlv1alpha1.ProxySQLCluster) string {
			if c.Spec.TLS == nil {
				return ""
			}
			return c.Spec.TLS.SecretName
		}},
		{tlsBackendCASecretIndex, func(c *proxysqlv1alpha1.ProxySQLCluster) string {
			if c.Spec.TLS == nil || c.Spec.TLS.Backend == nil {
				return ""
			}
			return c.Spec.TLS.Backend.CASecretName
		}},
		{tlsBackendCertSecretIndex, func(c *proxysqlv1alpha1.ProxySQLCluster) string {
			if c.Spec.TLS == nil || c.Spec.TLS.Backend == nil {
				return ""
			}
			return c.Spec.TLS.Backend.ClientCertSecretName
		}},
	} {
		extract := idx.extract
		if err := indexer.IndexField(context.Background(), &proxysqlv1alpha1.ProxySQLCluster{}, idx.key,
			func(o client.Object) []string {
				if name := extract(o.(*proxysqlv1alpha1.ProxySQLCluster)); name != "" {
					return []string{name}
				}
				return nil
			}); err != nil {
			return fmt.Errorf("index %s: %w", idx.key, err)
		}
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&proxysqlv1alpha1.ProxySQLCluster{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.Secret{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.clustersForTLSSecret)).
		Named("proxysqlcluster").
		Complete(r)
}

// clustersForTLSSecret maps a Secret event to the clusters that must
// re-reconcile: every cluster whose spec REFERENCES the Secret by name
// (via the three field indexes), plus — by naming convention — the
// cluster behind an operator-managed "<name>-tls"/"<name>-tls-ca" Secret.
// The convention covers the tier-2 Secret, which is owned by the
// cert-manager Certificate rather than the cluster, so Owns() never fires
// for it. Enqueueing a non-existent cluster is a cheap no-op reconcile.
func (r *ProxySQLClusterReconciler) clustersForTLSSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	seen := map[types.NamespacedName]bool{}
	var reqs []reconcile.Request
	add := func(name string) {
		if name == "" {
			return
		}
		key := types.NamespacedName{Name: name, Namespace: obj.GetNamespace()}
		if !seen[key] {
			seen[key] = true
			reqs = append(reqs, reconcile.Request{NamespacedName: key})
		}
	}

	for _, index := range []string{tlsSecretNameIndex, tlsBackendCASecretIndex, tlsBackendCertSecretIndex} {
		var list proxysqlv1alpha1.ProxySQLClusterList
		if err := r.List(ctx, &list,
			client.InNamespace(obj.GetNamespace()),
			client.MatchingFields{index: obj.GetName()}); err != nil {
			logf.FromContext(ctx).Error(err, "listing clusters for TLS secret", "index", index, "secret", obj.GetName())
			continue
		}
		for i := range list.Items {
			add(list.Items[i].Name)
		}
	}

	if name, ok := strings.CutSuffix(obj.GetName(), "-tls"); ok {
		add(name)
	}
	if name, ok := strings.CutSuffix(obj.GetName(), "-tls-ca"); ok {
		add(name)
	}
	return reqs
}
