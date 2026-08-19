package eviction

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"

	"github.com/kongxiangrui/kqos/pkg/apis/kqos/v1alpha1"
	"github.com/kongxiangrui/kqos/pkg/qos"
)

func init() {
	Register("memory-pressure", func() Plugin { return &memoryPressurePlugin{} })
	Register("cpu-suppression", func() Plugin { return &cpuSuppressionPlugin{} })
	Register("reclaimed-overrun", func() Plugin { return &reclaimedOverrunPlugin{} })
}

// memoryPressurePlugin evicts when the node is running out of memory.
//
// Memory is incompressible, so unlike CPU there is no throttle to reach for:
// either something is evicted or the kernel OOM killer picks a victim, and the
// kernel picks by badness score rather than by service tier. Evicting first is
// how kqos keeps that choice.
type memoryPressurePlugin struct{}

func (p *memoryPressurePlugin) Name() string { return "memory-pressure" }

func (p *memoryPressurePlugin) Evaluate(_ context.Context, sig Signal) ([]Verdict, error) {
	memAlloc := quantityBytes(sig.Allocatable, corev1.ResourceMemory)
	if memAlloc <= 0 {
		return nil, nil
	}
	threshold := int64(sig.Config.MemoryPressureThresholdPercent)
	used := int64(sig.Sample.MemoryBytes)
	utilPercent := used * 100 / memAlloc
	if utilPercent < threshold {
		return nil, nil
	}

	// Recover down to five points below the trigger, giving hysteresis. Aiming
	// exactly at the threshold guarantees a second eviction on the next tick.
	target := memAlloc * (threshold - 5) / 100
	mustFree := used - target
	if mustFree <= 0 {
		return nil, nil
	}

	candidates := rankByTierThenUsage(sig, func(s podUsage) float64 {
		return float64(s.memoryBytes)
	})

	var out []Verdict
	var freed int64
	for _, c := range candidates {
		if freed >= mustFree {
			break
		}
		freed += int64(c.memoryBytes)
		out = append(out, Verdict{
			Pod:                 c.pod,
			Plugin:              p.Name(),
			Reason:              "NodeMemoryPressure",
			Score:               float64(c.memoryBytes),
			Message:             fmt.Sprintf("node memory at %d%% of %s (threshold %d%%); evicting %s pod using %s to reclaim toward %d%%", utilPercent, humanBytes(memAlloc), threshold, c.level, humanBytes(int64(c.memoryBytes)), threshold-5),
			ReleasesMemoryBytes: int64(c.memoryBytes),
		})
	}
	return out, nil
}

// cpuSuppressionPlugin evicts reclaimed pods when the protected tiers are
// being starved of CPU despite cgroup weighting.
//
// CPU is compressible, so eviction is the last resort: cpu.weight should
// already be making reclaimed work yield. This plugin triggers on PSI rather
// than utilisation precisely because it is looking for the case where
// weighting has failed -- protected threads are stalled while the node's raw
// utilisation may look unremarkable.
type cpuSuppressionPlugin struct{}

func (p *cpuSuppressionPlugin) Name() string { return "cpu-suppression" }

func (p *cpuSuppressionPlugin) Evaluate(_ context.Context, sig Signal) ([]Verdict, error) {
	stall := sig.Recommendation.Pressure.CPUSomeStalledPercent
	util := sig.Recommendation.Pressure.CPUUtilizationPercent
	if stall < sig.Config.CPUSomeStalledThresholdPercent &&
		util < sig.Config.CPUPressureThresholdPercent {
		return nil, nil
	}

	usages := usagesFor(sig)
	var reclaimed []podUsage
	for _, u := range usages {
		if u.level == v1alpha1.QoSReclaimedCores {
			reclaimed = append(reclaimed, u)
		}
	}
	if len(reclaimed) == 0 {
		// Nothing oversold is running, so the contention is between protected
		// tiers. That is a capacity problem, not something eviction can fix,
		// and evicting a protected pod here would make it worse.
		return nil, nil
	}

	sort.Slice(reclaimed, func(i, j int) bool {
		return reclaimed[i].cpuMilli > reclaimed[j].cpuMilli
	})

	// Evict one per tick. CPU pressure responds within a few seconds, so
	// draining every reclaimed pod at once would overshoot badly.
	top := reclaimed[0]
	return []Verdict{{
		Pod:    top.pod,
		Plugin: p.Name(),
		Reason: "NodeCPUSuppression",
		Score:  top.cpuMilli,
		Message: fmt.Sprintf("protected workloads stalled on CPU (psi some avg10 %d%%, node cpu %d%%); evicting largest reclaimed pod using %.0fm",
			stall, util, top.cpuMilli),
		ReleasesCPUMilli: int64(top.cpuMilli),
	}}, nil
}

// reclaimedOverrunPlugin evicts reclaimed pods that consume materially more
// than they asked for.
//
// The reclaimed tier is a contract: you get cheap capacity, and in exchange
// your footprint stays predictable enough that the advisor's maths holds. A
// pod that requested 100m and is burning 900m has broken that contract and is
// silently eating the headroom that protects everyone else -- so this plugin
// fires without waiting for node-level pressure.
type reclaimedOverrunPlugin struct{}

func (p *reclaimedOverrunPlugin) Name() string { return "reclaimed-overrun" }

// overrunFactor is how far past its request a reclaimed pod may drift before
// it is considered to have broken the contract.
const overrunFactor = 3.0

func (p *reclaimedOverrunPlugin) Evaluate(_ context.Context, sig Signal) ([]Verdict, error) {
	// Only act when the node is at least moderately stressed. On an idle node
	// an overrunning reclaimed pod is harmless and evicting it would waste the
	// very capacity kqos exists to sell.
	if sig.Recommendation.Pressure.Level == v1alpha1.PressureNone {
		return nil, nil
	}

	var out []Verdict
	for _, u := range usagesFor(sig) {
		if u.level != v1alpha1.QoSReclaimedCores {
			continue
		}
		req := qos.EffectiveRequests(u.pod)
		reqCPU := req[corev1.ResourceCPU]
		reqMilli := reqCPU.MilliValue()
		if reqMilli <= 0 {
			continue
		}
		ratio := u.cpuMilli / float64(reqMilli)
		if ratio < overrunFactor {
			continue
		}
		out = append(out, Verdict{
			Pod:    u.pod,
			Plugin: p.Name(),
			Reason: "ReclaimedOverrun",
			Score:  ratio,
			Message: fmt.Sprintf("reclaimed pod using %.0fm against a %dm request (%.1fx) while node pressure is %s",
				u.cpuMilli, reqMilli, ratio, sig.Recommendation.Pressure.Level),
			ReleasesCPUMilli: int64(u.cpuMilli),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out, nil
}

// podUsage pairs a pod with its measured footprint.
type podUsage struct {
	pod         *corev1.Pod
	level       v1alpha1.QoSLevel
	cpuMilli    float64
	memoryBytes uint64
}

func usagesFor(sig Signal) []podUsage {
	out := make([]podUsage, 0, len(sig.Pods))
	for _, pod := range sig.Pods {
		u := podUsage{pod: pod, level: qos.LevelOf(pod)}
		if s, ok := sig.SampleByUID[string(pod.UID)]; ok {
			u.cpuMilli = s.CPUMilli
			u.memoryBytes = s.MemoryBytes
		}
		out = append(out, u)
	}
	return out
}

// rankByTierThenUsage orders candidates so that the cheapest-to-lose pods come
// first: reclaimed before shared before dedicated, and within a tier the
// biggest consumer first so that the fewest evictions recover the most.
func rankByTierThenUsage(sig Signal, weight func(podUsage) float64) []podUsage {
	usages := usagesFor(sig)
	sort.SliceStable(usages, func(i, j int) bool {
		ri, rj := usages[i].level.EvictionRank(), usages[j].level.EvictionRank()
		if ri != rj {
			return ri > rj
		}
		return weight(usages[i]) > weight(usages[j])
	})
	return usages
}

func quantityBytes(rl corev1.ResourceList, name corev1.ResourceName) int64 {
	if q, ok := rl[name]; ok {
		return q.Value()
	}
	return 0
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit && exp < 3; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(b)/float64(div), "KMGT"[exp])
}
