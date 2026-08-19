// Package agent is the node-local half of kqos: sample the node, decide how
// much capacity is genuinely spare, enforce the tier contract in cgroups,
// publish the result, and evict when the promise can no longer be kept.
package agent

import (
	"context"
	"fmt"
	"runtime"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/kongxiangrui/kqos/pkg/agent/cgroup"
	"github.com/kongxiangrui/kqos/pkg/agent/collector"
	"github.com/kongxiangrui/kqos/pkg/agent/cpupool"
	"github.com/kongxiangrui/kqos/pkg/agent/eviction"
	"github.com/kongxiangrui/kqos/pkg/agent/sysadvisor"
	"github.com/kongxiangrui/kqos/pkg/apis/kqos/v1alpha1"
	"github.com/kongxiangrui/kqos/pkg/metrics"
	"github.com/kongxiangrui/kqos/pkg/qos"
	"github.com/kongxiangrui/kqos/pkg/usage"
)

// Options configures one agent instance.
type Options struct {
	// NodeName is the node this agent manages. Sourced from the downward API.
	NodeName string

	// CgroupRoot is where the unified hierarchy is mounted.
	CgroupRoot string

	// SysNodePath is where NUMA topology is exposed.
	SysNodePath string

	// Interval is the sampling and reconciliation period.
	Interval time.Duration

	// UsageEndpoint is the controller's ingestion base URL. Empty disables
	// usage publishing.
	UsageEndpoint string

	// EstimateWhenDegraded lets the agent fabricate samples when cgroups are
	// unreadable, for running the binary off-cluster. Never set in production.
	EstimateWhenDegraded bool

	// UsageFanout delivers each report to every controller replica behind the
	// endpoint rather than to one via load balancing. See usage.Publisher for
	// why load balancing is the wrong answer here.
	UsageFanout bool

	// EnforceCPUWeight turns on cgroup writes. Read-only mode is genuinely
	// useful: it lets an operator watch what kqos would advertise for a week
	// before letting it touch anything.
	EnforceCPUWeight bool
}

// Agent runs the node loop. It satisfies controller-runtime's Runnable so the
// manager owns its lifecycle, leader election and shutdown.
type Agent struct {
	opts Options

	client   client.Client
	kube     kubernetes.Interface
	recorder record.EventRecorder

	resolver  *cgroup.Resolver
	collector *collector.Collector
	advisor   *sysadvisor.Advisor
	pools     *cpupool.Manager
	evictor   *eviction.Manager
	publisher *usage.Publisher

	// disabledPlugins remembers the last applied policy so plugins are only
	// rebuilt when the list actually changes.
	disabledPlugins string
}

// New wires an agent from its dependencies.
func New(opts Options, c client.Client, kube kubernetes.Interface, recorder record.EventRecorder) *Agent {
	if opts.Interval <= 0 {
		opts.Interval = 10 * time.Second
	}
	fs := cgroup.New(opts.CgroupRoot)
	resolver := cgroup.NewResolver(fs)
	advisorCfg := sysadvisor.DefaultConfig()
	advisorCfg.IntervalSeconds = int32(opts.Interval / time.Second)

	topology := cpupool.DetectTopology(opts.SysNodePath, runtime.NumCPU())

	return &Agent{
		opts:      opts,
		client:    c,
		kube:      kube,
		recorder:  recorder,
		resolver:  resolver,
		collector: collector.New(fs, resolver, opts.EstimateWhenDegraded),
		advisor:   sysadvisor.New(advisorCfg),
		pools: cpupool.NewManager(fs, resolver, topology, cpupool.Config{
			SharedPoolMinCPUs:    2,
			ReclaimedPoolMinCPUs: 1,
		}),
		evictor:   eviction.NewManager(opts.NodeName, kube, recorder, eviction.BuildPlugins(nil)),
		publisher: usage.NewPublisher(opts.UsageEndpoint, 5*time.Second, opts.UsageFanout),
	}
}

// NeedLeaderElection reports false: every node needs its own agent running, so
// this Runnable must not be gated on leadership.
func (a *Agent) NeedLeaderElection() bool { return false }

// Start runs the loop until the context is cancelled.
func (a *Agent) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("agent").WithValues("node", a.opts.NodeName)
	logger.Info("starting kqos agent",
		"interval", a.opts.Interval,
		"cgroupRoot", a.opts.CgroupRoot,
		"enforce", a.opts.EnforceCPUWeight,
		"topologyZones", len(a.pools.Topology().Zones),
		"onlineCPUs", a.pools.Topology().TotalCPUs(),
		"evictionPlugins", eviction.RegisteredNames(),
		"usageTargets", a.publisher.Targets(),
	)

	ticker := time.NewTicker(a.opts.Interval)
	defer ticker.Stop()

	// The first tick establishes CPU counter baselines and produces no rates,
	// so run it immediately rather than making the agent look dead for one
	// interval after start-up.
	a.tick(ctx, logger)

	for {
		select {
		case <-ctx.Done():
			logger.Info("stopping kqos agent")
			return nil
		case <-ticker.C:
			a.tick(ctx, logger)
		}
	}
}

func (a *Agent) tick(ctx context.Context, logger interface {
	Error(error, string, ...any)
	Info(string, ...any)
}) {
	start := time.Now()
	defer func() {
		metrics.ReconcileDuration.WithLabelValues(a.opts.NodeName).Observe(time.Since(start).Seconds())
	}()

	policy, err := a.loadPolicy(ctx)
	if err != nil {
		logger.Error(err, "loading QoSPolicy; continuing with previous configuration")
	}
	a.applyPolicy(policy)

	node := &corev1.Node{}
	if err := a.client.Get(ctx, types.NamespacedName{Name: a.opts.NodeName}, node); err != nil {
		logger.Error(err, "getting node")
		metrics.CollectErrorsTotal.WithLabelValues(a.opts.NodeName, "node-get").Inc()
		return
	}

	pods, err := a.livePods(ctx)
	if err != nil {
		logger.Error(err, "listing pods")
		metrics.CollectErrorsTotal.WithLabelValues(a.opts.NodeName, "pod-list").Inc()
		return
	}

	collectStart := time.Now()
	sample, err := a.collector.Collect(pods, a.pools.Topology().TotalCPUs())
	metrics.CollectDuration.WithLabelValues(a.opts.NodeName).Observe(time.Since(collectStart).Seconds())
	if err != nil {
		logger.Error(err, "collecting node sample")
		metrics.CollectErrorsTotal.WithLabelValues(a.opts.NodeName, "collect").Inc()
		return
	}

	a.advisor.Observe(sample)
	rec := a.advisor.Recommend(node.Status.Allocatable, sample.Timestamp)

	if a.opts.EnforceCPUWeight {
		if err := a.pools.Reconcile(ctx, pods); err != nil {
			logger.Error(err, "reconciling cpu pools")
		}
	}

	a.exportMetrics(sample, rec)

	if err := a.report(ctx, node, sample, rec); err != nil {
		logger.Error(err, "writing NodeResourceProfile status")
	}

	if a.publisher.Enabled() {
		if err := a.publisher.Publish(ctx, buildReport(a.opts.NodeName, sample, pods)); err != nil {
			// Losing profiling data is not worth a stack trace every ten
			// seconds; log it at info and move on.
			logger.Info("usage publish failed", "err", err.Error())
		}
	}

	sig := eviction.Signal{
		Sample:         sample,
		Recommendation: rec,
		Allocatable:    node.Status.Allocatable,
		Config:         policy.Spec.Eviction,
		Pods:           evictionCandidates(pods),
		SampleByUID:    indexSamples(sample),
	}
	if res := a.evictor.Run(ctx, sig); res.Proposed > 0 {
		logger.Info("eviction pass",
			"proposed", res.Proposed,
			"evicted", len(res.Evicted),
			"suppressed", res.Suppressed,
			"pressure", rec.Pressure.Level,
		)
	}
}

// loadPolicy fetches the singleton QoSPolicy, synthesising defaults when it
// does not exist so the agent is useful before anyone has configured anything.
func (a *Agent) loadPolicy(ctx context.Context) (*v1alpha1.QoSPolicy, error) {
	policy := &v1alpha1.QoSPolicy{}
	err := a.client.Get(ctx, types.NamespacedName{Name: v1alpha1.DefaultQoSPolicyName}, policy)
	if err == nil {
		return policy, nil
	}
	def := DefaultPolicy()
	if apierrors.IsNotFound(err) {
		return def, nil
	}
	return def, err
}

// DefaultPolicy is the in-memory fallback when no QoSPolicy object exists.
func DefaultPolicy() *v1alpha1.QoSPolicy {
	cfg := sysadvisor.DefaultConfig()
	return &v1alpha1.QoSPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.DefaultQoSPolicyName},
		Spec: v1alpha1.QoSPolicySpec{
			Overcommit: v1alpha1.OvercommitConfig{
				CPUTargetUtilizationPercent:    cfg.CPUTargetPercent,
				MemoryTargetUtilizationPercent: cfg.MemoryTargetPercent,
				HeadroomPercent:                cfg.HeadroomPercent,
				MaxReclaimRatioPercent:         cfg.MaxReclaimPercent,
			},
			Advisor: v1alpha1.AdvisorConfig{
				WindowSeconds:   cfg.WindowSeconds,
				IntervalSeconds: cfg.IntervalSeconds,
				Algorithm:       cfg.Algorithm,
				EWMAAlpha:       "0.3",
			},
			Eviction: v1alpha1.EvictionConfig{
				Enabled:                        true,
				MemoryPressureThresholdPercent: cfg.MemoryPressureThresholdPercent,
				CPUPressureThresholdPercent:    cfg.CPUPressureThresholdPercent,
				CPUSomeStalledThresholdPercent: cfg.CPUStallThresholdPercent,
				MaxEvictionsPerMinute:          3,
				GracePeriodSeconds:             30,
				StabilisationSeconds:           30,
			},
			CPUSet: v1alpha1.CPUSetConfig{
				SharedPoolMinCPUs:    2,
				ReclaimedPoolMinCPUs: 1,
			},
			Webhook: v1alpha1.WebhookConfig{
				DefaultQoSLevel:           v1alpha1.QoSSharedCores,
				RewriteReclaimedResources: true,
			},
		},
	}
}

// applyPolicy pushes the live configuration into the sub-components. This is
// what makes the QoSPolicy hot-reloadable: nothing here restarts, it just
// starts using different numbers on the next tick.
func (a *Agent) applyPolicy(p *v1alpha1.QoSPolicy) {
	cfg := a.advisor.Config()
	cfg.WindowSeconds = p.Spec.Advisor.WindowSeconds
	cfg.IntervalSeconds = p.Spec.Advisor.IntervalSeconds
	cfg.Algorithm = p.Spec.Advisor.Algorithm
	cfg.EWMAAlpha = parseAlpha(p.Spec.Advisor.EWMAAlpha)
	cfg.CPUTargetPercent = p.Spec.Overcommit.CPUTargetUtilizationPercent
	cfg.MemoryTargetPercent = p.Spec.Overcommit.MemoryTargetUtilizationPercent
	cfg.HeadroomPercent = p.Spec.Overcommit.HeadroomPercent
	cfg.MaxReclaimPercent = p.Spec.Overcommit.MaxReclaimRatioPercent
	if q := p.Spec.Overcommit.MinReclaimCPU; q != nil {
		cfg.MinReclaimCPUMilli = q.MilliValue()
	}
	if q := p.Spec.Overcommit.MinReclaimMemory; q != nil {
		cfg.MinReclaimMemBytes = q.Value()
	}
	cfg.MemoryPressureThresholdPercent = p.Spec.Eviction.MemoryPressureThresholdPercent
	cfg.CPUPressureThresholdPercent = p.Spec.Eviction.CPUPressureThresholdPercent
	cfg.CPUStallThresholdPercent = p.Spec.Eviction.CPUSomeStalledThresholdPercent
	a.advisor.SetConfig(cfg)

	a.pools.SetConfig(cpupool.Config{
		CPUSetEnabled:        p.Spec.CPUSet.Enabled,
		ReservedSystemCPUs:   p.Spec.CPUSet.ReservedSystemCPUs,
		SharedPoolMinCPUs:    p.Spec.CPUSet.SharedPoolMinCPUs,
		ReclaimedPoolMinCPUs: p.Spec.CPUSet.ReclaimedPoolMinCPUs,
	})

	if key := fmt.Sprint(p.Spec.Eviction.DisabledPlugins); key != a.disabledPlugins {
		a.disabledPlugins = key
		a.evictor.SetPlugins(eviction.BuildPlugins(p.Spec.Eviction.DisabledPlugins))
	}
}

// livePods returns the non-terminal pods scheduled on this node. The cache is
// already field-filtered to this node by the manager's cache options, so this
// is a local read with no API traffic.
func (a *Agent) livePods(ctx context.Context) ([]*corev1.Pod, error) {
	var list corev1.PodList
	if err := a.client.List(ctx, &list, client.MatchingFields{"spec.nodeName": a.opts.NodeName}); err != nil {
		return nil, err
	}
	out := make([]*corev1.Pod, 0, len(list.Items))
	for i := range list.Items {
		pod := &list.Items[i]
		if qos.IsTerminal(pod) {
			continue
		}
		out = append(out, pod)
	}
	return out, nil
}

// evictionCandidates filters out pods no policy is allowed to touch, so
// plugins do not have to repeat the check.
func evictionCandidates(pods []*corev1.Pod) []*corev1.Pod {
	out := make([]*corev1.Pod, 0, len(pods))
	for _, pod := range pods {
		if !qos.LevelOf(pod).Evictable() || qos.IsMirrorPod(pod) || pod.DeletionTimestamp != nil {
			continue
		}
		out = append(out, pod)
	}
	return out
}

func indexSamples(s collector.NodeSample) map[string]collector.PodSample {
	out := make(map[string]collector.PodSample, len(s.Pods))
	for _, p := range s.Pods {
		out[p.UID] = p
	}
	return out
}

func parseAlpha(s string) float64 {
	var f float64
	if _, err := fmt.Sscanf(s, "%g", &f); err != nil || f <= 0 || f > 1 {
		return 0.3
	}
	return f
}
