// Package cpupool enforces the QoS contract at the cgroup level.
//
// Two mechanisms, deliberately kept separate:
//
//   - cpu.weight, always on. Proportional CPU shares between tiers. Under
//     contention a reclaimed pod yields almost completely to a shared one;
//     when the node is idle it runs at full speed. This is the property a hard
//     CPU quota cannot express, and it is why reselling capacity is safe at all.
//
//   - cpuset.cpus, opt-in. Hard CPU partitioning for dedicated_cores. Stronger
//     isolation, but it requires the cpuset controller to be delegated down to
//     the pod cgroups, which is not true on every distribution, so it stays off
//     until an operator turns it on.
package cpupool

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/xkong-study/kqos/pkg/agent/cgroup"
	"github.com/xkong-study/kqos/pkg/apis/kqos/v1alpha1"
	"github.com/xkong-study/kqos/pkg/cpuset"
	"github.com/xkong-study/kqos/pkg/qos"
)

// Tier weights written to cpu.weight. The kernel range is 1..10000 with a
// default of 100.
//
// The ratio between shared (1000) and reclaimed (10) is the number that
// matters: under full contention a reclaimed pod receives roughly 1% of the
// CPU a shared pod does. That is aggressive on purpose. The reclaimed tier
// exists to absorb capacity nobody else wants, and the instant somebody else
// wants it the contract says they get it back immediately.
const (
	WeightSystem    = 10000
	WeightDedicated = 5000
	WeightShared    = 1000
	WeightReclaimed = 10
)

// WeightFor maps a QoS level onto its cpu.weight.
func WeightFor(level v1alpha1.QoSLevel) uint64 {
	switch level {
	case v1alpha1.QoSSystemCores:
		return WeightSystem
	case v1alpha1.QoSDedicatedCores:
		return WeightDedicated
	case v1alpha1.QoSReclaimedCores:
		return WeightReclaimed
	default:
		return WeightShared
	}
}

// Pool is one CPU assignment the manager has computed.
type Pool struct {
	Name    string
	Level   v1alpha1.QoSLevel
	CPUs    cpuset.CPUSet
	PodUIDs []string
}

// Config is the manager's slice of the QoSPolicy.
type Config struct {
	CPUSetEnabled        bool
	ReservedSystemCPUs   string
	SharedPoolMinCPUs    int32
	ReclaimedPoolMinCPUs int32
}

// Manager computes and applies the CPU layout for one node.
type Manager struct {
	fs       *cgroup.FS
	resolver *cgroup.Resolver
	topology Topology
	cfg      Config

	// applied remembers the last weight written per pod so that a steady-state
	// tick performs zero writes. Writing an unchanged value to a cgroup file is
	// cheap but not free, and on a node with hundreds of pods a needless write
	// storm every ten seconds is visible in kernel time.
	appliedWeight map[string]uint64
	appliedCPUSet map[string]string

	lastPools []Pool
}

// NewManager builds a CPU pool manager.
func NewManager(fs *cgroup.FS, resolver *cgroup.Resolver, topology Topology, cfg Config) *Manager {
	return &Manager{
		fs:            fs,
		resolver:      resolver,
		topology:      topology,
		cfg:           cfg,
		appliedWeight: make(map[string]uint64),
		appliedCPUSet: make(map[string]string),
	}
}

// SetConfig updates the manager's configuration in place.
func (m *Manager) SetConfig(cfg Config) { m.cfg = cfg }

// Topology exposes the detected layout for status reporting.
func (m *Manager) Topology() Topology { return m.topology }

// Pools returns the most recently computed layout.
func (m *Manager) Pools() []Pool { return m.lastPools }

// Reconcile computes the layout for the given pods and writes it to cgroups.
// Errors on individual pods are logged and counted rather than returned: a pod
// whose cgroup vanished mid-pass must not stop the other pods from being
// configured correctly.
func (m *Manager) Reconcile(ctx context.Context, pods []*corev1.Pod) error {
	logger := log.FromContext(ctx).WithName("cpupool")

	if !m.fs.Available() {
		return fmt.Errorf("cpupool: %w", cgroup.ErrUnsupported)
	}

	pools := m.computePools(pods)
	m.lastPools = pools

	alive := make(map[string]struct{}, len(pods))
	for _, pod := range pods {
		uid := string(pod.UID)
		alive[uid] = struct{}{}
		level := qos.LevelOf(pod)

		path, err := m.resolver.PodPath(uid)
		if err != nil {
			// Normal for a pod that is still being created.
			logger.V(4).Info("no cgroup yet", "pod", pod.Namespace+"/"+pod.Name)
			continue
		}

		weight := WeightFor(level)
		if m.appliedWeight[uid] != weight {
			if err := m.fs.SetCPUWeight(path, weight); err != nil {
				logger.V(2).Info("cpu.weight write failed", "pod", pod.Namespace+"/"+pod.Name, "err", err)
			} else {
				m.appliedWeight[uid] = weight
				logger.V(3).Info("applied cpu weight", "pod", pod.Namespace+"/"+pod.Name, "level", level, "weight", weight)
			}
		}

		if !m.cfg.CPUSetEnabled {
			continue
		}
		if cpus, ok := cpusForPod(pools, uid, level); ok {
			want := cpus.String()
			if want != "" && m.appliedCPUSet[uid] != want {
				if err := m.fs.SetCPUSetCPUs(path, want); err != nil {
					logger.V(2).Info("cpuset write failed", "pod", pod.Namespace+"/"+pod.Name, "err", err)
				} else {
					m.appliedCPUSet[uid] = want
					logger.V(3).Info("applied cpuset", "pod", pod.Namespace+"/"+pod.Name, "cpus", want)
				}
			}
		}
	}

	for uid := range m.appliedWeight {
		if _, ok := alive[uid]; !ok {
			delete(m.appliedWeight, uid)
			delete(m.appliedCPUSet, uid)
		}
	}
	return nil
}

// computePools partitions the node's CPUs between the tiers.
//
// Order matters and is not arbitrary: reserved CPUs are carved out first
// because the system must keep running; dedicated pods take theirs next
// because their whole contract is exclusivity; the shared tier gets what
// remains above its floor; and the reclaimed tier is handed the leftovers,
// overlapping the shared pool if there is nothing genuinely spare. Overlap is
// acceptable there precisely because cpu.weight already makes reclaimed work
// yield.
func (m *Manager) computePools(pods []*corev1.Pod) []Pool {
	free := m.topology.Online

	reserved := cpuset.CPUSet{}
	if m.cfg.ReservedSystemCPUs != "" {
		if parsed, err := cpuset.Parse(m.cfg.ReservedSystemCPUs); err == nil {
			reserved = parsed.Intersection(free)
			free = free.Difference(reserved)
		}
	}

	pools := []Pool{}
	if !reserved.IsEmpty() {
		pools = append(pools, Pool{Name: "reserved", Level: v1alpha1.QoSSystemCores, CPUs: reserved})
	}

	// Dedicated pods, largest first so the big ones get contiguous zones while
	// the topology is still unfragmented.
	dedicated := make([]*corev1.Pod, 0)
	for _, pod := range pods {
		if qos.LevelOf(pod) == v1alpha1.QoSDedicatedCores {
			dedicated = append(dedicated, pod)
		}
	}
	sortPodsByCPUDesc(dedicated)

	minShared := int(m.cfg.SharedPoolMinCPUs)
	if minShared < 1 {
		minShared = 1
	}

	for _, pod := range dedicated {
		want := wholeCoresFor(pod)
		if want < 1 {
			continue
		}
		// Never starve the shared tier to satisfy a dedicated request. A
		// dedicated pod that cannot be given exclusive CPUs falls back to the
		// shared pool with its dedicated weight, which is degraded but alive.
		if free.Size()-want < minShared {
			continue
		}
		var taken cpuset.CPUSet
		if qos.WantsNUMABinding(pod) {
			if zone, ok := m.topology.ZoneFor(free, want); ok {
				taken, free = free.TakeFrom(want, zone.CPUs)
			} else {
				taken, free = free.Take(want)
			}
		} else {
			taken, free = free.Take(want)
		}
		if taken.Size() == 0 {
			continue
		}
		pools = append(pools, Pool{
			Name:    string(pod.UID),
			Level:   v1alpha1.QoSDedicatedCores,
			CPUs:    taken,
			PodUIDs: []string{string(pod.UID)},
		})
	}

	shared := free
	if shared.Size() < minShared {
		// Borrow back from the reclaimed side rather than the dedicated one.
		shared = m.topology.Online.Difference(reserved)
	}
	pools = append(pools, Pool{Name: "shared", Level: v1alpha1.QoSSharedCores, CPUs: shared})

	minReclaimed := int(m.cfg.ReclaimedPoolMinCPUs)
	if minReclaimed < 1 {
		minReclaimed = 1
	}
	reclaimed := shared
	if reclaimed.Size() < minReclaimed {
		reclaimed = m.topology.Online.Difference(reserved)
	}
	pools = append(pools, Pool{Name: "reclaimed", Level: v1alpha1.QoSReclaimedCores, CPUs: reclaimed})

	// Record membership so status reporting shows which pods sit where.
	for i := range pools {
		if pools[i].Level == v1alpha1.QoSDedicatedCores {
			continue
		}
		for _, pod := range pods {
			if qos.LevelOf(pod) == pools[i].Level {
				pools[i].PodUIDs = append(pools[i].PodUIDs, string(pod.UID))
			}
		}
	}
	return pools
}

// cpusForPod finds the CPU set a pod should be confined to.
func cpusForPod(pools []Pool, uid string, level v1alpha1.QoSLevel) (cpuset.CPUSet, bool) {
	if level == v1alpha1.QoSDedicatedCores {
		for _, p := range pools {
			if p.Name == uid {
				return p.CPUs, true
			}
		}
	}
	name := "shared"
	switch level {
	case v1alpha1.QoSReclaimedCores:
		name = "reclaimed"
	case v1alpha1.QoSSystemCores:
		name = "reserved"
	}
	for _, p := range pools {
		if p.Name == name {
			return p.CPUs, true
		}
	}
	return cpuset.CPUSet{}, false
}

// wholeCoresFor rounds a dedicated pod's CPU request up to whole cores, since
// an exclusive assignment cannot be a fraction of one.
func wholeCoresFor(pod *corev1.Pod) int {
	req := qos.EffectiveRequests(pod)
	q, ok := req[corev1.ResourceCPU]
	if !ok {
		return 0
	}
	milli := q.MilliValue()
	cores := int(milli / 1000)
	if milli%1000 != 0 {
		cores++
	}
	return cores
}

func sortPodsByCPUDesc(pods []*corev1.Pod) {
	for i := 1; i < len(pods); i++ {
		for j := i; j > 0 && wholeCoresFor(pods[j]) > wholeCoresFor(pods[j-1]); j-- {
			pods[j], pods[j-1] = pods[j-1], pods[j]
		}
	}
}

// StatusPools renders the computed layout for the NodeResourceProfile status.
func (m *Manager) StatusPools() []v1alpha1.CPUSetPool {
	out := make([]v1alpha1.CPUSetPool, 0, len(m.lastPools))
	for _, p := range m.lastPools {
		out = append(out, v1alpha1.CPUSetPool{
			Name:     p.Name,
			QoSLevel: p.Level,
			CPUs:     p.CPUs.String(),
			Size:     int32(p.CPUs.Size()),
		})
	}
	return out
}

// StatusZones renders the NUMA topology for the status.
func (m *Manager) StatusZones() []v1alpha1.TopologyZone {
	dedicated := cpuset.CPUSet{}
	for _, p := range m.lastPools {
		if p.Level == v1alpha1.QoSDedicatedCores && p.Name != "reserved" {
			dedicated = dedicated.Union(p.CPUs)
		}
	}
	out := make([]v1alpha1.TopologyZone, 0, len(m.topology.Zones))
	for _, z := range m.topology.Zones {
		out = append(out, v1alpha1.TopologyZone{
			ID:              int32(z.ID),
			CPUs:            z.CPUs.String(),
			AllocatableCPUs: int32(z.CPUs.Difference(dedicated).Size()),
		})
	}
	return out
}
