// Command kqos-agent is the node-local daemon. One instance runs per node as
// a DaemonSet, samples cgroup v2, decides how much capacity is spare, enforces
// the QoS contract and evicts when it can no longer be honoured.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/xkong-study/kqos/pkg/agent"
	"github.com/xkong-study/kqos/pkg/agent/cgroup"
	"github.com/xkong-study/kqos/pkg/apis/kqos/v1alpha1"
	kqosmetrics "github.com/xkong-study/kqos/pkg/metrics"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		nodeName      string
		cgroupRoot    string
		sysNodePath   string
		interval      time.Duration
		usageEndpoint string
		metricsAddr   string
		probeAddr     string
		enforce       bool
		usageFanout   bool
		estimate      bool
	)

	flag.StringVar(&nodeName, "node-name", os.Getenv("NODE_NAME"),
		"Name of the node this agent manages. Normally injected via the downward API.")
	flag.StringVar(&cgroupRoot, "cgroup-root", cgroup.DefaultRoot,
		"Mount point of the cgroup v2 unified hierarchy.")
	flag.StringVar(&sysNodePath, "sys-node-path", "/host/sys/devices/system/node",
		"Path to the kernel's NUMA topology directory.")
	flag.DurationVar(&interval, "interval", 10*time.Second,
		"Sampling and reconciliation period.")
	flag.StringVar(&usageEndpoint, "usage-endpoint", os.Getenv("KQOS_USAGE_ENDPOINT"),
		"Base URL of the controller's usage ingestion endpoint. Empty disables workload profiling.")
	flag.BoolVar(&usageFanout, "usage-fanout", true,
		"Deliver each usage report to every address behind the endpoint. Requires a headless Service; "+
			"without it reports reach only one controller replica, which may not be the elected leader.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address for the Prometheus endpoint.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address for health probes.")
	flag.BoolVar(&enforce, "enforce", true,
		"Write cpu.weight and cpuset values to cgroups. Disable to run kqos in observe-only mode.")
	flag.BoolVar(&estimate, "estimate-when-degraded", false,
		"Fabricate samples when cgroups are unreadable. For off-cluster development only.")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	if nodeName == "" {
		setupLog.Error(fmt.Errorf("node name is empty"),
			"the agent must know which node it manages; set --node-name or the NODE_NAME environment variable")
		os.Exit(1)
	}

	kqosmetrics.MustRegister()

	// SetupSignalHandler installs process-wide signal handlers and panics if
	// called twice, so the context is established once and threaded through.
	ctx := ctrl.SetupSignalHandler()

	// The cache is restricted to this node's pods. Without this every agent in
	// the cluster would hold every pod in memory and watch every pod change --
	// on a large cluster that is the single biggest cost of running a node
	// agent, and it is entirely avoidable.
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         false,
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&corev1.Pod{}: {
					Field: fields.OneTermEqualSelector("spec.nodeName", nodeName),
				},
			},
		},
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// The agent lists pods by node name from its own cache, so that field has
	// to be indexed even though the cache is already filtered to it.
	if err := mgr.GetFieldIndexer().IndexField(ctx, &corev1.Pod{}, "spec.nodeName",
		func(o client.Object) []string {
			pod, ok := o.(*corev1.Pod)
			if !ok || pod.Spec.NodeName == "" {
				return nil
			}
			return []string{pod.Spec.NodeName}
		}); err != nil {
		setupLog.Error(err, "unable to index pods by node name")
		os.Exit(1)
	}

	kube, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		setupLog.Error(err, "unable to build kubernetes client")
		os.Exit(1)
	}

	recorder := mgr.GetEventRecorderFor("kqos-agent")

	a := agent.New(agent.Options{
		NodeName:             nodeName,
		CgroupRoot:           cgroupRoot,
		SysNodePath:          sysNodePath,
		Interval:             interval,
		UsageEndpoint:        usageEndpoint,
		UsageFanout:          usageFanout,
		EnforceCPUWeight:     enforce,
		EstimateWhenDegraded: estimate,
	}, mgr.GetClient(), kube, recorder)

	if err := mgr.Add(a); err != nil {
		setupLog.Error(err, "unable to add agent runnable")
		os.Exit(1)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting kqos-agent", "node", nodeName, "interval", interval, "enforce", enforce)
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "manager exited with error")
		os.Exit(1)
	}
}
