package webhook

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/xkong-study/kqos/pkg/apis/kqos/v1alpha1"
)

func reclaimedPod(cpu, mem string, withMemLimit bool) *corev1.Pod {
	c := corev1.Container{
		Name: "worker",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(cpu),
				corev1.ResourceMemory: resource.MustParse(mem),
			},
			Limits: corev1.ResourceList{},
		},
	}
	if withMemLimit {
		c.Resources.Limits[corev1.ResourceMemory] = resource.MustParse(mem)
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "batch",
			Namespace:   "demo",
			Annotations: map[string]string{v1alpha1.AnnotationQoSLevel: "reclaimed_cores"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{c}},
	}
}

func TestRewriteReclaimedMovesCPUIntoAnExtendedResource(t *testing.T) {
	p := reclaimedPod("1500m", "256Mi", true)
	if !rewriteReclaimed(p) {
		t.Fatal("rewrite reported no change")
	}
	res := p.Spec.Containers[0].Resources

	// The native CPU request is gone: this is what makes the pod invisible to
	// the scheduler's real-capacity accounting.
	if _, ok := res.Requests[corev1.ResourceCPU]; ok {
		t.Error("native cpu request survived the rewrite")
	}
	if _, ok := res.Limits[corev1.ResourceCPU]; ok {
		t.Error("a cpu limit survived; it would stop the pod using idle capacity")
	}

	// Extended resources must appear in equal amounts on both sides, which the
	// API server enforces.
	req := res.Requests[corev1.ResourceName(v1alpha1.ResourceReclaimedCPU)]
	lim := res.Limits[corev1.ResourceName(v1alpha1.ResourceReclaimedCPU)]
	if req.Value() != 1500 {
		t.Errorf("reclaimed-cpu request = %d, want 1500 (millicores)", req.Value())
	}
	if req.Cmp(lim) != 0 {
		t.Errorf("reclaimed-cpu request %v != limit %v", req.Value(), lim.Value())
	}
}

func TestRewriteReclaimedKeepsTheMemoryLimit(t *testing.T) {
	p := reclaimedPod("1500m", "256Mi", true)
	rewriteReclaimed(p)

	// Without a runtime ceiling a reclaimed pod could take the node down long
	// before any eviction policy noticed.
	lim, ok := p.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory]
	if !ok {
		t.Fatal("memory limit was removed")
	}
	if lim.Value() != 256*1024*1024 {
		t.Errorf("memory limit = %d, want 256Mi", lim.Value())
	}
	mib := p.Spec.Containers[0].Resources.Requests[corev1.ResourceName(v1alpha1.ResourceReclaimedMemory)]
	if mib.Value() != 256 {
		t.Errorf("reclaimed-memory = %d, want 256 (MiB)", mib.Value())
	}
}

func TestRewriteReclaimedSynthesisesAMemoryLimit(t *testing.T) {
	// A pod that declared a memory request but no limit still needs a ceiling
	// once its request has been converted away.
	p := reclaimedPod("500m", "128Mi", false)
	rewriteReclaimed(p)

	lim, ok := p.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory]
	if !ok {
		t.Fatal("no memory limit was synthesised")
	}
	if lim.Value() != 128*1024*1024 {
		t.Errorf("synthesised limit = %d, want the original request", lim.Value())
	}
}

func TestRewriteRecordsTheOriginalResources(t *testing.T) {
	p := reclaimedPod("1500m", "256Mi", true)
	rewriteReclaimed(p)

	raw, ok := p.Annotations[v1alpha1.AnnotationOriginalResources]
	if !ok {
		t.Fatal("no audit annotation was written")
	}
	// Debugging "why does my pod have no CPU request?" without this is
	// genuinely miserable, so the content matters, not just its presence.
	if !strings.Contains(raw, "1500m") {
		t.Errorf("audit annotation lost the original value: %s", raw)
	}
}

func TestValidateDedicatedRequiresWholeCores(t *testing.T) {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "demo"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "app",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1500m")},
			},
		}}},
	}
	err := validate(p, v1alpha1.QoSDedicatedCores)
	if err == nil {
		t.Fatal("a fractional dedicated request was accepted")
	}
	// The message has to say what to do about it, not just that it is wrong.
	if !strings.Contains(err.Error(), "whole cores") {
		t.Errorf("unhelpful error: %v", err)
	}
}

func TestValidateDedicatedRequiresACPURequest(t *testing.T) {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "demo"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
	}
	if err := validate(p, v1alpha1.QoSDedicatedCores); err == nil {
		t.Error("a dedicated pod with no CPU request was accepted")
	}
}

func TestValidateAcceptsWholeCoreDedicated(t *testing.T) {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "demo"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "app",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
			},
		}}},
	}
	if err := validate(p, v1alpha1.QoSDedicatedCores); err != nil {
		t.Errorf("2 whole cores were rejected: %v", err)
	}
}

func TestValidateRejectsPrioritisedReclaimedPods(t *testing.T) {
	prio := int32(1000)
	p := reclaimedPod("500m", "128Mi", true)
	p.Spec.Priority = &prio

	err := validate(p, v1alpha1.QoSReclaimedCores)
	if err == nil {
		t.Fatal("a high-priority reclaimed pod was accepted")
	}
	if !strings.Contains(err.Error(), "first thing to go") {
		t.Errorf("error does not explain the contradiction: %v", err)
	}
}

func TestValidateRejectsUnknownLevels(t *testing.T) {
	p := reclaimedPod("500m", "128Mi", true)
	err := validate(p, v1alpha1.QoSLevel("platinum"))
	if err == nil {
		t.Fatal("an unknown level was accepted")
	}
	// The error must list what is valid; a rejection with no alternatives is
	// just an obstacle.
	for _, level := range v1alpha1.KnownQoSLevels {
		if !strings.Contains(err.Error(), string(level)) {
			t.Errorf("error does not mention %s: %v", level, err)
		}
	}
}

func TestValidateAllowsNUMABindingOnDedicated(t *testing.T) {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "svc", Namespace: "demo",
			Annotations: map[string]string{
				v1alpha1.AnnotationQoSLevel:    "dedicated_cores",
				v1alpha1.AnnotationNUMABinding: "true",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "app",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
			},
		}}},
	}
	if err := validate(p, v1alpha1.QoSDedicatedCores); err != nil {
		t.Errorf("a NUMA-bound dedicated pod was rejected: %v", err)
	}

	// The same annotation on a shared pod is a request kqos cannot honour:
	// there are no exclusive CPUs to confine to a zone.
	p.Annotations[v1alpha1.AnnotationQoSLevel] = "shared_cores"
	if err := validate(p, v1alpha1.QoSSharedCores); err == nil {
		t.Error("numa-binding was accepted on a shared_cores pod")
	}
}

func TestRewriteIsANoOpWithoutRequests(t *testing.T) {
	// A pod declaring nothing has nothing to convert, and must not acquire an
	// audit annotation claiming otherwise.
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "batch", Namespace: "demo", Annotations: map[string]string{}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "worker"}}},
	}
	if rewriteReclaimed(p) {
		t.Error("rewrite reported a change on a pod with no requests")
	}
	if _, ok := p.Annotations[v1alpha1.AnnotationOriginalResources]; ok {
		t.Error("an audit annotation was written for a no-op rewrite")
	}
}
