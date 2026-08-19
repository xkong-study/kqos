// Package webhook implements kqos admission control for pods.
//
// Three jobs, in order of importance:
//
//  1. Rewrite reclaimed pods so their cpu/memory requests become
//     kqos.io/reclaimed-* extended resources. This is what makes the whole
//     scheme safe: after the rewrite the native scheduler sees a pod with no
//     cpu or memory request at all, so oversold pods cannot displace or crowd
//     out guaranteed ones, and they can only land where kqos has advertised
//     spare capacity.
//  2. Default the QoS annotation, so every pod has an explicit class.
//  3. Reject configurations that cannot work, with an error that says why.
package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/kongxiangrui/kqos/pkg/apis/kqos/v1alpha1"
	"github.com/kongxiangrui/kqos/pkg/metrics"
	"github.com/kongxiangrui/kqos/pkg/qos"
)

// PodMutator implements the mutating admission path.
type PodMutator struct {
	Client  client.Client
	decoder admission.Decoder
}

// NewPodMutator builds a mutator with its decoder.
func NewPodMutator(c client.Client, decoder admission.Decoder) *PodMutator {
	return &PodMutator{Client: c, decoder: decoder}
}

// Handle mutates one pod.
//
// Every failure path here returns Allowed. An admission webhook that fails
// closed on its own bugs takes the cluster down with it, and kqos is an
// optimisation: a pod admitted without kqos annotations runs perfectly well,
// it just does not participate in overcommit.
func (m *PodMutator) Handle(ctx context.Context, req admission.Request) admission.Response {
	logger := log.FromContext(ctx).WithName("pod-webhook")

	pod := &corev1.Pod{}
	if err := m.decoder.Decode(req, pod); err != nil {
		logger.Error(err, "decoding pod; admitting unchanged")
		metrics.WebhookMutations.WithLabelValues("unknown", "decode-error").Inc()
		return admission.Allowed("kqos could not decode the pod")
	}

	cfg := m.config(ctx)
	for _, ns := range cfg.ExemptNamespaces {
		if req.Namespace == ns {
			metrics.WebhookMutations.WithLabelValues("unknown", "exempt").Inc()
			return admission.Allowed("namespace is exempt from kqos")
		}
	}

	original := pod.DeepCopy()

	level, explicit := explicitLevel(pod)
	if !explicit {
		level = cfg.DefaultQoSLevel
		if !level.IsValid() {
			level = v1alpha1.QoSSharedCores
		}
		// Inferring from the native QoS class produces a better default than a
		// blanket one: a Guaranteed pod that says nothing clearly wants
		// isolation, and a BestEffort pod has already declared it wants nothing.
		if inferred := qos.LevelOf(pod); inferred != v1alpha1.QoSSharedCores {
			level = inferred
		}
	}

	if err := validate(pod, level); err != nil {
		metrics.WebhookMutations.WithLabelValues(string(level), "denied").Inc()
		return admission.Denied(err.Error())
	}

	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[v1alpha1.AnnotationQoSLevel] = string(level)

	action := "annotated"
	if level == v1alpha1.QoSReclaimedCores && cfg.RewriteReclaimedResources {
		if rewriteReclaimed(pod) {
			action = "rewritten"
		}
	}

	metrics.WebhookMutations.WithLabelValues(string(level), action).Inc()

	patched, err := json.Marshal(pod)
	if err != nil {
		logger.Error(err, "marshalling mutated pod; admitting unchanged")
		return admission.Allowed("kqos could not serialise its mutation")
	}
	_ = original
	return admission.PatchResponseFromRaw(req.Object.Raw, patched)
}

// explicitLevel reads a user-supplied QoS annotation from the pod or, for
// pods created by a controller, from nothing else -- the template annotation
// is already on the pod by the time admission sees it.
func explicitLevel(pod *corev1.Pod) (v1alpha1.QoSLevel, bool) {
	v, ok := pod.Annotations[v1alpha1.AnnotationQoSLevel]
	if !ok {
		return "", false
	}
	level := v1alpha1.QoSLevel(v)
	return level, level.IsValid()
}

// config reads the live webhook configuration, falling back to safe defaults.
func (m *PodMutator) config(ctx context.Context) v1alpha1.WebhookConfig {
	def := v1alpha1.WebhookConfig{
		DefaultQoSLevel:           v1alpha1.QoSSharedCores,
		RewriteReclaimedResources: true,
	}
	policy := &v1alpha1.QoSPolicy{}
	if err := m.Client.Get(ctx, client.ObjectKey{Name: v1alpha1.DefaultQoSPolicyName}, policy); err != nil {
		return def
	}
	cfg := policy.Spec.Webhook
	if !cfg.DefaultQoSLevel.IsValid() {
		cfg.DefaultQoSLevel = def.DefaultQoSLevel
	}
	return cfg
}

// validate rejects pods whose declared level cannot be honoured.
func validate(pod *corev1.Pod, level v1alpha1.QoSLevel) error {
	if !level.IsValid() {
		return fmt.Errorf("%s=%q is not a valid kqos level; valid values are %s",
			v1alpha1.AnnotationQoSLevel, level, joinLevels())
	}

	if level == v1alpha1.QoSDedicatedCores {
		// Exclusive CPUs can only be handed out in whole units, and a pod that
		// asks for 1500m would silently get 2 -- better to say so at admission
		// than to have someone discover it from a bill.
		for _, c := range pod.Spec.Containers {
			q, ok := c.Resources.Requests[corev1.ResourceCPU]
			if !ok || q.IsZero() {
				return fmt.Errorf("container %q is dedicated_cores but declares no CPU request; dedicated pods must state the cores they need", c.Name)
			}
			if q.MilliValue()%1000 != 0 {
				return fmt.Errorf("container %q is dedicated_cores with a fractional CPU request (%s); exclusive CPUs are allocated in whole cores", c.Name, q.String())
			}
		}
	}

	if level == v1alpha1.QoSReclaimedCores {
		for _, c := range pod.Spec.Containers {
			if q, ok := c.Resources.Limits[corev1.ResourceMemory]; ok && q.IsZero() {
				return fmt.Errorf("container %q is reclaimed_cores with a zero memory limit", c.Name)
			}
		}
		if pod.Spec.Priority != nil && *pod.Spec.Priority > 0 {
			return fmt.Errorf("reclaimed_cores pods must not carry a positive priority (%d); the reclaimed tier is defined by being the first thing to go", *pod.Spec.Priority)
		}
	}

	// Read the annotation directly rather than going through
	// qos.WantsNUMABinding, which is itself level-aware and would make this
	// check unreachable. A pod asking to be pinned to a NUMA zone without
	// exclusive CPUs to pin is a request kqos cannot honour, and silently
	// ignoring it is how someone ends up believing they have isolation.
	if pod.Annotations[v1alpha1.AnnotationNUMABinding] == "true" && level != v1alpha1.QoSDedicatedCores {
		return fmt.Errorf("%s is only meaningful for dedicated_cores pods, but this pod is %s",
			v1alpha1.AnnotationNUMABinding, level)
	}
	return nil
}

// rewriteReclaimed converts a pod's cpu/memory requests into kqos extended
// resources, returning whether anything changed.
//
// The two resources end up behaving differently, and that asymmetry is the
// point rather than an oversight.
//
// CPU is fully oversold. The request is removed outright, so a reclaimed pod
// is invisible to the scheduler's CPU accounting and can only land where
// kqos.io/reclaimed-cpu has been advertised. That is safe because CPU is
// compressible: when the online tier wants its cycles back, cpu.weight hands
// them over within a scheduling quantum.
//
// Memory is not oversold. The memory limit is deliberately kept -- without a
// runtime ceiling a reclaimed pod could take the node down before any eviction
// policy noticed -- and Kubernetes then defaults the memory *request* back
// from that limit, so the pod still occupies real memory in the scheduler's
// books. kqos does not fight this. Overselling an incompressible resource
// means the only way to honour the promise is to kill something, and a design
// whose steady state is "kill something" is not a design.
//
// kqos.io/reclaimed-memory therefore acts as a ceiling rather than a currency:
// it caps how much reclaimed work a node will accept regardless of how much
// raw memory happens to be free.
func rewriteReclaimed(pod *corev1.Pod) bool {
	changed := false
	snapshot := map[string]corev1.ResourceRequirements{}

	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		cpuReq, hasCPU := c.Resources.Requests[corev1.ResourceCPU]
		memReq, hasMem := c.Resources.Requests[corev1.ResourceMemory]
		if !hasCPU && !hasMem {
			continue
		}
		snapshot[c.Name] = *c.Resources.DeepCopy()

		if c.Resources.Requests == nil {
			c.Resources.Requests = corev1.ResourceList{}
		}
		if c.Resources.Limits == nil {
			c.Resources.Limits = corev1.ResourceList{}
		}

		if hasCPU && !cpuReq.IsZero() {
			milli := cpuReq.MilliValue()
			delete(c.Resources.Requests, corev1.ResourceCPU)
			// CPU limits on a reclaimed pod are counterproductive: cpu.weight
			// already makes it yield under contention, and a hard cap stops it
			// using the idle capacity it exists to consume.
			delete(c.Resources.Limits, corev1.ResourceCPU)
			setExtended(c, v1alpha1.ResourceReclaimedCPU, milli)
			changed = true
		}
		if hasMem && !memReq.IsZero() {
			mib := memReq.Value() / (1024 * 1024)
			if mib < 1 {
				mib = 1
			}
			delete(c.Resources.Requests, corev1.ResourceMemory)
			if _, ok := c.Resources.Limits[corev1.ResourceMemory]; !ok {
				// Give it a runtime ceiling equal to what it asked for, since
				// the request no longer provides one.
				c.Resources.Limits[corev1.ResourceMemory] = memReq.DeepCopy()
			}
			setExtended(c, v1alpha1.ResourceReclaimedMemory, mib)
			changed = true
		}
		if len(c.Resources.Limits) == 0 {
			c.Resources.Limits = nil
		}
	}

	if changed {
		if raw, err := json.Marshal(snapshot); err == nil {
			// Record what was rewritten. Debugging "why does my pod have no CPU
			// request?" without this annotation is genuinely miserable.
			pod.Annotations[v1alpha1.AnnotationOriginalResources] = string(raw)
		}
	}
	return changed
}

// setExtended adds an extended resource to both requests and limits, which the
// API server requires: extended resources must be requested in equal amounts
// on both sides.
func setExtended(c *corev1.Container, name string, value int64) {
	q := *resource.NewQuantity(value, resource.DecimalSI)
	c.Resources.Requests[corev1.ResourceName(name)] = q
	c.Resources.Limits[corev1.ResourceName(name)] = q
}

func joinLevels() string {
	parts := make([]string, 0, len(v1alpha1.KnownQoSLevels))
	for _, l := range v1alpha1.KnownQoSLevels {
		parts = append(parts, string(l))
	}
	return strings.Join(parts, ", ")
}

// Ensure the handler satisfies the admission interface at compile time.
var _ admission.Handler = &PodMutator{}

// HTTPStatusForError is used by the webhook server's error path.
const HTTPStatusForError = http.StatusInternalServerError
