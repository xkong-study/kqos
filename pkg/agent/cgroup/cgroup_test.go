package cgroup

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeHierarchy builds a cgroup v2 tree on disk so the reader can be tested
// without a Linux host. The pseudo-files are ordinary files; nothing in the
// reader depends on them being anything else.
func fakeHierarchy(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("cgroup.controllers", "cpuset cpu io memory pids\n")
	write("cpu.pressure", "some avg10=12.34 avg60=5.00 avg300=1.20 total=135235\nfull avg10=2.50 avg60=1.00 avg300=0.10 total=112123\n")
	write("memory.pressure", "some avg10=0.00 avg60=0.00 avg300=0.00 total=2\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=2\n")

	// A systemd-driver layout with an underscored pod UID, three levels deep,
	// exactly as a kind node presents it.
	podDir := "kubelet.slice/kubelet-kubepods.slice/kubelet-kubepods-besteffort.slice/" +
		"kubelet-kubepods-besteffort-podb8bffafc_562c_4c01_976f_d1eccfd17ec7.slice"
	write(filepath.Join(podDir, "cpu.stat"),
		"usage_usec 3025832\nuser_usec 1701653\nsystem_usec 1324178\nnice_usec 0\n"+
			"nr_periods 500\nnr_throttled 25\nthrottled_usec 4000\n")
	write(filepath.Join(podDir, "memory.current"), "268435456\n")
	write(filepath.Join(podDir, "memory.max"), "536870912\n")
	write(filepath.Join(podDir, "memory.stat"),
		"anon 134217728\nfile 100663296\ninactive_file 67108864\nslab 8388608\n")
	write(filepath.Join(podDir, "cpu.weight"), "100\n")
	write(filepath.Join(podDir, "cpuset.cpus"), "0-3\n")
	write(filepath.Join(podDir, "cpuset.cpus.effective"), "0-3\n")

	// A container scope inside the pod.
	write(filepath.Join(podDir, "cri-containerd-abc123.scope", "cpu.stat"), "usage_usec 100\n")

	// The kubepods rollup the collector prefers for node totals.
	write("kubelet.slice/kubelet-kubepods.slice/cpu.stat", "usage_usec 9000000\nnr_periods 0\n")
	write("kubelet.slice/kubelet-kubepods.slice/memory.current", "1073741824\n")

	return root
}

const podUID = "b8bffafc-562c-4c01-976f-d1eccfd17ec7"

func TestAvailable(t *testing.T) {
	fs := New(fakeHierarchy(t))
	if !fs.Available() {
		t.Error("hierarchy with cgroup.controllers should be reported available")
	}
	if New(t.TempDir()).Available() {
		t.Error("an empty directory should not be reported as a cgroup v2 hierarchy")
	}
}

func TestCPUStat(t *testing.T) {
	fs := New(fakeHierarchy(t))
	r := NewResolver(fs)

	path, err := r.PodPath(podUID)
	if err != nil {
		t.Fatalf("resolving pod cgroup: %v", err)
	}

	st, err := fs.CPUStat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.UsageUsec != 3025832 {
		t.Errorf("usage_usec = %d", st.UsageUsec)
	}
	if st.NrThrottled != 25 || st.NrPeriods != 500 {
		t.Errorf("throttling counters = %d/%d", st.NrThrottled, st.NrPeriods)
	}
	// The reader must skip keys it does not know rather than failing, because
	// the kernel adds them between versions.
	if st.UserUsec != 1701653 {
		t.Errorf("user_usec = %d", st.UserUsec)
	}
}

func TestMemoryStatWorkloadExcludesReclaimableCache(t *testing.T) {
	fs := New(fakeHierarchy(t))
	r := NewResolver(fs)
	path, _ := r.PodPath(podUID)

	st, err := fs.MemoryStat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Current != 268435456 {
		t.Errorf("memory.current = %d", st.Current)
	}
	if st.Max != 536870912 {
		t.Errorf("memory.max = %d", st.Max)
	}

	// anon 128Mi + slab 8Mi + active file (96Mi - 64Mi = 32Mi) = 168Mi.
	// Sizing the reclaimed tier off memory.current (256Mi) instead would
	// overstate occupancy by half.
	want := uint64(134217728 + 8388608 + (100663296 - 67108864))
	if got := st.Workload(); got != want {
		t.Errorf("Workload() = %d, want %d", got, want)
	}
	if st.Workload() >= st.Current {
		t.Error("workload memory should be below memory.current when cache is present")
	}
}

func TestMemoryMaxUnlimitedReadsAsZero(t *testing.T) {
	root := fakeHierarchy(t)
	dir := filepath.Join(root, "unlimited")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "memory.current"), []byte("1024\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "memory.max"), []byte("max\n"), 0o644)

	st, err := New(root).MemoryStat("unlimited")
	if err != nil {
		t.Fatal(err)
	}
	if st.Max != 0 {
		t.Errorf(`memory.max "max" should read as 0, got %d`, st.Max)
	}
}

func TestPressure(t *testing.T) {
	psi, err := New(fakeHierarchy(t)).Pressure("", "cpu")
	if err != nil {
		t.Fatal(err)
	}
	if psi.SomeAvg10 != 12.34 {
		t.Errorf("some avg10 = %v, want 12.34", psi.SomeAvg10)
	}
	if psi.FullAvg10 != 2.50 {
		t.Errorf("full avg10 = %v, want 2.50", psi.FullAvg10)
	}
}

func TestResolverFindsSystemdStylePaths(t *testing.T) {
	fs := New(fakeHierarchy(t))
	r := NewResolver(fs)

	// The systemd driver replaces dashes with underscores in the slice name,
	// because dashes are hierarchy separators. The resolver searches for both
	// spellings rather than reimplementing the kubelet's naming rules.
	path, err := r.PodPath(podUID)
	if err != nil {
		t.Fatalf("did not find the pod: %v", err)
	}
	if !fs.Exists(path) {
		t.Errorf("resolved path %q does not exist", path)
	}
	if r.CachedCount() != 1 {
		t.Errorf("resolution was not memoised: %d entries", r.CachedCount())
	}

	// A second lookup must come from the cache and still be correct.
	again, err := r.PodPath(podUID)
	if err != nil || again != path {
		t.Errorf("cached lookup returned %q / %v", again, err)
	}

	r.Forget(podUID)
	if r.CachedCount() != 0 {
		t.Error("Forget did not evict the entry")
	}
}

func TestResolverReportsMissingPods(t *testing.T) {
	r := NewResolver(New(fakeHierarchy(t)))
	if _, err := r.PodPath("00000000-0000-0000-0000-000000000000"); err == nil {
		t.Error("resolving an absent pod should fail rather than return a wrong path")
	}
}

func TestResolverFindsKubepods(t *testing.T) {
	r := NewResolver(New(fakeHierarchy(t)))
	path := r.KubepodsPath()
	if path == "" {
		t.Fatal("kubepods rollup not found")
	}
	// Breadth-first: the shallowest match wins, so this is the kubepods slice
	// itself rather than one of the per-QoS children beneath it.
	if filepath.Base(path) != "kubelet-kubepods.slice" {
		t.Errorf("kubepods path = %q", path)
	}
}

func TestContainerPaths(t *testing.T) {
	r := NewResolver(New(fakeHierarchy(t)))
	paths, err := r.ContainerPaths(podUID)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 {
		t.Fatalf("container paths = %v, want one", paths)
	}
	if filepath.Base(paths[0]) != "cri-containerd-abc123.scope" {
		t.Errorf("container path = %q", paths[0])
	}
}

func TestSetCPUWeightClamps(t *testing.T) {
	root := fakeHierarchy(t)
	fs := New(root)
	r := NewResolver(fs)
	path, _ := r.PodPath(podUID)

	// The kernel range is 1..10000; values outside it must be clamped rather
	// than written and rejected.
	if err := fs.SetCPUWeight(path, 0); err != nil {
		t.Fatal(err)
	}
	if w, _ := fs.CPUWeight(path); w != 1 {
		t.Errorf("weight 0 was written as %d, want the clamp to 1", w)
	}

	if err := fs.SetCPUWeight(path, 99999); err != nil {
		t.Fatal(err)
	}
	if w, _ := fs.CPUWeight(path); w != 10000 {
		t.Errorf("weight 99999 was written as %d, want the clamp to 10000", w)
	}

	if err := fs.SetCPUWeight(path, 10); err != nil {
		t.Fatal(err)
	}
	if w, _ := fs.CPUWeight(path); w != 10 {
		t.Errorf("weight = %d, want 10", w)
	}
}

func TestSetCPUMaxUnlimited(t *testing.T) {
	root := fakeHierarchy(t)
	fs := New(root)
	r := NewResolver(fs)
	path, _ := r.PodPath(podUID)

	if err := fs.SetCPUMax(path, -1, 100000); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, path, "cpu.max"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "max 100000" {
		t.Errorf("cpu.max = %q, want %q", raw, "max 100000")
	}
}
