package qos

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kongxiangrui/kqos/pkg/apis/kqos/v1alpha1"
)

func pod(annotations map[string]string, containers ...corev1.Container) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", Annotations: annotations},
		Spec:       corev1.PodSpec{Containers: containers},
	}
}

func container(name string, reqCPU, reqMem, limCPU, limMem string) corev1.Container {
	c := corev1.Container{Name: name, Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{},
		Limits:   corev1.ResourceList{},
	}}
	if reqCPU != "" {
		c.Resources.Requests[corev1.ResourceCPU] = resource.MustParse(reqCPU)
	}
	if reqMem != "" {
		c.Resources.Requests[corev1.ResourceMemory] = resource.MustParse(reqMem)
	}
	if limCPU != "" {
		c.Resources.Limits[corev1.ResourceCPU] = resource.MustParse(limCPU)
	}
	if limMem != "" {
		c.Resources.Limits[corev1.ResourceMemory] = resource.MustParse(limMem)
	}
	return c
}

func TestLevelOfPrefersTheAnnotation(t *testing.T) {
	p := pod(map[string]string{v1alpha1.AnnotationQoSLevel: "reclaimed_cores"},
		container("c", "1", "1Gi", "1", "1Gi"))
	// The container spec says Guaranteed, but the operator said reclaimed. The
	// declared intent wins, because that is the whole point of declaring it.
	if got := LevelOf(p); got != v1alpha1.QoSReclaimedCores {
		t.Errorf("LevelOf = %s, want reclaimed_cores", got)
	}
}

func TestLevelOfIgnoresGarbageAnnotations(t *testing.T) {
	p := pod(map[string]string{v1alpha1.AnnotationQoSLevel: "platinum"},
		container("c", "100m", "128Mi", "", ""))
	if got := LevelOf(p); got != v1alpha1.QoSSharedCores {
		t.Errorf("LevelOf = %s, want the inferred shared_cores", got)
	}
}

func TestLevelOfInfersFromNativeQoS(t *testing.T) {
	cases := []struct {
		name string
		pod  *corev1.Pod
		want v1alpha1.QoSLevel
	}{
		{
			// Requests == limits on every resource: Guaranteed, so this pod is
			// asking for isolation whether or not it knows about kqos.
			"guaranteed becomes dedicated",
			pod(nil, container("c", "1", "1Gi", "1", "1Gi")),
			v1alpha1.QoSDedicatedCores,
		},
		{
			"burstable becomes shared",
			pod(nil, container("c", "100m", "128Mi", "500m", "512Mi")),
			v1alpha1.QoSSharedCores,
		},
		{
			// BestEffort has promised nothing, so it costs nothing to treat it
			// as the reclaimed tier -- which is what makes kqos safe to install
			// on a cluster nobody has annotated.
			"besteffort becomes reclaimed",
			pod(nil, container("c", "", "", "", "")),
			v1alpha1.QoSReclaimedCores,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := LevelOf(tc.pod); got != tc.want {
				t.Errorf("LevelOf = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestSystemCriticalPodsAreNeverEvictable(t *testing.T) {
	p := pod(nil, container("c", "", "", "", ""))
	p.Spec.PriorityClassName = "system-node-critical"

	if got := LevelOf(p); got != v1alpha1.QoSSystemCores {
		t.Fatalf("LevelOf = %s, want system_cores", got)
	}
	if LevelOf(p).Evictable() {
		t.Error("system_cores must not be evictable")
	}
}

func TestEvictionRankOrdersTiers(t *testing.T) {
	ranks := map[v1alpha1.QoSLevel]int{}
	for _, l := range v1alpha1.KnownQoSLevels {
		ranks[l] = l.EvictionRank()
	}
	if !(ranks[v1alpha1.QoSReclaimedCores] > ranks[v1alpha1.QoSSharedCores] &&
		ranks[v1alpha1.QoSSharedCores] > ranks[v1alpha1.QoSDedicatedCores] &&
		ranks[v1alpha1.QoSDedicatedCores] > ranks[v1alpha1.QoSSystemCores]) {
		t.Errorf("eviction order is wrong: %v", ranks)
	}
}

func TestPodRequestsUsesTheInitContainerRule(t *testing.T) {
	p := pod(nil,
		container("a", "100m", "128Mi", "", ""),
		container("b", "200m", "256Mi", "", ""))
	p.Spec.InitContainers = []corev1.Container{
		container("init", "1", "1Gi", "", ""),
	}

	got := PodRequests(p)
	cpu := got[corev1.ResourceCPU]
	// Init containers run to completion first, so the pod needs the larger of
	// (max init) and (sum of app containers) -- not their sum.
	if cpu.MilliValue() != 1000 {
		t.Errorf("cpu request = %dm, want 1000m (the init container dominates)", cpu.MilliValue())
	}
}

func TestEffectiveRequestsReadsBackExtendedResources(t *testing.T) {
	// A reclaimed pod after the webhook has rewritten it: no cpu or memory
	// request at all, only the kqos extended resources.
	c := corev1.Container{
		Name: "worker",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceName(v1alpha1.ResourceReclaimedCPU):    resource.MustParse("1500"),
				corev1.ResourceName(v1alpha1.ResourceReclaimedMemory): resource.MustParse("256"),
			},
		},
	}
	p := pod(map[string]string{v1alpha1.AnnotationQoSLevel: "reclaimed_cores"}, c)

	raw := PodRequests(p)
	if rawCPU := raw[corev1.ResourceCPU]; !rawCPU.IsZero() {
		t.Error("raw requests should show no cpu; that is the point of the rewrite")
	}

	eff := EffectiveRequests(p)
	cpu := eff[corev1.ResourceCPU]
	mem := eff[corev1.ResourceMemory]
	if cpu.MilliValue() != 1500 {
		t.Errorf("effective cpu = %dm, want 1500m", cpu.MilliValue())
	}
	if mem.Value() != 256*1024*1024 {
		t.Errorf("effective memory = %d, want %d", mem.Value(), 256*1024*1024)
	}
}

func TestWantsNUMABindingOnlyForDedicated(t *testing.T) {
	dedicated := pod(map[string]string{
		v1alpha1.AnnotationQoSLevel:    "dedicated_cores",
		v1alpha1.AnnotationNUMABinding: "true",
	}, container("c", "2", "1Gi", "2", "1Gi"))
	if !WantsNUMABinding(dedicated) {
		t.Error("dedicated pod with the annotation should want NUMA binding")
	}

	shared := pod(map[string]string{
		v1alpha1.AnnotationQoSLevel:    "shared_cores",
		v1alpha1.AnnotationNUMABinding: "true",
	}, container("c", "1", "1Gi", "1", "1Gi"))
	if WantsNUMABinding(shared) {
		t.Error("NUMA binding is meaningless without exclusive CPUs")
	}
}

func TestIsMirrorPod(t *testing.T) {
	p := pod(map[string]string{corev1.MirrorPodAnnotationKey: "x"}, container("c", "", "", "", ""))
	if !IsMirrorPod(p) {
		t.Error("static pod should be detected as a mirror pod")
	}
}
