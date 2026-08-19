package collector

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/xkong-study/kqos/pkg/agent/cgroup"
)

func TestDeltaMilliCoresConvertsACounterIntoARate(t *testing.T) {
	now := time.Now()
	prev := cpuCounter{usageUsec: 1_000_000, at: now.Add(-10 * time.Second)}

	// One full CPU-second consumed over ten wall-clock seconds is 10% of one
	// CPU, i.e. 100 millicores.
	cur := cgroup.CPUStat{UsageUsec: 2_000_000}
	milli, ok := deltaMilliCores(prev, cur, now)
	if !ok {
		t.Fatal("rate was not produced")
	}
	if milli < 99 || milli > 101 {
		t.Errorf("rate = %.2fm, want ~100m", milli)
	}

	// Ten CPU-seconds over ten wall seconds is one whole core.
	cur = cgroup.CPUStat{UsageUsec: 11_000_000}
	milli, _ = deltaMilliCores(prev, cur, now)
	if milli < 999 || milli > 1001 {
		t.Errorf("rate = %.2fm, want ~1000m", milli)
	}
}

func TestNoRateWithoutAPreviousReading(t *testing.T) {
	// The first tick after start-up must yield nothing rather than treating
	// the cgroup's lifetime CPU total as if it were consumed in one interval.
	if _, ok := deltaMilliCores(cpuCounter{}, cgroup.CPUStat{UsageUsec: 999_999_999}, time.Now()); ok {
		t.Error("a rate was fabricated from the first sample")
	}
}

func TestCounterGoingBackwardsIsRejected(t *testing.T) {
	now := time.Now()
	prev := cpuCounter{usageUsec: 5_000_000, at: now.Add(-10 * time.Second)}

	// A cgroup recreated at the same path restarts its counter. Subtracting
	// would underflow into an enormous bogus rate.
	if _, ok := deltaMilliCores(prev, cgroup.CPUStat{UsageUsec: 1000}, now); ok {
		t.Error("a backwards counter produced a rate")
	}
}

func TestZeroElapsedIsRejected(t *testing.T) {
	now := time.Now()
	prev := cpuCounter{usageUsec: 1000, at: now}
	if _, ok := deltaMilliCores(prev, cgroup.CPUStat{UsageUsec: 2000}, now); ok {
		t.Error("a zero-length interval produced a rate")
	}
}

func TestByQoSAggregatesPerTier(t *testing.T) {
	s := NodeSample{
		Pods: []PodSample{
			{QoSLevel: "shared_cores", CPUMilli: 100, MemoryBytes: 1 << 20, Valid: true,
				Requests: rl("600m", "512Mi")},
			{QoSLevel: "shared_cores", CPUMilli: 50, MemoryBytes: 2 << 20, Valid: true,
				Requests: rl("600m", "512Mi")},
			{QoSLevel: "reclaimed_cores", CPUMilli: 900, MemoryBytes: 4 << 20, Valid: true,
				Requests: rl("0", "0")},
			// An invalid sample still counts as a pod and still contributes its
			// request, but must not contribute a fabricated usage figure.
			{QoSLevel: "shared_cores", CPUMilli: 9999, Valid: false, Requests: rl("600m", "512Mi")},
		},
	}
	byQoS := s.ByQoS()

	shared := byQoS["shared_cores"]
	if shared.Pods != 3 {
		t.Errorf("shared pods = %d, want 3", shared.Pods)
	}
	if shared.ActualCPUMilli != 150 {
		t.Errorf("shared actual cpu = %v, want 150 (the invalid sample must be excluded)", shared.ActualCPUMilli)
	}
	if shared.RequestedCPUMilli != 1800 {
		t.Errorf("shared requested cpu = %d, want 1800", shared.RequestedCPUMilli)
	}

	reclaimed := byQoS["reclaimed_cores"]
	if reclaimed.ActualCPUMilli != 900 {
		t.Errorf("reclaimed actual cpu = %v, want 900", reclaimed.ActualCPUMilli)
	}
	// This inequality -- a tier consuming far more than it requested -- is not
	// an error. It is the entire product.
	if reclaimed.RequestedCPUMilli >= int64(reclaimed.ActualCPUMilli) {
		t.Errorf("reclaimed tier is not overcommitted: requested %d, using %v",
			reclaimed.RequestedCPUMilli, reclaimed.ActualCPUMilli)
	}
}

func TestCollectFailsLoudlyWithoutCgroups(t *testing.T) {
	fs := cgroup.New(t.TempDir())
	c := New(fs, cgroup.NewResolver(fs), false)

	// Silently returning zeros would make an unreadable hierarchy look like a
	// completely idle node, and the advisor would resell the whole machine.
	if _, err := c.Collect(nil, 4); err == nil {
		t.Error("Collect succeeded with no cgroup hierarchy")
	}
}

func TestEstimateModeIsMarkedDegraded(t *testing.T) {
	fs := cgroup.New(t.TempDir())
	c := New(fs, cgroup.NewResolver(fs), true)

	sample, err := c.Collect(nil, 4)
	if err != nil {
		t.Fatal(err)
	}
	// Everything downstream keys off this flag to refuse to evict on a guess.
	if !sample.Degraded {
		t.Error("an estimated sample was not marked degraded")
	}
}

func rl(cpu, mem string) corev1.ResourceList {
	out := corev1.ResourceList{}
	if cpu != "" {
		out[corev1.ResourceCPU] = resource.MustParse(cpu)
	}
	if mem != "" {
		out[corev1.ResourceMemory] = resource.MustParse(mem)
	}
	return out
}
