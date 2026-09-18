package builders

import (
	"fmt"
	"maps"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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

// SatelliteStatefulSetName is the satellite StatefulSet name.
func (b *Builder) SatelliteStatefulSetName() string { return b.Name() + "-satellite" }

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
		ss.Spec.Template.Spec.Affinity = b.coreZoneAffinity(z.Zone)
		ss.Spec.Template.Spec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{
			MaxSkew:           1,
			TopologyKey:       "kubernetes.io/hostname",
			WhenUnsatisfiable: corev1.ScheduleAnyway,
			LabelSelector:     &metav1.LabelSelector{MatchLabels: b.RoleSelectorLabels(RoleCore, z.Zone)},
		}}
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

// coreZoneAffinity is the hard pin. Placement failure leaves pods Pending
// on purpose: the buyer's layout is what gets deployed, and the SaaS
// refuses an unschedulable zone before it ever reaches the CR.
func (b *Builder) coreZoneAffinity(zone string) *corev1.Affinity {
	return &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key:      ZoneTopologyKey,
						Operator: corev1.NodeSelectorOpIn,
						Values:   []string{zone},
					}},
				}},
			},
		},
	}
}
