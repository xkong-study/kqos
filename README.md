# kqos

A Kubernetes colocation resource governor: it measures what workloads actually
use, resells the difference to a lower service tier, and takes it back the
moment the owner wants it.

Every layer is real. The agent reads cgroup v2 and writes `cpu.weight` into
live pod cgroups; the controller patches genuine extended resources onto Node
objects; the admission webhook rewrites pod specs in flight; the whole thing
runs on a three-node kind cluster from `make up`.

```
$ kubectl get noderesourceprofiles
NAME                 PRESSURE   CPU%   MEM%   RECLAIM-CPU   RECLAIM-MEM
kqos-control-plane   None       1      7      6996m         3.9Gi
kqos-worker          None       18     1      6620m         3.4Gi
kqos-worker2         None       18     1      6619m         3.4Gi

$ kubectl get qospolicy default
NAME      NODES   READY   RECLAIM-CPU   OVERCOMMIT%   PRESSURE
default   3       3       11511m        28            0
```

Eleven and a half cores that did not exist a moment ago, on a cluster whose
nodes are 18% busy.

---

## The problem

Ask any cluster what it is doing and it will tell you it is full. Ask the
kernel and it will tell you it is idle. Both are true, because Kubernetes
schedules against *requests* and requests are a promise about the worst
case — sized during a load test, at the end of an incident, or by copying
another team's manifest.

From the demo workloads in `examples/`:

| workload | CPU request | measured p95 | waste |
|---|---|---|---|
| `web-frontend` | 600m | 87m | **85%** |
| `ads-ranking` | 1000m | 196m | 80% |

Those numbers come from `kubectl get workloadprofiles` on the running cluster,
not from a spreadsheet.

The safety margin is not wrong — it is what keeps the service fast during a
spike. It is just idle between spikes, and kqos lends it out in the meantime.

## The model

Four service tiers, declared per pod via `kqos.io/qos-level`:

| tier | contract | `cpu.weight` | evicted |
|---|---|---|---|
| `system_cores` | node agents, must survive anything | 10000 | never |
| `dedicated_cores` | exclusive CPUs, no noisy neighbours | 5000 | last |
| `shared_cores` | ordinary services, shared pool | 1000 | second |
| `reclaimed_cores` | runs on resold capacity | **10** | first |

The 100:1 ratio between `shared_cores` and `reclaimed_cores` is the number the
whole design rests on. Under contention a batch pod gets roughly one percent of
what a serving pod gets; on an idle node it runs flat out. A hard CPU quota
cannot express that, which is why kqos uses proportional weights rather than
`cpu.max`.

Pods with no annotation are classified from their native Kubernetes QoS class,
so kqos is safe to install on a cluster nobody has relabelled — Guaranteed pods
become `dedicated_cores`, BestEffort pods become the initial reclaimed tier.

## How much is spare

The advisor runs on every node, once per interval:

```
budget      = allocatable × cpuTargetUtilization%   (default 75%)
headroom    = allocatable × headroomPercent%        (default 10%)
reclaimable = budget − protectedUsage − headroom
              capped at allocatable × maxReclaimRatio%  (default 50%)
```

`protectedUsage` is the smoothed consumption of `system` + `dedicated` +
`shared` — **the reclaimed tier is deliberately excluded**. If reclaimed usage
counted, every batch pod that started would shrink the pool that admitted it,
and the node would oscillate: pack, shrink, evict, unpack, grow, pack. Leaving
it out makes the control loop monotone. (`TestReclaimedUsageDoesNotShrinkTheOffer`)

Three more decisions that are not obvious:

- **CPU uses a p95, memory uses the window maximum.** A percentile discards the
  top 5% of observations, which for memory are exactly the moments that would
  have OOMed the node. (`TestMemoryUsesWindowMaxNotPercentile`)
- **Growth is damped, shrinking is not.** The offer closes 25% of the gap per
  tick on the way up and drops instantly on the way down. A node that has just
  been drained looks gloriously empty; advertising all of it at once invites a
  stampede the next traffic peak has to evict.
  (`TestGrowthIsDampedButShrinkIsImmediate`)
- **Critical pressure withdraws everything.** Setting the offer to zero stops
  new reclaimed pods landing while the eviction manager deals with the ones
  already there. (`TestCriticalPressureWithdrawsEverything`)

Pressure is classified from utilisation *and* PSI, because they fail in
opposite directions: utilisation misses a node whose threads are runnable and
being denied CPU, PSI is noisy on a node that is merely busy.

## How the capacity is sold

**As an extended resource, never by inflating `cpu`.**

Inflating the node's real CPU is simpler and is what naive overcommit does. It
is also unrecoverable, because it lies to every component at once — scheduler
predicates, kubelet admission, HPA utilisation maths, and every dashboard in
the company all start working from a capacity that does not exist.

Instead the webhook rewrites reclaimed pods:

```yaml
# what you write                    # what the API server stores
requests:                           requests:
  cpu: 1500m            ───▶          kqos.io/reclaimed-cpu: "1500"
  memory: 256Mi                       kqos.io/reclaimed-memory: "256"
                                      memory: 256Mi
                                    limits:
                                      kqos.io/reclaimed-cpu: "1500"
                                      memory: 256Mi
```

After admission the pod has **no CPU request at all**. It is invisible to the
scheduler's real-capacity accounting and can only land where the controller has
advertised `kqos.io/reclaimed-cpu`. Guaranteed workloads are still scheduled
against genuine capacity; only pods that opted in can consume the invented kind.
The original spec is preserved in `kqos.io/original-resources`, because
debugging "why does my pod have no CPU request?" without it is miserable.

**Memory is not oversold, and that asymmetry is deliberate.** The memory limit
survives the rewrite — without a runtime ceiling a reclaimed pod could take the
node down before any policy noticed — and Kubernetes then defaults the memory
*request* back from that limit, so the pod still occupies real memory in the
scheduler's books. kqos does not fight this. Overselling an incompressible
resource means the only way to honour the promise is to kill something, and a
design whose steady state is "kill something" is not a design.
`kqos.io/reclaimed-memory` therefore acts as a ceiling on how much batch work a
node accepts, not as a currency.

## Taking it back

A plugin framework, three policies, and the machinery that stands between a
plugin's opinion and a pod actually dying.

| plugin | trigger | victims |
|---|---|---|
| `memory-pressure` | node memory ≥ 85% | reclaimed first, biggest first, only enough to reach 80% |
| `cpu-suppression` | PSI `cpu some avg10` ≥ 40% | largest reclaimed pod, **one per tick** |
| `reclaimed-overrun` | a reclaimed pod ≥ 3× its request, while under pressure | worst offender first |

Every guard exists because the failure it prevents is worse than the pressure
it responds to:

- **Stabilisation.** A threshold must stay breached for 30s. The clock resets
  when pressure clears, so two brief unrelated spikes never add up to an
  eviction.
- **Rate limit.** Three per minute, token-bucket. A metrics glitch cannot drain
  a node.
- **Degraded samples are refused outright.** If the cgroup hierarchy is
  unreadable the agent produces estimates, and estimates never kill anything.
- **The eviction API, not `delete`.** PodDisruptionBudgets are enforced by the
  API server; a 429 is the application saying it cannot afford to lose this
  replica, and kqos records the refusal rather than overriding it.
- **`system_cores`, mirror pods and system-critical priorities are vetoed**
  regardless of what any plugin says.

Observed on the running cluster under load:

```
"msg":"eviction pass","node":"kqos-worker","proposed":6,"evicted":0,
"suppressed":{"stabilising":6}
```

Six evictions proposed, six held back by the stabilisation window — the
policies and the brakes both doing their jobs.

## Architecture

```
                        ┌──────────────────────────────┐
   control plane        │  QoSPolicy  (hot-reloadable) │
   (CRDs, etcd)         │  NodeResourceProfile / node  │
                        │  WorkloadProfile / workload  │
                        └──────────────────────────────┘
                            ▲                  │
              status writes │                  │ watch
                            │                  ▼
  ┌──────────────┐    ┌─────┴────────┐   ┌─────────────────┐
  │ kqos-webhook │    │  kqos-agent  │   │ kqos-controller │
  │  (Deploy ×2) │    │ (DaemonSet)  │   │   (Deploy ×2)   │
  ├──────────────┤    ├──────────────┤   ├─────────────────┤
  │ classify     │    │ collector    │   │ overcommit  ────┼──▶ Node
  │ validate     │    │ sysadvisor   │   │ policy rollup   │  extended
  │ rewrite      │    │ cpupool ─────┼──▶│ profiler        │  resources
  └──────────────┘    │ eviction ────┼─┐ └─────────────────┘
                      └──────┬───────┘ │        ▲
                             │         │        │ usage reports
                       cgroup v2       │        │ (HTTP, fan-out)
                       /sys/fs/cgroup  └────────┴─▶ eviction API
```

**Two planes, on purpose.** Policy, recommendations and node profiles go
through CRDs, where they belong. Per-pod usage does not: it is high-frequency,
high-cardinality and worthless a minute later, so writing it through etcd would
put an amplification of (pods × nodes × sample rate) onto the cluster's most
contended component to store data that is immediately discarded. It goes over a
direct HTTP path that can drop samples without anybody caring.

**Agents fan out to every controller replica** rather than through a
load-balanced Service. This was a bug before it was a design: only the elected
leader runs the reconcilers that read the usage store, a ClusterIP Service does
not know which replica that is, and conntrack pins each client to whichever
backend it first reached. Every agent spent its life reporting to a follower
whose store nobody read. Nothing errored — ingestion succeeded, metrics looked
healthy, and the workload profiles were simply always empty. The fix is a
headless Service and one small POST per replica; a replica that wins an
election now inherits a warm store instead of starting blind.

## Running it

```bash
make up      # kind cluster + image + CRDs + webhook TLS + all components
make demo    # the example workloads
make status  # what kqos currently believes
```

`make up` needs Docker, kind, kubectl, Go 1.26 and openssl. Nothing else —
webhook certificates are self-signed by `hack/gen-webhook-certs.sh`, because a
demo that requires installing cert-manager first is a demo nobody runs.

To watch the eviction path, apply the stress load (kept out of `examples/` so
`make demo` does not pull it in) and tail the agent:

```bash
kubectl apply -f examples/stress/cpu-pressure.yaml
make logs-agent
```

Other targets: `make check` (fmt + vet + test), `make redeploy` after a code
change, `make events` for evictions, `make kind-down` to tear it all down.

## Verifying it is real

```bash
# cgroup weights actually written, per tier
NODE=$(kubectl -n kqos-demo get pod -l app=batch-etl -o jsonpath='{.items[0].spec.nodeName}')
UID=$(kubectl -n kqos-demo get pod -l app=batch-etl -o jsonpath='{.items[0].metadata.uid}' | tr - _)
docker exec $NODE sh -c "find /sys/fs/cgroup -name '*pod$UID*' -exec cat {}/cpu.weight \;"
# 10

# extended resources actually advertised
kubectl get nodes -o custom-columns='NODE:.metadata.name,\
RECL-CPU:.status.allocatable.kqos\.io/reclaimed-cpu'

# the rewrite actually happened
kubectl -n kqos-demo get pod -l app=batch-etl -o jsonpath='{.items[0].spec.containers[0].resources}'
```

## Layout

```
cmd/            three binaries: agent, controller, webhook
pkg/apis/       the three CRDs
pkg/agent/      cgroup, collector, sysadvisor, cpupool, eviction
pkg/controller/ overcommit, policy rollup, workload profiler
pkg/webhook/    admission: classify, validate, rewrite
pkg/usage/      the measurement data plane
pkg/cpuset/     Linux cpuset list algebra
deploy/         CRDs, kind config, manifests
examples/       demo workloads; examples/stress/ drives real pressure
```

`make check` runs 9 test packages. The interesting ones are
`pkg/agent/sysadvisor` (the reclaim formula and its damping),
`pkg/agent/eviction` (every safety guard, against a fake clientset) and
`pkg/agent/cgroup` (a synthetic cgroup tree, so the reader is testable off
Linux).

## What is not here

- **No scheduler plugin.** Reclaimed pods are placed correctly by the default
  scheduler, because extended resources already constrain them to nodes with
  advertised capacity. A plugin would add *scoring* — preferring the least
  pressured node — which is a refinement, not a requirement.
- **`cpuset` actuation is off by default** (`cpuSet.enabled: false`). The pool
  manager computes NUMA-aware exclusive assignments and reports them, but
  writing `cpuset.cpus` needs the cpuset controller delegated down to pod
  cgroups, which is not true everywhere. `cpu.weight` is always on and does the
  load-bearing work.
- **No dashboards.** Every component exports Prometheus metrics on `:8080`;
  nothing scrapes them in this setup.
- **kind reports the host VM's CPU count on every node**, so all three nodes
  claim the same capacity. Node-level readings come from each node's own cgroup
  root rather than `/proc`, which keeps them per-node and correct; the
  `allocatable` figure is still whatever kubelet says.
