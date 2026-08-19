package cpupool

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/kongxiangrui/kqos/pkg/apis/kqos/v1alpha1"
	"github.com/kongxiangrui/kqos/pkg/cpuset"
)

func pod(name string, level v1alpha1.QoSLevel, cpu string, numa bool) *corev1.Pod {
	ann := map[string]string{v1alpha1.AnnotationQoSLevel: string(level)}
	if numa {
		ann[v1alpha1.AnnotationNUMABinding] = "true"
	}
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "demo", UID: types.UID(name), Annotations: ann},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c"}}},
	}
	if cpu != "" {
		p.Spec.Containers[0].Resources.Requests = corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse(cpu),
		}
	}
	return p
}

// twoZones is a 16-CPU machine split across two NUMA nodes.
func twoZones() Topology {
	return Topology{
		Zones: []Zone{
			{ID: 0, CPUs: cpuset.MustParse("0-7")},
			{ID: 1, CPUs: cpuset.MustParse("8-15")},
		},
		Online: cpuset.MustParse("0-15"),
	}
}

func poolNamed(pools []Pool, name string) (Pool, bool) {
	for _, p := range pools {
		if p.Name == name {
			return p, true
		}
	}
	return Pool{}, false
}

func TestWeightsSeparateTheTiers(t *testing.T) {
	if WeightFor(v1alpha1.QoSSharedCores) <= WeightFor(v1alpha1.QoSReclaimedCores) {
		t.Error("shared must outweigh reclaimed")
	}
	if WeightFor(v1alpha1.QoSDedicatedCores) <= WeightFor(v1alpha1.QoSSharedCores) {
		t.Error("dedicated must outweigh shared")
	}
	if WeightFor(v1alpha1.QoSSystemCores) <= WeightFor(v1alpha1.QoSDedicatedCores) {
		t.Error("system must outweigh everything")
	}
	// The shared-to-reclaimed ratio is the number that makes overcommit safe:
	// under full contention batch work receives roughly one percent of what a
	// serving pod does.
	if ratio := WeightShared / WeightReclaimed; ratio < 50 {
		t.Errorf("shared:reclaimed ratio is only %d:1; reclaimed work will not yield fast enough", ratio)
	}
	// An unknown level must land on the safe middle rather than the top.
	if WeightFor("nonsense") != WeightShared {
		t.Error("unknown levels should default to the shared weight")
	}
}

func TestDedicatedPodsGetExclusiveCPUs(t *testing.T) {
	m := NewManager(nil, nil, twoZones(), Config{SharedPoolMinCPUs: 2, ReclaimedPoolMinCPUs: 1})
	pods := []*corev1.Pod{
		pod("big", v1alpha1.QoSDedicatedCores, "4", false),
		pod("small", v1alpha1.QoSDedicatedCores, "2", false),
		pod("web", v1alpha1.QoSSharedCores, "500m", false),
	}
	pools := m.computePools(pods)

	big, ok := poolNamed(pools, "big")
	if !ok {
		t.Fatal("no pool for the dedicated pod")
	}
	small, _ := poolNamed(pools, "small")
	if big.CPUs.Size() != 4 || small.CPUs.Size() != 2 {
		t.Errorf("dedicated sizes = %d / %d, want 4 / 2", big.CPUs.Size(), small.CPUs.Size())
	}
	// Exclusive means exclusive: two dedicated pods must not overlap.
	if !big.CPUs.Intersection(small.CPUs).IsEmpty() {
		t.Errorf("dedicated pools overlap: %s and %s", big.CPUs, small.CPUs)
	}
	shared, _ := poolNamed(pools, "shared")
	if !shared.CPUs.Intersection(big.CPUs).IsEmpty() {
		t.Errorf("shared pool overlaps a dedicated assignment: %s vs %s", shared.CPUs, big.CPUs)
	}
}

func TestFractionalDedicatedRequestRoundsUp(t *testing.T) {
	m := NewManager(nil, nil, twoZones(), Config{SharedPoolMinCPUs: 2, ReclaimedPoolMinCPUs: 1})
	pools := m.computePools([]*corev1.Pod{pod("odd", v1alpha1.QoSDedicatedCores, "1500m", false)})

	p, ok := poolNamed(pools, "odd")
	if !ok {
		t.Fatal("no pool allocated")
	}
	// An exclusive assignment cannot be a fraction of a core. The webhook
	// rejects this at admission; the pool manager rounds up as a backstop for
	// pods that predate the webhook.
	if p.CPUs.Size() != 2 {
		t.Errorf("allocated %d cpus for a 1500m request, want 2", p.CPUs.Size())
	}
}

func TestNUMABindingKeepsAPodInOneZone(t *testing.T) {
	m := NewManager(nil, nil, twoZones(), Config{SharedPoolMinCPUs: 2, ReclaimedPoolMinCPUs: 1})
	pools := m.computePools([]*corev1.Pod{pod("svc", v1alpha1.QoSDedicatedCores, "4", true)})

	p, _ := poolNamed(pools, "svc")
	zone0 := cpuset.MustParse("0-7")
	zone1 := cpuset.MustParse("8-15")

	inZone0 := p.CPUs.Difference(zone0).IsEmpty()
	inZone1 := p.CPUs.Difference(zone1).IsEmpty()
	if !inZone0 && !inZone1 {
		t.Errorf("NUMA-bound pod was split across zones: %s", p.CPUs)
	}
}

func TestSharedPoolFloorIsRespected(t *testing.T) {
	m := NewManager(nil, nil, twoZones(), Config{SharedPoolMinCPUs: 6, ReclaimedPoolMinCPUs: 1})
	// Two 8-core dedicated pods would consume the entire machine.
	pools := m.computePools([]*corev1.Pod{
		pod("hog-a", v1alpha1.QoSDedicatedCores, "8", false),
		pod("hog-b", v1alpha1.QoSDedicatedCores, "8", false),
	})

	shared, ok := poolNamed(pools, "shared")
	if !ok {
		t.Fatal("no shared pool")
	}
	// A dedicated pod that cannot be given exclusive CPUs without starving the
	// shared tier is degraded to the shared pool rather than granted.
	if shared.CPUs.Size() < 6 {
		t.Errorf("shared pool shrank to %d cpus, below the floor of 6", shared.CPUs.Size())
	}
	if _, ok := poolNamed(pools, "hog-b"); ok {
		t.Error("the second dedicated pod was granted exclusive cpus despite the floor")
	}
}

func TestReservedCPUsAreCarvedOutFirst(t *testing.T) {
	m := NewManager(nil, nil, twoZones(), Config{
		ReservedSystemCPUs:   "0-1",
		SharedPoolMinCPUs:    2,
		ReclaimedPoolMinCPUs: 1,
	})
	pools := m.computePools([]*corev1.Pod{pod("svc", v1alpha1.QoSDedicatedCores, "4", false)})

	reserved, ok := poolNamed(pools, "reserved")
	if !ok {
		t.Fatal("no reserved pool")
	}
	if reserved.CPUs.String() != "0,1" {
		t.Errorf("reserved = %s, want 0,1", reserved.CPUs)
	}
	// The system must keep running, so nothing else may claim its CPUs.
	dedicated, _ := poolNamed(pools, "svc")
	if !dedicated.CPUs.Intersection(reserved.CPUs).IsEmpty() {
		t.Errorf("a dedicated pod took reserved cpus: %s", dedicated.CPUs)
	}
	shared, _ := poolNamed(pools, "shared")
	if !shared.CPUs.Intersection(reserved.CPUs).IsEmpty() {
		t.Errorf("the shared pool took reserved cpus: %s", shared.CPUs)
	}
}

func TestReclaimedMayOverlapShared(t *testing.T) {
	m := NewManager(nil, nil, twoZones(), Config{SharedPoolMinCPUs: 2, ReclaimedPoolMinCPUs: 1})
	pools := m.computePools([]*corev1.Pod{pod("web", v1alpha1.QoSSharedCores, "500m", false)})

	shared, _ := poolNamed(pools, "shared")
	reclaimed, ok := poolNamed(pools, "reclaimed")
	if !ok {
		t.Fatal("no reclaimed pool")
	}
	// Overlap is deliberate. Partitioning CPUs away from the shared tier to
	// give batch work its own would defeat the point; cpu.weight already makes
	// the reclaimed tier yield on the CPUs they share.
	if reclaimed.CPUs.Intersection(shared.CPUs).IsEmpty() {
		t.Error("reclaimed pool has no overlap with shared; it will never use idle capacity")
	}
}

func TestDetectTopologyFallsBackToOneZone(t *testing.T) {
	// No sysfs, as on a machine without NUMA or inside a container that cannot
	// see it. The pool logic must degrade to plain first-fit, not break.
	topo := DetectTopology("/nonexistent", 8)
	if len(topo.Zones) != 1 {
		t.Fatalf("zones = %d, want a single fallback zone", len(topo.Zones))
	}
	if topo.TotalCPUs() != 8 {
		t.Errorf("total cpus = %d, want 8", topo.TotalCPUs())
	}
}

func TestZoneForPrefersTheTightestFit(t *testing.T) {
	topo := twoZones()
	// Zone 0 has 3 free, zone 1 has 8. A 2-CPU request should take zone 0 and
	// leave the roomy zone intact for a request that needs it.
	free := cpuset.MustParse("0-2,8-15")
	zone, ok := topo.ZoneFor(free, 2)
	if !ok {
		t.Fatal("no zone found")
	}
	if zone.ID != 0 {
		t.Errorf("chose zone %d, want the tighter zone 0", zone.ID)
	}

	// A request bigger than any single zone's free set finds nothing, and the
	// caller falls back to spilling across zones.
	if _, ok := topo.ZoneFor(free, 12); ok {
		t.Error("a 12-cpu request should not fit in any single zone here")
	}
}
