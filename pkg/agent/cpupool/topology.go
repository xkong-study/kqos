package cpupool

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/xkong-study/kqos/pkg/cpuset"
)

// SysNodePath is where the kernel exposes NUMA topology.
const SysNodePath = "/sys/devices/system/node"

// Zone is one NUMA node.
type Zone struct {
	ID   int
	CPUs cpuset.CPUSet
}

// Topology is the node's CPU layout. On a machine without NUMA -- which
// includes every kind cluster running inside a single VM -- this collapses to
// one zone holding every online CPU, and the placement logic below degrades
// into plain first-fit without any special-casing.
type Topology struct {
	Zones  []Zone
	Online cpuset.CPUSet
}

// TotalCPUs is the number of online CPUs.
func (t Topology) TotalCPUs() int { return t.Online.Size() }

// ZoneOf returns the zone containing a CPU, or -1.
func (t Topology) ZoneOf(cpu int) int {
	for _, z := range t.Zones {
		if z.CPUs.Contains(cpu) {
			return z.ID
		}
	}
	return -1
}

// ZoneFor returns the zone with the most free CPUs from the given free set,
// which is how a NUMA-bound dedicated pod is placed without splitting it
// across memory controllers.
func (t Topology) ZoneFor(free cpuset.CPUSet, want int) (Zone, bool) {
	var best Zone
	bestFree := -1
	for _, z := range t.Zones {
		n := z.CPUs.Intersection(free).Size()
		if n >= want && (bestFree < 0 || n < bestFree) {
			// Prefer the tightest zone that still fits: leaving the roomiest
			// zone intact keeps a later, larger request placeable.
			best, bestFree = z, n
		}
	}
	if bestFree < 0 {
		return Zone{}, false
	}
	return best, true
}

// DetectTopology reads the NUMA layout from sysfs, falling back to a single
// zone spanning the given CPU count when sysfs is unavailable.
func DetectTopology(sysNodePath string, fallbackCPUs int) Topology {
	if sysNodePath == "" {
		sysNodePath = SysNodePath
	}
	t := Topology{}

	if online, err := os.ReadFile(filepath.Join(sysNodePath, "online")); err == nil {
		if nodeIDs, err := cpuset.Parse(strings.TrimSpace(string(online))); err == nil {
			for _, id := range nodeIDs.List() {
				raw, err := os.ReadFile(filepath.Join(sysNodePath, "node"+strconv.Itoa(id), "cpulist"))
				if err != nil {
					continue
				}
				cpus, err := cpuset.Parse(strings.TrimSpace(string(raw)))
				if err != nil || cpus.IsEmpty() {
					continue
				}
				t.Zones = append(t.Zones, Zone{ID: id, CPUs: cpus})
				t.Online = t.Online.Union(cpus)
			}
		}
	}

	if len(t.Zones) == 0 {
		if fallbackCPUs < 1 {
			fallbackCPUs = 1
		}
		all := cpuset.NewRange(0, fallbackCPUs)
		t.Zones = []Zone{{ID: 0, CPUs: all}}
		t.Online = all
	}

	sort.Slice(t.Zones, func(i, j int) bool { return t.Zones[i].ID < t.Zones[j].ID })
	return t
}
