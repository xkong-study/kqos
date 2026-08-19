package eviction

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/xkong-study/kqos/pkg/agent/collector"
	"github.com/xkong-study/kqos/pkg/agent/sysadvisor"
	"github.com/xkong-study/kqos/pkg/apis/kqos/v1alpha1"
)

func testPod(name string, level v1alpha1.QoSLevel, cpuReq string) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "demo",
			UID:         types.UID(name),
			Annotations: map[string]string{v1alpha1.AnnotationQoSLevel: string(level)},
		},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "c"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if cpuReq != "" {
		p.Spec.Containers[0].Resources.Requests = corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse(cpuReq),
		}
	}
	return p
}

// signal builds a Signal wired up so the plugins see the supplied usage.
func signal(pods []*corev1.Pod, usage map[string]collector.PodSample, memUsed uint64, pressure v1alpha1.NodePressure) Signal {
	return Signal{
		Sample: collector.NodeSample{
			Timestamp:   time.Now(),
			Valid:       true,
			MemoryBytes: memUsed,
		},
		Recommendation: sysadvisor.Recommendation{Pressure: pressure},
		Allocatable: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("8"),
			corev1.ResourceMemory: *resource.NewQuantity(8*1024*1024*1024, resource.BinarySI),
		},
		Config: v1alpha1.EvictionConfig{
			Enabled:                        true,
			MemoryPressureThresholdPercent: 85,
			CPUPressureThresholdPercent:    92,
			CPUSomeStalledThresholdPercent: 40,
			MaxEvictionsPerMinute:          3,
			GracePeriodSeconds:             30,
			// Off by default in these tests; the stabilisation behaviour has its
			// own case.
			StabilisationSeconds: 0,
		},
		Pods:        pods,
		SampleByUID: usage,
	}
}

// newTestManager returns a manager over a fake clientset plus a func listing
// the pods it actually evicted.
func newTestManager(t *testing.T, plugins ...Plugin) (*Manager, func() []string) {
	t.Helper()
	client := fake.NewSimpleClientset()

	var evicted []string
	client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		create, ok := action.(k8stesting.CreateAction)
		if !ok || action.GetSubresource() != "eviction" {
			return false, nil, nil
		}
		if obj, ok := create.GetObject().(*metav1.PartialObjectMetadata); ok {
			evicted = append(evicted, obj.Name)
			return true, nil, nil
		}
		// The typed client sends a policy/v1 Eviction; read its name off the
		// generic accessor so this reactor does not care about the version.
		if acc, err := meta(create.GetObject()); err == nil {
			evicted = append(evicted, acc)
		}
		return true, nil, nil
	})

	m := NewManager("node-1", client, nil, plugins)
	return m, func() []string { return evicted }
}

func meta(obj runtime.Object) (string, error) {
	type named interface{ GetName() string }
	if n, ok := obj.(named); ok {
		return n.GetName(), nil
	}
	return "", nil
}

func TestMemoryPressurePluginPrefersReclaimedTier(t *testing.T) {
	shared := testPod("web", v1alpha1.QoSSharedCores, "500m")
	reclaimed := testPod("batch", v1alpha1.QoSReclaimedCores, "")

	const gib = uint64(1) << 30
	usage := map[string]collector.PodSample{
		// The shared pod is using more memory, but the reclaimed pod must
		// still be chosen first: tier beats size.
		"web":   {UID: "web", MemoryBytes: 3 * gib, Valid: true},
		"batch": {UID: "batch", MemoryBytes: 1 * gib, Valid: true},
	}
	sig := signal([]*corev1.Pod{shared, reclaimed}, usage, 7*gib,
		v1alpha1.NodePressure{Level: v1alpha1.PressureCritical, MemoryUtilizationPercent: 87})

	verdicts, err := (&memoryPressurePlugin{}).Evaluate(context.Background(), sig)
	if err != nil {
		t.Fatal(err)
	}
	if len(verdicts) == 0 {
		t.Fatal("plugin did not fire at 87% memory against an 85% threshold")
	}
	if verdicts[0].Pod.Name != "batch" {
		t.Errorf("first victim = %s, want the reclaimed pod", verdicts[0].Pod.Name)
	}
}

func TestMemoryPressurePluginSilentBelowThreshold(t *testing.T) {
	const gib = uint64(1) << 30
	sig := signal(
		[]*corev1.Pod{testPod("batch", v1alpha1.QoSReclaimedCores, "")},
		map[string]collector.PodSample{"batch": {UID: "batch", MemoryBytes: gib, Valid: true}},
		4*gib, // 50% of 8Gi
		v1alpha1.NodePressure{Level: v1alpha1.PressureNone})

	verdicts, err := (&memoryPressurePlugin{}).Evaluate(context.Background(), sig)
	if err != nil {
		t.Fatal(err)
	}
	if len(verdicts) != 0 {
		t.Errorf("plugin fired at 50%% memory: %d verdicts", len(verdicts))
	}
}

func TestCPUSuppressionNeedsSomethingReclaimedToEvict(t *testing.T) {
	// High stall, but only protected pods are running. Evicting one of those
	// would make the contention worse, not better.
	sig := signal(
		[]*corev1.Pod{testPod("web", v1alpha1.QoSSharedCores, "500m")},
		map[string]collector.PodSample{"web": {UID: "web", CPUMilli: 4000, Valid: true}},
		0,
		v1alpha1.NodePressure{Level: v1alpha1.PressureCritical, CPUSomeStalledPercent: 70})

	verdicts, err := (&cpuSuppressionPlugin{}).Evaluate(context.Background(), sig)
	if err != nil {
		t.Fatal(err)
	}
	if len(verdicts) != 0 {
		t.Errorf("plugin proposed evicting a protected pod: %+v", verdicts[0])
	}
}

func TestCPUSuppressionEvictsOneAtATime(t *testing.T) {
	pods := []*corev1.Pod{
		testPod("batch-a", v1alpha1.QoSReclaimedCores, ""),
		testPod("batch-b", v1alpha1.QoSReclaimedCores, ""),
		testPod("batch-c", v1alpha1.QoSReclaimedCores, ""),
	}
	usage := map[string]collector.PodSample{
		"batch-a": {UID: "batch-a", CPUMilli: 500, Valid: true},
		"batch-b": {UID: "batch-b", CPUMilli: 3000, Valid: true},
		"batch-c": {UID: "batch-c", CPUMilli: 1000, Valid: true},
	}
	sig := signal(pods, usage, 0,
		v1alpha1.NodePressure{Level: v1alpha1.PressureCritical, CPUSomeStalledPercent: 70})

	verdicts, err := (&cpuSuppressionPlugin{}).Evaluate(context.Background(), sig)
	if err != nil {
		t.Fatal(err)
	}
	// CPU pressure relaxes within seconds, so draining every batch pod at once
	// would overshoot badly.
	if len(verdicts) != 1 {
		t.Fatalf("proposed %d evictions, want exactly 1", len(verdicts))
	}
	if verdicts[0].Pod.Name != "batch-b" {
		t.Errorf("victim = %s, want the largest consumer batch-b", verdicts[0].Pod.Name)
	}
}

func TestReclaimedOverrunIgnoresIdleNodes(t *testing.T) {
	pod := testPod("batch", v1alpha1.QoSReclaimedCores, "100m")
	usage := map[string]collector.PodSample{
		"batch": {UID: "batch", CPUMilli: 900, Valid: true}, // 9x its request
	}

	// No pressure: an overrunning batch pod is consuming capacity nobody else
	// wants, which is exactly what it is for.
	idle := signal([]*corev1.Pod{pod}, usage, 0, v1alpha1.NodePressure{Level: v1alpha1.PressureNone})
	if v, _ := (&reclaimedOverrunPlugin{}).Evaluate(context.Background(), idle); len(v) != 0 {
		t.Errorf("fired on an idle node: %+v", v[0])
	}

	// Under pressure the same behaviour is theft of everyone else's headroom.
	stressed := signal([]*corev1.Pod{pod}, usage, 0, v1alpha1.NodePressure{Level: v1alpha1.PressureModerate})
	v, err := (&reclaimedOverrunPlugin{}).Evaluate(context.Background(), stressed)
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 1 {
		t.Fatalf("proposed %d evictions under pressure, want 1", len(v))
	}
}

func TestDryRunNeverEvicts(t *testing.T) {
	m, evicted := newTestManager(t, &alwaysPlugin{})
	sig := signal([]*corev1.Pod{testPod("batch", v1alpha1.QoSReclaimedCores, "")},
		map[string]collector.PodSample{}, 0,
		v1alpha1.NodePressure{Level: v1alpha1.PressureCritical})
	sig.Config.DryRun = true

	res := m.Run(context.Background(), sig)
	if len(res.Evicted) != 0 || len(evicted()) != 0 {
		t.Errorf("dry run evicted %d pods", len(evicted()))
	}
	if res.Suppressed["dry-run"] != 1 {
		t.Errorf("dry run was not recorded as the suppression cause: %v", res.Suppressed)
	}
}

func TestDegradedSampleSuspendsEviction(t *testing.T) {
	m, evicted := newTestManager(t, &alwaysPlugin{})
	sig := signal([]*corev1.Pod{testPod("batch", v1alpha1.QoSReclaimedCores, "")},
		map[string]collector.PodSample{}, 0,
		v1alpha1.NodePressure{Level: v1alpha1.PressureCritical})
	// An estimate, not a measurement. Killing pods on a guess is the one thing
	// the manager must never do.
	sig.Sample.Degraded = true

	res := m.Run(context.Background(), sig)
	if len(evicted()) != 0 {
		t.Error("evicted a pod from a degraded sample")
	}
	if res.Suppressed["degraded-sample"] != 1 {
		t.Errorf("suppression cause = %v", res.Suppressed)
	}
}

func TestSystemTierIsVetoed(t *testing.T) {
	m, evicted := newTestManager(t, &alwaysPlugin{})
	sig := signal([]*corev1.Pod{testPod("agent", v1alpha1.QoSSystemCores, "")},
		map[string]collector.PodSample{}, 0,
		v1alpha1.NodePressure{Level: v1alpha1.PressureCritical})

	res := m.Run(context.Background(), sig)
	if len(evicted()) != 0 {
		t.Error("evicted a system_cores pod")
	}
	if res.Suppressed["system-tier"] != 1 {
		t.Errorf("suppression cause = %v", res.Suppressed)
	}
}

func TestRateLimitCapsEvictionsPerPass(t *testing.T) {
	pods := []*corev1.Pod{
		testPod("b1", v1alpha1.QoSReclaimedCores, ""),
		testPod("b2", v1alpha1.QoSReclaimedCores, ""),
		testPod("b3", v1alpha1.QoSReclaimedCores, ""),
		testPod("b4", v1alpha1.QoSReclaimedCores, ""),
		testPod("b5", v1alpha1.QoSReclaimedCores, ""),
	}
	m, evicted := newTestManager(t, &alwaysPlugin{})
	sig := signal(pods, map[string]collector.PodSample{}, 0,
		v1alpha1.NodePressure{Level: v1alpha1.PressureCritical})
	sig.Config.MaxEvictionsPerMinute = 2

	res := m.Run(context.Background(), sig)
	if len(evicted()) != 2 {
		t.Errorf("evicted %d pods, want the 2 the budget allows", len(evicted()))
	}
	if res.Suppressed["rate-limited"] == 0 {
		t.Errorf("rate limiting was not recorded: %v", res.Suppressed)
	}
}

func TestStabilisationDelaysTheFirstEviction(t *testing.T) {
	m, evicted := newTestManager(t, &alwaysPlugin{})
	now := time.Now()
	m.Now = func() time.Time { return now }

	sig := signal([]*corev1.Pod{testPod("batch", v1alpha1.QoSReclaimedCores, "")},
		map[string]collector.PodSample{}, 0,
		v1alpha1.NodePressure{Level: v1alpha1.PressureCritical})
	sig.Config.StabilisationSeconds = 30

	// First breach: the clock starts, nothing is evicted.
	if res := m.Run(context.Background(), sig); res.Suppressed["stabilising"] == 0 {
		t.Errorf("first pass should have been suppressed: %v", res.Suppressed)
	}
	if len(evicted()) != 0 {
		t.Fatal("evicted on the first breached sample")
	}

	// Still breached 40 seconds later: now it acts.
	now = now.Add(40 * time.Second)
	m.Run(context.Background(), sig)
	if len(evicted()) != 1 {
		t.Errorf("evicted %d after the stabilisation period, want 1", len(evicted()))
	}
}

func TestPressureClearingResetsStabilisation(t *testing.T) {
	m, evicted := newTestManager(t, &alwaysPlugin{})
	now := time.Now()
	m.Now = func() time.Time { return now }

	breached := signal([]*corev1.Pod{testPod("batch", v1alpha1.QoSReclaimedCores, "")},
		map[string]collector.PodSample{}, 0,
		v1alpha1.NodePressure{Level: v1alpha1.PressureCritical})
	breached.Config.StabilisationSeconds = 30

	m.Run(context.Background(), breached)

	// The node recovers, then breaches again. The clock must restart, not
	// carry over -- otherwise two brief unrelated spikes an hour apart add up
	// to an eviction.
	calm := breached
	calm.Recommendation.Pressure.Level = v1alpha1.PressureNone
	now = now.Add(20 * time.Second)
	m.Run(context.Background(), calm)

	now = now.Add(20 * time.Second)
	m.Run(context.Background(), breached)
	if len(evicted()) != 0 {
		t.Error("stabilisation clock was not reset when pressure cleared")
	}
}

func TestTokenBucketRefills(t *testing.T) {
	b := newTokenBucket(2, time.Minute)
	now := time.Now()

	if !b.take(now) || !b.take(now) {
		t.Fatal("bucket should start full")
	}
	if b.take(now) {
		t.Error("bucket handed out a third token")
	}

	// Half a window later, one token is back.
	if !b.take(now.Add(30 * time.Second)) {
		t.Error("bucket did not refill")
	}

	// Lowering the capacity must take effect immediately rather than after the
	// existing tokens drain.
	b.configure(0, time.Minute)
	if b.take(now.Add(time.Hour)) {
		t.Error("a zero-capacity bucket handed out a token")
	}
}

// alwaysPlugin proposes evicting every candidate, so the manager's safety
// machinery can be tested without depending on a real policy's triggers.
type alwaysPlugin struct{}

func (alwaysPlugin) Name() string { return "always" }

func (alwaysPlugin) Evaluate(_ context.Context, sig Signal) ([]Verdict, error) {
	out := make([]Verdict, 0, len(sig.Pods))
	for _, p := range sig.Pods {
		out = append(out, Verdict{Pod: p, Plugin: "always", Reason: "Test", Message: "test"})
	}
	return out, nil
}
