// Package collector turns the cgroup v2 pseudo-filesystem into a stream of
// per-pod and per-node resource samples.
//
// The only subtlety here is that cgroup CPU accounting is cumulative: cpu.stat
// reports total microseconds burned since the cgroup was created. A usable
// sample is the derivative of that counter against wall-clock time, so the
// collector is stateful and the first tick after start-up deliberately yields
// no CPU figure rather than a fabricated one.
package collector

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/xkong-study/kqos/pkg/agent/cgroup"
	"github.com/xkong-study/kqos/pkg/apis/kqos/v1alpha1"
	"github.com/xkong-study/kqos/pkg/qos"
)

// PodSample is one observation of one pod.
type PodSample struct {
	UID       string
	Namespace string
	Name      string
	QoSLevel  v1alpha1.QoSLevel

	// CPUMilli is measured consumption in milli-cores over the interval.
	CPUMilli float64
	// MemoryBytes is the workload-attributable memory (see MemoryStat.Workload).
	MemoryBytes uint64
	// ThrottledRatio is the fraction of CFS periods in which the pod was
	// throttled during the interval, 0-1.
	ThrottledRatio float64

	Requests corev1.ResourceList
	Limits   corev1.ResourceList

	// Valid is false when this is the pod's first sample and no rate could be
	// derived yet, or when its cgroup could not be read.
	Valid bool
	// Err records why a sample is invalid, for logging without failing the tick.
	Err error
}

// RequestedCPUMilli is the pod's summed CPU request in milli-cores.
func (p PodSample) RequestedCPUMilli() int64 {
	if q, ok := p.Requests[corev1.ResourceCPU]; ok {
		return q.MilliValue()
	}
	return 0
}

// RequestedMemoryBytes is the pod's summed memory request.
func (p PodSample) RequestedMemoryBytes() int64 {
	if q, ok := p.Requests[corev1.ResourceMemory]; ok {
		return q.Value()
	}
	return 0
}

// NodeSample is one observation of the whole node.
type NodeSample struct {
	Timestamp time.Time
	// Interval is the wall time since the previous sample.
	Interval time.Duration

	// CPUMilli is total pod-attributable CPU consumption on the node.
	CPUMilli float64
	// MemoryBytes is total pod-attributable memory consumption.
	MemoryBytes uint64

	// CPUPressure and MemoryPressure are PSI readings for the node's cgroup.
	CPUPressure    cgroup.PSI
	MemoryPressure cgroup.PSI

	// OnlineCPUs is the number of CPUs the node's cgroup may run on.
	OnlineCPUs int

	Pods []PodSample

	// Degraded is set when the cgroup hierarchy was unreadable and the sample
	// is an estimate rather than a measurement. Every consumer checks this
	// before letting the reading influence an eviction decision.
	Degraded bool

	// Valid is false on the very first tick, before a rate can be derived.
	Valid bool
}

// ByQoS aggregates the pod samples into per-class totals.
func (n NodeSample) ByQoS() map[v1alpha1.QoSLevel]QoSTotals {
	out := make(map[v1alpha1.QoSLevel]QoSTotals, len(v1alpha1.KnownQoSLevels))
	for _, p := range n.Pods {
		t := out[p.QoSLevel]
		t.Pods++
		if p.Valid {
			t.ActualCPUMilli += p.CPUMilli
			t.ActualMemoryBytes += p.MemoryBytes
		}
		t.RequestedCPUMilli += p.RequestedCPUMilli()
		t.RequestedMemoryBytes += p.RequestedMemoryBytes()
		if q, ok := p.Limits[corev1.ResourceCPU]; ok {
			t.LimitCPUMilli += q.MilliValue()
		}
		if q, ok := p.Limits[corev1.ResourceMemory]; ok {
			t.LimitMemoryBytes += q.Value()
		}
		out[p.QoSLevel] = t
	}
	return out
}

// QoSTotals is the per-class rollup of a node sample.
type QoSTotals struct {
	Pods                 int32
	RequestedCPUMilli    int64
	RequestedMemoryBytes int64
	LimitCPUMilli        int64
	LimitMemoryBytes     int64
	ActualCPUMilli       float64
	ActualMemoryBytes    uint64
}

// ToResourceList renders the requested totals as a Kubernetes ResourceList.
func (t QoSTotals) ToResourceList(kind string) corev1.ResourceList {
	var cpuMilli int64
	var memBytes int64
	switch kind {
	case "requested":
		cpuMilli, memBytes = t.RequestedCPUMilli, t.RequestedMemoryBytes
	case "limits":
		cpuMilli, memBytes = t.LimitCPUMilli, t.LimitMemoryBytes
	case "actual":
		cpuMilli, memBytes = int64(t.ActualCPUMilli), int64(t.ActualMemoryBytes)
	}
	return corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewMilliQuantity(cpuMilli, resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(memBytes, resource.BinarySI),
	}
}

// cpuCounter remembers the previous cumulative reading for one cgroup.
type cpuCounter struct {
	usageUsec   uint64
	nrPeriods   uint64
	nrThrottled uint64
	at          time.Time
}

// Collector samples the node on demand. It is not safe for concurrent use; the
// agent drives it from a single loop.
type Collector struct {
	fs       *cgroup.FS
	resolver *cgroup.Resolver

	prevPods map[string]cpuCounter
	prevNode cpuCounter

	// estimateWhenDegraded makes an unreadable hierarchy produce a
	// request-derived estimate instead of nothing, so the pipeline can be
	// exercised on a developer machine. Never enabled in-cluster.
	estimateWhenDegraded bool
}

// New builds a collector over the given hierarchy.
func New(fs *cgroup.FS, resolver *cgroup.Resolver, estimateWhenDegraded bool) *Collector {
	return &Collector{
		fs:                   fs,
		resolver:             resolver,
		prevPods:             make(map[string]cpuCounter),
		estimateWhenDegraded: estimateWhenDegraded,
	}
}

// Collect takes one sample covering the supplied pods, which the caller has
// already filtered to those scheduled on this node and not terminal.
func (c *Collector) Collect(pods []*corev1.Pod, onlineCPUs int) (NodeSample, error) {
	now := time.Now()
	sample := NodeSample{Timestamp: now, OnlineCPUs: onlineCPUs}

	if !c.fs.Available() {
		if !c.estimateWhenDegraded {
			return sample, fmt.Errorf("collect: %w at %s", cgroup.ErrUnsupported, c.fs.Root())
		}
		return c.estimate(pods, now), nil
	}

	kubepods := c.resolver.KubepodsPath()
	if kubepods != "" {
		if st, err := c.fs.CPUStat(kubepods); err == nil {
			prev := c.prevNode
			milli, ok := deltaMilliCores(prev, st, now)
			c.prevNode = cpuCounter{usageUsec: st.UsageUsec, at: now}
			if ok {
				sample.CPUMilli = milli
				sample.Valid = true
				sample.Interval = now.Sub(prev.at)
			}
		}
		if st, err := c.fs.MemoryStat(kubepods); err == nil {
			sample.MemoryBytes = st.Workload()
		}
	}

	// PSI is read from the hierarchy root: inside a kind node container that is
	// the node's own cgroup, so the figure is genuinely node-scoped.
	if psi, err := c.fs.Pressure("", "cpu"); err == nil {
		sample.CPUPressure = psi
	}
	if psi, err := c.fs.Pressure("", "memory"); err == nil {
		sample.MemoryPressure = psi
	}

	seen := make(map[string]struct{}, len(pods))
	var podCPUSum float64
	var podMemSum uint64

	for _, pod := range pods {
		uid := string(pod.UID)
		seen[uid] = struct{}{}
		ps := PodSample{
			UID:       uid,
			Namespace: pod.Namespace,
			Name:      pod.Name,
			QoSLevel:  qos.LevelOf(pod),
			Requests:  qos.PodRequests(pod),
			Limits:    qos.PodLimits(pod),
		}

		path, err := c.resolver.PodPath(uid)
		if err != nil {
			ps.Err = err
			sample.Pods = append(sample.Pods, ps)
			continue
		}

		cpuStat, err := c.fs.CPUStat(path)
		if err != nil {
			ps.Err = err
			sample.Pods = append(sample.Pods, ps)
			continue
		}
		prev, hadPrev := c.prevPods[uid]
		milli, ok := deltaMilliCores(prev, cpuStat, now)
		if hadPrev && ok {
			ps.CPUMilli = milli
			ps.Valid = true
			if d := cpuStat.NrPeriods - prev.nrPeriods; d > 0 {
				ps.ThrottledRatio = float64(cpuStat.NrThrottled-prev.nrThrottled) / float64(d)
			}
		}
		c.prevPods[uid] = cpuCounter{
			usageUsec:   cpuStat.UsageUsec,
			nrPeriods:   cpuStat.NrPeriods,
			nrThrottled: cpuStat.NrThrottled,
			at:          now,
		}

		if memStat, err := c.fs.MemoryStat(path); err == nil {
			ps.MemoryBytes = memStat.Workload()
		}

		podCPUSum += ps.CPUMilli
		podMemSum += ps.MemoryBytes
		sample.Pods = append(sample.Pods, ps)
	}

	// If the kubepods rollup was unavailable, fall back to summing pods. This
	// undercounts the pod-infra containers slightly but keeps the node figure
	// usable rather than zero.
	if kubepods == "" {
		sample.CPUMilli = podCPUSum
		sample.MemoryBytes = podMemSum
		sample.Valid = len(sample.Pods) > 0
	}

	// Drop counters for pods that have gone away, otherwise a long-lived agent
	// on a churny node leaks one map entry per pod ever scheduled.
	for uid := range c.prevPods {
		if _, ok := seen[uid]; !ok {
			delete(c.prevPods, uid)
			c.resolver.Forget(uid)
		}
	}

	return sample, nil
}

// deltaMilliCores converts two cumulative cpu.stat readings into a rate. It
// returns ok=false when there is no usable previous reading or when the
// counter went backwards, which happens if a cgroup is recreated with the same
// path.
func deltaMilliCores(prev cpuCounter, cur cgroup.CPUStat, now time.Time) (float64, bool) {
	if prev.at.IsZero() || cur.UsageUsec < prev.usageUsec {
		return 0, false
	}
	elapsed := now.Sub(prev.at)
	if elapsed <= 0 {
		return 0, false
	}
	deltaUsec := float64(cur.UsageUsec - prev.usageUsec)
	// One CPU fully busy for the interval is 1000 milli-cores.
	return deltaUsec / float64(elapsed.Microseconds()) * 1000.0, true
}

// estimate produces a plausible sample without touching cgroups, so the
// advisor, eviction and reporting paths can be tested off-cluster. Samples it
// produces are flagged Degraded and the eviction manager refuses to act on
// them.
func (c *Collector) estimate(pods []*corev1.Pod, now time.Time) NodeSample {
	sample := NodeSample{Timestamp: now, Degraded: true, Valid: true}
	for _, pod := range pods {
		req := qos.PodRequests(pod)
		cpuReq := req[corev1.ResourceCPU]
		memReq := req[corev1.ResourceMemory]
		// Assume workloads use 40% of what they ask for, which is roughly the
		// industry-wide reality that motivates overcommit in the first place.
		ps := PodSample{
			UID:         string(pod.UID),
			Namespace:   pod.Namespace,
			Name:        pod.Name,
			QoSLevel:    qos.LevelOf(pod),
			CPUMilli:    float64(cpuReq.MilliValue()) * 0.4,
			MemoryBytes: uint64(float64(memReq.Value()) * 0.4),
			Requests:    req,
			Limits:      qos.PodLimits(pod),
			Valid:       true,
		}
		sample.CPUMilli += ps.CPUMilli
		sample.MemoryBytes += ps.MemoryBytes
		sample.Pods = append(sample.Pods, ps)
	}
	return sample
}
