// Command kqos-controller is the cluster-level half of kqos. It aggregates
// node profiles into advertised extended resources, maintains the cluster
// rollup, ingests the usage stream from the agents and turns it into workload
// resource recommendations.
package main

import (
	"context"
	"flag"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/kongxiangrui/kqos/pkg/agent"
	"github.com/kongxiangrui/kqos/pkg/apis/kqos/v1alpha1"
	"github.com/kongxiangrui/kqos/pkg/controller/overcommit"
	"github.com/kongxiangrui/kqos/pkg/controller/policy"
	"github.com/kongxiangrui/kqos/pkg/controller/profile"
	kqosmetrics "github.com/kongxiangrui/kqos/pkg/metrics"
	"github.com/kongxiangrui/kqos/pkg/usage"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr    string
		probeAddr      string
		usageAddr      string
		leaderElect    bool
		retention      time.Duration
		skipNamespaces string
		autoDiscover   bool
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address for the Prometheus endpoint.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address for health probes.")
	flag.StringVar(&usageAddr, "usage-bind-address", ":8090", "Address for the agent usage ingestion endpoint.")
	flag.BoolVar(&leaderElect, "leader-elect", true, "Run leader election so only one replica reconciles.")
	flag.DurationVar(&retention, "usage-retention", 10*time.Minute,
		"How long per-pod usage samples are kept in memory for workload profiling.")
	flag.StringVar(&skipNamespaces, "skip-namespaces", "kube-system,kube-public,kube-node-lease,local-path-storage,kqos-system",
		"Comma-separated namespaces excluded from automatic workload profiling.")
	flag.BoolVar(&autoDiscover, "auto-discover-workloads", true,
		"Create a WorkloadProfile for every Deployment automatically.")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	kqosmetrics.MustRegister()
	ctx := ctrl.SetupSignalHandler()

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "kqos-controller.kqos.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// The usage store lives in the leader's memory. Losing it on a failover is
	// acceptable and deliberate: the agents refill it within one retention
	// window, and persisting a high-churn metrics buffer would cost far more
	// than re-deriving it.
	store := usage.NewStore(retention, profile.NewOwnerResolver(mgr.GetClient()))

	if err := (&overcommit.Reconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up overcommit controller")
		os.Exit(1)
	}
	if err := (&policy.Reconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up policy controller")
		os.Exit(1)
	}
	if err := (&profile.Reconciler{Client: mgr.GetClient(), Store: store}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up workload profile controller")
		os.Exit(1)
	}
	if autoDiscover {
		d := &profile.DiscoveryReconciler{
			Client:         mgr.GetClient(),
			SkipNamespaces: splitAndTrim(skipNamespaces),
		}
		if err := d.SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to set up workload discovery controller")
			os.Exit(1)
		}
	}

	// Ingestion runs on every replica, not just the leader, so agents always
	// have somewhere to send to. Only the leader's store is read by the
	// reconcilers, which means followers buffer harmlessly and are already warm
	// if they win an election.
	if err := mgr.Add(&usage.Server{Addr: usageAddr, Store: store}); err != nil {
		setupLog.Error(err, "unable to add usage server")
		os.Exit(1)
	}
	if err := mgr.Add(&usage.Collector{Store: store, Interval: retention / 4}); err != nil {
		setupLog.Error(err, "unable to add usage garbage collector")
		os.Exit(1)
	}
	setupLog.Info("usage ingestion configured", "address", usageAddr, "path", usage.ReportPath)

	// Creating the default policy needs the cache, so it runs as a
	// leader-elected runnable after the manager has started rather than inline.
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		if err := policy.EnsureDefault(ctx, mgr.GetClient(), agent.DefaultPolicy().Spec); err != nil {
			setupLog.Error(err, "unable to create the default QoSPolicy")
			return err
		}
		setupLog.Info("default QoSPolicy is present")
		return nil
	})); err != nil {
		setupLog.Error(err, "unable to add policy bootstrap")
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

	setupLog.Info("starting kqos-controller", "leaderElection", leaderElect, "usageRetention", retention)
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "manager exited with error")
		os.Exit(1)
	}
}

func splitAndTrim(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
