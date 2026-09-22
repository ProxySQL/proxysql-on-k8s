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
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	proxysqlv1alpha1 "github.com/ProxySQL/kubernetes/operator/api/v1alpha1"
	"github.com/ProxySQL/kubernetes/operator/internal/controller/builders"
)

// clusterLabel marks every object the operator builds for a cluster; it is
// the prune query's scope (narrowed further by controller ownership).
const clusterLabel = "proxysql.com/cluster"

// minCoreSatellitePatch is the patch release in every supported ProxySQL
// series that first shipped PgSQL Cluster Sync (upstream #5297): 3.0.8,
// 3.1.8, 4.0.8. Below it, satellites could not pull the pgsql tables at
// all, so the mode is refused rather than half-working.
const minCoreSatellitePatch = 8

// checkTopologyVersion returns an error when the cluster asks for
// coreSatellite on an image older than x.y.8.
//
// A tag the operator cannot parse (a digest pin, "latest", "3.0", a vendor
// suffix, a private mirror's own scheme) is ALLOWED on purpose: refusing
// every unparseable tag would block legitimate mirrors, and the SaaS
// validates the version up front where it knows the real catalog. Only a
// tag that positively parses as major.minor.patch and reads older than the
// floor is refused.
func checkTopologyVersion(imageTag string) error {
	v := strings.TrimPrefix(strings.TrimSpace(imageTag), "v")
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 3 {
		return nil
	}
	patch := parts[2]
	if i := strings.IndexFunc(patch, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
		patch = patch[:i]
	}
	n, err := strconv.Atoi(patch)
	if err != nil {
		return nil
	}
	// The major/minor halves must be numeric too, or this is not a ProxySQL
	// version string and the patch digits mean nothing. The major is bounded
	// as well: ProxySQL majors are single digits, so a four-digit leading
	// number is a calendar tag ("2026.04.1") a mirror chose, not a release
	// this rule has any business judging.
	major, err := strconv.Atoi(parts[0])
	if err != nil || major < 1 || major > 99 {
		return nil
	}
	if _, err := strconv.Atoi(parts[1]); err != nil {
		return nil
	}
	if n < minCoreSatellitePatch {
		return fmt.Errorf("topology.mode=coreSatellite requires ProxySQL >= x.y.%d (PgSQL Cluster Sync); image tag is %q",
			minCoreSatellitePatch, imageTag)
	}
	return nil
}

// reconcileTopologyStatefulSets applies every StatefulSet the topology calls
// for and prunes the ones it no longer does.
//
// Direct mode keeps the historical single-StatefulSet path byte-identical;
// the extra work there is the prune of role sets left behind by a cluster
// that was converted BACK from coreSatellite (a no-op for a cluster that was
// never converted).
func (r *ProxySQLClusterReconciler) reconcileTopologyStatefulSets(
	ctx context.Context,
	cluster *proxysqlv1alpha1.ProxySQLCluster,
	b *builders.Builder,
	annotation string,
	markers stsMarkers,
) error {
	if !b.Spec.IsCoreSatellite() {
		if err := r.ensureStatefulSet(ctx, cluster, b.StatefulSet(annotation), markers); err != nil {
			return err
		}
		ready, err := r.statefulSetReady(ctx, cluster.Namespace, b.Name())
		if err != nil {
			return err
		}
		return r.pruneStaleStatefulSets(ctx, cluster, map[string]bool{b.Name(): true}, ready)
	}

	keep := make(map[string]bool, len(b.Spec.Topology.Core.Zones)+1)
	allReady := true
	for _, ss := range b.CoreStatefulSets(annotation) {
		if err := r.ensureStatefulSet(ctx, cluster, ss, markers); err != nil {
			return err
		}
		keep[ss.Name] = true
		ready, err := r.statefulSetReady(ctx, cluster.Namespace, ss.Name)
		if err != nil {
			return err
		}
		allReady = allReady && ready
	}
	sat := b.SatelliteStatefulSet(annotation)
	if err := r.ensureStatefulSet(ctx, cluster, sat, markers); err != nil {
		return err
	}
	keep[sat.Name] = true
	ready, err := r.statefulSetReady(ctx, cluster.Namespace, sat.Name)
	if err != nil {
		return err
	}
	allReady = allReady && ready

	return r.pruneStaleStatefulSets(ctx, cluster, keep, allReady)
}

// statefulSetReady reports whether every desired replica of a StatefulSet is
// ready. A set that does not exist yet is not ready (and not an error). A
// set scaled to 0 on purpose (spec.pause, satellites.replicas=0) counts as
// ready: it has nothing left to wait for.
func (r *ProxySQLClusterReconciler) statefulSetReady(ctx context.Context, ns, name string) (bool, error) {
	var ss appsv1.StatefulSet
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &ss); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	// A status that has not caught up with the current spec describes the
	// PREVIOUS generation: right after an apply that both drops a zone and
	// changes the pod template, the survivors still report the old pods as
	// ready while none of the replacements have rolled. Readiness gates a
	// prune that deletes PVCs, so believing a stale status destroys data
	// that the new pods were never given a chance to replace.
	if ss.Status.ObservedGeneration < ss.Generation {
		return false, nil
	}
	if ss.Spec.Replicas == nil {
		return ss.Status.ReadyReplicas > 0, nil
	}
	// UpdatedReplicas counts pods at the CURRENT revision. Requiring it as
	// well is what makes "ready" mean "the replacement is serving" rather
	// than "something is serving" — a rollout in progress is not done.
	return ss.Status.ReadyReplicas >= *ss.Spec.Replicas &&
		ss.Status.UpdatedReplicas >= *ss.Spec.Replicas, nil
}

// pruneStaleStatefulSets deletes operator-owned StatefulSets for this
// cluster that the current topology no longer calls for — a zone dropped
// from core.zones, or the old single StatefulSet after a conversion.
//
// It waits for allReady on purpose: deleting the previous shape before the
// replacement is serving would turn a topology change into an outage. A
// paused cluster never prunes at all: every set is scaled to 0, which
// satisfies "ready" vacuously, and pause's contract is that Services,
// Secrets and PVCs are retained — destroying a dropped zone's data while
// the cluster is deliberately stopped is exactly what it promises not to
// do. The prune resumes when the cluster does.
//
// Two further guards keep the blast radius at exactly this cluster's own
// stale objects: the list is scoped by the cluster label, and each candidate
// must be controlled by THIS cluster (an object that merely carries the
// label — a hand-made StatefulSet, one adopted from another cluster — is
// left alone).
//
// The pruned set's PVCs go with it: they are separate objects the
// StatefulSet controller never garbage-collects, and the persisted
// proxysql.db is reproducible from the operator or a peer.
func (r *ProxySQLClusterReconciler) pruneStaleStatefulSets(
	ctx context.Context,
	cluster *proxysqlv1alpha1.ProxySQLCluster,
	keep map[string]bool,
	allReady bool,
) error {
	// spec.pause is a plain bool with no defaulting, so the raw spec is
	// authoritative here.
	if !allReady || cluster.Spec.Pause {
		return nil
	}
	var list appsv1.StatefulSetList
	if err := r.List(ctx, &list, client.InNamespace(cluster.Namespace),
		client.MatchingLabels{clusterLabel: cluster.Name}); err != nil {
		return fmt.Errorf("list StatefulSets for prune: %w", err)
	}
	for i := range list.Items {
		ss := &list.Items[i]
		if keep[ss.Name] || !metav1.IsControlledBy(ss, cluster) {
			continue
		}
		logf.FromContext(ctx).Info("pruning StatefulSet the topology no longer calls for",
			"statefulset", ss.Name)
		// PVCs FIRST, and the order is load-bearing. A PVC's name is only
		// derivable from its StatefulSet, and this prune is driven by
		// LISTING StatefulSets — so once the set is gone, nothing can ever
		// find its claims again. Deleting the set first means any failure
		// in between (RBAC drift, eviction, operator restart) orphans the
		// volumes permanently, with no reconcile able to recover them.
		//
		// Deleting them first is safe precisely because it is not
		// immediate: kubernetes.io/pvc-protection holds a claim that a
		// running pod still mounts, so the delete only marks it, and the
		// reaping happens once the set below takes its pods away. A crash
		// after this point leaves claims already marked for deletion and a
		// set the next reconcile prunes again.
		if err := r.deleteStatefulSetPVCs(ctx, cluster, ss); err != nil {
			return err
		}
		if err := r.Delete(ctx, ss); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("prune stale StatefulSet %s: %w", ss.Name, err)
		}
	}
	return nil
}

// deleteStatefulSetPVCs removes the PVCs a pruned StatefulSet left behind.
//
// Matching is by NAME, not by label: a StatefulSet's PVCs inherit its
// SELECTOR labels, and the single-StatefulSet selector is a subset of every
// role selector — so a label query for the pruned bare set would sweep up
// the PVCs of the role sets that replaced it. "<claimTemplate>-<set>-<n>"
// with an all-digits ordinal names exactly one set's claims and nothing
// else's.
func (r *ProxySQLClusterReconciler) deleteStatefulSetPVCs(
	ctx context.Context,
	cluster *proxysqlv1alpha1.ProxySQLCluster,
	ss *appsv1.StatefulSet,
) error {
	if len(ss.Spec.VolumeClaimTemplates) == 0 {
		return nil
	}
	var list corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &list, client.InNamespace(ss.Namespace),
		client.MatchingLabels{clusterLabel: cluster.Name}); err != nil {
		return fmt.Errorf("list PVCs for prune: %w", err)
	}
	for i := range list.Items {
		pvc := &list.Items[i]
		if !pvcBelongsToStatefulSet(pvc.Name, ss) {
			continue
		}
		logf.FromContext(ctx).Info("deleting PVC of a pruned StatefulSet",
			"pvc", pvc.Name, "statefulset", ss.Name)
		if err := r.Delete(ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete PVC %s of pruned StatefulSet %s: %w", pvc.Name, ss.Name, err)
		}
	}
	return nil
}

// pvcBelongsToStatefulSet reports whether a PVC name is one the given
// StatefulSet's claim templates would have produced.
func pvcBelongsToStatefulSet(name string, ss *appsv1.StatefulSet) bool {
	for _, vct := range ss.Spec.VolumeClaimTemplates {
		ordinal, ok := strings.CutPrefix(name, vct.Name+"-"+ss.Name+"-")
		if !ok || ordinal == "" {
			continue
		}
		if _, err := strconv.Atoi(ordinal); err == nil {
			return true
		}
	}
	return false
}

// topologyStatus reads every role StatefulSet of a coreSatellite cluster and
// reports both the status.topology block and an AGGREGATE StatefulSet that
// the shared status path (derivePhase, the Available/Progressing conditions,
// the pause branch) consumes exactly as it consumes the single set in direct
// mode: ready/updated counts summed, creation timestamp zero until every set
// exists, and the revision pair left mismatched while any set is mid-roll.
func (r *ProxySQLClusterReconciler) topologyStatus(
	ctx context.Context,
	b *builders.Builder,
) (*proxysqlv1alpha1.TopologyStatus, appsv1.StatefulSet, bool, error) {
	topo := &proxysqlv1alpha1.TopologyStatus{
		Mode:              proxysqlv1alpha1.TopologyModeCoreSatellite,
		SatelliteReplicas: b.Spec.Topology.Satellites.Replicas,
	}
	var agg appsv1.StatefulSet
	anyMissing := false
	rolling := false

	read := func(name string) (appsv1.StatefulSet, bool, error) {
		var ss appsv1.StatefulSet
		err := r.Get(ctx, client.ObjectKey{Namespace: b.Namespace(), Name: name}, &ss)
		if apierrors.IsNotFound(err) {
			return appsv1.StatefulSet{}, false, nil
		}
		if err != nil {
			return appsv1.StatefulSet{}, false, err
		}
		return ss, true, nil
	}
	fold := func(ss appsv1.StatefulSet, found bool) {
		if !found || ss.CreationTimestamp.IsZero() {
			anyMissing = true
			return
		}
		agg.Status.ReadyReplicas += ss.Status.ReadyReplicas
		agg.Status.UpdatedReplicas += ss.Status.UpdatedReplicas
		if ss.Status.UpdateRevision != "" && ss.Status.UpdateRevision != ss.Status.CurrentRevision {
			rolling = true
		}
	}

	for _, z := range b.Spec.Topology.Core.Zones {
		ss, found, err := read(b.CoreStatefulSetName(z.Zone))
		if err != nil {
			return nil, agg, false, err
		}
		fold(ss, found)
		topo.CoreZones = append(topo.CoreZones, proxysqlv1alpha1.CoreZoneStatus{
			Zone:            z.Zone,
			DesiredReplicas: z.Replicas,
			ReadyReplicas:   ss.Status.ReadyReplicas,
		})
	}

	sat, found, err := read(b.SatelliteStatefulSetName())
	if err != nil {
		return nil, agg, false, err
	}
	fold(sat, found)
	topo.SatelliteReadyReplicas = sat.Status.ReadyReplicas

	if !anyMissing {
		// Any zero CreationTimestamp reads as "not created yet" to
		// derivePhase, so only stamp one when every set is really there.
		agg.CreationTimestamp = metav1.Now()
	}
	if rolling {
		agg.Status.CurrentRevision = "current"
		agg.Status.UpdateRevision = "updating"
	}
	return topo, agg, anyMissing, nil
}

// topologyDesiredReplicas is the cluster's total pod count: spec.replicas in
// direct mode, every core pod plus every satellite in coreSatellite mode
// (where spec.replicas is ignored). Pause is deliberately NOT subtracted —
// status.replicas reports what was asked for, as it always has.
func topologyDesiredReplicas(b *builders.Builder) int32 {
	if !b.Spec.IsCoreSatellite() {
		if b.Spec.Replicas == nil {
			return 0
		}
		return *b.Spec.Replicas
	}
	return b.Spec.CoreTotal() + b.Spec.Topology.Satellites.Replicas
}

// markerStatefulSetNames lists, in preference order, the StatefulSets whose
// marker annotations could carry this cluster's restart-checksum and
// TLS-rotation state. ensureStatefulSet writes the same object-level markers
// to every set it applies, so any EXISTING one of these reads back the same
// values — but which ones exist varies:
//
//   - a direct-mode cluster has only <cluster>;
//   - a settled coreSatellite cluster has every role set;
//   - a cluster mid-conversion (direct -> coreSatellite, or a new zone
//     prepended to core.zones) has the OLD shape's sets and not yet the
//     first-preference one.
//
// The REVERSE conversion (coreSatellite -> direct) is deliberately NOT
// covered here: spec.topology is usually deleted outright, so the live core
// sets' zone names are unrecoverable from the spec. currentStatefulSetAnnotations
// falls back to a label-scoped List when none of these names exists.
//
// Reading a name that does not exist yet would hand the engines an empty
// marker set, which reads as a fresh cluster: the cnf checksum would reset
// to bootHash and, worse, an empty tls-applied marker makes
// classifyTLSRotation ADOPT — silently marking an in-flight rotation applied
// though no pod ever reloaded the certificate. Hence: first one that exists.
func markerStatefulSetNames(b *builders.Builder) []string {
	if !b.Spec.IsCoreSatellite() {
		return []string{b.Name()}
	}
	names := make([]string, 0, len(b.Spec.Topology.Core.Zones)+2)
	for _, z := range b.Spec.Topology.Core.Zones {
		names = append(names, b.CoreStatefulSetName(z.Zone))
	}
	names = append(names, b.SatelliteStatefulSetName())
	// The pre-conversion single set, still carrying the live markers until
	// the role sets take over and it is pruned.
	return append(names, b.Name())
}
