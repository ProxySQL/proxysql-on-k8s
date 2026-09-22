package builders

import (
	"fmt"
	"maps"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	proxysqlv1alpha1 "github.com/ProxySQL/kubernetes/operator/api/v1alpha1"
)

// Role is a pod's part in the core/satellite topology.
type Role string

const (
	// RoleCore pods are the ones the operator writes configuration to.
	RoleCore Role = "core"
	// RoleSatellite pods pull configuration via ProxySQL Cluster sync and
	// are never written to by the operator.
	RoleSatellite Role = "satellite"
)

const (
	// RoleLabel marks a pod's topology role. Present only in coreSatellite
	// mode: direct-mode objects must stay byte-identical.
	RoleLabel = "proxysql.com/role"
	// CoreZoneLabel names the availability zone a core StatefulSet is
	// pinned to. Present on core objects only.
	CoreZoneLabel = "proxysql.com/core-zone"
	// ZoneTopologyKey is the well-known node label core pods are pinned on.
	ZoneTopologyKey = "topology.kubernetes.io/zone"
)

// CoreStatefulSetName is the per-zone core StatefulSet name.
func (b *Builder) CoreStatefulSetName(zone string) string {
	return fmt.Sprintf("%s-core-%s", b.Name(), zone)
}

// SatelliteStatefulSetName is the satellite StatefulSet name. The satellite
// PDB shares it (one PDB per role, and the satellite role is one set).
func (b *Builder) SatelliteStatefulSetName() string { return b.Name() + "-satellite" }

// CorePDBName is the core tier's PodDisruptionBudget name. It is not a
// StatefulSet name: the core PDB spans every zone's set.
func (b *Builder) CorePDBName() string { return b.Name() + "-core" }

// RoleSelectorLabels is SelectorLabels() plus the role (and, for core, the
// zone). It is the selector of a per-role StatefulSet and the pod-template
// label set on its pods.
//
// SelectorLabels() itself must never gain these keys: it is the immutable
// .spec.selector of every cluster created before this feature.
func (b *Builder) RoleSelectorLabels(role Role, zone string) map[string]string {
	l := make(map[string]string, 5)
	maps.Copy(l, b.SelectorLabels())
	l[RoleLabel] = string(role)
	if role == RoleCore {
		l[CoreZoneLabel] = zone
	}
	return l
}

// CorePodDNS returns the stable per-pod DNS names of every core pod, in
// zone order then ordinal order — the peer list that seeds proxysql_servers
// for BOTH roles (satellites list only the cores, and cores list only each
// other, so one list serves both). Nil outside coreSatellite mode.
func (b *Builder) CorePodDNS() []string {
	if !b.Spec.IsCoreSatellite() {
		return nil
	}
	headless := b.HeadlessName()
	out := make([]string, 0, b.Spec.CoreTotal())
	for _, z := range b.Spec.Topology.Core.Zones {
		sts := b.CoreStatefulSetName(z.Zone)
		for i := int32(0); i < z.Replicas; i++ {
			out = append(out, fmt.Sprintf("%s-%d.%s.%s.svc", sts, i, headless, b.Namespace()))
		}
	}
	return out
}

// ServeTrafficFromCore reports whether core pods sit behind the client
// Services. Defaults to true (the CRD default); false gives a dedicated
// config tier.
func (b *Builder) ServeTrafficFromCore() bool {
	if !b.Spec.IsCoreSatellite() {
		return true
	}
	if b.Spec.Topology.Core.ServeTraffic == nil {
		return true
	}
	return *b.Spec.Topology.Core.ServeTraffic
}

// coreZones is a nil-safe accessor used by the object builders.
func (b *Builder) coreZones() []proxysqlv1alpha1.CoreZone {
	if !b.Spec.IsCoreSatellite() {
		return nil
	}
	return b.Spec.Topology.Core.Zones
}

// CoreStatefulSets returns one StatefulSet per core zone, each hard-pinned
// to its zone. Empty outside coreSatellite mode.
//
// Each set is the shared pod template from StatefulSet() with the name,
// replica count, selector, pod labels and zone affinity overridden — so
// container spec, security context, volumes and probes stay in one place.
func (b *Builder) CoreStatefulSets(cnfChecksum string) []*appsv1.StatefulSet {
	zones := b.coreZones()
	if len(zones) == 0 {
		return nil
	}
	out := make([]*appsv1.StatefulSet, 0, len(zones))
	for _, z := range zones {
		ss := b.roleStatefulSet(cnfChecksum, RoleCore, z.Zone, z.Replicas)
		ss.Spec.Template.Spec.Affinity = b.coreZoneAffinity(z.Zone, ss.Spec.Template.Spec.Affinity)
		// APPEND, never replace: a user's own spread constraints are the
		// only expression of intent the operator has no substitute for.
		// The in-zone hostname spread is additive to them — EXCEPT when
		// the user already declares the same (topologyKey,
		// whenUnsatisfiable) pair. Kubernetes rejects a duplicate pair
		// outright ("Duplicate value: {kubernetes.io/hostname,
		// ScheduleAnyway}"), which would fail the StatefulSet apply and
		// wedge the reconcile — strictly worse than the dropped constraint
		// this append exists to fix. Their constraint already expresses
		// the same intent, so defer to it.
		ss.Spec.Template.Spec.TopologySpreadConstraints = appendSpreadUnlessPairExists(
			ss.Spec.Template.Spec.TopologySpreadConstraints,
			corev1.TopologySpreadConstraint{
				MaxSkew:           1,
				TopologyKey:       "kubernetes.io/hostname",
				WhenUnsatisfiable: corev1.ScheduleAnyway,
				LabelSelector:     &metav1.LabelSelector{MatchLabels: b.RoleSelectorLabels(RoleCore, z.Zone)},
			})
		out = append(out, ss)
	}
	return out
}

// SatelliteStatefulSet returns the satellite tier, or nil outside
// coreSatellite mode. Placement is whatever spec.affinity /
// spec.topologySpreadConstraints say — the same #207-driven fields a
// direct-mode cluster uses.
func (b *Builder) SatelliteStatefulSet(cnfChecksum string) *appsv1.StatefulSet {
	if !b.Spec.IsCoreSatellite() {
		return nil
	}
	return b.roleStatefulSet(cnfChecksum, RoleSatellite, "", b.Spec.Topology.Satellites.Replicas)
}

// roleStatefulSet clones the shared StatefulSet and re-stamps the
// role-specific identity fields.
func (b *Builder) roleStatefulSet(cnfChecksum string, role Role, zone string, replicas int32) *appsv1.StatefulSet {
	ss := b.StatefulSet(cnfChecksum)
	labels := b.RoleSelectorLabels(role, zone)

	switch role {
	case RoleCore:
		ss.Name = b.CoreStatefulSetName(zone)
	case RoleSatellite:
		ss.Name = b.SatelliteStatefulSetName()
	}
	maps.Copy(ss.Labels, labels)
	ss.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}

	podLabels := make(map[string]string, len(labels)+len(b.Spec.PodLabels))
	maps.Copy(podLabels, b.Spec.PodLabels)
	maps.Copy(podLabels, labels) // role identity wins over user pod labels
	ss.Spec.Template.Labels = podLabels

	n := replicas
	if b.Spec.Pause {
		n = 0
	}
	ss.Spec.Replicas = &n
	return ss
}

// appendSpreadUnlessPairExists adds one spread constraint unless the list
// already carries its (topologyKey, whenUnsatisfiable) pair. Kubernetes
// validates that pair as unique per pod spec, so appending a duplicate makes
// the whole pod template invalid and the StatefulSet apply fail.
func appendSpreadUnlessPairExists(existing []corev1.TopologySpreadConstraint, add corev1.TopologySpreadConstraint) []corev1.TopologySpreadConstraint {
	for _, c := range existing {
		if c.TopologyKey == add.TopologyKey && c.WhenUnsatisfiable == add.WhenUnsatisfiable {
			return existing
		}
	}
	return append(existing, add)
}

// coreZoneAffinity is the hard pin. Placement failure leaves pods Pending
// on purpose: the buyer's layout is what gets deployed, and the SaaS
// refuses an unschedulable zone before it ever reaches the CR.
//
// It pins a core StatefulSet to one zone while PRESERVING the
// placement the user asked for in spec.affinity. Overwriting it wholesale
// silently drops pod anti-affinity — the rule that keeps two config sources
// off one node — so a cluster following the docs' own hardening advice would
// lose it on conversion and never be told.
//
// The zone pin is a node requirement, and pod (anti-)affinity is a different
// field entirely, so those carry through untouched. A user's own REQUIRED
// node terms are ANDed with the zone rather than replaced: node selector
// TERMS are ORed, while the expressions inside one term are ANDed, so the
// zone requirement is appended to each of the user's terms.
func (b *Builder) coreZoneAffinity(zone string, base *corev1.Affinity) *corev1.Affinity {
	zoneReq := corev1.NodeSelectorRequirement{
		Key:      ZoneTopologyKey,
		Operator: corev1.NodeSelectorOpIn,
		Values:   []string{zone},
	}

	var out *corev1.Affinity
	if base != nil {
		out = base.DeepCopy()
	} else {
		out = &corev1.Affinity{}
	}
	if out.NodeAffinity == nil {
		out.NodeAffinity = &corev1.NodeAffinity{}
	}

	req := out.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if req == nil || len(req.NodeSelectorTerms) == 0 {
		out.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{zoneReq},
			}},
		}
		return out
	}
	for i := range req.NodeSelectorTerms {
		req.NodeSelectorTerms[i].MatchExpressions = append(
			req.NodeSelectorTerms[i].MatchExpressions, zoneReq)
	}
	return out
}

// ClientServiceSelector is the selector for the client-facing Services.
// It is SelectorLabels() everywhere except a coreSatellite cluster with
// core.serveTraffic=false, where only satellites take client traffic.
//
// The headless Service never uses this: it must resolve every pod, because
// it is the StatefulSets' serviceName and the DNS ProxySQL Cluster sync
// dials.
func (b *Builder) ClientServiceSelector() map[string]string {
	if !b.Spec.IsCoreSatellite() || b.ServeTrafficFromCore() {
		return b.SelectorLabels()
	}
	return b.RoleSelectorLabels(RoleSatellite, "")
}

// CorePDB budgets the core tier as a whole: at most one core pod
// unavailable across every zone, so a drain can never take out two config
// sources at once. Nil outside coreSatellite mode, when the PDB is
// disabled, or when there is only one core pod (nothing to budget).
func (b *Builder) CorePDB() *policyv1.PodDisruptionBudget {
	if !b.Spec.IsCoreSatellite() || !isTrue(b.Spec.PodDisruptionBudget.Enabled) {
		return nil
	}
	if b.Spec.CoreTotal() <= 1 {
		return nil
	}
	one := intstr.FromInt32(1)
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      b.CorePDBName(),
			Namespace: b.Namespace(),
			Labels:    b.Labels(),
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector:       &metav1.LabelSelector{MatchLabels: b.roleOnlySelector(RoleCore)},
			MaxUnavailable: &one,
		},
	}
}

// SatellitePDB keeps all but one satellite available, mirroring the
// single-StatefulSet default. Nil outside coreSatellite mode, when the PDB
// is disabled, or with <= 1 satellite.
func (b *Builder) SatellitePDB() *policyv1.PodDisruptionBudget {
	if !b.Spec.IsCoreSatellite() || !isTrue(b.Spec.PodDisruptionBudget.Enabled) {
		return nil
	}
	n := b.Spec.Topology.Satellites.Replicas
	if n <= 1 {
		return nil
	}
	minAvail := intstr.FromInt32(n - 1)
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      b.SatelliteStatefulSetName(),
			Namespace: b.Namespace(),
			Labels:    b.Labels(),
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector:     &metav1.LabelSelector{MatchLabels: b.roleOnlySelector(RoleSatellite)},
			MinAvailable: &minAvail,
		},
	}
}

// roleOnlySelector selects a role across every zone (no core-zone key).
func (b *Builder) roleOnlySelector(role Role) map[string]string {
	l := make(map[string]string, 4)
	maps.Copy(l, b.SelectorLabels())
	l[RoleLabel] = string(role)
	return l
}
