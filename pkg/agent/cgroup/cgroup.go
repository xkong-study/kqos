// Package cgroup is a minimal cgroup v2 reader/writer. It deliberately covers
// only the controllers kqos acts on -- cpu, memory, cpuset and PSI -- and does
// nothing clever: every read is a fresh open of the pseudo-file, because the
// kernel invalidates cached contents in ways that are easy to get wrong.
package cgroup

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DefaultRoot is where the unified hierarchy is mounted on a modern Linux host
// and inside a kind node container.
const DefaultRoot = "/sys/fs/cgroup"

// ErrUnsupported is returned when the unified hierarchy is not present, which
// is the normal case when the agent binary runs on a developer laptop.
var ErrUnsupported = errors.New("cgroup v2 unified hierarchy not available")

// FS is a handle on one cgroup v2 hierarchy root.
type FS struct {
	root string
}

// New opens the hierarchy at root. It does not verify the mount; use Available
// for that, so callers can degrade gracefully rather than fail to start.
func New(root string) *FS {
	if root == "" {
		root = DefaultRoot
	}
	return &FS{root: root}
}

// Root is the hierarchy root path.
func (f *FS) Root() string { return f.root }

// Available reports whether this really is a cgroup v2 hierarchy. The presence
// of cgroup.controllers at the root is the standard probe.
func (f *FS) Available() bool {
	_, err := os.Stat(filepath.Join(f.root, "cgroup.controllers"))
	return err == nil
}

// path joins a cgroup-relative path onto the root.
func (f *FS) path(rel string, file string) string {
	return filepath.Join(f.root, filepath.Clean("/"+rel), file)
}

// Exists reports whether a cgroup directory is present.
func (f *FS) Exists(rel string) bool {
	st, err := os.Stat(filepath.Join(f.root, filepath.Clean("/"+rel)))
	return err == nil && st.IsDir()
}

// Controllers lists the controllers enabled in a cgroup.
func (f *FS) Controllers(rel string) ([]string, error) {
	b, err := os.ReadFile(f.path(rel, "cgroup.controllers"))
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(b)), nil
}

// CPUStat is the subset of cpu.stat kqos consumes. All times are microseconds.
type CPUStat struct {
	// UsageUsec is cumulative CPU time consumed by the cgroup.
	UsageUsec uint64
	// UserUsec and SystemUsec split that time by mode.
	UserUsec   uint64
	SystemUsec uint64
	// NrPeriods and NrThrottled describe CFS bandwidth enforcement. A rising
	// NrThrottled on a shared_cores pod is the clearest signal that the
	// reclaimed tier has been packed too aggressively.
	NrPeriods     uint64
	NrThrottled   uint64
	ThrottledUsec uint64
}

// CPUStat reads cpu.stat.
func (f *FS) CPUStat(rel string) (CPUStat, error) {
	var st CPUStat
	kv, err := readKeyedFile(f.path(rel, "cpu.stat"))
	if err != nil {
		return st, err
	}
	st.UsageUsec = kv["usage_usec"]
	st.UserUsec = kv["user_usec"]
	st.SystemUsec = kv["system_usec"]
	st.NrPeriods = kv["nr_periods"]
	st.NrThrottled = kv["nr_throttled"]
	st.ThrottledUsec = kv["throttled_usec"]
	return st, nil
}

// MemoryStat is the subset of the memory controller kqos consumes, in bytes.
type MemoryStat struct {
	// Current is memory.current: everything charged to the cgroup, including
	// page cache. Using this raw would overstate pressure, which is why
	// Workload subtracts reclaimable file pages.
	Current uint64
	// Peak is memory.peak, or 0 on kernels that do not export it.
	Peak uint64
	// Max is the hard limit, or 0 when unlimited.
	Max uint64
	// Anon is anonymous memory: the part that cannot be reclaimed without swap.
	Anon uint64
	// File is page cache charged to the cgroup.
	File uint64
	// InactiveFile is the readily-reclaimable slice of File.
	InactiveFile uint64
	// Slab is kernel memory.
	Slab uint64
}

// Workload is the memory figure kqos treats as genuinely occupied: anonymous
// memory plus kernel slab plus the page cache the kernel would struggle to
// reclaim. Sizing the reclaimed tier off memory.current instead of this is the
// classic way to end up with a node that looks full and is actually idle.
func (m MemoryStat) Workload() uint64 {
	if m.Anon == 0 && m.Slab == 0 {
		// Kernel did not populate memory.stat; fall back to the raw figure
		// rather than reporting a misleading zero.
		return m.Current
	}
	active := m.File - m.InactiveFile
	if m.InactiveFile > m.File {
		active = 0
	}
	return m.Anon + m.Slab + active
}

// MemoryStat reads memory.current, memory.max, memory.peak and memory.stat.
func (f *FS) MemoryStat(rel string) (MemoryStat, error) {
	var st MemoryStat
	cur, err := readUintFile(f.path(rel, "memory.current"))
	if err != nil {
		return st, err
	}
	st.Current = cur

	// memory.peak is recent; its absence is not an error.
	if peak, err := readUintFile(f.path(rel, "memory.peak")); err == nil {
		st.Peak = peak
	}
	if raw, err := os.ReadFile(f.path(rel, "memory.max")); err == nil {
		s := strings.TrimSpace(string(raw))
		if s != "max" {
			if v, err := strconv.ParseUint(s, 10, 64); err == nil {
				st.Max = v
			}
		}
	}
	if kv, err := readKeyedFile(f.path(rel, "memory.stat")); err == nil {
		st.Anon = kv["anon"]
		st.File = kv["file"]
		st.InactiveFile = kv["inactive_file"]
		st.Slab = kv["slab"]
	}
	return st, nil
}

// PSI holds pressure-stall information for one resource. Values are the
// percentage of wall time in which at least one task ("some") or every task
// ("full") was stalled waiting for the resource.
type PSI struct {
	SomeAvg10  float64
	SomeAvg60  float64
	SomeAvg300 float64
	FullAvg10  float64
	FullAvg60  float64
	FullAvg300 float64
}

// Pressure reads <controller>.pressure, e.g. controller "cpu" or "memory".
// PSI is what lets kqos distinguish a node that is busy from a node that is
// suffering: utilisation says how much work happened, PSI says how much work
// was blocked from happening.
func (f *FS) Pressure(rel, controller string) (PSI, error) {
	var psi PSI
	fh, err := os.Open(f.path(rel, controller+".pressure"))
	if err != nil {
		return psi, err
	}
	defer fh.Close()

	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 {
			continue
		}
		kind := fields[0]
		vals := map[string]float64{}
		for _, kvp := range fields[1:] {
			k, v, ok := strings.Cut(kvp, "=")
			if !ok {
				continue
			}
			pv, err := strconv.ParseFloat(v, 64)
			if err != nil {
				continue
			}
			vals[k] = pv
		}
		switch kind {
		case "some":
			psi.SomeAvg10, psi.SomeAvg60, psi.SomeAvg300 = vals["avg10"], vals["avg60"], vals["avg300"]
		case "full":
			psi.FullAvg10, psi.FullAvg60, psi.FullAvg300 = vals["avg10"], vals["avg60"], vals["avg300"]
		}
	}
	return psi, sc.Err()
}

// CPUSetEffective reads cpuset.cpus.effective, the CPUs the cgroup may actually
// run on after the parent's constraints are applied. This differs from
// cpuset.cpus and it is the effective set that matters.
func (f *FS) CPUSetEffective(rel string) (string, error) {
	b, err := os.ReadFile(f.path(rel, "cpuset.cpus.effective"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// CPUSetCPUs reads the configured cpuset.cpus.
func (f *FS) CPUSetCPUs(rel string) (string, error) {
	b, err := os.ReadFile(f.path(rel, "cpuset.cpus"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// SetCPUSetCPUs writes cpuset.cpus. Writing an empty string clears the
// constraint and lets the cgroup inherit its parent's set.
func (f *FS) SetCPUSetCPUs(rel, cpus string) error {
	return os.WriteFile(f.path(rel, "cpuset.cpus"), []byte(cpus), 0o644)
}

// SetCPUWeight writes cpu.weight (1-10000, default 100). This is the knob kqos
// uses to make reclaimed_cores yield to shared_cores under contention without
// capping their throughput when the node is idle -- a proportional control that
// a hard quota cannot express.
func (f *FS) SetCPUWeight(rel string, weight uint64) error {
	if weight < 1 {
		weight = 1
	}
	if weight > 10000 {
		weight = 10000
	}
	return os.WriteFile(f.path(rel, "cpu.weight"), []byte(strconv.FormatUint(weight, 10)), 0o644)
}

// CPUWeight reads cpu.weight.
func (f *FS) CPUWeight(rel string) (uint64, error) {
	return readUintFile(f.path(rel, "cpu.weight"))
}

// SetCPUMax writes cpu.max as "<quota> <period>" in microseconds. A negative
// quota writes "max", removing the bandwidth cap.
func (f *FS) SetCPUMax(rel string, quotaUsec, periodUsec int64) error {
	if periodUsec <= 0 {
		periodUsec = 100000
	}
	q := "max"
	if quotaUsec >= 0 {
		q = strconv.FormatInt(quotaUsec, 10)
	}
	val := fmt.Sprintf("%s %d", q, periodUsec)
	return os.WriteFile(f.path(rel, "cpu.max"), []byte(val), 0o644)
}

// ListChildren returns the immediate child cgroup names of rel.
func (f *FS) ListChildren(rel string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(f.root, filepath.Clean("/"+rel)))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// readKeyedFile parses the "key value\n" format used by cpu.stat and
// memory.stat. Unparseable lines are skipped rather than failing the read: the
// kernel adds keys between versions and kqos should not break when it does.
func readKeyedFile(path string) (map[string]uint64, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()

	out := make(map[string]uint64, 16)
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
		if err != nil {
			continue
		}
		out[k] = n
	}
	return out, sc.Err()
}

func readUintFile(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(b))
	if s == "max" {
		return 0, nil
	}
	return strconv.ParseUint(s, 10, 64)
}
