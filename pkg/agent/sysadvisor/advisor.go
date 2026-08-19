// Package sysadvisor decides how much of a node's capacity kqos is willing to
// resell to the reclaimed tier.
//
// The whole system reduces to one question -- "how much of what we promised is
// nobody actually using?" -- and this is where that question is answered. Every
// other component either feeds this package data or carries out its verdict.
package sysadvisor

import (
	"math"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/kongxiangrui/kqos/pkg/agent/collector"
	"github.com/kongxiangrui/kqos/pkg/apis/kqos/v1alpha1"
)

// Recommendation is one advisor verdict for one node.
type Recommendation struct {
	// ReclaimableCPUMilli and ReclaimableMemoryBytes are what may be advertised
	// to the reclaimed tier, after damping and every cap.
	ReclaimableCPUMilli    int64
	ReclaimableMemoryBytes int64

	// ProtectedCPUMilli and ProtectedMemoryBytes are the smoothed consumption of
	// the tiers kqos guarantees. This is the figure the reclaim maths is built
	// on, surfaced so the number is auditable rather than a black box.
	ProtectedCPUMilli    int64
	ProtectedMemoryBytes int64

	// Pressure is the classification the eviction manager keys off.
	Pressure v1alpha1.NodePressure

	// Confidence is Low until the observation window has filled.
	Confidence string

	// Reason explains which constraint bound the result, e.g. "max-reclaim-cap".
	// Without this the number is impossible to debug in production.
	Reason string

	// Revision increments whenever the recommendation materially changes.
	Revision int64
}

// ReclaimableResourceList renders the recommendation as a ResourceList for the
// NodeResourceProfile status.
func (r Recommendation) ReclaimableResourceList() corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewMilliQuantity(r.ReclaimableCPUMilli, resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(r.ReclaimableMemoryBytes, resource.BinarySI),
	}
}

// Advisor holds the per-node observation windows and the last verdict.
type Advisor struct {
	// protectedCPU and protectedMemory track consumption of the system,
	// dedicated and shared tiers only.
	//
	// Excluding the reclaimed tier from this figure is the single most
	// important decision in the package. If reclaimed usage were counted, then
	// every reclaimed pod that started would shrink the reclaimable pool that
	// admitted it, which drives the node into an oscillation: pack, shrink,
	// evict, unpack, grow, pack. Measuring only the tiers kqos actually
	// protects makes the control loop monotone.
	protectedCPU    *window
	protectedMemory *window

	// nodeCPU and nodeMemory track total utilisation, used for pressure only.
	nodeCPU    *window
	nodeMemory *window
	cpuPSI     *window
	memPSI     *window

	last     Recommendation
	revision int64
	cfg      Config
}

// Config is the advisor's slice of the QoSPolicy, resolved into native types.
type Config struct {
	WindowSeconds   int32
	IntervalSeconds int32
	Algorithm       string
	EWMAAlpha       float64

	CPUTargetPercent    int32
	MemoryTargetPercent int32
	HeadroomPercent     int32
	MaxReclaimPercent   int32
	MinReclaimCPUMilli  int64
	MinReclaimMemBytes  int64

	MemoryPressureThresholdPercent int32
	CPUPressureThresholdPercent    int32
	CPUStallThresholdPercent       int32

	// GrowthStepPercent limits how fast the reclaimable figure may rise, as a
	// percentage of the gap to the new target. Shrinking is never damped.
	GrowthStepPercent int32
}

// DefaultConfig mirrors the CRD defaults so the advisor is usable before any
// QoSPolicy exists.
func DefaultConfig() Config {
	return Config{
		WindowSeconds:                  300,
		IntervalSeconds:                10,
		Algorithm:                      "p95",
		EWMAAlpha:                      0.3,
		CPUTargetPercent:               75,
		MemoryTargetPercent:            65,
		HeadroomPercent:                10,
		MaxReclaimPercent:              50,
		MinReclaimCPUMilli:             200,
		MinReclaimMemBytes:             256 * 1024 * 1024,
		MemoryPressureThresholdPercent: 85,
		CPUPressureThresholdPercent:    92,
		CPUStallThresholdPercent:       40,
		GrowthStepPercent:              25,
	}
}

// New builds an advisor sized from the config.
func New(cfg Config) *Advisor {
	a := &Advisor{cfg: cfg}
	a.resize()
	return a
}

// SetConfig applies a new configuration. Window geometry changes rebuild the
// ring buffers, which discards history -- acceptable, because a change to the
// window is a statement that the old history is no longer the right basis.
func (a *Advisor) SetConfig(cfg Config) {
	geometryChanged := cfg.WindowSeconds != a.cfg.WindowSeconds ||
		cfg.IntervalSeconds != a.cfg.IntervalSeconds
	a.cfg = cfg
	if geometryChanged || a.protectedCPU == nil {
		a.resize()
	}
}

// Config returns the current configuration.
func (a *Advisor) Config() Config { return a.cfg }

func (a *Advisor) resize() {
	interval := a.cfg.IntervalSeconds
	if interval < 1 {
		interval = 1
	}
	capacity := int(a.cfg.WindowSeconds / interval)
	if capacity < 3 {
		capacity = 3
	}
	horizon := time.Duration(a.cfg.WindowSeconds) * time.Second

	a.protectedCPU = newWindow(capacity, horizon)
	a.protectedMemory = newWindow(capacity, horizon)
	a.nodeCPU = newWindow(capacity, horizon)
	a.nodeMemory = newWindow(capacity, horizon)
	a.cpuPSI = newWindow(capacity, horizon)
	a.memPSI = newWindow(capacity, horizon)
}

// Observe folds one sample into the windows.
func (a *Advisor) Observe(s collector.NodeSample) {
	if !s.Valid {
		return
	}
	var protCPU float64
	var protMem uint64
	for _, p := range s.Pods {
		if p.QoSLevel == v1alpha1.QoSReclaimedCores {
			continue
		}
		if p.Valid {
			protCPU += p.CPUMilli
		}
		protMem += p.MemoryBytes
	}

	a.protectedCPU.push(protCPU, s.Timestamp)
	a.protectedMemory.push(float64(protMem), s.Timestamp)
	a.nodeCPU.push(s.CPUMilli, s.Timestamp)
	a.nodeMemory.push(float64(s.MemoryBytes), s.Timestamp)
	a.cpuPSI.push(s.CPUPressure.SomeAvg10, s.Timestamp)
	a.memPSI.push(s.MemoryPressure.FullAvg10, s.Timestamp)
}

// Recommend produces a verdict from the current windows against the node's
// allocatable capacity.
func (a *Advisor) Recommend(allocatable corev1.ResourceList, now time.Time) Recommendation {
	cpuAlloc := milliOf(allocatable, corev1.ResourceCPU)
	memAlloc := bytesOf(allocatable, corev1.ResourceMemory)

	protCPU := int64(a.protectedCPU.aggregate(a.cfg.Algorithm, a.cfg.EWMAAlpha, now))
	// Memory is incompressible: a percentile that discards the top 5% of
	// observations discards exactly the moments that would OOM the node. The
	// memory aggregate is therefore always the window maximum, regardless of
	// the configured algorithm.
	protMem := int64(a.protectedMemory.max(now))

	cpuReclaim, cpuReason := a.reclaimable(cpuAlloc, protCPU, a.cfg.CPUTargetPercent, a.cfg.MinReclaimCPUMilli)
	memReclaim, memReason := a.reclaimable(memAlloc, protMem, a.cfg.MemoryTargetPercent, a.cfg.MinReclaimMemBytes)

	// Damp growth. A node that has just been drained looks gloriously empty;
	// advertising all of it at once invites a stampede of reclaimed pods that
	// the next traffic peak then has to evict.
	cpuReclaim = a.damp(a.last.ReclaimableCPUMilli, cpuReclaim)
	memReclaim = a.damp(a.last.ReclaimableMemoryBytes, memReclaim)

	pressure := a.pressure(cpuAlloc, memAlloc, now)

	// Under real pressure kqos stops selling. This is the fast path out of a
	// bad state: withdrawing the advertisement prevents new reclaimed pods from
	// landing while the eviction manager deals with the ones already there.
	reason := cpuReason + "/" + memReason
	if pressure.Level == v1alpha1.PressureCritical {
		cpuReclaim, memReclaim = 0, 0
		reason = "critical-pressure"
	}

	confidence := "High"
	if !a.protectedCPU.full() {
		confidence = "Low"
	} else if a.protectedCPU.count(now) < a.protectedCPU.capacity/2 {
		confidence = "Medium"
	}

	rec := Recommendation{
		ReclaimableCPUMilli:    cpuReclaim,
		ReclaimableMemoryBytes: memReclaim,
		ProtectedCPUMilli:      protCPU,
		ProtectedMemoryBytes:   protMem,
		Pressure:               pressure,
		Confidence:             confidence,
		Reason:                 reason,
	}

	if a.materiallyDiffers(rec) {
		a.revision++
	}
	rec.Revision = a.revision
	a.last = rec
	return rec
}

// reclaimable applies the core formula and reports which term bound it.
func (a *Advisor) reclaimable(allocatable, protected int64, targetPercent int32, floor int64) (int64, string) {
	if allocatable <= 0 {
		return 0, "no-allocatable"
	}
	budget := allocatable * int64(targetPercent) / 100
	headroom := allocatable * int64(a.cfg.HeadroomPercent) / 100

	value := budget - protected - headroom
	reason := "usage-derived"

	if value <= 0 {
		return 0, "no-headroom"
	}
	if cap := allocatable * int64(a.cfg.MaxReclaimPercent) / 100; value > cap {
		value, reason = cap, "max-reclaim-cap"
	}
	if value < floor {
		return 0, "below-floor"
	}
	return value, reason
}

// damp lets the figure fall instantly but rise in steps.
func (a *Advisor) damp(current, target int64) int64 {
	if target <= current {
		return target
	}
	step := a.cfg.GrowthStepPercent
	if step <= 0 || step >= 100 {
		return target
	}
	delta := (target - current) * int64(step) / 100
	if delta < 1 {
		delta = 1
	}
	next := current + delta
	if next > target {
		return target
	}
	return next
}

// pressure classifies the node. Utilisation and PSI are both consulted because
// they fail in opposite directions: utilisation misses contention on a node
// with many runnable-but-blocked threads, and PSI is noisy on a node that is
// merely busy.
func (a *Advisor) pressure(cpuAlloc, memAlloc int64, now time.Time) v1alpha1.NodePressure {
	cpuUtil := pct(int64(a.nodeCPU.percentile(90, now)), cpuAlloc)
	memUtil := pct(int64(a.nodeMemory.max(now)), memAlloc)
	cpuStall := a.cpuPSI.percentile(90, now)
	memStall := a.memPSI.percentile(90, now)

	p := v1alpha1.NodePressure{
		Level:                    v1alpha1.PressureNone,
		CPUUtilizationPercent:    int32(cpuUtil),
		MemoryUtilizationPercent: int32(memUtil),
		CPUSomeStalledPercent:    int32(math.Round(cpuStall)),
		MemoryFullStalledPercent: int32(math.Round(memStall)),
	}

	critical := memUtil >= int64(a.cfg.MemoryPressureThresholdPercent) ||
		cpuUtil >= int64(a.cfg.CPUPressureThresholdPercent) ||
		cpuStall >= float64(a.cfg.CPUStallThresholdPercent) ||
		memStall >= 10
	moderate := memUtil >= int64(a.cfg.MemoryPressureThresholdPercent)-10 ||
		cpuUtil >= int64(a.cfg.CPUPressureThresholdPercent)-10 ||
		cpuStall >= float64(a.cfg.CPUStallThresholdPercent)/2

	switch {
	case critical:
		p.Level = v1alpha1.PressureCritical
	case moderate:
		p.Level = v1alpha1.PressureModerate
	}
	return p
}

// materiallyDiffers suppresses revision churn from sub-percent wobble, so
// consumers watching AdvisorRevision are not woken for noise.
func (a *Advisor) materiallyDiffers(rec Recommendation) bool {
	if a.last.Pressure.Level != rec.Pressure.Level {
		return true
	}
	return relDiff(a.last.ReclaimableCPUMilli, rec.ReclaimableCPUMilli) > 0.02 ||
		relDiff(a.last.ReclaimableMemoryBytes, rec.ReclaimableMemoryBytes) > 0.02
}

func relDiff(a, b int64) float64 {
	if a == b {
		return 0
	}
	base := math.Abs(float64(a))
	if base < 1 {
		return 1
	}
	return math.Abs(float64(b-a)) / base
}

func pct(part, whole int64) int64 {
	if whole <= 0 {
		return 0
	}
	v := part * 100 / whole
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func milliOf(rl corev1.ResourceList, name corev1.ResourceName) int64 {
	if q, ok := rl[name]; ok {
		return q.MilliValue()
	}
	return 0
}

func bytesOf(rl corev1.ResourceList, name corev1.ResourceName) int64 {
	if q, ok := rl[name]; ok {
		return q.Value()
	}
	return 0
}
