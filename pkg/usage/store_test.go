package usage

import (
	"testing"
	"time"
)

func report(node string, at time.Time, cpu ...float64) Report {
	r := Report{Node: node, Timestamp: at}
	for i, c := range cpu {
		r.Pods = append(r.Pods, PodUsage{
			UID:                node + "-pod",
			Namespace:          "demo",
			Name:               "web-abc",
			QoSLevel:           "shared_cores",
			OwnerKind:          "ReplicaSet",
			OwnerName:          "web-7d9f",
			CPUMilli:           c,
			MemoryBytes:        uint64(100+i) * 1024 * 1024,
			RequestCPUMilli:    600,
			RequestMemoryBytes: 512 * 1024 * 1024,
		})
	}
	return r
}

// deploymentResolver collapses the ReplicaSet into its Deployment, the way the
// controller's real resolver does.
func deploymentResolver(namespace, kind, name string) (WorkloadKey, bool) {
	if kind == "ReplicaSet" {
		return WorkloadKey{Namespace: namespace, Kind: "Deployment", Name: "web"}, true
	}
	return WorkloadKey{Namespace: namespace, Kind: kind, Name: name}, true
}

func TestIngestAndSummarise(t *testing.T) {
	s := NewStore(10*time.Minute, deploymentResolver)
	now := time.Now()

	for i := 0; i < 100; i++ {
		s.Ingest(report("node-1", now.Add(time.Duration(i)*time.Second), float64(i)))
	}

	key := WorkloadKey{Namespace: "demo", Kind: "Deployment", Name: "web"}
	cpu, _, pods, ok := s.WorkloadStats(key)
	if !ok {
		t.Fatal("no stats for the workload")
	}
	if cpu.Samples != 100 {
		t.Errorf("samples = %d, want 100", cpu.Samples)
	}
	if pods != 1 {
		t.Errorf("distinct pods = %d, want 1", pods)
	}
	// Nearest-rank over 0..99: the 95th percentile is the 95th value, 94.
	if cpu.P95 != 94 {
		t.Errorf("p95 = %v, want 94", cpu.P95)
	}
	if cpu.Max != 99 {
		t.Errorf("max = %v, want 99", cpu.Max)
	}
}

func TestReplicaSetIsCollapsedIntoItsDeployment(t *testing.T) {
	s := NewStore(10*time.Minute, deploymentResolver)
	s.Ingest(report("node-1", time.Now(), 100))

	keys := s.Keys()
	if len(keys) != 1 {
		t.Fatalf("keys = %v, want exactly one", keys)
	}
	// Keying on the ReplicaSet would reset a workload's history on every
	// rollout -- precisely when the history becomes interesting.
	if keys[0].Kind != "Deployment" || keys[0].Name != "web" {
		t.Errorf("key = %v, want the Deployment", keys[0])
	}
}

func TestDegradedReportsAreRefused(t *testing.T) {
	s := NewStore(10*time.Minute, deploymentResolver)
	r := report("node-1", time.Now(), 100)
	r.Degraded = true

	if n := s.Ingest(r); n != 0 {
		t.Errorf("accepted %d samples from a degraded report", n)
	}
	// Estimated numbers must never reach a recommendation a human might apply.
	if len(s.Keys()) != 0 {
		t.Error("a degraded report created a series")
	}
}

func TestRetentionWindowExcludesOldSamples(t *testing.T) {
	s := NewStore(time.Minute, deploymentResolver)
	now := time.Now()

	// Two samples well outside the window, one inside.
	s.Ingest(report("node-1", now.Add(-10*time.Minute), 999))
	s.Ingest(report("node-1", now.Add(-9*time.Minute), 999))
	s.Ingest(report("node-1", now, 50))

	key := WorkloadKey{Namespace: "demo", Kind: "Deployment", Name: "web"}
	cpu, _, _, ok := s.WorkloadStats(key)
	if !ok {
		t.Fatal("no stats")
	}
	if cpu.Samples != 1 {
		t.Errorf("samples = %d, want only the one inside the window", cpu.Samples)
	}
	if cpu.Max != 50 {
		t.Errorf("max = %v; an expired spike is still influencing the result", cpu.Max)
	}
}

func TestGCDropsAbandonedSeries(t *testing.T) {
	s := NewStore(time.Minute, deploymentResolver)
	base := time.Now()
	s.now = func() time.Time { return base }

	s.Ingest(report("node-1", base, 100))
	if len(s.Keys()) != 1 {
		t.Fatal("series was not created")
	}

	// Well past two retention windows with no new reports: the workload is
	// gone and holding its buffer forever is just a slow leak.
	s.now = func() time.Time { return base.Add(10 * time.Minute) }
	s.GC()

	if len(s.Keys()) != 0 {
		t.Errorf("abandoned series survived GC: %v", s.Keys())
	}
	if len(s.Nodes()) != 0 {
		t.Errorf("abandoned node entry survived GC: %v", s.Nodes())
	}
}

func TestSampleCapIsEnforced(t *testing.T) {
	s := NewStore(time.Hour, deploymentResolver)
	s.maxPerKey = 10
	now := time.Now()

	for i := 0; i < 100; i++ {
		s.Ingest(report("node-1", now.Add(time.Duration(i)*time.Second), float64(i)))
	}

	key := WorkloadKey{Namespace: "demo", Kind: "Deployment", Name: "web"}
	cpu, _, _, _ := s.WorkloadStats(key)
	if cpu.Samples > 10 {
		t.Errorf("samples = %d, want the cap of 10 to bound an over-reporting agent", cpu.Samples)
	}
	// The cap must keep the newest samples, not the oldest.
	if cpu.Max != 99 {
		t.Errorf("max = %v, want the most recent value 99", cpu.Max)
	}
}

func TestBarePodsAreStillProfiled(t *testing.T) {
	s := NewStore(time.Minute, deploymentResolver)
	r := Report{Node: "node-1", Timestamp: time.Now(), Pods: []PodUsage{{
		UID: "u", Namespace: "demo", Name: "standalone", CPUMilli: 10,
	}}}
	s.Ingest(r)

	keys := s.Keys()
	if len(keys) != 1 || keys[0].Kind != "Pod" || keys[0].Name != "standalone" {
		t.Errorf("bare pod was not profiled under its own name: %v", keys)
	}
}

func TestNodesTracksLastReport(t *testing.T) {
	s := NewStore(time.Minute, deploymentResolver)
	s.Ingest(report("node-1", time.Now(), 10))
	s.Ingest(report("node-2", time.Now(), 10))

	nodes := s.Nodes()
	if len(nodes) != 2 {
		t.Errorf("nodes = %v, want two", nodes)
	}
}
