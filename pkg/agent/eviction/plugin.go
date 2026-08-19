// Package eviction implements the node-local eviction manager: a small plugin
// framework, three plugins, and the safety machinery that stands between a
// plugin's opinion and a pod actually dying.
//
// The framework exists because eviction policy is the part of a resource
// manager that every operator wants to change. Making each policy a plugin
// with an explicit trigger and an explicit victim ordering means a new policy
// is fifty lines and a registration, not a patch to the control loop.
package eviction

import (
	"context"
	"sort"

	corev1 "k8s.io/api/core/v1"

	"github.com/xkong-study/kqos/pkg/agent/collector"
	"github.com/xkong-study/kqos/pkg/agent/sysadvisor"
	"github.com/xkong-study/kqos/pkg/apis/kqos/v1alpha1"
)

// Signal is everything a plugin gets to reason about. Passing one struct
// rather than a growing argument list keeps plugin signatures stable as the
// agent learns to measure more things.
type Signal struct {
	// Sample is the most recent observation of the node.
	Sample collector.NodeSample

	// Recommendation is the advisor's current verdict, including the smoothed
	// pressure classification. Plugins should prefer this to raw sample values
	// when deciding whether to trigger, because it is already de-noised.
	Recommendation sysadvisor.Recommendation

	// Allocatable is the node's allocatable capacity.
	Allocatable corev1.ResourceList

	// Config is the live eviction configuration from the QoSPolicy.
	Config v1alpha1.EvictionConfig

	// Pods are the candidate pods on this node, already filtered to those that
	// are running, non-mirror and non-system.
	Pods []*corev1.Pod

	// SampleByUID indexes Sample.Pods for plugins that need per-pod usage.
	SampleByUID map[string]collector.PodSample
}

// Verdict is one plugin's proposal to evict one pod.
type Verdict struct {
	Pod *corev1.Pod

	// Plugin is the name of the plugin that proposed the eviction.
	Plugin string

	// Reason is a short machine-readable cause, used as the Kubernetes event
	// reason and as a metric label.
	Reason string

	// Message is the human-readable explanation recorded on the event. It must
	// contain the numbers that triggered the decision: an eviction nobody can
	// explain afterwards is an outage nobody can prevent.
	Message string

	// Score orders victims within a plugin's proposal; higher is evicted first.
	Score float64

	// ReleasesCPUMilli and ReleasesMemoryBytes estimate what evicting this pod
	// recovers, letting the manager stop as soon as enough has been freed
	// rather than draining every candidate.
	ReleasesCPUMilli    int64
	ReleasesMemoryBytes int64
}

// Plugin is one eviction policy.
type Plugin interface {
	// Name identifies the plugin in config, metrics and events.
	Name() string

	// Evaluate returns the pods this plugin wants evicted, or nil when its
	// trigger condition is not met. It must be side-effect free: the manager
	// calls it on every tick including in dry-run mode.
	Evaluate(ctx context.Context, sig Signal) ([]Verdict, error)
}

// registry is the set of plugin constructors known to the binary.
var registry = map[string]func() Plugin{}

// Register adds a plugin constructor. Called from plugin file init functions,
// so adding a policy needs no edit to the manager.
func Register(name string, ctor func() Plugin) {
	registry[name] = ctor
}

// BuildPlugins instantiates every registered plugin except those disabled in
// the policy, in a stable order so logs are comparable across restarts.
func BuildPlugins(disabled []string) []Plugin {
	skip := make(map[string]struct{}, len(disabled))
	for _, d := range disabled {
		skip[d] = struct{}{}
	}
	names := make([]string, 0, len(registry))
	for name := range registry {
		if _, ok := skip[name]; ok {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]Plugin, 0, len(names))
	for _, name := range names {
		out = append(out, registry[name]())
	}
	return out
}

// RegisteredNames lists every plugin compiled into the binary.
func RegisteredNames() []string {
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
