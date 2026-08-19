package eviction

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/kongxiangrui/kqos/pkg/apis/kqos/v1alpha1"
	"github.com/kongxiangrui/kqos/pkg/metrics"
	"github.com/kongxiangrui/kqos/pkg/qos"
)

// Manager runs the plugins and decides which of their verdicts survive.
//
// Every guard in this type exists because the failure it prevents is worse
// than the pressure it is responding to. A resource manager that evicts too
// eagerly is strictly worse than one that does nothing: doing nothing degrades
// service, evicting wrongly destroys it.
type Manager struct {
	nodeName string
	client   kubernetes.Interface
	recorder record.EventRecorder
	plugins  []Plugin

	// budget rate-limits evictions.
	budget *tokenBucket

	// breachedSince tracks when the current pressure episode began, enforcing
	// the stabilisation period so a single bad sample cannot evict anything.
	breachedSince time.Time

	// recentlyEvicted suppresses repeat verdicts for a pod that has already
	// been told to go but has not finished terminating. Without this the
	// manager spends its whole budget re-evicting the same pod.
	recentlyEvicted map[string]time.Time

	// Now is swappable for tests.
	Now func() time.Time
}

// NewManager builds an eviction manager for one node.
func NewManager(nodeName string, client kubernetes.Interface, recorder record.EventRecorder, plugins []Plugin) *Manager {
	return &Manager{
		nodeName:        nodeName,
		client:          client,
		recorder:        recorder,
		plugins:         plugins,
		budget:          newTokenBucket(3, time.Minute),
		recentlyEvicted: make(map[string]time.Time),
		Now:             time.Now,
	}
}

// SetPlugins swaps the active plugin set, used when the QoSPolicy changes the
// disabled list without a restart.
func (m *Manager) SetPlugins(plugins []Plugin) { m.plugins = plugins }

// Result summarises one pass for logging and tests.
type Result struct {
	Proposed   int
	Evicted    []Verdict
	Suppressed map[string]int
}

// Run evaluates every plugin against the signal and executes the surviving
// verdicts. It never returns an error for a single failed eviction: one pod
// protected by a disruption budget must not stop the manager from relieving
// pressure using another.
func (m *Manager) Run(ctx context.Context, sig Signal) Result {
	logger := log.FromContext(ctx).WithName("eviction")
	res := Result{Suppressed: map[string]int{}}

	if !sig.Config.Enabled {
		return res
	}

	// A degraded sample is an estimate, not a measurement. Acting on one would
	// mean evicting pods based on a guess about their memory use.
	if sig.Sample.Degraded {
		res.Suppressed["degraded-sample"]++
		metrics.EvictionsSuppressedTotal.WithLabelValues(m.nodeName, "degraded-sample").Inc()
		return res
	}

	now := m.Now()
	m.trackBreach(sig, now)

	var verdicts []Verdict
	for _, p := range m.plugins {
		vs, err := p.Evaluate(ctx, sig)
		if err != nil {
			logger.Error(err, "eviction plugin failed", "plugin", p.Name())
			continue
		}
		for _, v := range vs {
			metrics.EvictionsProposedTotal.WithLabelValues(m.nodeName, v.Plugin, v.Reason).Inc()
		}
		verdicts = append(verdicts, vs...)
	}
	res.Proposed = len(verdicts)
	if len(verdicts) == 0 {
		return res
	}

	// The stabilisation period is checked after plugins run so that proposals
	// are still counted and visible in metrics while kqos waits.
	if wait := time.Duration(sig.Config.StabilisationSeconds) * time.Second; wait > 0 {
		if m.breachedSince.IsZero() || now.Sub(m.breachedSince) < wait {
			res.Suppressed["stabilising"] = len(verdicts)
			metrics.EvictionsSuppressedTotal.WithLabelValues(m.nodeName, "stabilising").Add(float64(len(verdicts)))
			return res
		}
	}

	m.budget.configure(int(sig.Config.MaxEvictionsPerMinute), time.Minute)
	seen := make(map[string]struct{}, len(verdicts))

	for _, v := range verdicts {
		key := v.Pod.Namespace + "/" + v.Pod.Name
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}

		if cause := m.veto(v, now); cause != "" {
			res.Suppressed[cause]++
			metrics.EvictionsSuppressedTotal.WithLabelValues(m.nodeName, cause).Inc()
			continue
		}
		if !m.budget.take(now) {
			res.Suppressed["rate-limited"]++
			metrics.EvictionsSuppressedTotal.WithLabelValues(m.nodeName, "rate-limited").Inc()
			break
		}

		level := qos.LevelOf(v.Pod)
		if sig.Config.DryRun {
			logger.Info("dry-run eviction", "pod", key, "plugin", v.Plugin, "reason", v.Reason, "message", v.Message)
			m.event(v.Pod, corev1.EventTypeNormal, "KqosEvictionDryRun", v.Message)
			res.Suppressed["dry-run"]++
			metrics.EvictionsSuppressedTotal.WithLabelValues(m.nodeName, "dry-run").Inc()
			continue
		}

		if err := m.evict(ctx, v, sig.Config.GracePeriodSeconds); err != nil {
			cause := "api-error"
			if apierrors.IsTooManyRequests(err) {
				// The eviction API returns 429 when a PodDisruptionBudget would
				// be violated. That is the application telling kqos it cannot
				// afford to lose this replica, and it is authoritative.
				cause = "disruption-budget"
			}
			logger.Error(err, "eviction rejected", "pod", key, "cause", cause)
			metrics.EvictionErrorsTotal.WithLabelValues(m.nodeName, cause).Inc()
			res.Suppressed[cause]++
			continue
		}

		m.recentlyEvicted[key] = now
		metrics.EvictionsTotal.WithLabelValues(m.nodeName, v.Plugin, v.Reason, string(level)).Inc()
		m.event(v.Pod, corev1.EventTypeWarning, "KqosEvicted", v.Message)
		logger.Info("evicted pod", "pod", key, "plugin", v.Plugin, "reason", v.Reason, "qosLevel", level)
		res.Evicted = append(res.Evicted, v)
	}

	m.gcRecent(now)
	return res
}

// trackBreach maintains the stabilisation clock.
func (m *Manager) trackBreach(sig Signal, now time.Time) {
	if sig.Recommendation.Pressure.Level == v1alpha1.PressureNone {
		m.breachedSince = time.Time{}
		return
	}
	if m.breachedSince.IsZero() {
		m.breachedSince = now
	}
}

// veto applies the rules that no plugin may override, returning the name of
// the rule that blocked the eviction or "" to allow it.
func (m *Manager) veto(v Verdict, now time.Time) string {
	pod := v.Pod
	level := qos.LevelOf(pod)

	if !level.Evictable() {
		return "system-tier"
	}
	if qos.IsMirrorPod(pod) {
		// The kubelet would recreate it within seconds, so the eviction buys
		// nothing and costs a restart.
		return "mirror-pod"
	}
	if pod.DeletionTimestamp != nil {
		return "already-terminating"
	}
	if qos.IsTerminal(pod) {
		return "already-terminal"
	}
	if pod.Spec.Priority != nil && *pod.Spec.Priority >= 2_000_000_000 {
		// The reserved range for system-critical priorities.
		return "system-critical-priority"
	}
	if at, ok := m.recentlyEvicted[pod.Namespace+"/"+pod.Name]; ok && now.Sub(at) < 2*time.Minute {
		return "recently-evicted"
	}
	return ""
}

func (m *Manager) evict(ctx context.Context, v Verdict, grace int64) error {
	e := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      v.Pod.Name,
			Namespace: v.Pod.Namespace,
		},
		DeleteOptions: &metav1.DeleteOptions{GracePeriodSeconds: &grace},
	}
	if err := m.client.CoreV1().Pods(v.Pod.Namespace).EvictV1(ctx, e); err != nil {
		return fmt.Errorf("evict %s/%s: %w", v.Pod.Namespace, v.Pod.Name, err)
	}
	return nil
}

func (m *Manager) event(pod *corev1.Pod, kind, reason, message string) {
	if m.recorder == nil {
		return
	}
	m.recorder.Event(pod, kind, reason, message)
}

// gcRecent trims the recently-evicted map so a long-lived agent does not grow
// one entry per eviction forever.
func (m *Manager) gcRecent(now time.Time) {
	for k, at := range m.recentlyEvicted {
		if now.Sub(at) > 10*time.Minute {
			delete(m.recentlyEvicted, k)
		}
	}
}

// tokenBucket is a plain rate limiter over a sliding refill window.
type tokenBucket struct {
	capacity int
	tokens   float64
	window   time.Duration
	last     time.Time
}

func newTokenBucket(capacity int, window time.Duration) *tokenBucket {
	return &tokenBucket{capacity: capacity, tokens: float64(capacity), window: window}
}

// configure updates capacity in place, which happens when the QoSPolicy
// changes. Tokens are clamped rather than reset so lowering the limit takes
// effect immediately and raising it does not hand out a burst.
func (b *tokenBucket) configure(capacity int, window time.Duration) {
	if capacity < 0 {
		capacity = 0
	}
	b.capacity = capacity
	b.window = window
	if b.tokens > float64(capacity) {
		b.tokens = float64(capacity)
	}
}

func (b *tokenBucket) take(now time.Time) bool {
	if b.capacity == 0 {
		return false
	}
	if b.last.IsZero() {
		b.last = now
	}
	elapsed := now.Sub(b.last)
	if elapsed > 0 {
		b.tokens += float64(b.capacity) * (float64(elapsed) / float64(b.window))
		if b.tokens > float64(b.capacity) {
			b.tokens = float64(b.capacity)
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
