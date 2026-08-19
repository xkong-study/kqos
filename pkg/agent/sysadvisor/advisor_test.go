package sysadvisor

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/xkong-study/kqos/pkg/agent/collector"
	"github.com/xkong-study/kqos/pkg/apis/kqos/v1alpha1"
)

// allocatable builds a node capacity of n whole CPUs and g GiB of memory.
func allocatable(cpus int64, gib int64) corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewQuantity(cpus, resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(gib*1024*1024*1024, resource.BinarySI),
	}
}

// sample builds a node sample with one protected pod and one reclaimed pod at
// the given consumption levels.
func sample(at time.Time, protectedMilli, reclaimedMilli float64, protectedMem, reclaimedMem uint64) collector.NodeSample {
	return collector.NodeSample{
		Timestamp:   at,
		Valid:       true,
		CPUMilli:    protectedMilli + reclaimedMilli,
		MemoryBytes: protectedMem + reclaimedMem,
		Pods: []collector.PodSample{
			{UID: "protected", QoSLevel: v1alpha1.QoSSharedCores, CPUMilli: protectedMilli, MemoryBytes: protectedMem, Valid: true},
			{UID: "reclaimed", QoSLevel: v1alpha1.QoSReclaimedCores, CPUMilli: reclaimedMilli, MemoryBytes: reclaimedMem, Valid: true},
		},
	}
}

// feed pushes n identical samples spaced one interval apart and returns the
// final recommendation.
func feed(a *Advisor, n int, protectedMilli, reclaimedMilli float64, protectedMem, reclaimedMem uint64, alloc corev1.ResourceList) Recommendation {
	now := time.Now()
	var rec Recommendation
	for i := 0; i < n; i++ {
		at := now.Add(time.Duration(i) * 10 * time.Second)
		a.Observe(sample(at, protectedMilli, reclaimedMilli, protectedMem, reclaimedMem))
		rec = a.Recommend(alloc, at)
	}
	return rec
}

func testConfig() Config {
	cfg := DefaultConfig()
	// No growth damping: the formula and the damping are tested separately.
	cfg.GrowthStepPercent = 100
	cfg.MinReclaimCPUMilli = 0
	cfg.MinReclaimMemBytes = 0
	return cfg
}

func TestReclaimableFollowsTheFormula(t *testing.T) {
	cfg := testConfig()
	a := New(cfg)

	// 8 cores allocatable, protected tier steadily using 2 cores.
	// budget   = 8000 * 75% = 6000
	// headroom = 8000 * 10% =  800
	// value    = 6000 - 2000 - 800 = 3200
	// cap      = 8000 * 50% = 4000, so the cap does not bind.
	rec := feed(a, 40, 2000, 0, 1<<30, 0, allocatable(8, 16))

	if rec.ReclaimableCPUMilli != 3200 {
		t.Errorf("reclaimable cpu = %d, want 3200 (reason %q)", rec.ReclaimableCPUMilli, rec.Reason)
	}
	if rec.Reason != "usage-derived/usage-derived" {
		t.Errorf("reason = %q, want the usage term to bind", rec.Reason)
	}
	if rec.ProtectedCPUMilli != 2000 {
		t.Errorf("protected cpu = %d, want 2000", rec.ProtectedCPUMilli)
	}
}

func TestMaxReclaimCapBinds(t *testing.T) {
	a := New(testConfig())

	// An almost idle node: the usage term would allow 6000-0-800 = 5200, but
	// the 50% cap holds it to 4000. Without that cap a quiet node gets packed
	// to a point no traffic spike can be absorbed from.
	rec := feed(a, 40, 0, 0, 0, 0, allocatable(8, 16))

	if rec.ReclaimableCPUMilli != 4000 {
		t.Errorf("reclaimable cpu = %d, want the 4000 cap", rec.ReclaimableCPUMilli)
	}
	if rec.Reason != "max-reclaim-cap/max-reclaim-cap" {
		t.Errorf("reason = %q, want the cap to bind", rec.Reason)
	}
}

func TestNoHeadroomYieldsZero(t *testing.T) {
	a := New(testConfig())

	// Protected tier already past the target: nothing is spare, and the
	// advisor must say zero rather than a negative number.
	rec := feed(a, 40, 7000, 0, 1<<30, 0, allocatable(8, 16))

	if rec.ReclaimableCPUMilli != 0 {
		t.Errorf("reclaimable cpu = %d, want 0", rec.ReclaimableCPUMilli)
	}
}

func TestReclaimedUsageDoesNotShrinkTheOffer(t *testing.T) {
	alloc := allocatable(8, 16)

	quiet := feed(New(testConfig()), 40, 2000, 0, 1<<30, 0, alloc)
	busy := feed(New(testConfig()), 40, 2000, 4000, 1<<30, 1<<29, alloc)

	// This is the property that keeps the control loop from oscillating: the
	// reclaimed tier consuming what it was sold must not reduce what is on
	// offer, or every pod that lands shrinks the pool that admitted it.
	if quiet.ReclaimableCPUMilli != busy.ReclaimableCPUMilli {
		t.Errorf("reclaimed usage changed the offer: %d with idle batch, %d with busy batch",
			quiet.ReclaimableCPUMilli, busy.ReclaimableCPUMilli)
	}
}

func TestDampGrowsInStepsAndShrinksInstantly(t *testing.T) {
	cfg := testConfig()
	cfg.GrowthStepPercent = 25
	a := New(cfg)

	// Growth closes a quarter of the gap per tick.
	if got := a.damp(1000, 5000); got != 2000 {
		t.Errorf("damp(1000 -> 5000) = %d, want 2000", got)
	}
	if got := a.damp(2000, 5000); got != 2750 {
		t.Errorf("damp(2000 -> 5000) = %d, want 2750", got)
	}
	// A gap too small to take a quarter of still advances, so the offer cannot
	// stall one unit short of its target forever.
	if got := a.damp(4999, 5000); got != 5000 {
		t.Errorf("damp(4999 -> 5000) = %d, want 5000", got)
	}
	// Shrinking is never damped: withdrawing capacity is the safe direction
	// and delaying it is how a node ends up oversold during a spike.
	if got := a.damp(5000, 0); got != 0 {
		t.Errorf("damp(5000 -> 0) = %d, want 0", got)
	}
	if got := a.damp(5000, 4000); got != 4000 {
		t.Errorf("damp(5000 -> 4000) = %d, want 4000", got)
	}
}

func TestGrowthIsDampedButShrinkIsImmediate(t *testing.T) {
	cfg := testConfig()
	cfg.GrowthStepPercent = 25
	// A three-sample window so the history can actually be replaced inside the
	// test. With the production five-minute window a handful of idle samples
	// cannot move the p95 at all -- which is the smoothing working correctly,
	// not something worth defeating here.
	cfg.WindowSeconds = 30
	cfg.IntervalSeconds = 10
	a := New(cfg)
	alloc := allocatable(8, 16)
	now := time.Now()
	tick := 0

	step := func(protectedMilli float64) Recommendation {
		at := now.Add(time.Duration(tick) * 10 * time.Second)
		tick++
		a.Observe(sample(at, protectedMilli, 0, 1<<30, 0))
		return a.Recommend(alloc, at)
	}

	// A busy node: budget 6000 - used 5000 - headroom 800 = 200 on offer.
	// Damping applies on the way up from zero too, so give it enough ticks to
	// converge before using it as a baseline.
	var busy Recommendation
	for i := 0; i < 25; i++ {
		busy = step(5000)
	}
	if busy.ReclaimableCPUMilli != 200 {
		t.Fatalf("busy offer = %d, want 200", busy.ReclaimableCPUMilli)
	}

	// The node empties. Once the window has turned over, the offer climbs --
	// but in steps, nowhere near the 4000 cap on the first tick.
	var grown Recommendation
	for i := 0; i < 3; i++ {
		grown = step(0)
	}
	if grown.ReclaimableCPUMilli <= busy.ReclaimableCPUMilli {
		t.Fatalf("offer did not grow: %d -> %d", busy.ReclaimableCPUMilli, grown.ReclaimableCPUMilli)
	}
	if grown.ReclaimableCPUMilli >= 4000 {
		t.Errorf("offer jumped straight to the cap (%d); growth should be damped", grown.ReclaimableCPUMilli)
	}

	// Traffic returns. Withdrawal happens on the very next tick.
	shrunk := step(7500)
	if shrunk.ReclaimableCPUMilli != 0 {
		t.Errorf("offer did not withdraw immediately: %d", shrunk.ReclaimableCPUMilli)
	}
}

func TestMemoryUsesWindowMaxNotPercentile(t *testing.T) {
	cfg := testConfig()
	cfg.Algorithm = "p95"
	a := New(cfg)
	alloc := allocatable(8, 16)
	now := time.Now()

	const gib = uint64(1) << 30

	// Thirty quiet samples then one spike. A p95 would discard the spike --
	// which is exactly the sample that would have OOMed the node.
	for i := 0; i < 30; i++ {
		at := now.Add(time.Duration(i) * 10 * time.Second)
		a.Observe(sample(at, 1000, 0, gib, 0))
		a.Recommend(alloc, at)
	}
	quiet := a.last.ReclaimableMemoryBytes

	at := now.Add(310 * time.Second)
	a.Observe(sample(at, 1000, 0, 8*gib, 0))
	spiked := a.Recommend(alloc, at)

	if spiked.ReclaimableMemoryBytes >= quiet {
		t.Errorf("a memory spike did not reduce the offer: %d -> %d",
			quiet, spiked.ReclaimableMemoryBytes)
	}
	if spiked.ProtectedMemoryBytes != int64(8*gib) {
		t.Errorf("protected memory = %d, want the window maximum %d",
			spiked.ProtectedMemoryBytes, 8*gib)
	}
}

func TestCriticalPressureWithdrawsEverything(t *testing.T) {
	cfg := testConfig()
	a := New(cfg)
	alloc := allocatable(8, 16)
	now := time.Now()

	// Memory past the 85% threshold on an otherwise idle node: the usage
	// formula would still offer CPU, but a node in trouble must stop selling.
	const gib = uint64(1) << 30
	var rec Recommendation
	for i := 0; i < 40; i++ {
		at := now.Add(time.Duration(i) * 10 * time.Second)
		a.Observe(sample(at, 100, 0, 15*gib, 0))
		rec = a.Recommend(alloc, at)
	}

	if rec.Pressure.Level != v1alpha1.PressureCritical {
		t.Fatalf("pressure = %s, want Critical", rec.Pressure.Level)
	}
	if rec.ReclaimableCPUMilli != 0 || rec.ReclaimableMemoryBytes != 0 {
		t.Errorf("still offering capacity under critical pressure: cpu=%d mem=%d",
			rec.ReclaimableCPUMilli, rec.ReclaimableMemoryBytes)
	}
	if rec.Reason != "critical-pressure" {
		t.Errorf("reason = %q, want critical-pressure", rec.Reason)
	}
}

func TestPSITriggersPressureWithoutHighUtilisation(t *testing.T) {
	a := New(testConfig())
	alloc := allocatable(8, 16)
	now := time.Now()

	var rec Recommendation
	for i := 0; i < 40; i++ {
		at := now.Add(time.Duration(i) * 10 * time.Second)
		s := sample(at, 500, 0, 1<<30, 0)
		// Low utilisation, high stall: threads are runnable and being denied
		// CPU. Utilisation alone would call this node healthy.
		s.CPUPressure.SomeAvg10 = 60
		a.Observe(s)
		rec = a.Recommend(alloc, at)
	}

	if rec.Pressure.CPUUtilizationPercent > 20 {
		t.Fatalf("test setup wrong: utilisation is %d%%", rec.Pressure.CPUUtilizationPercent)
	}
	if rec.Pressure.Level != v1alpha1.PressureCritical {
		t.Errorf("pressure = %s, want Critical from PSI alone", rec.Pressure.Level)
	}
}

func TestRevisionOnlyMovesOnMaterialChange(t *testing.T) {
	a := New(testConfig())
	alloc := allocatable(8, 16)
	now := time.Now()

	for i := 0; i < 40; i++ {
		at := now.Add(time.Duration(i) * 10 * time.Second)
		a.Observe(sample(at, 2000, 0, 1<<30, 0))
		a.Recommend(alloc, at)
	}
	stable := a.last.Revision

	// A sub-percent wobble must not wake every watcher in the cluster.
	at := now.Add(410 * time.Second)
	a.Observe(sample(at, 2001, 0, 1<<30, 0))
	if got := a.Recommend(alloc, at).Revision; got != stable {
		t.Errorf("revision moved on noise: %d -> %d", stable, got)
	}

	// A real change must.
	for i := 0; i < 40; i++ {
		at = now.Add(time.Duration(420+i*10) * time.Second)
		a.Observe(sample(at, 5500, 0, 1<<30, 0))
	}
	if got := a.Recommend(alloc, at).Revision; got == stable {
		t.Error("revision did not move on a real change")
	}
}

func TestConfidenceStartsLow(t *testing.T) {
	a := New(testConfig())
	alloc := allocatable(8, 16)
	now := time.Now()

	a.Observe(sample(now, 1000, 0, 1<<30, 0))
	if rec := a.Recommend(alloc, now); rec.Confidence != "Low" {
		t.Errorf("confidence = %q on the first sample, want Low", rec.Confidence)
	}

	rec := feed(a, 40, 1000, 0, 1<<30, 0, alloc)
	if rec.Confidence == "Low" {
		t.Error("confidence stayed Low after a full window")
	}
}
