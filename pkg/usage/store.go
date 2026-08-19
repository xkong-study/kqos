package usage

import (
	"math"
	"sort"
	"sync"
	"time"
)

// OwnerResolver maps a pod's immediate controller onto the workload a human
// would name. Supplied by the controller, which watches ReplicaSets and can
// therefore collapse ("ReplicaSet", "web-7d9f") into ("Deployment", "web").
type OwnerResolver func(namespace, kind, name string) (WorkloadKey, bool)

// sample is one observation of one pod.
type sample struct {
	at          time.Time
	cpuMilli    float64
	memoryBytes float64
	requestCPU  float64
	requestMem  float64
	podUID      string
}

// Store accumulates usage reports in memory and answers distribution queries.
//
// Everything here is bounded: samples older than the retention window are
// dropped on read, and a workload nobody has reported on for two windows is
// deleted entirely. An unbounded metrics buffer in a long-lived controller is
// simply a slow memory leak with extra steps.
type Store struct {
	mu sync.RWMutex

	retention time.Duration
	maxPerKey int

	series  map[string]*series
	resolve OwnerResolver

	// nodeSeen records the last report time per node, exposed so the controller
	// can tell a quiet node from a dead agent.
	nodeSeen map[string]time.Time

	now func() time.Time
}

type series struct {
	key     WorkloadKey
	samples []sample
	pods    map[string]time.Time
	last    time.Time
}

// NewStore builds a store with the given retention window.
func NewStore(retention time.Duration, resolve OwnerResolver) *Store {
	if retention <= 0 {
		retention = 10 * time.Minute
	}
	return &Store{
		retention: retention,
		// A window of 10 minutes at one sample per 10s per pod is 60 samples
		// per pod; 4096 comfortably holds a 60-replica workload without
		// growing without bound if an agent misbehaves and reports too often.
		maxPerKey: 4096,
		series:    make(map[string]*series),
		nodeSeen:  make(map[string]time.Time),
		resolve:   resolve,
		now:       time.Now,
	}
}

// Ingest folds one agent report into the store.
func (s *Store) Ingest(r Report) int {
	if r.Degraded {
		// Estimated numbers must never reach a resource recommendation that a
		// human might act on.
		return 0
	}
	now := s.now()
	if r.Timestamp.IsZero() {
		r.Timestamp = now
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.nodeSeen[r.Node] = now
	accepted := 0

	for _, p := range r.Pods {
		key, ok := s.workloadKey(p)
		if !ok {
			continue
		}
		ser := s.series[key.String()]
		if ser == nil {
			ser = &series{key: key, pods: make(map[string]time.Time)}
			s.series[key.String()] = ser
		}
		ser.samples = append(ser.samples, sample{
			at:          r.Timestamp,
			cpuMilli:    p.CPUMilli,
			memoryBytes: float64(p.MemoryBytes),
			requestCPU:  float64(p.RequestCPUMilli),
			requestMem:  float64(p.RequestMemoryBytes),
			podUID:      p.UID,
		})
		ser.pods[p.UID] = r.Timestamp
		ser.last = now
		if len(ser.samples) > s.maxPerKey {
			ser.samples = ser.samples[len(ser.samples)-s.maxPerKey:]
		}
		accepted++
	}
	return accepted
}

// workloadKey resolves a pod's owner, falling back to treating the pod itself
// as its own workload so that bare pods are still profiled.
func (s *Store) workloadKey(p PodUsage) (WorkloadKey, bool) {
	if p.OwnerKind != "" && s.resolve != nil {
		if key, ok := s.resolve(p.Namespace, p.OwnerKind, p.OwnerName); ok {
			return key, true
		}
	}
	if p.OwnerKind != "" {
		return WorkloadKey{Namespace: p.Namespace, Kind: p.OwnerKind, Name: p.OwnerName}, true
	}
	return WorkloadKey{Namespace: p.Namespace, Kind: "Pod", Name: p.Name}, true
}

// WorkloadStats returns the per-pod CPU and memory distributions for a
// workload, plus the number of distinct pods observed.
//
// The distributions are over per-pod samples, not per-workload sums. That is
// the right basis for a request recommendation: requests are set per pod, so
// the question is "how much does one replica use", not "how much does the
// fleet use".
func (s *Store) WorkloadStats(key WorkloadKey) (cpu, memory Stats, pods int, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	ser := s.series[key.String()]
	if ser == nil {
		return Stats{}, Stats{}, 0, false
	}
	cutoff := s.now().Add(-s.retention)

	cpuVals := make([]float64, 0, len(ser.samples))
	memVals := make([]float64, 0, len(ser.samples))
	livePods := make(map[string]struct{})
	for _, sm := range ser.samples {
		if sm.at.Before(cutoff) {
			continue
		}
		cpuVals = append(cpuVals, sm.cpuMilli)
		memVals = append(memVals, sm.memoryBytes)
		livePods[sm.podUID] = struct{}{}
	}
	if len(cpuVals) == 0 {
		return Stats{}, Stats{}, 0, false
	}
	return summarise(cpuVals), summarise(memVals), len(livePods), true
}

// WorkloadRequests returns the most recently reported per-pod requests, used
// as the denominator of the waste figure.
func (s *Store) WorkloadRequests(key WorkloadKey) (cpuMilli, memBytes int64, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	ser := s.series[key.String()]
	if ser == nil || len(ser.samples) == 0 {
		return 0, 0, false
	}
	last := ser.samples[len(ser.samples)-1]
	return int64(last.requestCPU), int64(last.requestMem), true
}

// Keys lists every workload the store currently knows about.
func (s *Store) Keys() []WorkloadKey {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]WorkloadKey, 0, len(s.series))
	for _, ser := range s.series {
		out = append(out, ser.key)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// Nodes returns the last report time per node.
func (s *Store) Nodes() map[string]time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make(map[string]time.Time, len(s.nodeSeen))
	for k, v := range s.nodeSeen {
		out[k] = v
	}
	return out
}

// GC drops expired samples and workloads nobody has reported on. Called on a
// timer by the controller.
func (s *Store) GC() {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	cutoff := now.Add(-s.retention)
	for k, ser := range s.series {
		kept := ser.samples[:0]
		for _, sm := range ser.samples {
			if !sm.at.Before(cutoff) {
				kept = append(kept, sm)
			}
		}
		ser.samples = kept
		if len(ser.samples) == 0 && now.Sub(ser.last) > 2*s.retention {
			delete(s.series, k)
		}
	}
	for node, at := range s.nodeSeen {
		if now.Sub(at) > 2*s.retention {
			delete(s.nodeSeen, node)
		}
	}
}

// summarise computes the distribution of a slice of observations.
func summarise(vals []float64) Stats {
	if len(vals) == 0 {
		return Stats{}
	}
	sorted := append([]float64(nil), vals...)
	sort.Float64s(sorted)

	var sum float64
	for _, v := range sorted {
		sum += v
	}
	return Stats{
		Samples: int64(len(sorted)),
		P50:     nearestRank(sorted, 50),
		P90:     nearestRank(sorted, 90),
		P95:     nearestRank(sorted, 95),
		P99:     nearestRank(sorted, 99),
		Max:     sorted[len(sorted)-1],
		Mean:    sum / float64(len(sorted)),
	}
}

func nearestRank(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}
